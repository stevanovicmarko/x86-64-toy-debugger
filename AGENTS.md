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

# Build container image (use whichever runtime is available: podman or docker)
podman build -t toydbg .
# docker build -t toydbg .

# Run inside container (requires ptrace capabilities)
podman run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
# docker run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
#   -it toydbg /path/to/program
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
- **Sequence diagram:** The file [`docs/sequence-diagram.mmd`](docs/sequence-diagram.mmd) is the canonical Mermaid sequence diagram documenting the debugger's attach-and-REPL lifecycle (User, Debugger, Kernel, Tracee). When adding new debugger features or changing the attach/resume/wait flow, update the diagram to reflect the new interactions. Every agentic coding flow that modifies the debugger's control flow **must** update this diagram and commit it alongside the code changes.

## Platform-Specific Setup

### Linux (native — recommended)

Build and run directly — no container needed:

```bash
go build ./cmd/toydbg
./toydbg /path/to/program
```

Containers also work on Linux but native execution is preferred (less overhead, no extra security flags).

### macOS (via Podman, Docker, or another container runtime)

macOS does not support ptrace in the way the debugger requires. Use a Linux container:

```bash
podman build -t toydbg .   # or: docker build -t toydbg .
podman run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
```

### Windows (via WSL2, Podman, or Docker)

Use **WSL2**, which provides a real Linux kernel with full ptrace support:

```bash
# Inside a WSL2 terminal
go build ./cmd/toydbg
./toydbg /path/to/program
```

Alternatively, use **Docker Desktop** or **Podman** with the same container commands as macOS above. WSL**1** does *not* support ptrace — you must use WSL**2** or containers.

> **⚠ Container ptrace note:** Containers drop `SYS_PTRACE` by default, so `ptrace(PTRACE_TRACEME, ...)` inside `Launch` would fail with `EPERM`. The default seccomp profile also blocks ptrace. Both `--cap-add=SYS_PTRACE` and `--security-opt seccomp=unconfined` are required. Podman is rootless by default, so these capabilities are granted explicitly without running as root on the host.

## Agentic Workflow Requirement

After any code change, you **must** build and run the debugger inside a Linux environment and verify it produces valid output before considering the task complete. A passing `go test ./...` on the host is **not** sufficient — ptrace behavior can only be tested inside a real Linux kernel.

### Step 0 — Detect the available Linux environment

Run the following checks (in order) and use the **first** match:

| Priority | Check | Environment |
|----------|-------|-------------|
| 1 | `uname -s` returns `Linux` (native or WSL2) | **Native Linux / WSL2** — build and run directly, no container needed. |
| 2 | `command -v podman` succeeds | **Podman** |
| 3 | `command -v docker` succeeds | **Docker** |

If none match, stop and inform the user that a Linux environment is required.

### Step 1 — Build

**Native Linux / WSL2:**

```bash
go build ./cmd/toydbg && go test ./...
```

**Podman / Docker** (substitute `docker` for `podman` if that is what was detected):

```bash
podman build -t toydbg .
```

### Step 2 — Smoke-test: continue to exit

**Native Linux / WSL2:**

```bash
echo "c" | ./toydbg /bin/true
# Expected output: "process exited with code 0"
```

**Podman / Docker:**

```bash
echo "c" | podman run --rm -i --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  toydbg /bin/true
# Expected output: "process exited with code 0"
```

### Step 3 — Smoke-test: REPL help

**Native Linux / WSL2:**

```bash
echo "help" | ./toydbg /bin/true
# Expected output: "commands: continue (c), quit (q), help (h)"
```

**Podman / Docker:**

```bash
echo "help" | podman run --rm -i --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  toydbg /bin/true
# Expected output: "commands: continue (c), quit (q), help (h)"
```

Do **not** skip these steps or assume the task is complete without them.

## Key Files

- `cmd/toydbg/main.go` — CLI entry point: argument parsing, attach logic, REPL, command dispatch.
- `debugger/process.go` — process primitives: Launch, AttachPID, Resume, WaitOnSignal.
- `debugger/debugger.go` — public library root.
- `test/debugger_test.go` — integration tests.
- `docs/sequence-diagram.mmd` — Mermaid sequence diagram showing the debugger's attach-and-REPL lifecycle.
