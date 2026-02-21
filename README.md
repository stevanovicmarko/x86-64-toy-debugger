# toydbg

A toy x86-64 debugger written in Go, ported from the C++ project in [*Building a Debugger*](https://nostarch.com/building-a-debugger) by No Starch Press.

## Prerequisites

- Go 1.25+
- Linux x86-64

## Build & Run

```bash
# Build the binary
go build ./cmd/toydbg

# Run directly
go run ./cmd/toydbg
```

## Test

```bash
go test ./...
```

## Project Structure

```
cmd/toydbg/   CLI binary (interactive REPL)
debugger/     Public library — all debugger primitives are exported from here
internal/     Private implementation details (compiler-enforced boundary)
test/         Black-box integration tests
docs/         Documentation and notes
```

The `debugger` package is the sole public API surface. Both the CLI and tests consume it; neither reaches into `internal/` directly.

## License

MIT
