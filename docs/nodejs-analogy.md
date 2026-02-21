# Go Project Structure — Node.js Analogy

## Directory Structure

```
x86-64-toy-debugger/          my-npm-project/
├── go.mod                    ├── package.json
├── go.sum                    ├── package-lock.json
├── debugger/                 ├── src/
│   └── debugger.go           │   └── index.js          ← the published library
├── internal/                 ├── (private, unenforced) ← enforced by compiler
├── cmd/
│   └── toydbg/               ├── bin/
│       └── main.go           │   └── toydbg.js         ← CLI entry point
└── test/                     └── test/
    └── debugger_test.go          └── debugger.test.js
```

---

## File-by-File Mapping

| Go file | Node.js equivalent | Purpose |
|---|---|---|
| `go.mod` | `package.json` | Module name + dependency declarations |
| `go.sum` | `package-lock.json` | Locked dependency hashes |
| `debugger/debugger.go` | `src/index.js` | Public library / exported API |
| `internal/` | _(no enforced equivalent)_ | Private implementation, compiler-blocked |
| `cmd/toydbg/main.go` | `bin/toydbg.js` + `"bin"` in package.json | CLI executable entry point |
| `test/debugger_test.go` | `test/debugger.test.js` | Integration tests |

---

## Visibility Model

```
Node.js                            Go
─────────────────────────────      ─────────────────────────────
export function sayHello() {}  →   func SayHello() {}   // capital = exported
                                                         // lowercase = private
function _private() {}         →   func private() {}
```

> In Go, **capitalization is the export keyword**. No `export` statement needed.

---

## Dependency Workflow

```
Node.js                            Go
─────────────────────────────      ─────────────────────────────
npm install                    →   go mod tidy
npm registry                   →   proxy.golang.org  (module proxy)
package.json "dependencies"    →   go.mod require block
package-lock.json              →   go.sum  (hash-based, not version ranges)
node_modules/                  →   ~/go/pkg/mod/  (global cache, not local)
```

---

## Import Syntax

```js
// Node.js
const readline = require('@scope/readline')
const { SayHello } = require('x86-64-toy-debugger/debugger')
```

```go
// Go
import (
    "x86-64-toy-debugger/debugger"
    "github.com/chzyer/readline"
)
```

> The import path **is** the module path from `go.mod`, extended by the subdirectory.
> There is no registry shorthand — you always use the full path.

---

## Running & Building

| Action | Node.js | Go |
|---|---|---|
| Run without building | `node cmd/toydbg/main.js` | `go run ./cmd/toydbg` |
| Compile to binary | `npm run build` | `go build ./cmd/toydbg` |
| Run tests | `npm test` / `jest` | `go test ./...` |
| Run tests (recursive) | `jest --testPathPattern='.'` | `go test ./...` (built-in) |
| Add a dependency | `npm install some-pkg` | `go get github.com/some/pkg` |

---

## The `internal/` Directory — The Key Difference

```
Node.js (no enforcement)           Go (compiler-enforced)
─────────────────────────────      ─────────────────────────────
// Anyone can require this:        // This import FAILS at compile time
require('./src/_private')          import "x86-64-toy-debugger/internal/core"
                                   //  ^^^  only code within this module
                                   //       can import internal packages
```

The closest Node analogy is publishing a package with `"private": true` in
`package.json` — except Go makes the runtime guarantee at **compile time**,
with no workarounds.

---

## Test Framework

```
Node.js (Jest)                     Go (stdlib testing)
─────────────────────────────      ─────────────────────────────
describe('validate env', () => {   func TestValidateEnvironment(
  it('should work', () => {            t *testing.T,
    expect(true).toBe(true)        ) {
  })                                   debugger.SayHello()
})                                 }
```

> Go's `testing` package ships with the language — no Jest, Mocha, or Catch2 needed.
