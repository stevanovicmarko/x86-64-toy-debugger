# Architecture Guide

This document explains how **toydbg** works, from high-level design decisions
down to individual data structures. It is written for anyone learning how a
debugger interacts with the Linux kernel — whether you are studying the
codebase, extending it, or building your own debugger from scratch.

---

## Table of Contents

1. [Package Layout](#1-package-layout)
2. [How ptrace Works](#2-how-ptrace-works)
3. [Launching a Process](#3-launching-a-process)
4. [Attaching to a Process](#4-attaching-to-a-process)
5. [The Process State Machine](#5-the-process-state-machine)
6. [The REPL](#6-the-repl)
7. [The Register Table](#7-the-register-table)
8. [Reading and Writing Registers](#8-reading-and-writing-registers)
9. [The `struct user` Memory Layout](#9-the-struct-user-memory-layout)
10. [Platform Abstraction](#10-platform-abstraction)
11. [Error Handling](#11-error-handling)
12. [Testing Strategy](#12-testing-strategy)
13. [File Reference](#13-file-reference)

---

## 1. Package Layout

```
cmd/toydbg/          CLI binary — argument parsing + REPL loop
debugger/            Public library — the sole exported API
internal/            Private implementation (compiler-enforced boundary)
test/                Black-box integration tests
  targets/           Minimal programs used as tracees during tests
docs/                Documentation and diagrams
```

**Key constraint:** `debugger` is the only public package. The CLI
(`cmd/toydbg`) and the test suite (`test/`) both consume it. Neither is
allowed to import `internal/`. This mirrors how real debugger libraries
(like LLDB's `lldb` module) separate the engine from the UI.

### Why this matters

Keeping a single public API surface means:

- The CLI can be replaced (e.g., with a GUI or a DAP server) without
  changing the debugger engine.
- Tests exercise the same interface that real users call, catching
  integration bugs that unit tests on private functions would miss.
- Go's compiler enforces the `internal/` boundary — code that accidentally
  imports a private package will not compile.

---

## 2. How ptrace Works

`ptrace` is the Linux system call that lets one process (the **tracer**)
observe and control another (the **tracee**). Every debugger on Linux — GDB,
LLDB, strace — is built on ptrace.

### The mental model

Think of ptrace as giving the tracer a set of remote controls for the
tracee:

| Operation | What it does |
|-----------|-------------|
| `PTRACE_TRACEME` | Child tells kernel: "my parent is my tracer" |
| `PTRACE_SEIZE` | Tracer attaches to an already-running process |
| `PTRACE_INTERRUPT` | Tracer asks the kernel to stop the tracee |
| `PTRACE_CONT` | Tracer tells kernel: "let the tracee run" |
| `PTRACE_GETREGS` | Tracer reads the tracee's general-purpose registers |
| `PTRACE_GETFPREGS` | Tracer reads the tracee's floating-point registers |
| `PTRACE_PEEKUSER` | Tracer reads one 8-byte word from the tracee's `struct user` |
| `PTRACE_POKEUSER` | Tracer writes one 8-byte word into the tracee's `struct user` |
| `PTRACE_DETACH` | Tracer disconnects; tracee resumes normal execution |

### The per-thread rule

On Linux, ptrace is **per-thread**: only the OS thread that initiated the
trace can issue subsequent ptrace calls for that tracee. Go's runtime
multiplexes goroutines across OS threads, so the debugger must call
`runtime.LockOSThread()` to pin the goroutine to a single thread for the
entire lifetime of the trace. This pinning is released in `Close()`.

### Source files

- **`debugger/ptrace_linux.go`** — thin wrappers around the raw syscalls.
  Functions like `ptraceSeizeProcess` and `ptracePeekUser` translate Go
  arguments into `syscall.RawSyscall6` calls.
- **`debugger/ptrace_unsupported.go`** — stubs that return `ENOSYS` on
  non-Linux platforms, so the code compiles everywhere but fails gracefully
  at runtime.

---

## 3. Launching a Process

**Entry point:** `debugger.Launch(program, args...)`
**Source:** `debugger/process.go`

When the user runs `./toydbg /bin/true`, the debugger needs to:

1. Start the target program.
2. Immediately stop it *before it executes any user code*.
3. Read the initial register state.

Here is how `Launch` accomplishes this:

```
Debugger (parent)                   Target (child)
─────────────────                   ──────────────
runtime.LockOSThread()
exec.Command(program)
  SysProcAttr{Ptrace: true}
cmd.Start()  ───────────────────▶   fork()
                                    // Go runtime sets up PTRACE_TRACEME
                                    exec(program)
                                    // kernel sees PTRACE_TRACEME, sends SIGTRAP
                                    ◀── STOPS (trace stop)
Wait4(pid) ◀──── unblocks ─────
state = ProcessStopped
regs.readAll()  // cache GPRs, FPRs, debug regs
return Process
```

### Why `SysProcAttr{Ptrace: true}`?

Go's `os/exec` package supports `Ptrace: true` in the `SysProcAttr` struct.
Under the hood, after `fork()` and before `exec()`, the child calls
`PTRACE_TRACEME`. This tells the kernel that the child's parent is its
tracer. When the child subsequently calls `exec()`, the kernel intercepts
the transition and sends `SIGTRAP` to the child, causing it to stop before
executing any instructions from the new program image.

### `LaunchNoDebug`

`LaunchNoDebug` starts a process *without* ptrace. The process runs freely.
This exists for tests that need a running target to later `Attach` to — it
avoids the circular problem of needing a PID before you can call `Attach`.

---

## 4. Attaching to a Process

**Entry point:** `debugger.Attach(pid)`
**Source:** `debugger/process.go`

Attaching to an already-running process uses a different ptrace flow:

```
Debugger                            Target (already running)
────────                            ───────────────────────
runtime.LockOSThread()
ptraceSeizeProcess(pid)  ──────▶    // kernel marks target as traced
                                    // target keeps running (no SIGSTOP!)
ptraceInterruptProcess(pid)  ──▶    // kernel stops target
                                    ◀── STOPS (ptrace-stop)
Wait4(pid)  ◀──── unblocks ────
state = ProcessStopped
regs.readAll()
return Process
```

### Why PTRACE_SEIZE instead of PTRACE_ATTACH?

The older `PTRACE_ATTACH` sends `SIGSTOP` to the target. This creates a
problem: the kernel can deliver two stop events (the `SIGSTOP` plus the
ptrace-stop), and the tracer must carefully consume both. If it doesn't,
the next `Resume` call may see a stale stop event instead of the real one.

`PTRACE_SEIZE` (Linux 3.4+) attaches *without* sending any signal. The
tracer then explicitly calls `PTRACE_INTERRUPT` to stop the tracee at a
clean ptrace-stop. This produces exactly one stop event, eliminating the
double-stop bug.

### Ownership semantics

| How created | `terminateOnEnd` | Behavior on `Close()` |
|-------------|-------------------|-----------------------|
| `Launch` | `true` | Debugger kills the process (it created it) |
| `Attach` | `false` | Debugger detaches; process continues running |

This mirrors how real debuggers work: if you launch a program under GDB, GDB
kills it when you quit. If you attach to a running server, GDB detaches and
the server keeps running.

---

## 5. The Process State Machine

**Source:** `debugger/process.go`

A traced process moves through four states:

```
                  Launch / Attach
                        │
                        ▼
              ┌──── STOPPED ◀──────────┐
              │         │              │
         Resume()       │         WaitOnSignal()
              │         │       (stopped by signal)
              ▼         │              │
           RUNNING ─────┘──────────────┘
              │
              │  WaitOnSignal()
              │  (process exits or is killed)
              ▼
        EXITED  or  TERMINATED
```

- **`ProcessStopped`** — The tracee is frozen. The debugger can read/write
  registers, inspect memory, set breakpoints. This is the only state where
  ptrace data-access operations are valid.
- **`ProcessRunning`** — The tracee is executing. The debugger is blocked in
  `Wait4()`, waiting for the next stop event.
- **`ProcessExited`** — The tracee called `exit()` or returned from `main`.
  `Info` holds the exit code.
- **`ProcessTerminated`** — The tracee was killed by a signal (e.g., `SIGSEGV`).
  `Info` holds the signal number.

### `StopReason`

When the process stops, `WaitOnSignal()` returns a `StopReason`:

```go
type StopReason struct {
    Reason ProcessState  // Why it stopped
    Info   uint8         // Exit code or signal number
}
```

This is parsed from the kernel's `WaitStatus` using `newStopReason()`, which
checks `ws.Exited()`, `ws.Signaled()`, and `ws.Stopped()` in that order.

---

## 6. The REPL

**Source:** `cmd/toydbg/main.go`

The REPL (Read-Eval-Print Loop) is intentionally minimal — it is a thin
shell over the `debugger` API, not an independent system.

### Flow

```
┌────────────────────────────────────────────┐
│  1. Parse flags: -p <pid> or /path/to/prog │
│  2. Call debugger.Launch() or Attach()     │
│  3. Initialize readline with (toydbg)      │
│     prompt                                 │
│  4. Loop:                                  │
│     ├─ Read line                           │
│     ├─ Match command:                      │
│     │   "continue" → Resume + Wait         │
│     │   "help"     → print commands        │
│     │   "quit"     → break                 │
│     │   other      → print error           │
│     └─ If process exited → break           │
│  5. defer proc.Close()                     │
│  6. defer rl.Close()                       │
└────────────────────────────────────────────┘
```

### Commands

| Command | Aliases | Action |
|---------|---------|--------|
| `continue` | `c` | Resume the tracee, block until it stops again, print the stop reason |
| `help` | `h`, empty line | Print the list of available commands |
| `quit` | `q`, `exit` | Exit the debugger |

### Input handling

- **Ctrl+C** (`readline.ErrInterrupt`) — ignored, loops back to prompt.
- **Ctrl+D** (`io.EOF`) — exits gracefully.
- The `readline` library provides line editing, history, and signal handling.

---

## 7. The Register Table

**Source:** `debugger/register_info.go`

The register table is the **single source of truth** for every x86-64 CPU
register the debugger knows about. It is a slice of 125 `RegisterInfo`
entries — one per register — that maps each register's name, size, and
location to its position in the kernel's `struct user` data structure.

### Why a table?

A debugger needs to answer questions like:

- "The user typed `rax` — where do I read that from ptrace?"
- "DWARF debug info says the return value is in register 0 — which
  register is that?"
- "The user wants to see `eax` — is that 4 bytes or 8?"

Rather than scattering this knowledge across `if/else` chains, all 125
registers are described declaratively in one place. Lookup functions then
search the table by whatever key is needed.

### The `RegisterInfo` struct

```go
type RegisterInfo struct {
    ID      RegisterID      // Index in the table (auto-assigned by init())
    Name    string          // Human-readable name: "rax", "xmm0", "dr3"
    DwarfID int             // DWARF register number from the SysV ABI (-1 = none)
    Size    int             // Width in bytes: 1, 2, 4, 8, or 16
    Offset  int             // Byte offset within Linux struct user
    Type    RegisterType    // Category: GPR, SubGPR, FPR, or DR
    Format  RegisterFormat  // How to interpret the bytes: Uint, Double, LongDouble, Vector
}
```

### Register categories

| Type | Registers | Count | Description |
|------|-----------|-------|-------------|
| `RegisterTypeGPR` | `rax`..`gs`, `orig_rax` | 25 | 64-bit general-purpose registers |
| `RegisterTypeSubGPR` | `eax`, `ax`, `ah`, `al`, etc. | 36 | Narrower views into GPRs (32/16/8-bit) |
| `RegisterTypeFPR` | `fcw`, `st0`..`st7`, `mm0`..`mm7`, `xmm0`..`xmm15` | 40 | Floating-point, MMX, and SSE registers |
| `RegisterTypeDR` | `dr0`..`dr7` | 8 | Hardware debug/breakpoint registers |

### Display formats

| Format | Meaning | Used for |
|--------|---------|----------|
| `RegisterFormatUint` | Unsigned integer | GPRs, sub-GPRs, FP control words, debug regs |
| `RegisterFormatDoubleFloat` | 64-bit IEEE 754 double | (reserved for future use) |
| `RegisterFormatLongDouble` | 80-bit x87 extended precision | `st0`..`st7` (stored as 16 bytes with padding) |
| `RegisterFormatVector` | Opaque byte vector | `mm0`..`mm7` (8 bytes), `xmm0`..`xmm15` (16 bytes) |

### Sub-register aliasing

On little-endian x86-64, a sub-register is just a narrower window into
the same storage as its parent. For example:

```
rax (8 bytes)  ┌──┬──┬──┬──┬──┬──┬──┬──┐  offset = 80
               │07│06│05│04│03│02│01│00│  (byte positions)
               └──┴──┴──┴──┴──┴──┴──┴──┘
eax (4 bytes)                  ╰──┴──┴──┴──╯  offset = 80, size = 4
 ax (2 bytes)                        ╰──┴──╯  offset = 80, size = 2
 al (1 byte)                              ╰──╯  offset = 80, size = 1
 ah (1 byte)                         ╰──╯       offset = 81, size = 1
```

All of `eax`, `ax`, and `al` share the same `Offset` as `rax` — only the
`Size` differs. Because x86-64 is little-endian, the least-significant
bytes come first in memory, so reading fewer bytes from the same offset
naturally gives you the low portion of the register. The exception is `ah`,
which reads the *second* byte (`offset + 1`) to get the high byte of the
16-bit `ax` register.

### Lookup functions

```go
RegisterInfoByName("rax")      // lookup by human name (REPL input)
RegisterInfoByDwarf(0)         // lookup by DWARF ID (debug info)
RegisterInfoByID(RegisterID(3)) // lookup by table index (internal use)
AllRegisterInfos()             // full copy of the table
```

### The `init()` function purpose

To keep register IDs in sync with the table, `init()` auto-assigns
`ID = RegisterID(i)` for each entry. This eliminates the class of bugs
where IDs and table positions drift apart after someone inserts or
reorders a row.

---

## 8. Reading and Writing Registers

**Source:** `debugger/registers_linux.go`

### The `Registers` cache

```go
type Registers struct {
    pid   int               // tracee PID (for ptrace calls)
    gprs  syscall.PtraceRegs // 216 bytes — general-purpose registers
    fprs  fpRegs            // 512 bytes — floating-point / SSE state
    dregs [8]uint64         // 64 bytes  — debug registers
}
```

This struct mirrors the three regions of the kernel's `struct user` that
contain register data. When the tracee stops, `readAll()` populates all
three regions in one shot:

```
readAll()
  ├── ptraceGetRegs(pid, &gprs)         // PTRACE_GETREGS — bulk read 216 bytes
  ├── ptraceGetFPRegs(pid, &fprs)       // PTRACE_GETFPREGS — bulk read 512 bytes
  └── ptracePeekUser × 8               // PTRACE_PEEKUSER — 8 individual reads
```

### Reading a register

`Registers.Read(info)` takes a `RegisterInfo` and returns the value as a
Go `any` type. The steps are:

1. **Find the bytes:** `bytesForOffset(info.Offset, info.Size)` returns a
   slice pointing into whichever backing array (gprs, fprs, or dregs)
   contains the register.
2. **Decode by format:**
   - `FormatUint` → `uint8`, `uint16`, `uint32`, or `uint64`
   - `FormatDoubleFloat` → `float64`
   - `FormatLongDouble` → `[16]byte` (raw 80-bit extended precision)
   - `FormatVector` → `[8]byte` or `[16]byte`

### Writing a register

`Registers.Write(info, val)` does the reverse:

1. **Encode the value:** `encodeValue()` converts the Go value into bytes,
   widening if necessary (zero-extend unsigned, sign-extend signed).
2. **Update the cache:** copy the encoded bytes into the local backing store.
3. **Sync to the tracee:** the sync strategy depends on the register type:

| Register type | Sync method | Why |
|--------------|-------------|-----|
| FPR (floating-point) | `PTRACE_SETFPREGS` (writes all 512 bytes) | The kernel doesn't support word-level writes to the FPR area via `POKEUSER` |
| GPR / sub-GPR / DR | `PTRACE_POKEUSER` (writes one 8-byte word) | More efficient; only touches the one word that changed |

For sub-registers (e.g., writing to `eax`), the code aligns the offset to
an 8-byte boundary (`info.Offset &^ 7`) and writes the entire enclosing
word. This ensures the untouched high bytes are preserved.

### Automatic refresh

The register cache is refreshed automatically by `WaitOnSignal()` every time
the tracee stops. This means the cache is always consistent with the
tracee's actual state when the user is at the REPL prompt.

---

## 9. The `struct user` Memory Layout

**Source:** `debugger/register_info.go` (offset constants)

When a Linux process is traced, the kernel exposes its register state
through a virtual data structure called `struct user`. The ptrace operations
`PEEKUSER` and `POKEUSER` read and write bytes at offsets within this
structure. Understanding the layout is essential for register access.

```
struct user (x86-64)
═════════════════════════════════════════════════════════════════════
Offset    Size    Field                  Notes
─────────────────────────────────────────────────────────────────────
  0       216     user_regs_struct       25 × uint64 GPRs
                    0   r15                (see full GPR order below)
                    8   r14
                   16   r13
                   24   r12
                   32   rbp
                   40   rbx
                   48   r11
                   56   r10
                   64   r9
                   72   r8
                   80   rax
                   88   rcx
                   96   rdx
                  104   rsi
                  112   rdi
                  120   orig_rax
                  128   rip
                  136   cs
                  144   eflags
                  152   rsp
                  160   ss
                  168   fs_base
                  176   gs_base
                  184   ds
                  192   es
                  200   fs
                  208   gs
─────────────────────────────────────────────────────────────────────
216         4     u_fpvalid              (not used by debugger)
220         4     (padding)
─────────────────────────────────────────────────────────────────────
224       512     user_fpregs_struct     Floating-point + SSE state
                  224+0    cwd    (2B)     FP control word
                  224+2    swd    (2B)     FP status word
                  224+4    ftw    (2B)     FP tag word
                  224+6    fop    (2B)     FP opcode
                  224+8    rip    (8B)     FP instruction pointer
                  224+16   rdp    (8B)     FP data pointer
                  224+24   mxcsr  (4B)     SSE control/status
                  224+28   mxcr_mask (4B)  SSE control mask
                  224+32   st_space  (128B) 8 ST/MM regs × 16 bytes
                  224+160  xmm_space (256B) 16 XMM regs × 16 bytes
                  224+416  padding   (96B)
─────────────────────────────────────────────────────────────────────
736       112     (misc fields)          Not used by debugger
─────────────────────────────────────────────────────────────────────
848        64     u_debugreg[8]          8 × uint64 debug registers
                  848   dr0
                  856   dr1
                  864   dr2
                  872   dr3
                  880   dr4
                  888   dr5
                  896   dr6
                  904   dr7
═════════════════════════════════════════════════════════════════════
```

### Why `orig_rax` exists

When a process makes a system call, the kernel saves the syscall number in
`orig_rax` before using `rax` for the return value. This lets the debugger
distinguish "the process returned 1" from "the process called syscall 1
(`write`)". No DWARF ID is assigned to `orig_rax` because it is a
kernel/ptrace artifact, not an architectural register.

### ST vs MM register aliasing

The x87 `st0`–`st7` registers and the MMX `mm0`–`mm7` registers occupy the
**same physical storage** in the CPU (and in `struct user`). They differ
only in how they are interpreted:

- `st0` is 80-bit extended precision (stored as 16 bytes with 6 bytes of
  padding). Format: `LongDouble`.
- `mm0` is the low 64 bits of the same 16-byte slot. Format: `Vector`.

The register table models this by giving both the same `Offset` but
different `Size` and `Format` values.

---

## 10. Platform Abstraction

**Source:** `debugger/ptrace_unsupported.go`, `debugger/registers_unsupported.go`

The debugger only works on Linux (ptrace is a Linux-specific API), but the
code is structured to compile on any platform:

```
//go:build linux       →  ptrace_linux.go, registers_linux.go
//go:build !linux      →  ptrace_unsupported.go, registers_unsupported.go
```

The `_unsupported` files export the same function signatures but return
`syscall.ENOSYS` ("function not implemented"). This means:

- `go build` and `go vet` work on macOS for development.
- `go test` on macOS will fail with clear error messages rather than
  compile errors.
- CI can run linters on any platform.

For actual execution, a Linux environment is required — either native, WSL2,
or a container with `--cap-add=SYS_PTRACE --security-opt seccomp=unconfined`.

---

## 11. Error Handling

**Source:** `debugger/error.go`

The debugger defines a custom error type:

```go
type Error struct {
    msg string
}
```

This lets callers distinguish debugger errors from generic Go errors using a
type assertion:

```go
var debugErr *debugger.Error
if errors.As(err, &debugErr) {
    // This is a debugger-specific error (e.g., "could not resume")
} else {
    // This is a system error (e.g., file not found)
}
```

All internal error creation goes through `newError(msg)` and
`newErrorf(format, args...)`, which wrap messages in `*Error`.

---

## 12. Testing Strategy

**Source:** `test/debugger_test.go`, `test/targets/`

### Black-box testing

Tests live in `package test`, not `package debugger`. They import the
`debugger` package as an external consumer would, exercising only the public
API. This ensures the API contract is correct and complete.

### Test targets

Two minimal Go programs serve as tracees:

| Target | Path | Behavior |
|--------|------|----------|
| `end_immediately` | `test/targets/end_immediately/main.go` | Exits immediately (tests process exit handling) |
| `run_endlessly` | `test/targets/run_endlessly/main.go` | Infinite loop (tests attach, resume, and signal delivery) |

`TestMain()` compiles both targets into a temporary directory before any
tests run.

### Test categories

| Category | What it verifies |
|----------|-----------------|
| **Launch** | Process exists after launch; error on invalid program |
| **Attach** | Process enters traced stop (`'t'` in `/proc/<pid>/stat`); error on PID 0 |
| **Resume** | Process transitions to running state (`'R'` or `'S'`); error after exit |
| **Register metadata** | 125 registers; unique names; unique DWARF IDs; correct offsets |
| **Register I/O** | `rip` and `rsp` are non-zero after launch; sub-registers consistent with parent; write-then-read round-trips |

### Process state verification

Tests verify tracee state by reading `/proc/<pid>/stat` and checking the
process state character:

- `'t'` — tracing stop (ptrace-stopped)
- `'R'` — running
- `'S'` — sleeping (interruptible)

This goes beyond just checking the debugger's internal state — it confirms
the *kernel* sees the process in the expected state.

---

## 13. File Reference

| File | Lines | Purpose |
|------|-------|---------|
| `cmd/toydbg/main.go` | 112 | CLI entry point: argument parsing, REPL loop, command dispatch |
| `debugger/debugger.go` | 3 | Package documentation |
| `debugger/error.go` | 24 | Custom error type and constructors |
| `debugger/process.go` | 238 | Process lifecycle: Launch, Attach, Resume, WaitOnSignal, Close |
| `debugger/ptrace_linux.go` | 110 | Linux ptrace syscall wrappers |
| `debugger/ptrace_unsupported.go` | 42 | Non-Linux stubs (return `ENOSYS`) |
| `debugger/register_info.go` | 321 | Register metadata table (125 entries) and lookup functions |
| `debugger/registers_linux.go` | 284 | Register cache: read/write via ptrace |
| `debugger/registers_unsupported.go` | 22 | Non-Linux register stubs |
| `test/debugger_test.go` | 486 | Integration tests |
| `test/targets/end_immediately/main.go` | ~5 | Test target: exits immediately |
| `test/targets/run_endlessly/main.go` | ~5 | Test target: infinite loop |
| `docs/sequence-diagram.mmd` | 65 | Mermaid sequence diagram of the attach-and-REPL lifecycle |
| `Dockerfile` | ~20 | Multi-stage build: compile + slim runtime image |

---

## Further Reading

- [*Building a Debugger*](https://nostarch.com/building-a-debugger) — the
  book this project ports from C++ to Go.
- [`ptrace(2)` man page](https://man7.org/linux/man-pages/man2/ptrace.2.html) —
  the definitive reference for ptrace operations.
- [DWARF Debugging Standard](https://dwarfstd.org/) — the debug information
  format that assigns register numbers.
- [System V ABI (x86-64 supplement)](https://gitlab.com/x86-psABIs/x86-64-ABI) —
  defines the DWARF register numbering used in `register_info.go`.
