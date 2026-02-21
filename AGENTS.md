# AGENTS.md

This file provides guidance to AI coding assistants working with this repository.

## Project Overview

A toy x86-64 debugger written in Go, ported from the C++ project in *Building a Debugger* by No Starch Press. The debugger can launch or attach to processes via ptrace and has an interactive REPL with the `continue` command.

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

### Dependencies

- `github.com/chzyer/readline` — interactive command line (equivalent to `libedit` / what LLDB uses).

## Conventions

- **Exported symbols** use PascalCase (Go convention). Unexported symbols use camelCase.
- **Tests** live in `test/` as a separate package to enforce black-box testing through the public API.
- **Error handling** follows Go idioms: return `error` values, don't panic.
- Run `go mod tidy` after adding or removing imports.
- **Diagram regeneration:** After modifying `.go` files in `cmd/`, `debugger/`, or `internal/`, run `go generate ./...` and commit the updated `docs/code-flow.mmd`.

## Key Files

- `cmd/toydbg/main.go` — CLI entry point: argument parsing, attach logic, REPL, command dispatch.
- `debugger/process.go` — process primitives: Launch, AttachPID, Resume, WaitOnSignal.
- `debugger/debugger.go` — public library root.
- `test/debugger_test.go` — integration tests.
- `tools/flowgen/main.go` — code-flow diagram generator (Mermaid sequence diagrams).
- `docs/code-flow.mmd` — generated Mermaid diagram (auto-generated, do not hand-edit).
