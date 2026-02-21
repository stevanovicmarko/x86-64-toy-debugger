# toydbg

A toy x86-64 debugger written in Go, inspired by the C++ project in [*Building a Debugger*](https://nostarch.com/building-a-debugger) by No Starch Press.

## Prerequisites

- Go 1.25+
- Linux x86-64 (see platform-specific sections below for macOS / Windows)

## Build & Run

```bash
# Build the binary
go build ./cmd/toydbg

# Launch a program under the debugger
./toydbg /path/to/program

# Attach to a running process by PID
./toydbg -p <pid>
```

## Running on Linux (native — recommended)

Linux is the native platform. Build and run directly — no container needed:

```bash
go build ./cmd/toydbg
./toydbg /path/to/program
```

You can also run via containers on Linux (see below), but native execution is preferred because it avoids the overhead of container setup and the need for extra security flags.

## Running on macOS (via Docker / Podman)

macOS does not support ptrace in the way the debugger requires, so you must use a Linux container.

```bash
# Build the image (also runs tests during the build)
docker build -t toydbg .

# Launch a program under the debugger
docker run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
```

If you have Podman instead of Docker, substitute `podman` for `docker` — it accepts the same arguments.

## Running on Windows (via WSL2)

The recommended approach on Windows is to use WSL2, which provides a real Linux kernel with full ptrace support. Inside a WSL2 terminal:

```bash
# Clone and build natively inside WSL2
go build ./cmd/toydbg
./toydbg /path/to/program
```

Alternatively, if you have Docker Desktop or Podman on Windows, you can use the container approach:

```bash
docker build -t toydbg .
docker run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
```

> **Note:** WSL**1** does *not* support ptrace. You must use WSL**2** or containers.

> **⚠ Why `--cap-add=SYS_PTRACE` and `--security-opt seccomp=unconfined`?**
>
> Containers drop the `SYS_PTRACE` capability by default, so the `ptrace(PTRACE_TRACEME, ...)` call inside the debugger's `Launch` function would fail with `EPERM`. The default seccomp profile also blocks ptrace. Both flags are required to allow the debugger to trace child processes inside the container. Podman is rootless by default, so these capabilities are granted explicitly without running as root on the host.

## Test

```bash
go test ./...
```

## Project Structure

```
cmd/toydbg/      CLI binary (interactive REPL)
debugger/        Public library — all debugger primitives are exported from here
internal/        Private implementation details (compiler-enforced boundary)
test/            Black-box integration tests
tools/flowgen/   Code-flow diagram generator (sequence diagrams)
tools/progflow/  Program-flow diagram generator (control-flow flowcharts)
docs/            Documentation and generated diagrams
```

The `debugger` package is the sole public API surface. Both the CLI and tests consume it; neither reaches into `internal/` directly.

## Code Flow Diagram

A Mermaid sequence diagram at [`docs/code-flow.mmd`](docs/code-flow.mmd) visualizes the call flow between the CLI, debugger library, and OS syscalls. It is auto-generated from the source AST by `tools/flowgen`.

Regenerate it after changing `.go` files in `cmd/`, `debugger/`, or `internal/`:

```bash
go generate ./...
```

## Program Flow Diagram

A Mermaid flowchart at [`docs/program-flow.mmd`](docs/program-flow.mmd) shows the internal control flow of every function — if/else branches, for loops, switch statements, function calls, and returns. It is auto-generated from the source AST by `tools/progflow`.

## License

MIT
