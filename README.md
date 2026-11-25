# Workstave

[English](#english) | [简体中文](#简体中文)

## English

Workstave is an early-stage, local-first persistent work runtime written in Go. The goal is to let several independent conversations share one Work, and to coordinate sessions of different agent harnesses (currently Claude Code and Codex CLI) through a single controller.

**Status: experimental.** The project is in its first milestone (a unified session runtime). It is not usable as a product yet. What exists today:

- Minimal drivers for the Claude Code CLI and the Codex CLI (`internal/claudecode`, `internal/codex`, `internal/appserver`).
- Cross-platform process-tree termination (`internal/procgroup`): process groups on Linux and macOS, `taskkill /T /F` on Windows.
- Run outcome classification from terminal events only (`internal/outcome`).
- A SQLite store for sessions, native bindings and runs (`internal/store`) and a first runner (`internal/runner`).

### Important notes

- **A run without a terminal event is recorded as `unknown`**, never as success or failure. Exit codes are not trusted.
- Workstave uses **your own harness configuration** (provider, endpoint, keys). It does not override it. Never commit API keys or tokens.
- Tests that talk to a real harness are **opt-in and billable**: they only run with `WORKSTAVE_REAL_HARNESS=1`.
- Verification so far: Windows (amd64) is the main platform; Linux (amd64) was tested for local tests in a container; macOS and the other architectures are only cross-compile checked.

### Build and test

Requires Go 1.26 or newer. The harness CLIs are only needed for the opt-in real-harness tests.

```
go vet ./...
go test ./...
```

`go test -race ./...` additionally needs CGO and a C compiler.

### License

Licensed under the Apache License, Version 2.0. See [LICENSE](LICENSE).

## 简体中文

Workstave 是一个使用 Go 编写、处于早期阶段的本地优先持久化工作运行时。目标是让多个独立对话共享同一项工作（Work），并通过同一个控制器协调不同 Agent Harness（目前是 Claude Code 与 Codex CLI）的会话。

**状态：实验阶段。** 项目处于第一个里程碑（统一的 Session 运行时），目前还不能作为产品使用。已有内容：

- Claude Code CLI 与 Codex CLI 的最小驱动（`internal/claudecode`、`internal/codex`、`internal/appserver`）。
- 跨平台的进程树终止（`internal/procgroup`）：Linux 和 macOS 使用进程组，Windows 使用 `taskkill /T /F`。
- 仅依据终态事件的运行结果分类（`internal/outcome`）。
- 用于保存 Session、Native Binding 和 Run 的 SQLite 存储（`internal/store`）以及第一个 runner（`internal/runner`）。

### 重要说明

- **没有终态事件的运行一律记为 `unknown`**，不会记成成功或失败，也不信任退出码。
- Workstave 使用**你自己的 Harness 配置**（provider、endpoint、密钥），不会覆盖它。请不要提交 API Key 或令牌。
- 会连接真实 Harness 的测试**需要主动开启且会产生费用**：只有设置 `WORKSTAVE_REAL_HARNESS=1` 才会运行。
- 目前的验证情况：Windows（amd64）是主要平台；Linux（amd64）在容器里跑过本地测试；macOS 和其他架构只做了交叉编译检查。

### 构建与测试

需要 Go 1.26 或更新版本。只有主动开启的真实 Harness 测试才需要安装对应的 CLI。

```
go vet ./...
go test ./...
```

`go test -race ./...` 另外需要 CGO 和 C 编译器。

### 许可证

基于 Apache License 2.0 授权，详见 [LICENSE](LICENSE)。
