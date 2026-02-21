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

# Build container image (use whichever runtime is available: podman or docker)
podman build -t toydbg .
# docker build -t toydbg .

# Run inside container (requires ptrace capabilities)
podman run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
# docker run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
#   -it toydbg /path/to/program
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

### macOS (via Podman, Docker, or another container runtime)

macOS does not support ptrace in the way the debugger requires. Use the container commands above with whichever runtime is installed (`podman` or `docker`).

### Windows (via WSL2, Podman, or Docker)

Use **WSL2**, which provides a real Linux kernel with full ptrace support. Build natively inside a WSL2 terminal. Alternatively use **Docker Desktop** or **Podman** with the container commands above. WSL**1** does *not* support ptrace.

> **⚠ Container ptrace note:** Containers drop `SYS_PTRACE` by default, so `ptrace(PTRACE_TRACEME, ...)` inside `Launch` would fail with `EPERM`. The default seccomp profile also blocks ptrace. Both `--cap-add=SYS_PTRACE` and `--security-opt seccomp=unconfined` are required. Podman is rootless by default, so these capabilities are granted explicitly without running as root on the host.

## Verification Rule

After any code change, **always** build and run the debugger inside a Linux environment and verify it produces valid output. A passing `go test ./...` on the host is **not** sufficient — ptrace behavior can only be tested inside a real Linux kernel.

### Detect the available Linux environment

Run these checks in order and use the **first** match:

| Priority | Check | Environment |
|----------|-------|-------------|
| 1 | `uname -s` returns `Linux` (native or WSL2) | **Native Linux / WSL2** — build and run directly. |
| 2 | `command -v podman` succeeds | **Podman** |
| 3 | `command -v docker` succeeds | **Docker** |

If none match, stop and inform the user that a Linux environment is required.

### Required verification steps

**Native Linux / WSL2:**

```bash
# 1. Build and test
go build ./cmd/toydbg && go test ./...

# 2. Smoke-test: continue to exit
echo "c" | ./toydbg /bin/true
# Expected: "process exited with code 0"

# 3. Smoke-test: REPL help
echo "help" | ./toydbg /bin/true
# Expected: "commands: continue (c), quit (q), help (h)"
```

**Podman / Docker** (substitute `docker` for `podman` if that is what was detected):

```bash
# 1. Build image (includes go build + go test)
podman build -t toydbg .

# 2. Smoke-test: continue to exit
echo "c" | podman run --rm -i --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  toydbg /bin/true
# Expected: "process exited with code 0"

# 3. Smoke-test: REPL help
echo "help" | podman run --rm -i --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  toydbg /bin/true
# Expected: "commands: continue (c), quit (q), help (h)"
```

Do **not** skip these steps or assume the task is complete without them.
