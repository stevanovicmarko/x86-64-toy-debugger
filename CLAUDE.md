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
```

## Architecture

This is a Go port of a C++ x86-64 debugger from the book *Building a Debugger* by No Starch Press. The project is in early scaffolding; most debugger logic is yet to be written.

**Package layout:**

- `debugger/` — the public library package. All debugger primitives (processes, breakpoints, registers, etc.) will be implemented and exported from here. This is the equivalent of the book's `libsdb`.
- `internal/` — private implementation details not intended for consumers of the `debugger` package. The Go compiler enforces this boundary.
- `cmd/toydbg/` — the CLI binary. It imports `debugger/` and uses `github.com/chzyer/readline` for the interactive REPL prompt (`(toydbg) `).
- `test/` — black-box integration tests that import `debugger/` as an external consumer would.

**Key architectural constraint:** the `debugger` package is the sole public API surface. The CLI (`cmd/toydbg`) and tests (`test/`) must go through it; neither should reach into `internal/` directly.

**Dependency:** `github.com/chzyer/readline` provides the interactive command line (equivalent to `libedit` in the C++ version — the same library LLDB uses).
