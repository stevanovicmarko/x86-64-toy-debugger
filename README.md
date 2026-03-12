# toydbg

A toy x86-64 debugger written in Go.

Yes, its mostly implemented by AI, but it still requires a lot of human guidance to get it right. Things such as the ptrace syscalls, the register table, and the process state machine are all very tricky to get right and AI. Knowing to use PTRACE_SEIZE + PTRACE_INTERRUPT instead of PTRACE_ATTACH is something that AI would not know without human guidance.

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

## Example Session

Compile a small C program with debug info and step through it:

```bash
cat > /tmp/hello.c << 'EOF'
#include <stdio.h>

int add(int a, int b) {
    return a + b;
}

int main() {
    int x = add(3, 4);
    printf("result: %d\n", x);
    return 0;
}
EOF

gcc -g -O0 -o /tmp/hello /tmp/hello.c
./toydbg /tmp/hello
```

```
(toydbg) break set main
breakpoint 1 set at 0x... in main (hello.c:8)
(toydbg) c
hit breakpoint 1 at 0x... in main (hello.c:8)
(toydbg) list
   6│
   7│ int main() {
 → 8│     int x = add(3, 4);
   9│     printf("result: %d\n", x);
  10│     return 0;
(toydbg) s
   2│ int add(int a, int b) {
 → 3│     return a + b;
   4│ }
(toydbg) bt
#0  add (a=3, b=4) at hello.c:3
#1  main () at hello.c:8
(toydbg) fin
   7│ int main() {
   8│     int x = add(3, 4);
 → 9│     printf("result: %d\n", x);
(toydbg) var x
x = 7
(toydbg) c
result: 7
process exited with code 0
```

Compile with `-g -O0` for the best experience — `-g` emits DWARF debug info (source lines, variable locations) and `-O0` disables optimizations so stepping matches your code.

### Commands

| Command | Short | What it does |
|---|---|---|
| `continue` | `c` | Resume execution |
| `step` | `s` | Step one source line (into calls) |
| `next` | `n` | Step one source line (over calls) |
| `finish` | `fin` | Run until current function returns |
| `stepi` | `si` | Step one machine instruction |
| `list` | `l` | Show source around current PC |
| `backtrace` | `bt` | Print call stack |
| `breakpoint` | `break` | Set/list/delete breakpoints |
| `watchpoint` | `watch` | Set/list/delete data watchpoints |
| `register` | `reg` | Read/write CPU registers |
| `memory` | `mem` | Read/write process memory |
| `disassemble` | `disas` | Disassemble instructions |
| `catchpoint` | `catch` | Catch syscalls |
| `variable` | `var` | Read local/global variables |
| `expression` | `expr` | Evaluate expressions |
| `thread` | `t` | List/switch threads |
| `quit` | `q` | Exit the debugger |

## Running on Linux (native — recommended)

Linux is the native platform. Build and run directly — no container needed:

```bash
go build ./cmd/toydbg
./toydbg /path/to/program
```

You can also run via containers on Linux (see below), but native execution is preferred because it avoids the overhead of container setup and the need for extra security flags.

## Running on macOS / Windows (via dev container)

The debugger requires Linux — ptrace is a Linux-specific API and the code only compiles on Linux. The repository includes a **dev container** configuration (`.devcontainer/`) for non-Linux development.

- **VS Code:** Install the [Dev Containers](https://marketplace.visualstudio.com/items?itemName=ms-vscode-remote.remote-containers) extension, then "Reopen in Container."
- **JetBrains (GoLand, etc.):** Use Gateway → Dev Container.
- **GitHub Codespaces:** Click "Open in Codespaces" on GitHub — zero local setup.
- **WSL2 (Windows only):** Build natively inside a WSL2 terminal — no container needed. WSL**1** does *not* support ptrace.

You can also build and run the debugger directly in a container using the root `Dockerfile`:

```bash
docker build -t toydbg .   # or: podman build -t toydbg .
docker run --cap-add=SYS_PTRACE --security-opt seccomp=unconfined \
  -it toydbg /path/to/program
```

> **⚠ Why `--cap-add=SYS_PTRACE` and `--security-opt seccomp=unconfined`?**
>
> Containers drop the `SYS_PTRACE` capability by default, so the `ptrace(PTRACE_TRACEME, ...)` call inside the debugger's `Launch` function would fail with `EPERM`. The default seccomp profile also blocks ptrace. Both flags are required to allow the debugger to trace child processes inside the container. The dev container's `devcontainer.json` includes these flags automatically.

## Test

```bash
go test ./...
```

## Project Structure

```
cmd/toydbg/      CLI binary (interactive REPL)
debugger/        Public library — all debugger primitives are exported from here
test/            Black-box integration tests
.devcontainer/   Dev container configuration for non-Linux development
docs/            Documentation and diagrams
```

The `debugger` package is the sole public API surface. Both the CLI and tests consume it.

## Documentation

- **[Architecture Guide](docs/architecture.md)** — educational deep-dive into how the debugger works: ptrace mechanics, the register table, process lifecycle, `struct user` memory layout, and more.
- **[Sequence Diagram](docs/sequence-diagram.mmd)** — Mermaid sequence diagram of the attach-and-REPL lifecycle (User, Debugger, Kernel, Tracee).
- **[Book](https://nostarch.com/building-a-debugger)** — inspired by the C++ project in [*Building a Debugger*](https://nostarch.com/building-a-debugger) by No Starch Press. Support the author by purchasing the book.

## License

MIT
