# AGENTS.md

This is the single source of guidance for coding agents working in this repository. `CLAUDE.md` only points here. Maintain this file, not copies of it.

## Project

Workstave is a local-first persistent work runtime written in Go. Several independent conversations share one Work, and sessions of different agent harnesses (currently Claude Code CLI and Codex CLI) are coordinated by one controller. Status: experimental, first milestone (P0-A, unified session runtime). Nothing here is a finished product; do not describe unverified behaviour as working.

## Design baseline

The design documents live in the local-only `design/` directory (git-ignored, never committed or pushed): Architecture Vision v3.3, Engineering Contracts (C1-C6) and Implementation Plan. If they are present, read the relevant section before changing behaviour. Do not edit them unless the user asks. If real harness behaviour contradicts a contract, record the evidence and propose the smallest adjustment; do not hide the conflict behind a compatibility layer.

Invariants that code must keep:

- Work owns state. Conversations, sessions, harnesses and processes are replaceable.
- A logical session is not a native session. Native ids live in a binding; only one binding per session is active (P0).
- Authoritative state is the persisted record. A model's native context is a cache that may be stale.
- Agents propose, the kernel enforces. Identity, authorization, state transitions and version checks are code, not prompts.
- Task responsibility is separate from a run (one execution attempt). A failed or lost run does not erase the task.
- Unknown is a first-class result. A run without a terminal event is `unknown`, never success or failure, and is reconciled, not blindly retried.
- A request being accepted is not the same as the work being done. Cancel accepted is not stopped. Pause stops new dispatch authorization only.

## Engineering rules

- Go-first, modular monolith. Modules are responsibility boundaries, not processes or services.
- High cohesion, low coupling, one-way dependencies. No god files, god objects or catch-all managers. Review a file that grows past roughly 400-500 lines.
- No interface without a real caller and a real second implementation or test need. No speculative abstractions, plugin systems, frameworks or placeholder modules.
- Errors are returned with context. Never swallow an error, retry unconditionally, or turn an unobserved result into success.
- Every goroutine, process, channel and connection has a named owner and an exit path. Context cancellation must reach child processes.
- Validate at boundaries (external input, authorization, state-owner transitions). Do not repeat defensive checks in every layer.
- Add a dependency only with a concrete use. Prefer the standard library. SQLite uses the pure-Go `modernc.org/sqlite` driver (no CGO).
- No long-lived transitional code. Migration code needs scope, verification and an exit condition.
- Never put secrets in code, tests, logs, commits or docs. Redact API-key-like strings before storing harness output.

## Harness rules (from real experiments)

- Use the user's own harness configuration (provider, endpoint, keys, MCP, plugins). Never override or silently disable it, and do not use `--ignore-user-config`.
- Decide a run's outcome from terminal events only (`internal/outcome`). Exit codes are not trusted: Claude Code exits 1 on budget exhaustion with a result event; a killed process of either harness exits 1 with no terminal event.
- Codex item-level `error` events can be configuration warnings. Only `turn.failed` means failure.
- Claude Code lets the caller pre-assign the session id (`--session-id`); Codex reports its thread id only after creation. Persist the binding and operation id before launching.
- `clientUserMessageId` on Codex `turn/start` does not deduplicate. Idempotency must come from the kernel's own operation id.
- Process trees must be terminated as a tree (`internal/procgroup`): process group on Unix, `taskkill /T /F` on Windows. Cancelling a context alone leaves grandchildren running.
- Resume keeps the native id, but Claude Code needs the same cwd, and `codex exec resume` cannot change sandbox or root.
- Do not claim a capability that is only observed once. Label it as observed, with its limits.

## Working method

Read, inspect, plan briefly, implement, test, verify, report. Make the smallest real vertical slice and run it.

- Separate real-harness evidence from fake or mocked tests. A fake harness test proves ordering and error paths only. Never present mocks, stubs or empty implementations as a successful harness integration.
- Label every unverified capability as one of: Not Tested, Unsupported, Blocked, Unknown, with the reason. Never fabricate results.
- When a test fails, find the cause before changing it. Do not weaken a test to make it pass.
- Stop and ask the user when: a core invariant would change, a real harness cannot meet a required contract, credentials or authorization are missing, an external side effect cannot be controlled, or an action is destructive, global or costs money.
- Do not install software globally, delete data you did not create, or force-push without explicit approval.

## Verification

Requires Go 1.26 or newer on a 64-bit toolchain (32-bit Go cannot run the race detector).

```
go fmt ./...
go vet ./...
go test ./...
```

- Also check the cross-compile targets with `GOOS`/`GOARCH` set to linux and darwin (amd64, arm64) using `go vet` and `CGO_ENABLED=0`. That proves compilation only.
- `go test -race ./...` needs CGO and a C compiler.
- Tests that talk to real harnesses are opt-in and billable: set `WORKSTAVE_REAL_HARNESS=1`. They use the user's configuration. Run only the test you need and keep budgets low.
- Windows-only process tests carry `//go:build windows`. After process tests, check that no test processes were left behind.
- Linux behaviour can be checked in a `golang` container with the source mounted read-only. macOS is not run locally.

## Layout

- `cmd/workstave`: minimal CLI (`run`, `recover`).
- `internal/claudecode`, `internal/codex`: harness drivers for Claude Code and `codex exec --json`.
- `internal/appserver`: minimal synchronous JSON-RPC client for `codex app-server` (experiments).
- `internal/procgroup`: process-tree termination per platform.
- `internal/outcome`: completed, failed or unknown classification.
- `internal/store`: SQLite sessions, native bindings, runs.
- `internal/runner`: persists binding, operation id and run before launch, records the outcome.

Not built yet, and not to be added without a real need: scheduler, context compiler, multiple conversations, desktop client, smart router, web UI.

## Git and secrets

- Commit and push only when the user asks. Keep commits focused; titles are plain imperative sentences without milestone prefixes. Do not add co-author or attribution trailers.
- Never commit `design/`, `docs/`, `.scratch/`, databases, `.env` files or logs; they are git-ignored.
- Never write a token into a remote URL, git config or any file. If the user supplies a temporary token, pass it only as a one-shot HTTP header on a single command, then verify the remote head and that no auth header was stored.
- Scan changed files for key-like strings before every commit.

## Planned client stack (not started)

CLI and desktop are both first-class clients of one local daemon API (HTTP/JSON plus SSE, bound to loopback with a local token). Desktop is Electron with React and TypeScript as a thin client: it never talks to harnesses or SQLite directly and does not own the daemon's lifecycle. Closing a window is a detach, not a stop. Start the desktop client only after the daemon API for conversations, event subscription and control is stable.
