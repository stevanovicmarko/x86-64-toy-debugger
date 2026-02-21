# AGENTS.md

This file provides guidance to AI coding assistants working with this repository.

## Project Overview

A toy x86-64 debugger written in Go, inspired by the C++ project in *Building a Debugger* by No Starch Press. The debugger is Linux-only, can launch or attach to processes via ptrace and has an interactive REPL with the `continue` command. It is not intended for production use. It can be used on other operating systems by running it inside a container (Docker or Podman).

## Commands

```bash
# Build the CLI binary
go build ./cmd/toydbg

# Run the CLI
go run ./cmd/toydbg

# Run all tests
go test ./...

# Run a single test
go test ./test/ -run TestValidateEnvironment

# Run tests with verbose output
go test ./... -v

# Sync dependencies after editing go.mod or adding imports
go mod tidy

# Build Docker image (for macOS / Windows; substitute podman for docker if needed)
docker build -t toydbg . # or podman build -t toydbg .

# Run inside Docker / Podman
docker run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program # or podman run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
```

## Architecture

### Package Layout

| Package | Role | Visibility |
|---------|------|------------|
| `debugger/` | Public library — all debugger primitives (processes, breakpoints, registers, etc.) | **Exported** |
| `internal/` | Private implementation details | **Private** (compiler-enforced) |
| `cmd/toydbg/` | CLI binary with interactive REPL | Consumes `debugger/` |
| `test/` | Black-box integration tests | Consumes `debugger/` |
| `docs/` | Documentation and notes | — |

### Constraints

- The `debugger` package is the **sole public API surface**. The CLI and tests must go through it; neither should import `internal/` directly.
- Go enforces the `internal/` boundary at compile time — any import of `internal/` from outside this module will fail to build.
- The `debugger` package requires **Linux** (ptrace syscalls). See [Platform-Specific Setup](#platform-specific-setup) below for how to run on each OS.

### Dependencies

- `github.com/chzyer/readline` — interactive command line (equivalent to `libedit` / what LLDB uses).

## Conventions

- **Exported symbols** use PascalCase (Go convention). Unexported symbols use camelCase.
- **Tests** live in `test/` as a separate package to enforce black-box testing through the public API.
- **Error handling** follows Go idioms: return `error` values, don't panic.
- Run `go mod tidy` after adding or removing imports.
- **Diagram regeneration:** After modifying `.go` files in `cmd/`, `debugger/`, or `internal/`, run `go generate ./...` and commit the updated `docs/code-flow.mmd` and `docs/program-flow.mmd`.

## Platform-Specific Setup

### Linux (native — recommended)

Build and run directly — no container needed:

```bash
go build ./cmd/toydbg
./toydbg /path/to/program
```

Containers also work on Linux but native execution is preferred (less overhead, no extra security flags).

### macOS (via Docker / Podman)

macOS does not support ptrace in the way the debugger requires. Use a Linux container:

```bash
docker build -t toydbg .   # or: podman build -t toydbg .
docker run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
```

### Windows (via WSL2)

Use WSL2, which provides a real Linux kernel with full ptrace support:

```bash
# Inside a WSL2 terminal
go build ./cmd/toydbg
./toydbg /path/to/program
```

Alternatively, use Docker Desktop or Podman with the same container commands as macOS above. WSL**1** does *not* support ptrace — you must use WSL**2** or containers.

> **⚠ Container ptrace note:** Containers drop `SYS_PTRACE` by default, so `ptrace(PTRACE_TRACEME, ...)` inside `Launch` would fail with `EPERM`. The default seccomp profile also blocks ptrace. Both `--cap-add=SYS_PTRACE` and `--security-opt seccomp=unconfined` are required. Podman is rootless by default, so these capabilities are granted explicitly without running as root on the host.

## Key Files

- `cmd/toydbg/main.go` — CLI entry point: argument parsing, attach logic, REPL, command dispatch.
- `debugger/process.go` — process primitives: Launch, AttachPID, Resume, WaitOnSignal.
- `debugger/debugger.go` — public library root.
- `test/debugger_test.go` — integration tests.
- `tools/flowgen/main.go` — code-flow diagram generator (Mermaid sequence diagrams).
- `tools/progflow/main.go` — program-flow diagram generator (Mermaid flowcharts with control flow).
- `docs/code-flow.mmd` — generated Mermaid sequence diagram (auto-generated, do not hand-edit).
- `docs/program-flow.mmd` — generated Mermaid flowchart (auto-generated, do not hand-edit).
