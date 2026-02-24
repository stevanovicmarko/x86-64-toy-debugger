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
10. [Software Breakpoints](#10-software-breakpoints)
11. [Memory Access (PEEKDATA / POKEDATA)](#11-memory-access-peekdata--pokedata)
12. [Single Stepping](#12-single-stepping)
13. [Bulk Memory Operations](#13-bulk-memory-operations)
14. [Disassembly](#14-disassembly)
15. [Platform Abstraction](#15-platform-abstraction)
16. [Error Handling](#16-error-handling)
17. [Testing Strategy](#17-testing-strategy)
18. [File Reference](#18-file-reference)

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
| `PTRACE_PEEKDATA` | Tracer reads one 8-byte word from the tracee's address space |
| `PTRACE_POKEDATA` | Tracer writes one 8-byte word into the tracee's address space |
| `PTRACE_SINGLESTEP` | Tracer resumes the tracee for one instruction, then stops it |
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

### `LaunchWithOptions`

`LaunchWithOptions` behaves like `Launch` but accepts a `LaunchOptions`
struct that can redirect the child's stdout and stderr:

```go
type LaunchOptions struct {
    Stdout io.Writer  // redirect child's stdout (nil = os.Stdout)
    Stderr io.Writer  // redirect child's stderr (nil = os.Stderr)
}
```

This is primarily used in tests that capture the inferior's output via an
`os.Pipe()` — for example, the assembly write test launches the inferior
with `Stdout: pipeWriter` so it can verify that the debugger's register
writes are visible from the inferior's perspective.

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
      Resume()│         │              │ WaitOnSignal()
              │         │              │ (stopped by signal)
              ▼         │              │
           RUNNING ─────┘──────────────┘
              │
              │  WaitOnSignal()
              │  (process exits or is killed)
              ▼
       EXITED or TERMINATED
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
┌──────────────────────────────────────────────────────┐
│  1. Parse flags: -p <pid> or /path/to/prog           │
│  2. Call debugger.Launch() or Attach()               │
│  3. Initialize readline with (toydbg) prompt         │
│  4. Loop:                                            │
│     ├─ Read line, split into fields                  │
│     ├─ Match command:                                │
│     │   "continue"    → Resume + Wait + print PC     │
│     │   "step"        → StepInstruction + print PC   │
│     │   "breakpoint"  → set/list/enable/disable/del  │
│     │   "register"    → read/write subcommands       │
│     │   "help"        → print commands               │
│     │   "quit"        → break                        │
│     │   other         → print error                  │
│     └─ If process exited → break                     │
│  5. defer proc.Close()                               │
│  6. defer rl.Close()                                 │
└──────────────────────────────────────────────────────┘
```

### Commands

| Command | Aliases | Action |
|---------|---------|--------|
| `continue` | `c` | Resume the tracee, block until it stops again, print the stop reason with PC |
| `step` | `s` | Execute one machine instruction, print the stop reason with PC |
| `breakpoint set <addr>` | `break set <addr>` | Set and enable a breakpoint at the given hex address |
| `breakpoint list` | `break list` | List all breakpoints with ID, address, and status |
| `breakpoint enable <id>` | `break enable <id>` | Enable a breakpoint by ID |
| `breakpoint disable <id>` | `break disable <id>` | Disable a breakpoint by ID |
| `breakpoint delete <id>` | `break delete <id>` | Delete a breakpoint by ID |
| `register read` | `reg read` | Print all GPR values |
| `register read all` | `reg read all` | Print all 125 register values |
| `register read <name>` | `reg read <name>` | Print a single register |
| `register write <name> <value>` | `reg write <name> <value>` | Write a value to a register |
| `memory read <addr>` | `mem read <addr>` | Read 32 bytes at address, print hex dump |
| `memory read <addr> <n>` | `mem read <addr> <n>` | Read n bytes at address |
| `memory write <addr> [0xff,...]` | `mem write <addr> [...]` | Write bytes at address |
| `disassemble` | `disas` | Disassemble 5 instructions at current PC |
| `disassemble -c <n> -a <addr>` | `disas -c <n> -a <addr>` | Disassemble n instructions at address |
| `help` | `h`, empty line | Print the list of available commands |
| `help register` | `help reg` | Print register subcommand help |
| `help breakpoint` | `help break` | Print breakpoint subcommand help |
| `help memory` | `help mem` | Print memory subcommand help |
| `help disassemble` | `help disas` | Print disassemble options help |
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

### Complete register map

The following tables list every entry in `registerInfos` with its resolved
byte offset inside `struct user`. The **ID** column is the slice index
(auto-assigned by `init()`).

#### 64-bit GPRs (IDs 0–24)

| ID | Name | DWARF | Size | Offset | Notes |
|----|------|-------|------|--------|-------|
| 0 | `rax` | 0 | 8 | 80 | Accumulator / return value |
| 1 | `rdx` | 1 | 8 | 96 | 3rd arg / 128-bit multiply hi |
| 2 | `rcx` | 2 | 8 | 88 | 4th arg / loop counter |
| 3 | `rbx` | 3 | 8 | 40 | Callee-saved |
| 4 | `rsi` | 4 | 8 | 104 | 2nd arg (source index) |
| 5 | `rdi` | 5 | 8 | 112 | 1st arg (dest index) |
| 6 | `rbp` | 6 | 8 | 32 | Frame pointer (callee-saved) |
| 7 | `rsp` | 7 | 8 | 152 | Stack pointer |
| 8 | `r8` | 8 | 8 | 72 | 5th arg |
| 9 | `r9` | 9 | 8 | 64 | 6th arg |
| 10 | `r10` | 10 | 8 | 56 | Caller-saved |
| 11 | `r11` | 11 | 8 | 48 | Caller-saved |
| 12 | `r12` | 12 | 8 | 24 | Callee-saved |
| 13 | `r13` | 13 | 8 | 16 | Callee-saved |
| 14 | `r14` | 14 | 8 | 8 | Callee-saved |
| 15 | `r15` | 15 | 8 | 0 | Callee-saved |
| 16 | `rip` | 16 | 8 | 128 | Instruction pointer |
| 17 | `eflags` | 49 | 8 | 144 | CPU flags |
| 18 | `cs` | 51 | 8 | 136 | Code segment |
| 19 | `fs` | 54 | 8 | 200 | TLS segment |
| 20 | `gs` | 55 | 8 | 208 | TLS segment |
| 21 | `ss` | 52 | 8 | 160 | Stack segment |
| 22 | `ds` | 53 | 8 | 184 | Data segment |
| 23 | `es` | 50 | 8 | 192 | Extra segment |
| 24 | `orig_rax` | -1 | 8 | 120 | Syscall number (ptrace-only) |

> **Note:** The DWARF-to-offset mapping is *not* sequential — the kernel's
> `user_regs_struct` stores registers in a different order than the DWARF
> numbering. For example, DWARF 0 (`rax`) is at offset 80, while the struct
> starts with `r15` at offset 0.

#### 32-bit sub-GPRs (IDs 25–40)

All share the same offset as their 64-bit parent (little-endian: low 4 bytes).

| ID | Name | Size | Offset | Parent |
|----|------|------|--------|--------|
| 25 | `eax` | 4 | 80 | `rax` |
| 26 | `edx` | 4 | 96 | `rdx` |
| 27 | `ecx` | 4 | 88 | `rcx` |
| 28 | `ebx` | 4 | 40 | `rbx` |
| 29 | `esi` | 4 | 104 | `rsi` |
| 30 | `edi` | 4 | 112 | `rdi` |
| 31 | `ebp` | 4 | 32 | `rbp` |
| 32 | `esp` | 4 | 152 | `rsp` |
| 33 | `r8d` | 4 | 72 | `r8` |
| 34 | `r9d` | 4 | 64 | `r9` |
| 35 | `r10d` | 4 | 56 | `r10` |
| 36 | `r11d` | 4 | 48 | `r11` |
| 37 | `r12d` | 4 | 24 | `r12` |
| 38 | `r13d` | 4 | 16 | `r13` |
| 39 | `r14d` | 4 | 8 | `r14` |
| 40 | `r15d` | 4 | 0 | `r15` |

#### 16-bit sub-GPRs (IDs 41–56)

| ID | Name | Size | Offset | Parent |
|----|------|------|--------|--------|
| 41 | `ax` | 2 | 80 | `rax` |
| 42 | `dx` | 2 | 96 | `rdx` |
| 43 | `cx` | 2 | 88 | `rcx` |
| 44 | `bx` | 2 | 40 | `rbx` |
| 45 | `si` | 2 | 104 | `rsi` |
| 46 | `di` | 2 | 112 | `rdi` |
| 47 | `bp` | 2 | 32 | `rbp` |
| 48 | `sp` | 2 | 152 | `rsp` |
| 49 | `r8w` | 2 | 72 | `r8` |
| 50 | `r9w` | 2 | 64 | `r9` |
| 51 | `r10w` | 2 | 56 | `r10` |
| 52 | `r11w` | 2 | 48 | `r11` |
| 53 | `r12w` | 2 | 24 | `r12` |
| 54 | `r13w` | 2 | 16 | `r13` |
| 55 | `r14w` | 2 | 8 | `r14` |
| 56 | `r15w` | 2 | 0 | `r15` |

#### 8-bit sub-GPRs (IDs 57–76)

High-byte registers (`ah`, `bh`, `ch`, `dh`) use `offset + 1` to reach
bits 8–15 of the 16-bit register — an encoding inherited from the 8086.
Low-byte registers share the base offset.

| ID | Name | Size | Offset | Parent | Notes |
|----|------|------|--------|--------|-------|
| 57 | `ah` | 1 | 81 | `rax` | High byte of `ax` |
| 58 | `bh` | 1 | 41 | `rbx` | High byte of `bx` |
| 59 | `ch` | 1 | 89 | `rcx` | High byte of `cx` |
| 60 | `dh` | 1 | 97 | `rdx` | High byte of `dx` |
| 61 | `al` | 1 | 80 | `rax` | Low byte |
| 62 | `bl` | 1 | 40 | `rbx` | Low byte |
| 63 | `cl` | 1 | 88 | `rcx` | Low byte |
| 64 | `dl` | 1 | 96 | `rdx` | Low byte |
| 65 | `sil` | 1 | 104 | `rsi` | Low byte (REX prefix) |
| 66 | `dil` | 1 | 112 | `rdi` | Low byte (REX prefix) |
| 67 | `bpl` | 1 | 32 | `rbp` | Low byte (REX prefix) |
| 68 | `spl` | 1 | 152 | `rsp` | Low byte (REX prefix) |
| 69 | `r8b` | 1 | 72 | `r8` | Low byte |
| 70 | `r9b` | 1 | 64 | `r9` | Low byte |
| 71 | `r10b` | 1 | 56 | `r10` | Low byte |
| 72 | `r11b` | 1 | 48 | `r11` | Low byte |
| 73 | `r12b` | 1 | 24 | `r12` | Low byte |
| 74 | `r13b` | 1 | 16 | `r13` | Low byte |
| 75 | `r14b` | 1 | 8 | `r14` | Low byte |
| 76 | `r15b` | 1 | 0 | `r15` | Low byte |

> **Note:** `sil`, `dil`, `bpl`, and `spl` only exist in 64-bit mode (they
> require a REX prefix). In 32-bit mode, those encodings map to `ah`, `ch`,
> `dh`, `bh` instead — a classic x86 encoding quirk.

#### FP control/status (IDs 77–84)

All offsets are relative to `struct user` (base 224 = `user_fpregs_struct`).

| ID | Name | DWARF | Size | Offset | Description |
|----|------|-------|------|--------|-------------|
| 77 | `fcw` | 65 | 2 | 224 | FP control word |
| 78 | `fsw` | 66 | 2 | 226 | FP status word |
| 79 | `ftw` | -1 | 2 | 228 | FP tag word |
| 80 | `fop` | -1 | 2 | 230 | FP opcode |
| 81 | `frip` | -1 | 8 | 232 | FP instruction pointer |
| 82 | `frdp` | -1 | 8 | 240 | FP data pointer |
| 83 | `mxcsr` | 64 | 4 | 248 | SSE control/status |
| 84 | `mxcsrmask` | -1 | 4 | 252 | SSE control mask |

#### ST registers — x87 stack (IDs 85–92)

80-bit extended precision, stored as 16-byte slots (6 bytes padding each).
Format: `LongDouble`.

| ID | Name | DWARF | Size | Offset |
|----|------|-------|------|--------|
| 85 | `st0` | 33 | 16 | 256 |
| 86 | `st1` | 34 | 16 | 272 |
| 87 | `st2` | 35 | 16 | 288 |
| 88 | `st3` | 36 | 16 | 304 |
| 89 | `st4` | 37 | 16 | 320 |
| 90 | `st5` | 38 | 16 | 336 |
| 91 | `st6` | 39 | 16 | 352 |
| 92 | `st7` | 40 | 16 | 368 |

#### MM registers — MMX (IDs 93–100)

64-bit values aliasing the same storage as ST registers (same offsets, smaller
size). Format: `Vector`.

| ID | Name | DWARF | Size | Offset | Aliases |
|----|------|-------|------|--------|---------|
| 93 | `mm0` | 41 | 8 | 256 | `st0` storage |
| 94 | `mm1` | 42 | 8 | 272 | `st1` storage |
| 95 | `mm2` | 43 | 8 | 288 | `st2` storage |
| 96 | `mm3` | 44 | 8 | 304 | `st3` storage |
| 97 | `mm4` | 45 | 8 | 320 | `st4` storage |
| 98 | `mm5` | 46 | 8 | 336 | `st5` storage |
| 99 | `mm6` | 47 | 8 | 352 | `st6` storage |
| 100 | `mm7` | 48 | 8 | 368 | `st7` storage |

#### XMM registers — SSE (IDs 101–116)

128-bit vector registers. Format: `Vector`.

| ID | Name | DWARF | Size | Offset |
|----|------|-------|------|--------|
| 101 | `xmm0` | 17 | 16 | 384 |
| 102 | `xmm1` | 18 | 16 | 400 |
| 103 | `xmm2` | 19 | 16 | 416 |
| 104 | `xmm3` | 20 | 16 | 432 |
| 105 | `xmm4` | 21 | 16 | 448 |
| 106 | `xmm5` | 22 | 16 | 464 |
| 107 | `xmm6` | 23 | 16 | 480 |
| 108 | `xmm7` | 24 | 16 | 496 |
| 109 | `xmm8` | 25 | 16 | 512 |
| 110 | `xmm9` | 26 | 16 | 528 |
| 111 | `xmm10` | 27 | 16 | 544 |
| 112 | `xmm11` | 28 | 16 | 560 |
| 113 | `xmm12` | 29 | 16 | 576 |
| 114 | `xmm13` | 30 | 16 | 592 |
| 115 | `xmm14` | 31 | 16 | 608 |
| 116 | `xmm15` | 32 | 16 | 624 |

#### Debug registers (IDs 117–124)

| ID | Name | Size | Offset | Purpose |
|----|------|------|--------|---------|
| 117 | `dr0` | 8 | 848 | Hardware breakpoint address |
| 118 | `dr1` | 8 | 856 | Hardware breakpoint address |
| 119 | `dr2` | 8 | 864 | Hardware breakpoint address |
| 120 | `dr3` | 8 | 872 | Hardware breakpoint address |
| 121 | `dr4` | 8 | 880 | Reserved (alias for dr6) |
| 122 | `dr5` | 8 | 888 | Reserved (alias for dr7) |
| 123 | `dr6` | 8 | 896 | Debug status |
| 124 | `dr7` | 8 | 904 | Debug control |

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

### `GetPC()`

`Process.GetPC()` is a convenience method that reads the instruction pointer
(`rip`). It looks up the register by name and returns the `uint64` value.
This is used by the REPL to display the PC in stop-reason messages.

### Format and Parse utilities

Two platform-independent files provide string conversion for register values:

- **`debugger/format.go`** — `FormatRegisterValue(info, val)` converts a
  typed value to a display string. Unsigned integers become `0x%0Nx` (width
  based on register size), floats use `%g`, and vectors use
  `[0x01,0x02,...]` bracket notation.

- **`debugger/parse.go`** — `ParseRegisterValue(info, s)` converts a user
  string back to the typed value that `Write()` expects. It dispatches on
  `RegisterFormat`: unsigned integers accept `0x` prefix (via `strconv.ParseUint`
  with base 0), floats use `strconv.ParseFloat`, and vectors parse the
  `[0x01,0x02,...]` bracket format.

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

## 10. Software Breakpoints

**Source:** `debugger/breakpoint_site.go`, `debugger/process.go`

Software breakpoints are the fundamental mechanism that lets a debugger stop
a program at a specific instruction. Understanding how they work reveals an
elegant interplay between CPU hardware, the kernel, and the debugger.

### The int3 mechanism

On x86-64, the opcode `0xCC` is a single-byte instruction called `int3`.
When the CPU encounters it, the CPU:

1. Raises a **trap exception** (interrupt 3).
2. Increments RIP **past** the 0xCC byte (so RIP now points to the *next*
   instruction).
3. The kernel translates this trap into a **SIGTRAP** signal delivered to
   the tracee.

The debugger sets a breakpoint by **replacing** the first byte of the
target instruction with `0xCC`. When the CPU reaches that address, it
executes `int3` instead of the original instruction, causing the program
to stop.

### Setting a breakpoint

```
Before breakpoint:              After breakpoint:
┌────────────────┐              ┌────────────────┐
│ 48 c7 c0 01 …  │ ← original   │ CC c7 c0 01 …  │ ← 0xCC replaces first byte
└────────────────┘              └────────────────┘
  addr 0x401000                   addr 0x401000
```

The `BreakpointSite.Enable()` method:
1. Reads the 8-byte word at the target address via `PTRACE_PEEKDATA`.
2. Saves the low byte (the original instruction byte) in `savedData`.
3. Replaces the low byte with `0xCC`.
4. Writes the modified word back via `PTRACE_POKEDATA`.

### Hitting a breakpoint

When the tracee hits the `0xCC`:
1. CPU executes `int3`, raises trap, sets RIP = address + 1.
2. Kernel delivers SIGTRAP to the tracer via `wait4()`.
3. `WaitOnSignal()` reads registers and sees SIGTRAP.
4. It checks: is there an enabled breakpoint at **PC - 1**?
5. If yes, it adjusts PC back to the breakpoint address.

The PC adjustment is critical — without it, the debugger would think the
program is at the instruction *after* the breakpoint, and resuming would
skip the original instruction entirely.

### Resuming from a breakpoint (the step-over dance)

When the user types `continue` while stopped at a breakpoint, `Resume()`
cannot simply call `PTRACE_CONT` — the `0xCC` byte is still in memory,
and the CPU would immediately hit it again. Instead, it performs a
**step-over dance**:

```
1. Disable breakpoint  → restore original byte
2. PTRACE_SINGLESTEP   → execute the original instruction
3. wait4()             → wait for single-step stop
4. Re-enable breakpoint → put 0xCC back
5. PTRACE_CONT         → continue normal execution
```

### The BreakpointSite type

```go
type BreakpointSite struct {
    id        int32   // unique ID (monotonically increasing)
    pid       int     // tracee PID (for ptrace calls)
    address   uint64  // where the breakpoint is set
    isEnabled bool    // whether 0xCC is currently in memory
    savedData byte    // the original byte that was replaced
}
```

IDs are assigned via `sync/atomic.AddInt32`, ensuring uniqueness even if
breakpoints were created from multiple goroutines (though the current
debugger is single-threaded).

### The breakpointSiteCollection

Breakpoint sites are stored in a `breakpointSiteCollection` — a simple
slice-backed collection with lookup methods. This keeps things concrete
(no generics) while providing the operations needed:

- `containsAddress` — reject duplicate breakpoints at the same address
- `enabledAtAddress` — find the breakpoint to adjust PC after SIGTRAP
- `getByID` / `getByAddress` — REPL lookup commands
- `forEach` — iterate for listing

### Design decisions

- **Separate create from enable:** `CreateBreakpointSite` returns a
  *disabled* site. The caller must explicitly call `Enable()`.
  This allows setting up breakpoints without immediately modifying process memory.
- **Duplicate rejection:** Only one breakpoint can exist at a given
  address. Allowing duplicates would corrupt the `savedData` (the second
  breakpoint would save `0xCC` instead of the original byte).

---

## 11. Memory Access (PEEKDATA / POKEDATA)

**Source:** `debugger/ptrace_linux.go`

`PTRACE_PEEKDATA` and `PTRACE_POKEDATA` read and write the tracee's
**address space** (program memory). This is distinct from `PEEKUSER` /
`POKEUSER`, which access the kernel's `struct user` (register state).

```
                    ┌─────────────────────┐
PEEKUSER/POKEUSER → │   struct user       │ ← register state
                    │   (kernel memory)   │
                    └─────────────────────┘

                    ┌─────────────────────┐
PEEKDATA/POKEDATA → │   process memory    │ ← code + data
                    │   (address space)   │
                    └─────────────────────┘
```

Both operate on 8-byte (64-bit) words. To modify a single byte (as
breakpoints need to do), the debugger:
1. Reads the full 8-byte word containing the target byte.
2. Modifies just the target byte using bit masking.
3. Writes the full word back.

On little-endian x86-64, the byte at address `A` is the **low byte**
(bits 0–7) of the word read from address `A`. This is why `Enable()`
masks with `& ^uint64(0xFF)` and ORs in `0xCC`.

---

## 12. Single Stepping

**Source:** `debugger/process.go`

`PTRACE_SINGLESTEP` resumes the tracee for exactly **one machine
instruction**, then stops it again with SIGTRAP. This is implemented in
hardware via the CPU's **trap flag** (TF, bit 8 of EFLAGS) — the kernel
sets TF before resuming, and the CPU clears it after executing one
instruction.

### StepInstruction

`Process.StepInstruction()` wraps single-stepping with breakpoint
awareness:

1. If stopped at an enabled breakpoint, **disable** it temporarily.
2. Call `PTRACE_SINGLESTEP` to execute one instruction.
3. Call `WaitOnSignal()` to wait for the single-step stop.
4. If a breakpoint was disabled in step 1, **re-enable** it.

This ensures that stepping over a breakpoint executes the original
instruction (not `int3`) and leaves the breakpoint intact for future
hits.

### Interaction with Resume

`Resume()` also uses single-stepping internally when stopped at a
breakpoint — the `stepOverBreakpoint()` helper performs the same
disable-step-reenable dance before calling `PTRACE_CONT`. The difference
is that `StepInstruction` returns to the REPL after one instruction, while
`Resume` continues until the next stop event.

---

## 13. Bulk Memory Operations

**Source:** `debugger/process.go`, `debugger/memory_linux.go`

While `PTRACE_PEEKDATA` / `PTRACE_POKEDATA` work on individual 8-byte
words, debuggers frequently need to read or write larger regions — for
example, reading enough bytes to disassemble several instructions, or
writing a string into the tracee's memory.

### ReadMemory — efficient bulk reads with process_vm_readv

`Process.ReadMemory(addr, amount)` uses the `process_vm_readv(2)` syscall
instead of repeated `PTRACE_PEEKDATA` calls. This syscall transfers data
directly between address spaces in a single call, avoiding the per-word
overhead of ptrace.

```
Debugger                          Kernel
────────                          ──────
process_vm_readv(pid,             copies 'amount' bytes
  local_iov, remote_iov)  ──▶    directly from tracee
                           ◀──    to debugger buffer
```

The implementation splits the remote address range into page-aligned
`iovec` chunks (4096 bytes each). This is important because
`process_vm_readv` handles each iovec independently — if one page is
unmapped, the syscall returns a short read rather than failing with
`EFAULT`. Without page alignment, a read that spans a mapped-to-unmapped
boundary could fail entirely.

### WriteMemory — word-at-a-time with read-modify-write

`Process.WriteMemory(addr, data)` uses `PTRACE_POKEDATA` one word at a
time. For partial words at the start and end of the write region, it
performs a read-modify-write cycle: read the existing 8-byte word, patch
in the new bytes, write it back.

```
Writing 3 bytes at address 0x1005 (not 8-byte aligned):

  Aligned word at 0x1000: [aa bb cc dd ee ff gg hh]
                                       ↑↑ ↑↑ ↑↑
                                       new bytes go here (offsets 5-7)

  1. PEEKDATA(0x1000) → read existing word
  2. Patch bytes 5, 6, 7 with new data
  3. POKEDATA(0x1000, patched_word)
```

### ReadMemoryWithoutTraps — breakpoint-aware reads

`Process.ReadMemoryWithoutTraps(addr, amount)` reads memory and then
patches out `0xCC` bytes from any enabled breakpoints in the read region.
This restores the original instruction bytes, which is essential for
disassembly — without it, the disassembler would see `int3` instructions
at every breakpoint location instead of the real code.

The method uses `breakpointSiteCollection.getInRegion(low, high)` to
find all enabled breakpoints in the address range, then replaces the
`0xCC` byte with the breakpoint's `savedData` (the original byte).

---

## 14. Disassembly

**Source:** `debugger/disassembler.go`

The disassembler decodes raw machine code from the tracee's memory into
human-readable assembly text. It uses the pure-Go `golang.org/x/arch/x86/x86asm`
package — no CGo or external tools required.

### The Disassembler type

```go
type Disassembler struct {
    proc *Process  // the traced process to read memory from
}

type Instruction struct {
    Address uint64  // virtual address of the instruction
    Text    string  // human-readable disassembly (AT&T syntax)
}
```

### How disassembly works

1. **Read bytes:** Fetch `nInstructions × 15` bytes via
   `ReadMemoryWithoutTraps`. The factor of 15 comes from x86-64's
   maximum instruction length — this guarantees enough data to decode the
   requested number of instructions.

2. **Decode loop:** Call `x86asm.Decode(data, 64)` repeatedly. Each call
   returns one instruction and its byte length. Advance the offset by
   the instruction length.

3. **Format:** Convert each decoded instruction to AT&T syntax using
   `x86asm.GNUSyntax`. This matches the convention used by GDB and
   objdump on Linux.

4. **Error recovery:** If a byte sequence cannot be decoded, emit a
   `.byte 0xNN` pseudo-instruction and advance by one byte.

### Auto-disassembly on stop

The REPL's `handleStop` function automatically disassembles 5 instructions
at the current PC whenever the process stops (after `continue` or `step`).
This provides immediate context about what instruction the program is about
to execute — similar to GDB's `display/i $pc` behavior.

---

## 15. Platform Abstraction

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

## 16. Error Handling

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

## 17. Testing Strategy

**Source:** `test/debugger_test.go`, `test/targets/`

### Black-box testing

Tests live in `package test`, not `package debugger`. They import the
`debugger` package as an external consumer would, exercising only the public
API. This ensures the API contract is correct and complete.

### Test targets

Two minimal Go programs serve as tracees for basic lifecycle tests:

| Target | Path | Behavior |
|--------|------|----------|
| `end_immediately` | `test/targets/end_immediately/main.go` | Exits immediately (tests process exit handling) |
| `run_endlessly` | `test/targets/run_endlessly/main.go` | Infinite loop (tests attach, resume, and signal delivery) |

`TestMain()` compiles both Go targets with `go build` and all three
assembly targets with `gcc` into a temporary directory before any tests run.

### Test categories

| Category | What it verifies |
|----------|-----------------|
| **Launch** | Process exists after launch; error on invalid program |
| **Attach** | Process enters traced stop (`'t'` in `/proc/<pid>/stat`); error on PID 0 |
| **Resume** | Process transitions to running state (`'R'` or `'S'`); error after exit |
| **Register metadata** | 125 registers; unique names; unique DWARF IDs; correct offsets |
| **Register I/O** | `rip` and `rsp` are non-zero after launch; sub-registers consistent with parent; write-then-read round-trips |
| **Assembly register read** | Inferior sets known values in GPR, sub-GPR, MM, XMM, and ST registers; debugger reads them back |
| **Assembly register write** | Debugger writes registers; inferior prints them via printf; test verifies output via pipe |
| **Breakpoint collection** | Create, ID monotonicity, duplicate rejection, list, remove, enable/disable |
| **Breakpoint end-to-end** | Set BP ahead of PC and verify stop; step instruction; step over breakpoint; continue from breakpoint to exit |
| **Memory read** | Inferior stores known value, writes address to stdout; debugger reads memory at that address and verifies the value |
| **Memory write** | Debugger writes a string into inferior's buffer; inferior prints buffer contents; test verifies output |

### Assembly test targets

Some register behaviors can only be tested from the inferior's perspective —
for example, verifying that a debugger write to `rsi` is visible when the
inferior uses that value in a `printf` call. Go programs cannot control
which values land in specific registers (the compiler and runtime manage
register allocation), so these tests use hand-written x86-64 assembly.

| Target | Path | Libc | Build flags | Entry |
|--------|------|------|-------------|-------|
| `reg_read` | `test/targets/reg_read.s` | No | `-nostdlib -no-pie` | `_start` |
| `reg_write` | `test/targets/reg_write.s` | Yes | `-no-pie` | `main` |
| `hello_toydbg` | `test/targets/hello_toydbg.s` | No | `-nostdlib -no-pie` | `_start` |
| `memory` | `test/targets/memory.s` | No | `-nostdlib -no-pie` | `_start` |

The register targets (`reg_read`, `reg_write`) use the **trap-resume-read
pattern**: the assembly program executes `int3` (software breakpoint) at
known points. Each `int3` delivers `SIGTRAP` to the tracer, creating a
synchronization point where the test can read or write registers. Then
the test resumes the inferior to the next trap.

`reg_read` uses raw syscalls (`syscall` instruction) and no C library,
since it only needs to set register values and trap. `reg_write` links
with libc so it can call `printf` and `fflush` to print values to stdout,
which the test captures via `os.Pipe()` passed through `LaunchOptions`.

`hello_toydbg` is a minimal write-and-exit program with no `int3`
instructions — breakpoint tests set breakpoints programmatically via the
`CreateBreakpointSite` API rather than embedding traps in the code.

`memory` uses the same trap-resume-interact pattern to test bulk memory
operations. It stores a known value (`0xcafecafe`) in a stack variable,
writes the address to stdout, and traps — the test uses `ReadMemory` to
verify the value. For the write test, it provides a zeroed buffer, traps,
and the test writes a string into it via `WriteMemory`; after resuming,
the inferior prints the buffer contents so the test can verify the write.

`TestMain` builds all four assembly targets with `gcc` alongside the Go
targets.

### Process state verification

Tests verify tracee state by reading `/proc/<pid>/stat` and checking the
process state character:

- `'t'` — tracing stop (ptrace-stopped)
- `'R'` — running
- `'S'` — sleeping (interruptible)

This goes beyond just checking the debugger's internal state — it confirms
the *kernel* sees the process in the expected state.

---

## 18. File Reference

| File | Purpose |
|------|---------|
| `cmd/toydbg/main.go` | CLI entry point: argument parsing, REPL loop, command dispatch (continue, step, breakpoint, register, memory, disassemble, help, quit) |
| `debugger/debugger.go` | Package documentation |
| `debugger/error.go` | Custom error type and constructors |
| `debugger/format.go` | `FormatRegisterValue` — display formatting for all register types |
| `debugger/parse.go` | `ParseRegisterValue` — CLI string → typed value conversion |
| `debugger/breakpoint_site.go` | BreakpointSite type (enable/disable via PEEKDATA/POKEDATA) and breakpointSiteCollection |
| `debugger/process.go` | Process lifecycle: Launch, LaunchWithOptions, Attach, Resume, WaitOnSignal, GetPC, SetPC, breakpoint management, StepInstruction, ReadMemory, WriteMemory, ReadMemoryWithoutTraps, Close |
| `debugger/memory_linux.go` | Linux `process_vm_readv` wrapper for bulk memory reads |
| `debugger/memory_unsupported.go` | Non-Linux memory read stub |
| `debugger/disassembler.go` | Disassembler type: decodes x86-64 instructions via `x86asm` into AT&T syntax |
| `debugger/ptrace_linux.go` | Linux ptrace syscall wrappers |
| `debugger/ptrace_unsupported.go` | Non-Linux stubs (return `ENOSYS`) |
| `debugger/register_info.go` | Register metadata table (125 entries) and lookup functions |
| `debugger/registers_linux.go` | Register cache: read/write via ptrace |
| `debugger/registers_unsupported.go` | Non-Linux register stubs |
| `test/debugger_test.go` | Integration tests (launch, attach, resume, register metadata, register I/O, assembly register tests, breakpoint tests, memory read/write tests) |
| `test/targets/end_immediately/main.go` | Test target: exits immediately |
| `test/targets/run_endlessly/main.go` | Test target: infinite loop |
| `test/targets/reg_read.s` | Assembly test target: sets known register values and traps (no libc) |
| `test/targets/reg_write.s` | Assembly test target: prints debugger-written register values via printf |
| `test/targets/hello_toydbg.s` | Assembly test target: write + exit (no libc, non-PIE, used for breakpoint tests) |
| `test/targets/memory.s` | Assembly test target: stores known values and provides buffers for memory read/write tests |
| `docs/sequence-diagram.mmd` | Mermaid sequence diagram of the attach-and-REPL lifecycle |
| `Dockerfile` | Multi-stage build: compile + slim runtime image |

---

## Further Reading

- [*Building a Debugger*](https://nostarch.com/building-a-debugger) — Best book on hands-on debugger construction.
- [`ptrace(2)` man page](https://man7.org/linux/man-pages/man2/ptrace.2.html) —
  the definitive reference for ptrace operations.
- [DWARF Debugging Standard](https://dwarfstd.org/) — the debug information
  format that assigns register numbers.
- [System V ABI (x86-64 supplement)](https://gitlab.com/x86-psABIs/x86-64-ABI) —
  defines the DWARF register numbering used in `register_info.go`.
