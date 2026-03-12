# AGENTS.md

This file provides guidance to AI coding assistants working with this repository.

## Project Overview

A toy x86-64 debugger written in Go, inspired by the C++ project in *Building a Debugger* by No Starch Press. The debugger is Linux-only, can launch or attach to processes via ptrace and has an interactive REPL with commands for continuing execution, reading/writing registers, and more. It is not intended for production use. A dev container configuration (`.devcontainer/`) is provided for development on macOS and Windows.

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
| `cmd/toydbg/` | CLI binary with interactive REPL | Consumes `debugger/` |
| `test/` | Black-box integration tests | Consumes `debugger/` |
| `.devcontainer/` | Dev container configuration (Dockerfile + devcontainer.json) | — |
| `docs/` | Documentation and notes | — |

### Constraints

- The `debugger` package is the **sole public API surface**. The CLI and tests must go through it.
- The `debugger` package **only compiles on Linux** (ptrace syscalls, no cross-platform stubs). See [Platform-Specific Setup](#platform-specific-setup) below for non-Linux development.

### Dependencies

- `github.com/chzyer/readline` — interactive command line (equivalent to `libedit` / what LLDB uses).

## Conventions

- **Exported symbols** use PascalCase (Go convention). Unexported symbols use camelCase.
- **Tests** live in `test/` as a separate package to enforce black-box testing through the public API.
- **Error handling** follows Go idioms: return `error` values, don't panic.
- Run `go mod tidy` after adding or removing imports.
- **Sequence diagram:** The file [`docs/sequence-diagram.mmd`](docs/sequence-diagram.mmd) is the canonical Mermaid sequence diagram documenting the debugger's attach-and-REPL lifecycle (User, Debugger, Kernel, Tracee). When adding new debugger features or changing the attach/resume/wait flow, update the diagram to reflect the new interactions. Every agentic coding flow that modifies the debugger's control flow **must** update this diagram and commit it alongside the code changes.
- **Architecture doc:** The file [`docs/architecture.md`](docs/architecture.md) is the educational architecture guide for the entire codebase. It follows an **onion teaching pattern** — outermost layers (package layout, what ptrace is) come first, peeling inward toward implementation details (byte offsets, encoding strategies). When adding new features, changing existing behavior, or introducing new packages/files, update the relevant sections of `architecture.md`. New sections must follow the same onion pattern: start with *why* and the mental model, then explain *how* with diagrams and code examples, then cover edge cases and implementation details. Every agentic coding flow that modifies the debugger **must** update this document and commit it alongside the code changes.

## Platform-Specific Setup

The `debugger` package only compiles on Linux. There are no cross-platform stubs.

### Linux (native — recommended)

Build and run directly — no container needed:

```bash
go build ./cmd/toydbg
./toydbg /path/to/program
```

### macOS / Windows (via dev container)

The repository includes a dev container configuration (`.devcontainer/`) that provides a full Linux environment with Go, gcc, and ptrace capabilities.

- **VS Code:** Install the Dev Containers extension, then "Reopen in Container."
- **JetBrains (GoLand, etc.):** Use Gateway → Dev Container.
- **GitHub Codespaces:** Click "Open in Codespaces" on GitHub.
- **WSL2 (Windows only):** Build natively inside a WSL2 terminal. WSL**1** does *not* support ptrace.

The root `Dockerfile` can also be used to run the debugger in a container:

```bash
podman build -t toydbg .   # or: docker build -t toydbg .
podman run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
```

> **⚠ Container ptrace note:** Containers drop `SYS_PTRACE` by default, so `ptrace(PTRACE_TRACEME, ...)` inside `Launch` would fail with `EPERM`. The default seccomp profile also blocks ptrace. Both `--cap-add=SYS_PTRACE` and `--security-opt seccomp=unconfined` are required. The dev container's `devcontainer.json` includes these flags automatically.

## Agentic Workflow Requirement

After any code change, you **must** verify the debugger works in **both** the native environment and the container before considering the task complete. The container build is **mandatory** — it is the canonical environment that proves all tests pass (including assembly targets that require `gcc`).

### Step 0 — Detect the available container runtime

Run the following checks (in order) and use the **first** match:

| Priority | Check | Runtime |
|----------|-------|---------|
| 1 | `command -v podman` succeeds | **Podman** |
| 2 | `command -v docker` succeeds | **Docker** |

If neither is available, stop and inform the user that a container runtime is required.

### Step 1 — Native build and test (if on Linux / WSL2)

```bash
go build ./cmd/toydbg && go test ./...
```

### Step 2 — Container build (MANDATORY)

The Dockerfile runs `go test ./...` inside the image, which compiles assembly test targets with `gcc` and executes all tests including ptrace-based register tests. A passing container build proves the full toolchain works.

Substitute `docker` for `podman` if that is the available runtime.

```bash
podman build -t toydbg .
```

### Step 3 — Container smoke-test: continue to exit

```bash
echo "c" | podman run --rm -i --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  toydbg /bin/true
# Expected output: "process exited with code 0"
```

### Step 4 — Container smoke-test: REPL help

```bash
echo "help" | podman run --rm -i --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  toydbg /bin/true
# Expected output: "commands: continue (c), step (s), next (n), finish (fin), stepi (si), list (l), backtrace (bt), breakpoint (break), watchpoint (watch), register (reg), memory (mem), disassemble (disas), catchpoint (catch), variable (var), expression (expr), thread (t), quit (q), help (h)"
```

> **Why is the container build mandatory?** Native-only testing does not guarantee the container environment works. The container is where `gcc` compiles assembly test targets, and where the Dockerfile's `RUN go test ./...` gate catches regressions in the full toolchain.

Do **not** skip the container build or assume the task is complete without it.

## Key Files

- `cmd/toydbg/main.go` — CLI entry point: argument parsing, REPL loop, command dispatch (continue, step, breakpoint, register, help, quit).
- `debugger/process.go` — process lifecycle: Launch, LaunchWithOptions, Attach, Resume, WaitOnSignal, GetPC, SetPC, breakpoint management, StepInstruction, Close.
- `debugger/breakpoint_site.go` — BreakpointSite type (enable/disable via PEEKDATA/POKEDATA) and breakpointSiteCollection.
- `debugger/format.go` — `FormatRegisterValue` — display formatting for all register types.
- `debugger/parse.go` — `ParseRegisterValue` — CLI string → typed value conversion.
- `debugger/register_info.go` — register metadata table (125 entries) and lookup functions.
- `debugger/registers_linux.go` — register cache: read/write via ptrace.
- `debugger/debugger.go` — public library root (package documentation).
- `debugger/error.go` — custom error type and constructors.
- `test/debugger_test.go` — integration tests (launch, attach, resume, register metadata, register I/O, assembly register tests, breakpoint tests).
- `test/targets/reg_read.s` — assembly test target: sets known register values and traps (no libc, built with gcc).
- `test/targets/reg_write.s` — assembly test target: prints debugger-written register values via printf (built with gcc).
- `test/targets/hello_toydbg.s` — assembly test target: write + exit (no libc, non-PIE, used for breakpoint tests).
- `docs/sequence-diagram.mmd` — Mermaid sequence diagram showing the debugger's attach-and-REPL lifecycle.
- `docs/architecture.md` — Educational architecture guide (onion pattern: high-level concepts → implementation details). Must be updated alongside code changes.
