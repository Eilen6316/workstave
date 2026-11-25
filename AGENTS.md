# AGENTS.md

This is the single, long-lived guidance file for coding agents and human contributors working in this repository. `CLAUDE.md` only points here. It is meant to change rarely: it holds principles and working methods, not status. Facts that change (experiment results, package lists, progress, plans) belong in code, tests, README, package comments or local notes, never here.

## Project

Workstave is a local-first persistent work runtime written in Go. Several independent conversations share one Work, and sessions of different agent harnesses are coordinated by one controller. The project is early-stage and experimental. Never describe unverified behavior as working.

## Source of truth

Behavior is defined by the code, its tests, the README and package comments. Design documents may exist in a maintainer's local-only `design/` directory that is not part of this repository. If you do not have them, do not assume their content; ask the maintainer. If you do have them, read the relevant section before changing behavior and do not edit them unless asked. If real harness behavior contradicts a documented contract, record the evidence and propose the smallest adjustment; do not hide the conflict behind a compatibility layer.

## Invariants

- Work owns state. Conversations, sessions, harnesses and processes are replaceable.
- A logical session is not a native session. Native identity lives in a binding, and a session has at most one active binding at a time.
- Authoritative state is the persisted record. A model's native context is a cache and may be stale.
- Agents propose, the kernel enforces. Identity, authorization, state transitions and version checks are code, not prompts.
- Task responsibility is separate from a run (one execution attempt). A failed or lost run does not erase the task.
- Unknown is a first-class result. An execution whose end was not observed is unknown, never success or failure, and is reconciled, not blindly retried.
- Accepted is not done. A cancel request being accepted is not the process being stopped. Pause only stops new dispatch authorization.

## Engineering rules

- Go-first modular monolith. Modules are responsibility boundaries, not processes or services.
- High cohesion, low coupling, one-way dependencies. No god files, god objects or catch-all managers. Review any file that grows past roughly 400-500 lines for cohesion.
- Introduce an interface only for a real caller and a real second implementation or test need. No speculative abstractions, frameworks, plugin systems or placeholder modules.
- Return errors with context. Never swallow an error, retry unconditionally, or turn an unobserved result into success.
- Every goroutine, process, channel and connection has a named owner and an exit path. Context cancellation must reach child processes.
- Validate at boundaries: external input, authorization, and state-owner transitions. Do not repeat defensive checks in every layer.
- Add a dependency only for a concrete use. Prefer the standard library and pure-Go dependencies that avoid CGO.
- No long-lived transitional code. Migration code needs a scope, verification and an exit condition.
- Never put secrets in code, tests, logs, commits or docs. Redact key-like strings before storing any harness output.

## Harness integration principles

- Use the harness configuration of whoever runs the code (provider, endpoint, keys, MCP, plugins). Never override or silently disable it.
- Decide a run's outcome from terminal events only. Exit codes are not trusted.
- Persist identities (binding, operation id, run) before launching an external process, so a crash between creation and acknowledgement can be reconciled.
- Idempotency comes from the kernel's own stable operation id, not from harness-provided message ids.
- Terminate processes as a whole tree. Cancelling a context alone may leave descendants running.
- A capability observed once is an observation, not a guarantee. State it with its limits.
- Harness-specific findings are recorded as tests and local notes, not in this file.

## Working method

Read, inspect, plan briefly, implement, test, verify, report. Build the smallest real vertical slice and run it.

- Keep real-harness evidence separate from fake or mocked tests. A fake harness proves ordering and error paths only. Never present mocks, stubs or empty implementations as a successful integration.
- Label every unverified capability as Not Tested, Unsupported, Blocked or Unknown, with the reason. Never fabricate results.
- When a test fails, find the cause before changing it. Do not weaken a test to make it pass.
- Stop and ask the person directing the work when a core invariant would change, a real harness cannot meet a required contract, credentials or authorization are missing, an external side effect cannot be controlled, or an action is destructive, global or costs money.
- Do not install software globally, delete data you did not create, or force-push without explicit approval.

## Verification

Use a 64-bit Go toolchain at the version declared in `go.mod`.

```
go fmt ./...
go vet ./...
go test ./...
```

- Also run `go vet` with `GOOS`/`GOARCH` set for the other supported platforms (linux and darwin, amd64 and arm64) with `CGO_ENABLED=0`. That proves compilation only, not behavior.
- `go test -race ./...` needs CGO and a C compiler.
- Tests that talk to real harnesses are opt-in and billable: set `WORKSTAVE_REAL_HARNESS=1`. They use your own harness configuration and credentials. Run only the test you need and keep budgets low.
- Platform-specific tests carry build tags. After process tests, check that no test processes were left behind.
- Linux behavior can be checked in a container with the source mounted read-only. State plainly which platforms were actually run.

## Structure

Entry points are under `cmd/`, modules under `internal/`. Each package states its single responsibility in its package comment; read the tree rather than relying on a list here. Do not add scheduler, context compiler, router, UI or other large subsystems without a real, current need and the maintainers' agreement.

## Git and secrets

- Commit and push only when the person directing you asks. Keep commits focused. Titles are plain imperative sentences without milestone prefixes. Do not add agent or co-author attribution trailers.
- Never commit local-only paths listed in `.gitignore` (such as `design/`, `docs/`, `.scratch/`), databases, `.env` files or logs.
- Never write a token into a remote URL, git config or any file. Pass a temporary credential only for a single command, and verify afterward that it was not stored.
- Scan changed files for key-like strings before every commit.
