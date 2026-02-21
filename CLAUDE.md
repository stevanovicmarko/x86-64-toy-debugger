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

# Regenerate code-flow diagram
go generate ./...

# Build Docker/Podman image (runs tests during the build)
docker build -t toydbg .   # or: podman build -t toydbg .

# Run inside Docker / Podman
docker run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
```

**Diagram rule:** After modifying `.go` files in `cmd/`, `debugger/`, or `internal/`, run `go generate ./...` and commit the updated `docs/code-flow.mmd` and `docs/program-flow.mmd`.

## Architecture

This is a Go port of a C++ x86-64 debugger from the book *Building a Debugger* by No Starch Press. The debugger can launch or attach to processes via ptrace and provides an interactive REPL with the `continue` command.

**Package layout:**

- `debugger/` — public library. Exports process primitives: `Launch`, `AttachPID`, `Resume`, `WaitOnSignal`.
- `internal/` — private implementation details. Go compiler enforces this boundary.
- `cmd/toydbg/` — CLI binary with interactive REPL (`(toydbg) ` prompt). Handles argument parsing and command dispatch.
- `test/` — black-box integration tests that consume `debugger/` as an external package.

**Key constraint:** `debugger` is the sole public API. CLI and tests must go through it; neither should import `internal/`.

**Dependency:** `github.com/chzyer/readline` provides the interactive command line (equivalent to `libedit` in the C++ version).

## Platform-Specific Setup

### Linux (native — recommended)

Build and run directly — no container needed. Native execution is preferred over containers (less overhead, no extra security flags).

### macOS (via Docker / Podman)

macOS does not support ptrace in the way the debugger requires. Use the container commands above. Substitute `podman` for `docker` if using Podman.

### Windows (via WSL2)

Use WSL2, which provides a real Linux kernel with full ptrace support. Build natively inside a WSL2 terminal. Alternatively use Docker Desktop or Podman with the container commands above. WSL**1** does *not* support ptrace.

> **⚠ Container ptrace note:** Containers drop `SYS_PTRACE` by default, so `ptrace(PTRACE_TRACEME, ...)` inside `Launch` would fail with `EPERM`. The default seccomp profile also blocks ptrace. Both `--cap-add=SYS_PTRACE` and `--security-opt seccomp=unconfined` are required. Podman is rootless by default, so these capabilities are granted explicitly without running as root on the host.
