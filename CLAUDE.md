# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

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

# Add a dependency
go get github.com/some/package

# Sync dependencies (after editing go.mod or adding imports)
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

**Sequence diagram rule:** The file [`docs/sequence-diagram.mmd`](docs/sequence-diagram.mmd) is the canonical Mermaid sequence diagram documenting the debugger's attach-and-REPL lifecycle (User, Debugger, Kernel, Tracee). When adding new debugger features or changing the attach/resume/wait flow, update the diagram to reflect the new interactions. Every agentic coding flow that modifies the debugger's control flow **must** update this diagram and commit it alongside the code changes.

**Architecture doc rule:** The file [`docs/architecture.md`](docs/architecture.md) is the educational architecture guide for the entire codebase. It follows an **onion teaching pattern** — outermost layers (package layout, what ptrace is) first, peeling inward toward implementation details (byte offsets, encoding strategies). When adding new features, changing existing behavior, or introducing new packages/files, update the relevant sections of `architecture.md` to reflect the changes. New sections should follow the same onion pattern: start with *why* and the mental model, then explain *how* with diagrams and code examples, then cover edge cases and implementation details. Every agentic coding flow that modifies the debugger **must** update this document and commit it alongside the code changes.

## Architecture

This is a Go project, inspired by the C++ project *Building a Debugger* by No Starch Press. The debugger can launch or attach to processes via ptrace and provides an interactive REPL with commands for continuing execution, reading/writing registers, and more.

**Package layout:**

- `debugger/` — public library. Exports process primitives (`Launch`, `Attach`, `Resume`, `WaitOnSignal`, etc.) and higher-level `Target` type for symbolic debugging (`LaunchTarget`, `StepIn`, `StepOver`, `StepOut`, `UnwindStack`, etc.). Private implementation details use unexported (lowercase) names within this package.
- `cmd/toydbg/` — CLI binary with interactive REPL (`(toydbg) ` prompt). Handles argument parsing and command dispatch.
- `test/` — black-box integration tests that consume `debugger/` as an external package.

**Key constraint:** `debugger` is the sole public API. CLI and tests must go through it.

**Dependency:** `github.com/chzyer/readline` provides the interactive command line.

## Platform-Specific Setup

### Linux (native — recommended)

Build and run directly — no container needed. Native execution is preferred over containers (less overhead, no extra security flags).

### macOS (via Podman, Docker, or another container runtime)

macOS does not support ptrace in the way the debugger requires. Use the container commands above with whichever runtime is installed (`podman` or `docker`).

### Windows (via WSL2, Podman, or Docker)

Use **WSL2**, which provides a real Linux kernel with full ptrace support. Build natively inside a WSL2 terminal. Alternatively use **Docker Desktop** or **Podman** with the container commands above. WSL**1** does *not* support ptrace.

> **⚠ Container ptrace note:** Containers drop `SYS_PTRACE` by default, so `ptrace(PTRACE_TRACEME, ...)` inside `Launch` would fail with `EPERM`. The default seccomp profile also blocks ptrace. Both `--cap-add=SYS_PTRACE` and `--security-opt seccomp=unconfined` are required. Podman is rootless by default, so these capabilities are granted explicitly without running as root on the host.

## Verification Rule

After any code change, you **must** verify the debugger works in **both** the native environment and the container. The container build is **mandatory** — it is the canonical environment that proves all tests pass (including assembly targets that require `gcc`).

### Detect the available container runtime

Run these checks in order and use the **first** match:

| Priority | Check | Runtime |
|----------|-------|---------|
| 1 | `command -v podman` succeeds | **Podman** |
| 2 | `command -v docker` succeeds | **Docker** |

If neither is available, stop and inform the user that a container runtime is required.

### Required verification steps

All steps below are **mandatory**. Substitute `docker` for `podman` if that is the available runtime.

```bash
# Step 1: Native build and test (if on Linux / WSL2)
go build ./cmd/toydbg && go test ./...

# Step 2: Container build (MANDATORY — runs go build + go test inside the container)
podman build -t toydbg .

# Step 3: Container smoke-test — continue to exit
echo "c" | podman run --rm -i --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  toydbg /bin/true
# Expected: "process exited with code 0"

# Step 4: Container smoke-test — REPL help
echo "help" | podman run --rm -i --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  toydbg /bin/true
# Expected: "commands: continue (c), step (s), next (n), finish (fin), stepi (si), list (l), backtrace (bt), breakpoint (break), watchpoint (watch), register (reg), memory (mem), disassemble (disas), catchpoint (catch), quit (q), help (h)"
```

> **Why is the container build mandatory?** The Dockerfile runs `go test ./...` inside the image, which compiles the assembly test targets with `gcc` and executes all tests including the ptrace-based register tests. A passing container build proves the full toolchain works: Go compiler, `gcc` for assembly, and ptrace syscalls. Native-only testing does not guarantee the container environment works.

Do **not** skip the container build or assume the task is complete without it.
