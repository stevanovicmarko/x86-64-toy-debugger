# toydbg

A toy x86-64 debugger written in Go, inspired by the C++ project in [*Building a Debugger*](https://nostarch.com/building-a-debugger) by No Starch Press.

## Prerequisites

- Go 1.25+
- Linux x86-64s

## Build & Run

```bash
# Build the binary
go build ./cmd/toydbg

# Launch a program under the debugger
./toydbg /path/to/program

# Attach to a running process by PID
./toydbg -p <pid>
```

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
