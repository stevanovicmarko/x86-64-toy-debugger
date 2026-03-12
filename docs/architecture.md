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
15. [Hardware Breakpoints](#15-hardware-breakpoints)
16. [Watchpoints](#16-watchpoints)
17. [Platform Abstraction](#17-platform-abstraction)
18. [Error Handling](#18-error-handling)
19. [Signal Handling and Process Groups](#19-signal-handling-and-process-groups)
20. [Enhanced Stop Reasons (TrapType)](#20-enhanced-stop-reasons-traptype)
21. [Syscall Catchpoints](#21-syscall-catchpoints)
22. [Watchpoint Data Tracking](#22-watchpoint-data-tracking)
23. [Testing Strategy](#23-testing-strategy)
24. [ELF Parsing and the Target Type](#24-elf-parsing-and-the-target-type)
25. [DWARF Debug Information](#25-dwarf-debug-information)
26. [Source-Level Stepping and Breakpoints](#26-source-level-stepping-and-breakpoints)
27. [Call Frame Information (CFI)](#27-call-frame-information-cfi)
28. [Stack Unwinding](#28-stack-unwinding)
29. [Shared Library Support](#29-shared-library-support)
30. [Multithreading Support](#30-multithreading-support)
31. [DWARF Expressions and Variable Reading](#31-dwarf-expressions-and-variable-reading)
32. [Types and Variables](#32-types-and-variables)
33. [Expression Evaluation](#33-expression-evaluation)
34. [File Reference](#34-file-reference)

---

## 1. Package Layout

```
cmd/toydbg/          CLI binary — argument parsing + REPL loop
debugger/            Public library — the sole exported API
test/                Black-box integration tests
  targets/           Minimal programs used as tracees during tests
docs/                Documentation and diagrams
```

**Key constraint:** `debugger` is the only public package. The CLI
(`cmd/toydbg`) and the test suite (`test/`) both consume it. This mirrors
how real debugger libraries (like LLDB's `lldb` module) separate the
engine from the UI. Private implementation details within `debugger/` use
Go's standard unexported (lowercase) naming convention.

### Why this matters

Keeping a single public API surface means:

- The CLI can be replaced (e.g., with a GUI or a DAP server) without
  changing the debugger engine.
- Tests exercise the same interface that real users call, catching
  integration bugs that unit tests on private functions would miss.

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
- The package only compiles on Linux. Non-Linux development uses
  dev containers (see `.devcontainer/`).

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
    Reason      ProcessState           // Why it stopped
    Info        uint8                  // Exit code or signal number
    TID         int                    // Thread that caused this stop (0 for single-threaded compat)
    TrapReason  *TrapType              // non-nil only for SIGTRAP stops
    SyscallInfo *SyscallInformation    // non-nil only for syscall traps
}
```

This is parsed from the kernel's `WaitStatus` using `newStopReason()`, which
checks `ws.Exited()`, `ws.Signaled()`, and `ws.Stopped()` in that order.
The `TID` field records which thread caused the stop — essential for
multithreaded debugging (see [Section 30](#30-multithreading-support)).

---

## 6. The REPL

**Source:** `cmd/toydbg/main.go`

The REPL (Read-Eval-Print Loop) is intentionally minimal — it is a thin
shell over the `debugger` API, not an independent system.

### Flow

```
┌──────────────────────────────────────────────────────────┐
│  1. Parse flags: -p <pid> or /path/to/prog               │
│  2. Call debugger.LaunchTarget() or AttachTarget()        │
│  3. Initialize readline with (toydbg) prompt              │
│  4. Loop:                                                 │
│     ├─ Read line, split into fields                       │
│     ├─ Match command:                                     │
│     │   "continue"    → Resume + Wait + print stop        │
│     │   "step"        → StepIn (source-level)             │
│     │   "next"        → StepOver (source-level)           │
│     │   "finish"      → StepOut (source-level)            │
│     │   "stepi"       → StepInstruction + print stop      │
│     │   "list"        → PrintSourceAtPC                   │
│     │   "backtrace"   → UnwindStack + FormatBacktrace     │
│     │   "breakpoint"  → set/list/enable/disable/del       │
│     │   "watchpoint"  → set/list/enable/disable/del       │
│     │   "register"    → read/write subcommands            │
│     │   "memory"      → read/write subcommands            │
│     │   "disassemble" → decode instructions at PC         │
│     │   "catchpoint"  → syscall catch configuration       │
│     │   "help"        → print commands                    │
│     │   "quit"        → break                             │
│     │   other         → print error                       │
│     └─ If process exited → break                          │
│  5. defer target.Close()                                  │
│  6. defer rl.Close()                                      │
└──────────────────────────────────────────────────────────┘
```

### Commands

| Command | Aliases | Action |
|---------|---------|--------|
| `continue` | `c` | Resume the tracee, block until it stops again, print the stop reason with PC |
| `step` | `s` | Source-level step into: advance until the source line changes (calls `StepIn`) |
| `next` | `n` | Source-level step over: advance to next source line, stepping over CALL instructions (calls `StepOver`) |
| `finish` | `fin` | Step out of the current function (calls `StepOut`) |
| `stepi` | `si` | Execute one machine instruction (calls `StepInstruction`) |
| `list` | `l` | Display source code at current PC via DWARF |
| `backtrace` | `bt` | Print the call stack (physical + inlined frames via CFI unwinding) |
| `breakpoint set <0xaddr>` | `break set <0xaddr>` | Set and enable a software breakpoint at the given hex address |
| `breakpoint set <func>` | `break set <func>` | Set breakpoint at named function (past prologue via DWARF) |
| `breakpoint set <file>:<line>` | `break set <file>:<line>` | Set breakpoint at source line (via DWARF line table) |
| `breakpoint set -h <location>` | `break set -h <location>` | Set a hardware breakpoint at any location type (address, function, or file:line) |
| `breakpoint list` | `break list` | List all breakpoints with ID, address, type [hw/sw], and status |
| `breakpoint enable <id>` | `break enable <id>` | Enable a breakpoint by ID |
| `breakpoint disable <id>` | `break disable <id>` | Disable a breakpoint by ID |
| `breakpoint delete <id>` | `break delete <id>` | Delete a breakpoint by ID |
| `watchpoint set <addr> <mode> <size>` | `watch set ...` | Set a data watchpoint (mode: `write` or `rw`; size: 1, 2, 4, 8) |
| `watchpoint list` | `watch list` | List all watchpoints with ID, address, mode, size, and status |
| `watchpoint enable <id>` | `watch enable <id>` | Enable a watchpoint by ID |
| `watchpoint disable <id>` | `watch disable <id>` | Disable a watchpoint by ID |
| `watchpoint delete <id>` | `watch delete <id>` | Delete a watchpoint by ID |
| `register read` | `reg read` | Print all GPR values |
| `register read all` | `reg read all` | Print all 125 register values |
| `register read <name>` | `reg read <name>` | Print a single register |
| `register write <name> <value>` | `reg write <name> <value>` | Write a value to a register |
| `memory read <addr>` | `mem read <addr>` | Read 32 bytes at address, print hex dump |
| `memory read <addr> <n>` | `mem read <addr> <n>` | Read n bytes at address |
| `memory write <addr> [0xff,...]` | `mem write <addr> [...]` | Write bytes at address |
| `disassemble` | `disas` | Disassemble 5 instructions at current PC |
| `disassemble -c <n> -a <addr>` | `disas -c <n> -a <addr>` | Disassemble n instructions at address |
| `catchpoint syscall` | `catch syscall` | Catch all syscalls (entry and exit) |
| `catchpoint syscall none` | `catch syscall none` | Stop catching syscalls |
| `catchpoint syscall <list>` | `catch syscall <list>` | Catch specific syscalls by name or number (comma-separated) |
| `variable read <name>` | `var read <name>` | Read a variable (supports `name.field`, `ptr->field`, `arr[i]`) |
| `variable locals` | `var locals` | Print all local variables in the current scope |
| `variable location <name>` | `var location <name>` | Print where a variable lives (register or memory address) |
| `expression <func>(<args>)` | `expr <func>(<args>)` | Call a function in the inferior and display the return value |
| `help` | `h`, empty line | Print the list of available commands |
| `help register` | `help reg` | Print register subcommand help |
| `help breakpoint` | `help break` | Print breakpoint subcommand help |
| `help watchpoint` | `help watch` | Print watchpoint subcommand help |
| `help memory` | `help mem` | Print memory subcommand help |
| `help disassemble` | `help disas` | Print disassemble options help |
| `help catchpoint` | `help catch` | Print catchpoint subcommand help |
| `help variable` | `help var` | Print variable subcommand help |
| `help expression` | `help expr` | Print expression usage and examples |
| `thread list` | `t list` | List all threads with their states (`*` marks current) |
| `thread select <tid>` | `t select <tid>` | Switch the current thread for register/step operations |
| `quit` | `q`, `exit` | Exit the debugger |

### Input handling

- **Ctrl+C** — sends `SIGSTOP` to the inferior (see [Section 19](#19-signal-handling-and-process-groups)). If the tracee is running, this interrupts it and drops back to the prompt. If already at the prompt, readline absorbs the interrupt.
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
| `RegisterTypeSubGPR` | `eax`, `ax`, `ah`, `al`, etc. | 52 | Narrower views into GPRs (32/16/8-bit) |
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
    id                    int32    // unique ID (monotonically increasing)
    pid                   int      // tracee PID (for ptrace calls)
    proc                  *Process // back-pointer to owning Process
    address               uint64   // where the breakpoint is set
    isEnabled             bool     // whether 0xCC is currently in memory
    savedData             byte     // the original byte that was replaced
    isHardware            bool     // true = debug register, false = int3
    isInternal            bool     // true = hidden from user (e.g., step-over)
    hardwareRegisterIndex int      // DR0–DR3 index (hardware BPs only)
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

### Source display on stop

The REPL's `handleStop` function prefers showing source context when DWARF
info is available (via `PrintSourceAtPC`), falling back to disassembling 5
instructions at the current PC when no source info exists. This provides
immediate context — similar to GDB's `list` or `display/i $pc` behavior.

---

## 15. Hardware Breakpoints

**Source:** `debugger/breakpoint_site.go`, `debugger/process.go`, `debugger/stoppoint_mode.go`

### Why hardware breakpoints?

Software breakpoints (section 10) inject a `0xCC` byte into the tracee's code.
This works well, but it *modifies memory*. Anti-debugging techniques can detect
this by checksumming their own instructions — if a byte changes, a debugger is
present. Hardware breakpoints avoid this entirely by using the CPU's built-in
debug registers, which are invisible to the tracee.

### The debug registers

x86-64 provides eight debug registers (DR0–DR7):

```
DR0–DR3  Address registers   — hold the breakpoint/watchpoint address
DR4–DR5  Reserved            — aliased to DR6/DR7 in some modes
DR6      Status register     — tells which debug register triggered
DR7      Control register    — enable bits, access mode, address size
```

Only four simultaneous hardware breakpoints/watchpoints are possible (DR0–DR3).
This is a CPU limitation — software breakpoints have no such cap.

### DR7 bit layout

DR7 is the heart of hardware breakpoint programming. Each of the four slots
gets a local-enable bit and a 4-bit condition+length field:

```
Bits 0,2,4,6    Local enable for DR0–DR3 (1 = active)
Bits 1,3,5,7    Global enable (not used in user-mode debugging)
Bits 16–17      Condition (RW0): 00=execute, 01=write, 11=read/write
Bits 18–19      Length (LEN0):   00=1 byte, 01=2 bytes, 11=4 bytes, 10=8 bytes
Bits 20–23      RW1 + LEN1 (same pattern for DR1)
Bits 24–27      RW2 + LEN2
Bits 28–31      RW3 + LEN3
```

### Programming a hardware breakpoint

The `setHardwareStoppoint` method in `process.go`:

1. Reads the current DR7 value from the register cache
2. Calls `findFreeStoppointRegister` — scans DR7 local-enable bits to find an unused slot
3. Writes the target address to the corresponding DR0–DR3 register via `PTRACE_POKEUSER`
4. Sets the local-enable bit and encodes the mode/size into DR7
5. Writes DR7 back via `PTRACE_POKEUSER`

### Breakpoint hit behavior

When a hardware breakpoint fires, the CPU raises a `#DB` exception
(debug exception), which Linux delivers as `SIGTRAP` — the same signal as
a software breakpoint. However, there is a critical difference:

| | Software BP | Hardware BP |
|---|---|---|
| **RIP on stop** | Past the `0xCC` (PC = addr + 1) | AT the instruction (PC = addr) |
| **PC adjustment** | Subtract 1 to point back at the BP | None needed |
| **Memory modified** | Yes (0xCC injected) | No |

The `WaitOnSignal` method handles this by using `enabledSoftwareAtAddress(pc - 1)`
instead of `enabledAtAddress(pc - 1)`. Hardware breakpoints are excluded from
the PC-1 check, so no adjustment occurs when they fire.

### REPL usage

```
(toydbg) breakpoint set -h 0x401000    # hardware breakpoint
(toydbg) breakpoint set 0x401000       # software breakpoint (default)
(toydbg) breakpoint list               # shows [hw] or [sw] tag
```

---

## 16. Watchpoints

**Source:** `debugger/watchpoint.go`, `debugger/process.go`, `debugger/stoppoint_mode.go`

### What is a watchpoint?

A watchpoint triggers when a specific memory address is *accessed* (read or
written), rather than when an instruction is *executed*. This is invaluable
for debugging data corruption — set a watchpoint on a variable and the
debugger stops the instant something modifies it.

### Shared mechanism with hardware breakpoints

Watchpoints use the same debug registers (DR0–DR3) and DR7 control bits as
hardware breakpoints. The only difference is the **condition field** in DR7:

| Mode | DR7 condition bits | Triggers on |
|------|-------------------|-------------|
| Execute | `00` | Instruction execution |
| Write | `01` | Data writes only |
| Read/Write | `11` | Data reads and writes |

Because they share the four debug register slots, the total number of
hardware breakpoints + watchpoints combined cannot exceed four.

### The `StoppointMode` enum

```go
type StoppointMode int
const (
    StoppointModeExecute   StoppointMode = iota  // hardware breakpoint
    StoppointModeWrite                            // write watchpoint
    StoppointModeReadWrite                        // access watchpoint
)
```

### The `Watchpoint` type

Watchpoints are a separate type from `BreakpointSite` because they have
different semantics: a size (1/2/4/8 bytes), an access mode, and an
alignment requirement (the address must be aligned to the size).

```go
type Watchpoint struct {
    id, proc, address, mode, size, isEnabled, hardwareRegisterIndex
}
```

The `watchpointCollection` mirrors the `breakpointSiteCollection` pattern.

### Alignment requirement

Hardware watchpoints require the address to be naturally aligned:

```
size=1  →  any address
size=2  →  address & 1 == 0
size=4  →  address & 3 == 0
size=8  →  address & 7 == 0
```

`CreateWatchpoint` validates this with `addr & uint64(size-1) != 0`.

### REPL usage

```
(toydbg) watchpoint set 0x7ffd5000 write 8    # 8-byte write watchpoint
(toydbg) watchpoint set 0x7ffd5000 rw 4       # 4-byte read/write watchpoint
(toydbg) watchpoint list
(toydbg) watchpoint delete 1
```

---

## 17. Platform Requirements and Dev Containers

**Source:** `.devcontainer/devcontainer.json`, `.devcontainer/Dockerfile`

The debugger requires Linux — ptrace is a Linux-specific API and the entire
`debugger` package only compiles on Linux. Platform-specific source files
use Go's `_linux.go` suffix convention:

```
ptrace_linux.go       →  ptrace syscall wrappers
registers_linux.go    →  register cache (GPR, FPR, debug regs)
memory_linux.go       →  process_vm_readv wrapper
auxv_linux.go         →  /proc/<pid>/auxv reader
```

### Development on non-Linux platforms

For macOS and Windows developers, the repository provides a **dev container**
configuration (`.devcontainer/`). This gives a full Linux environment with
Go, gcc, and ptrace capabilities — no manual setup required.

**Supported workflows:**

| Environment | How it works |
|---|---|
| VS Code + Dev Containers extension | "Reopen in Container" — automatic |
| JetBrains (GoLand, etc.) | Gateway → Dev Container |
| GitHub Codespaces | Click "Open in Codespaces" on GitHub |
| WSL2 (Windows) | Native Linux kernel — build directly |
| Podman / Docker | Use the project `Dockerfile` with `--cap-add=SYS_PTRACE --security-opt seccomp=unconfined` |

The dev container's `runArgs` include `--cap-add=SYS_PTRACE` and
`--security-opt seccomp=unconfined` so that ptrace works inside the
container without any manual flag passing.

---

## 18. Error Handling

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

## 19. Signal Handling and Process Groups

**Source:** `debugger/process.go` (Setpgid), `cmd/toydbg/main.go` (SIGINT handler)

### The problem

When the user presses Ctrl+C, the terminal sends `SIGINT` to every process in
the *foreground process group*. Without intervention, both the debugger and the
inferior receive SIGINT — killing them both. A real debugger should intercept
Ctrl+C and *stop* the inferior gracefully instead.

### Process group isolation

The solution has two parts:

1. **Setpgid in the child.** During `Launch`, the `SysProcAttr` includes
   `Setpgid: true`, which calls `setpgid(0, 0)` in the child before exec.
   This puts the inferior in its own process group, so terminal-generated
   signals only reach the debugger.

2. **SIGINT handler in the debugger.** The REPL installs a goroutine that
   listens on `os/signal.Notify(SIGINT)`. When Ctrl+C arrives, the handler
   sends `SIGSTOP` to the inferior via `kill(pid, SIGSTOP)`. The inferior
   stops, `wait4` unblocks, and the debugger drops back to the prompt.

```
Terminal                Debugger (PG 1)        Inferior (PG 2)
  │                         │                       │
  │── SIGINT ──────────────►│                       │
  │                         │── kill(pid, SIGSTOP) ─►│
  │                         │                       │ STOPS
  │                         │◄── wait4 unblocks ────│
  │                         │                       │
  │◄── "(toydbg) " ────────│                       │
```

### Why SIGSTOP instead of SIGINT?

SIGSTOP cannot be caught, blocked, or ignored — it is guaranteed to stop the
process. SIGINT can be caught by the inferior (many programs install custom
handlers), which would not reliably interrupt execution.

---

## 20. Enhanced Stop Reasons (TrapType)

**Source:** `debugger/process.go` (TrapType, augmentStopReason, siginfo)

### The problem

On x86-64, `SIGTRAP` (signal 5) is delivered for at least four distinct
events: software breakpoints (`int3`), single stepping (`EFLAGS.TF`),
hardware breakpoints/watchpoints (`#DB` exception), and syscall-stops. The
kernel delivers the same signal for all of them — the debugger must determine
the *actual* cause to give the user meaningful feedback.

### TrapType enum

```go
type TrapType int
const (
    TrapSingleStep    TrapType = iota  // PTRACE_SINGLESTEP completed
    TrapSoftwareBreak                  // int3 (0xCC)
    TrapHardwareBreak                  // Debug register match (#DB)
    TrapSyscall                        // Syscall entry/exit
    TrapUnknown                        // Unrecognized
)
```

### How augmentStopReason works

The `augmentStopReason` method runs after every `WaitOnSignal` when the
process is stopped:

1. **TRACESYSGOOD check:** If the stop signal is `SIGTRAP|0x80` (133),
   this is a syscall stop. The `PTRACE_O_TRACESYSGOOD` option (set during
   launch/attach) makes the kernel set bit 7 for syscall-stops, providing
   an instant discriminator without needing `PTRACE_GETSIGINFO`.

2. **PTRACE_GETSIGINFO:** For regular SIGTRAP, the kernel fills a
   `siginfo_t` whose `si_code` field identifies the cause:

   | `si_code`        | Value | Meaning              |
   |------------------|-------|----------------------|
   | `SI_KERNEL`      | 0x80  | Software breakpoint  |
   | `TRAP_TRACE`     | 2     | Single step          |
   | `TRAP_HWBKPT`    | 4     | Hardware BP/watchpoint |

3. **StopReason enrichment:** The `TrapReason` pointer is set, and for
   syscall traps, a `SyscallInformation` struct captures the syscall number
   (from `orig_rax`), arguments (from `rdi/rsi/rdx/r10/r8/r9` on entry),
   and return value (from `rax` on exit).

### GetCurrentHardwareStoppoint

When `TrapHardwareBreak` fires, the debugger reads DR6 (debug status
register) to find which of the four debug registers (DR0–DR3) triggered.
The low 4 bits of DR6 indicate which register matched; `bits.TrailingZeros64`
(Go's equivalent of `__builtin_ctzll`) finds the index. The address in the
corresponding DR is then looked up in the breakpoint and watchpoint
collections.

---

## 21. Syscall Catchpoints

**Source:** `debugger/process.go` (SyscallCatchPolicy, maybeResumeFromSyscall),
`debugger/syscalls.go` (SyscallIDToName, SyscallNameToID)

### Mental model

Syscall catchpoints let the user trace specific system calls made by the
inferior. This is the debugger equivalent of `strace` — but integrated into
the REPL so the user can inspect registers and memory at each syscall
boundary.

### PTRACE_SYSCALL

`PTRACE_SYSCALL` is like `PTRACE_CONT` but stops the tracee at the next
syscall entry *or* exit. Combined with `PTRACE_O_TRACESYSGOOD`, the kernel
delivers the stop as `SIGTRAP|0x80`, making it instantly distinguishable
from other SIGTRAP causes.

```
                   PTRACE_SYSCALL
                        │
  ┌─────────────────────▼─────────────────────────┐
  │ Tracee runs ... hits syscall(write, ...) ...   │
  │                                                │
  │ STOP #1: syscall entry                         │
  │   orig_rax = 1 (write)                         │
  │   rdi/rsi/rdx/... = args                       │
  │                                                │
  │                   PTRACE_SYSCALL                │
  │                        │                       │
  │ ... kernel executes write() ...                │
  │                                                │
  │ STOP #2: syscall exit                          │
  │   rax = return value (bytes written)           │
  └────────────────────────────────────────────────┘
```

### Entry/exit tracking

The kernel doesn't tell you whether a syscall stop is entry or exit. The
`Process` maintains an `expectingSyscallExit` boolean toggle: false → entry,
true → exit, flipped at each syscall stop.

### SyscallCatchPolicy

Three modes:
- **None** (default): No syscall tracing. `Resume` uses `PTRACE_CONT`.
- **All**: Stop at every syscall. `Resume` uses `PTRACE_SYSCALL`.
- **Some**: Stop only at listed syscalls. `Resume` uses `PTRACE_SYSCALL`,
  and `maybeResumeFromSyscall` transparently resumes through non-matching
  syscalls.

### Syscall name table

`debugger/syscalls.go` contains a static map of ~383 x86-64 syscall numbers
to names, generated from `/usr/include/asm/unistd_64.h`. Two lookup functions
provide bidirectional mapping:

- `SyscallIDToName(id) → string` (e.g., 1 → "write")
- `SyscallNameToID(name) → (int, bool)` (e.g., "write" → 1, true)

### REPL command

```
catchpoint syscall              → catch all syscalls
catchpoint syscall none         → stop catching
catchpoint syscall write,read   → catch specific syscalls (by name or number)
```

---

## 22. Watchpoint Data Tracking

**Source:** `debugger/watchpoint.go` (Data, PreviousData, UpdateData)

When a watchpoint triggers, the user wants to know *what changed*. The
`Watchpoint` struct now tracks two values:

- `data` — the current value at the watched address
- `previousData` — the value before the most recent write

`UpdateData()` reads `size` bytes from the watched address via
`ReadMemory`, stores the result in `data`, and shifts the old value
into `previousData`. It is called:

1. When the watchpoint is first created (captures the initial value).
2. Each time the watchpoint triggers (in `WaitOnSignal` when
   `TrapHardwareBreak` is detected and the stoppoint is a watchpoint).

The REPL uses these fields to display old/new values:

```
process stopped by SIGTRAP at 0x401042 (watchpoint 1)
Old value: 0x0
New value: 0x42
```

---

## 23. Testing Strategy

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

`TestMain()` compiles both Go targets with `go build` and all seven native
targets (five assembly, two C) with `gcc` into a temporary directory before
any tests run.

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
| **Hardware breakpoint** | Software BP at a function is detected by anti-debugger checksum (prints "pepperoni"); hardware BP at the same address evades the checksum (prints "pineapple"); verifies PC lands exactly at the breakpoint address |
| **Watchpoint** | Write watchpoint creation, alignment validation, CRUD operations (list, get by ID, remove); verifies watchpoint enable/disable cycle through debug registers |
| **ELF parsing** | Open ELF binaries (non-PIE assembly and C); find symbols by name (`_start`, `main`, `an_innocent_function`); verify `FunctionContainingAddress` resolves correctly; verify unknown addresses return false |
| **Target** | Launch via `LaunchTargetWithOptions`; verify ELF is loaded and load bias is computed; resume to a known point inside `main()` and verify `FunctionContainingAddress` returns `"main"` |

### Native test targets (assembly and C)

Some register behaviors can only be tested from the inferior's perspective —
for example, verifying that a debugger write to `rsi` is visible when the
inferior uses that value in a `printf` call. Go programs cannot control
which values land in specific registers (the compiler and runtime manage
register allocation), so these tests use hand-written x86-64 assembly.
The `anti_debugger` target uses C to test hardware breakpoint invisibility.
The `stepping_target` uses C compiled with `-g` to test source-level stepping.

| Target | Path | Libc | Build flags | Entry |
|--------|------|------|-------------|-------|
| `reg_read` | `test/targets/reg_read.s` | No | `-nostdlib -no-pie` | `_start` |
| `reg_write` | `test/targets/reg_write.s` | Yes | `-no-pie` | `main` |
| `hello_toydbg` | `test/targets/hello_toydbg.s` | No | `-nostdlib -no-pie` | `_start` |
| `memory` | `test/targets/memory.s` | No | `-nostdlib -no-pie` | `_start` |
| `anti_debugger` | `test/targets/anti_debugger.c` | Yes | `-no-pie` | `main` |
| `dwarf_target` | `test/targets/dwarf_target.c` | Yes | `-g -no-pie` | `main` |
| `stepping_target` | `test/targets/stepping_target.c` | Yes | `-g -no-pie` | `main` |
| `multi_threaded` | `test/targets/multi_threaded.c` | Yes | `-g -no-pie -lpthread` | `main` |
| `global_variable` | `test/targets/global_variable.c` | Yes | `-g -no-pie` | `main` |
| `blocks` | `test/targets/blocks.c` | Yes | `-g -no-pie -O0` | `main` |
| `expr_target` | `test/targets/expr_target.c` | Yes | `-g -no-pie -O0` | `main` |

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

`anti_debugger` is a C program that demonstrates why hardware breakpoints
exist. It computes a checksum of its own `an_innocent_function` body. When
a software breakpoint injects `0xCC` into the function, the checksum
changes and the program prints "pepperoni" (detected tampering). A hardware
breakpoint at the same address is invisible to memory — the checksum matches
and the program calls the function, printing "pineapple". The test uses
both breakpoint types in sequence to verify this distinction.

`TestMain` builds all seven native targets with `gcc` alongside the Go
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

## 24. ELF Parsing and the Target Type

**Source:** `debugger/elf.go`, `debugger/auxv_linux.go`, `debugger/target.go`

### Why ELF parsing?

Until now, the debugger operated purely at the ptrace level — it knew
process IDs, register values, and memory addresses, but had no idea what
*function* an address belonged to. When a breakpoint triggers at `0x401156`,
the user must mentally map that address to a function name. Real debuggers
display `main` or `an_innocent_function` next to the address.

ELF (Executable and Linkable Format) binaries contain **symbol tables**
that map names to address ranges. There are two: `.symtab` (static, may be
stripped) and `.dynsym` (dynamic, always present in shared libraries). The
debugger loads both tables so it can resolve symbols in the main executable
*and* in dynamically loaded libraries like libc (e.g. `malloc`). By merging
these tables, the debugger can answer "which function contains address X?"

### Go's `debug/elf` package

Go's standard library provides `debug/elf`, which handles all the low-level
ELF parsing: file headers, section headers, program headers, symbol tables,
and string tables. We wrap it with two pre-built lookup structures:

```
ELF
├── symbolsByName   map[string][]*elf.Symbol    ← name → symbol(s)
├── symbolsByAddr   []addrSymbol (sorted)       ← binary search by address
├── dwarf           *DWARF                      ← debug info (optional)
└── cfi             *CallFrameInformation       ← stack unwinding (optional)
```

- **`symbolsByName`** — direct map lookup for "find the symbol named `main`".
- **`symbolsByAddr`** — sorted slice of `{address, *Symbol}` pairs. For
  "which function contains address X?", we binary search for the last symbol
  with `addr ≤ X`, then check if `X < addr + size`.

Only symbols with `Size > 0` enter the address index, since zero-size
symbols cannot meaningfully "contain" an address.

### Load bias and the auxiliary vector

For a traditional `ET_EXEC` binary (compiled with `-no-pie`), symbol
addresses in the ELF file are absolute virtual addresses — the kernel loads
the binary at exactly the address specified. The **load bias** is 0.

For a PIE (`ET_DYN`) binary, the kernel chooses a random base address
(ASLR). Symbol values in the ELF file are offsets from that base. The load
bias is `actual_load_address - elf_expected_address`.

To compute the load bias, we read the **auxiliary vector** from
`/proc/<pid>/auxv`. The kernel populates this array of `{tag, value}` pairs
on the stack during `execve`. The key entry is `AT_ENTRY` (tag 9), which
holds the actual entry point address. The load bias is:

```
load_bias = auxv[AT_ENTRY] - elf.Entry
```

For non-PIE: both are the same address, so `load_bias = 0`.
For PIE: `auxv[AT_ENTRY]` includes the ASLR offset, `elf.Entry` does not.

The `ELF` type stores the load bias and all address-lookup methods subtract
it to convert virtual addresses (from the running process) to file addresses
(from the symbol table).

### The Target type

`Target` is the new top-level abstraction that combines a `Process` (ptrace
operations) with an `ELF` (symbol resolution):

```
Target
├── process  *Process   ← ptrace, registers, breakpoints
└── elf      *ELF       ← symbol table, load bias
```

`LaunchTarget` and `AttachTarget` replace direct `Launch`/`Attach` calls in
the CLI. They:

1. Create the `Process` via existing `Launch`/`Attach`
2. Open the ELF file (path from argument, or `/proc/<pid>/exe` for attach)
3. Read `/proc/<pid>/auxv` to get `AT_ENTRY`
4. Compute and store the load bias

ELF loading is **non-fatal** — if the binary is stripped, statically linked
with no symbols, or `/proc` is unavailable, the `Target` works normally
without symbol resolution.

### CLI integration

The REPL's stop message now includes the function name when available:

```
process stopped by SIGTRAP at 0x401156 (main) (breakpoint 1)
```

The `printStopReason` function calls `formatFunctionInfo(target, pc)` which
prefers DWARF (showing source file and line) over the plain symbol table:

```
DWARF present:  "process stopped by SIGTRAP at 0x401156 (main at dwarf_target.c:30)"
DWARF absent:   "process stopped by SIGTRAP at 0x401156 (main)"
No symbols:     "process stopped by SIGTRAP at 0x401156"
```

---

## 25. DWARF Debug Information

DWARF (Debugging With Arbitrary Record Formats) is the standard encoding for
debug information on Linux. When a program is compiled with `-g`, the compiler
writes DWARF data into ELF sections (`.debug_info`, `.debug_line`,
`.debug_abbrev`, `.debug_ranges`). This data maps machine addresses back to
source constructs: function names, source files, line numbers.

### Why DWARF when we already have `.symtab`/`.dynsym`?

The ELF symbol tables (`.symtab` + `.dynsym`) give us function names and
sizes — enough for "which function is this address in?" But they cannot answer:

- **What source file and line produced this instruction?**
- **Where does an inlined function live in the caller?**
- **What are the variable types and locations?**

DWARF provides all of this. For now, toydbg uses it for function lookup with
source locations. The infrastructure supports future extensions (variable
inspection, line-level stepping).

### Mental model: the DIE tree

DWARF organizes debug info as a tree of **DIEs** (Debugging Information
Entries). Each DIE has a **tag** and **attributes**:

```
TagCompileUnit (name="main.c", language=C)
├── TagSubprogram (name="main", low_pc=0x401130, high_pc=0x4011a0)
│   └── TagInlinedSubroutine (abstract_origin→"add_numbers", ranges=...)
├── TagSubprogram (name="add_numbers", low_pc=0x401100, high_pc=0x401120)
└── TagSubprogram (name="multiply_numbers", low_pc=0x401120, high_pc=0x401130)
```

- **`TagCompileUnit`** — one per source file, owns a line number program
- **`TagSubprogram`** — a function, with address range(s)
- **`TagInlinedSubroutine`** — an inlined function call, references its
  original definition via `AttrAbstractOrigin`

### Go's `debug/dwarf` package

Go's standard library handles all the complex DWARF encoding:

| What | Go API |
|------|--------|
| Load all DWARF sections | `elf.File.DWARF()` → `*dwarf.Data` |
| Walk the DIE tree | `dwarf.Reader.Next()`, `SkipChildren()` |
| Read a DIE's attributes | `dwarf.Entry.Val(attr)` |
| Get address ranges | `dwarf.Data.Ranges(entry)` → `[][2]uint64` |
| Map PC → source line | `dwarf.LineReader.SeekPC()` |

This mirrors how `debugger/elf.go` wraps `debug/elf` — the stdlib parses,
we build indexes.

### Our wrapper: `debugger/dwarf.go`

The `DWARF` struct holds pre-built indexes for fast lookup:

```go
type DWARF struct {
    data        *dwarf.Data
    funcsByAddr []funcAddrEntry          // sorted by startPC for binary search
    funcsByName map[string][]*FunctionEntry
    lines       lineIndex
    loadBias    uint64
}
```

**Constructor (`newDWARF`):** Single-pass walk of the DIE tree:
1. `TagSubprogram` with ranges → add to both `funcsByAddr` and `funcsByName`
2. `TagInlinedSubroutine` → add to `funcsByName` only (resolve name via
   `AttrAbstractOrigin`)
3. Sort `funcsByAddr` by start address

**Query methods:**

| Method | Purpose | Complexity |
|--------|---------|------------|
| `FunctionContainingPC(addr)` | Binary search on sorted function address index | O(log n) |
| `FunctionsByName(name)` | Map lookup | O(1) |
| `PCToSourceLocation(addr)` | Delegates to `GetEntryByAddress`, returns `SourceLocation` | O(log n) |
| `GetEntryByAddress(addr)` | Binary search on sorted line table index | O(log n) |
| `GetEntriesByLine(path, line)` | Map lookup + path suffix matching | O(matches) |
| `AllLineEntries()` | Return a copy of all line entries (file-address space) | O(n) |
| `InlineStackAtAddress(addr)` | Walk DIE tree to build inline call stack at address | O(tree depth) |
| `PrologueEndForRange(low, high)` | Scan line table for `PrologueEnd` flag or second entry in range | O(entries in range) |

### Line table index

The DWARF line table maps machine addresses to source positions. Go's
`dwarf.LineReader` runs the DWARF line number state machine internally —
we iterate `Next()` to collect all rows into a `LineEntry` slice:

```go
type LineEntry struct {
    Address       uint64
    File          string   // resolved from dwarf.LineFile.Name
    Line          int
    Column        int
    IsStmt        bool
    BasicBlock    bool
    EndSequence   bool
    PrologueEnd   bool
    EpilogueBegin bool
    Discriminator int
}
```

The `lineIndex` holds two data structures built during `newDWARF`:

1. **`entries []LineEntry`** — all line table rows, sorted by address.
   `GetEntryByAddress` binary-searches this slice, skipping `EndSequence`
   markers (which are range terminators, not real source positions).

2. **`byFileLine map[fileLineKey][]int`** — maps `{file, line}` pairs to
   indices into `entries`. `GetEntriesByLine` iterates matching keys and
   uses `pathEndsWith` for flexible path matching (e.g., `"dwarf_target.c"`
   matches `/full/path/to/targets/dwarf_target.c`).

This replaces the old `PCToSourceLocation` approach of re-iterating
compile units on every call. The `PCToSourceLocation` method still exists
for backward compatibility but now delegates to `GetEntryByAddress`.

### Load bias propagation

DWARF addresses are file addresses (the addresses in the ELF before ASLR
relocation). When querying by virtual address, we subtract the load bias first.
The `ELF.SetLoadBias()` method propagates the bias to both the DWARF and
CFI wrappers:

```go
func (e *ELF) SetLoadBias(bias uint64) {
    e.loadBias = bias
    if e.dwarf != nil {
        e.dwarf.loadBias = bias
    }
    if e.cfi != nil {
        e.cfi.loadBias = bias
    }
}
```

### Integration with the CLI

The `formatFunctionInfo` function in `cmd/toydbg/main.go` prefers DWARF
when available, falling back to the symbol table:

1. Try `DWARF.FunctionContainingPC(pc)` + `PCToSourceLocation(pc)`
2. Fall back to `ELF.FunctionContainingAddress(pc)` (symtab)
3. Return empty string if neither works

---

## 26. Source-Level Stepping and Breakpoints

The previous sections described instruction-level stepping (`StepInstruction`) and
address-only breakpoints (`CreateBreakpointSite`). This section covers the
**source-level** layer built on top: stepping by source lines, setting breakpoints
by function name or file:line, and displaying source context.

All source-level operations live in **`debugger/stepping.go`** as methods on
`*Target` (not `*Process`), because they need both the DWARF metadata and the
ptrace process handle.

### Mental model

Think of the source-level layer as a "macro" layer that repeatedly calls the
instruction-level primitives until a source-line-level condition is met:

```
  User types "step"
      │
      ▼
  StepIn() loop:
      ├── StepInstruction()   ← instruction-level
      ├── GetEntryByAddress() ← check DWARF line table
      └── Line changed? ──no──→ loop
                │yes
                ▼
          skipPrologueIfNeeded()
                │
                ▼
          return StopReason
```

### RunUntilAddress — the temp-breakpoint helper

`RunUntilAddress(addr)` is the building block for StepOver, StepOut, and
prologue skipping. It:

1. Creates an internal (hidden) software breakpoint at `addr`
2. Calls `Resume()` + `WaitOnSignal()`
3. Removes the temporary breakpoint
4. If we stopped at the target, marks the trap reason as `TrapSingleStep`

This avoids burning CPU on single-step loops when the destination is far away.

### StepIn (source-level step into)

Steps instructions until the DWARF line table reports a different `file:line`.
After arriving at a new line, checks whether the PC is at the start of a
function (matching any range's `lowPC`). If so, it calls `PrologueEndForRange`
and `RunUntilAddress` to skip past register-saving instructions.

### StepOver (source-level step over)

Like StepIn, but when the current instruction is a `CALL`:

1. Decode instruction via `x86asm.Decode()`
2. If `inst.Op == CALL`, set temp breakpoint at `pc + inst.Len` (the return
   address, i.e., the instruction after the CALL) and `RunUntilAddress()`
3. Otherwise, `StepInstruction()` as usual

The loop continues until the source line changes.

### StepOut (step out of current function)

Three strategies, tried in order:

1. **Inlined frame:** Walk `InlineStackAtAddress()`. If the deepest frame is
   inlined, `RunUntilAddress(highPC + loadBias)` — the end of the inlined range.
2. **CFI unwinding:** Call `cfi.UnwindFrame()` to compute the caller's register
   state, read the return address from the unwound `rip`, and `RunUntilAddress()`.
   This works with any calling convention, including `-fomit-frame-pointer`.
3. **Fallback (rbp):** Read `rbp` register, read 8 bytes at `rbp+8` (the return
   address in the frame-pointer calling convention), `RunUntilAddress(retAddr)`.

### Source-level breakpoints

**By function name:** `SetBreakpointAtFunction(name)` looks up functions via
`DWARF.FunctionsByName()`, finds the prologue end via `PrologueEndForRange()`,
adjusts by load bias, and creates a breakpoint site. Falls back to ELF symbol
table if no DWARF match.

**By file:line:** `SetBreakpointAtLine(file, line)` calls
`DWARF.GetEntriesByLine()` and creates breakpoints at each matching address.
If the address happens to be a function's `lowPC`, it skips the prologue.

### Prologue skipping

When a breakpoint lands at a function's first instruction, the frame pointer
hasn't been set up yet — local variables are inaccessible and `rbp` still
points to the caller's frame. `PrologueEndForRange(lowPC, highPC)` finds the
end of the prologue by:

1. Scanning the DWARF line table for `PrologueEnd == true` (DWARF 3+ compilers
   like GCC set this flag)
2. Falling back to the second line entry in the function range (the book's
   heuristic: first entry = prologue, second = body)

### Source display (PrintSource / PrintSourceAtPC)

`PrintSourceAtPC` resolves the current PC to a source file and line via DWARF,
opens the source file, and prints surrounding lines with a `>` marker on the
current line. `handleStop` in the CLI prefers source display over disassembly
when DWARF info is available.

### Inline stack tracking

`DWARF.InlineStackAtAddress(addr)` walks the DWARF DIE tree under the containing
function, recursing through `TagInlinedSubroutine` and `TagLexDwarfBlock` entries
whose ranges contain the address. The result is a stack ordered outermost →
innermost. This is used by `StepOut` to detect when we're in an inlined frame.

### CLI commands

| Command | Action |
|---------|--------|
| `step` / `s` | Source-level step into (StepIn) |
| `next` / `n` | Source-level step over (StepOver) |
| `finish` / `fin` | Step out of current function (StepOut) |
| `stepi` / `si` | Single machine instruction step (StepInstruction) |
| `list` / `l` | Display source code at current PC |
| `break set <func>` | Set breakpoint at named function |
| `break set file:line` | Set breakpoint at source line |
| `break set 0xaddr` | Set breakpoint at address (unchanged) |

---

## 27. Call Frame Information (CFI)

### Why CFI matters

Without CFI, a debugger must read `[rbp+8]` to find the return address. This
works with frame-pointer ABI, but breaks with `-fomit-frame-pointer` or optimized code
where `rbp` is used as a general-purpose register. **Call Frame Information** is the
DWARF mechanism for stack unwinding that works with *any* calling convention — each
program counter maps to rules for recovering the previous frame's registers.
`StepOut()` now uses CFI as its primary strategy, falling back to `[rbp+8]` only
when CFI is unavailable.

### .eh_frame vs .debug_frame

Both sections encode the same CFI data, but `.eh_frame` is the standard on Linux:

| Aspect | `.eh_frame` | `.debug_frame` |
|--------|-------------|----------------|
| Purpose | Exception handling + debugging | Debugging only |
| CIE ID field | 0 (backwards pointer convention) | 0xffffffff |
| Pointer encoding | Supports pcrel, datarel, etc. | Absolute only |
| Retained in stripped binaries? | Yes (needed for C++ exceptions) | No |

The debugger parses `.eh_frame` because it is always present (even in stripped binaries),
while `.debug_frame` is often absent.

### CIE and FDE structure

CFI data is organized as a series of entries in `.eh_frame`:

```
┌─────────────────────────────────────────┐
│ CIE (Common Information Entry)          │
│  ├── length, CIE_id=0, version          │
│  ├── augmentation string ("zR", "zPLR") │
│  ├── code_alignment_factor (ULEB128)    │  ← x86-64: 1
│  ├── data_alignment_factor (SLEB128)    │  ← x86-64: -8
│  ├── return_address_register (ULEB128)  │  ← x86-64: 16 (RA)
│  ├── [augmentation data: R, L, P, ...]  │
│  └── initial_instructions               │
├─────────────────────────────────────────┤
│ FDE (Frame Description Entry)           │
│  ├── length, CIE_pointer (backward)     │
│  ├── initial_location + address_range   │
│  ├── [augmentation data]                │
│  └── instructions                       │
├─────────────────────────────────────────┤
│ FDE ...                                 │
├─────────────────────────────────────────┤
│ CIE (another group) ...                │
└─────────────────────────────────────────┘
```

The **CIE pointer** in an FDE is a *backward offset* from the CIE pointer field
itself to the start of the referenced CIE. This means `cieOffset = fieldPosition - ciePtr`.

### Pointer encoding

`.eh_frame` uses a compact pointer encoding scheme where a single byte describes both
*format* (how bytes are stored) and *application* (what the value is relative to):

```
 encoding byte: [ application (4 bits) | format (4 bits) ]

 Format (low nibble):        Application (high nibble):
   0x00 = absptr (8 bytes)     0x00 = absolute
   0x01 = uleb128              0x10 = pcrel (relative to this byte's address)
   0x02 = udata2               0x20 = textrel (relative to .text)
   0x03 = udata4               0x30 = datarel (relative to section start)
   0x04 = udata8
   0x09 = sleb128            Special:
   0x0a = sdata2               0xff = omit (value absent)
   0x0b = sdata4
   0x0c = sdata8
```

On x86-64 Linux, the typical encoding is `0x1b` = `DW_EH_PE_pcrel | DW_EH_PE_sdata4`,
meaning each pointer is a 4-byte signed offset relative to its own position. This makes
`.eh_frame` position-independent — critical for shared libraries.

### Augmentation string

The augmentation string in a CIE (e.g., `"zR"`, `"zPLR"`) describes optional data:

- **`z`**: Augmentation data has a ULEB128 length prefix (enables skipping unknown data)
- **`R`**: FDE pointer encoding byte follows — controls how `initial_location` is encoded
- **`L`**: LSDA (Language-Specific Data Area) encoding byte for exception tables
- **`P`**: Personality routine encoding byte + pointer (for C++ exception handling)
- **`S`**: Signal frame marker (no data)

### .eh_frame_hdr binary search

The `.eh_frame_hdr` section provides a **sorted table** of `(initial_location, fde_pointer)`
pairs, enabling O(log n) FDE lookup instead of scanning the entire `.eh_frame`:

```
.eh_frame_hdr layout:
  version (1 byte, must be 1)
  eh_frame_ptr_enc (1 byte)
  fde_count_enc (1 byte)
  table_enc (1 byte)
  eh_frame_ptr (encoded)
  fde_count (encoded)
  ┌──────────────────────────────┐
  │ search table (sorted):       │
  │   initial_location₀, fde₀   │
  │   initial_location₁, fde₁   │
  │   ...                        │
  └──────────────────────────────┘
```

`FDEForPC()` binary searches this table, then parses the FDE at the found offset.
If `.eh_frame_hdr` is absent, it falls back to a linear scan of `.eh_frame`.

### Implementation

CFI parsing lives in **`debugger/cfi.go`** with these key types:

- **`cfiReader`**: Stateful cursor over a byte slice with a `baseAddr` field tracking
  the virtual address of `data[0]`. This is essential for `DW_EH_PE_pcrel` decoding —
  the "current PC" is `baseAddr + pos`.

- **`CIE`**: Parsed Common Information Entry with alignment factors, return register,
  augmentation flags, and initial instructions.

- **`FDE`**: Parsed Frame Description Entry with initial location, address range,
  CIE reference, and instructions.

- **`CallFrameInformation`**: Main container that holds `.eh_frame` data, a CIE cache,
  and the `.eh_frame_hdr` binary search table. Provides `FDEForPC(pc)` for lookup.

### ELF integration

`OpenELF()` reads `.eh_frame`, `.eh_frame_hdr`, and `.text` sections, constructing
`CallFrameInformation` if `.eh_frame` exists. `SetLoadBias()` propagates the bias
to CFI (just as it does for DWARF). Access via `ELF.CFI()`.

### SLEB128 note

Go's `encoding/binary.Varint` uses **zigzag encoding** (protobuf style), not SLEB128.
SLEB128 uses sign extension of the last byte's bit 6. The `cfiReader.readSLEB128()`
method implements the DWARF algorithm directly.

---

## 28. Stack Unwinding

**Source:** `debugger/unwind.go`, `debugger/stack.go`

Stack unwinding is the process of reconstructing the call chain from the
current execution point back to `main()`. It enables the `backtrace` command
and powers CFI-based `StepOut` (which no longer requires frame pointers).

### Mental model

The unwinder walks the call stack one physical frame at a time. At each frame
it:

1. **Finds the FDE** for the current PC via `FDEForPC()`
2. **Executes CFI instructions** (a bytecode program) to build the unwind
   table row for that PC
3. **Applies register restore rules** to produce the caller's register state
4. **Checks for inlined frames** at the PC and synthesizes logical frames

```
  Current Frame (add)          Caller Frame (main)
  ┌──────────────────┐         ┌──────────────────┐
  │ registers (live)  │  CFI   │ registers (restored)│
  │ PC = 0x400470     │ ────►  │ PC = 0x4004c4       │
  │ CFA = rsp + 16    │        │ CFA = rsp + 8        │
  └──────────────────┘         └──────────────────┘
         │                            │
         ▼                            ▼
    FDE instructions:             Next FDE...
    DW_CFA_def_cfa r7, 8
    DW_CFA_offset r16, -8
    DW_CFA_advance_loc ...
```

### CFI instruction interpreter

The CFI bytecode has 25 instructions operating on an abstract machine with:

- **location** — the file address of the current table row
- **cfa_rule** — how to compute the Canonical Frame Address (register + offset)
- **register_rules** — map from DWARF register number to restore rule

Instructions are split into two groups:

| Encoding | Instructions |
|----------|-------------|
| Primary (high 2 bits) | `DW_CFA_advance_loc` (delta in low 6 bits), `DW_CFA_offset` (reg + ULEB128), `DW_CFA_restore` |
| Extended (low 6 bits) | `DW_CFA_def_cfa`, `DW_CFA_def_cfa_register`, `DW_CFA_def_cfa_offset`, `DW_CFA_set_loc`, `DW_CFA_advance_loc{1,2,4}`, `DW_CFA_undefined`, `DW_CFA_same_value`, `DW_CFA_register`, `DW_CFA_remember_state`, `DW_CFA_restore_state`, and signed/extended variants |

The interpreter runs in two phases:

1. Execute the CIE's `initial_instructions` to establish baseline rules
2. Execute the FDE's `instructions` until `location > target_PC`

### Register restore rules

Each register can have one of five rules:

| Rule | Meaning |
|------|---------|
| `undefined` | Cannot restore (register clobbered) |
| `same_value` | Register wasn't modified |
| `offset(N)` | Previous value at memory `CFA + N` |
| `val_offset(N)` | Previous value IS `CFA + N` |
| `register(R)` | Previous value stored in register R |

### Stack frame type

```go
type StackFrame struct {
    Regs         *Registers
    PC           uint64
    FunctionName string
    Inlined      bool
    Location     SourceLocation
    CFA          uint64
}
```

### Inline frame handling

When the PC falls within a `DW_TAG_inlined_subroutine`, the unwinder creates
multiple logical frames for the single physical frame:

- **Innermost inlined frame:** PC from actual registers, source from line table
- **Outer frames:** PC from `DW_AT_low_pc` of inner subroutine, source from
  `DW_AT_call_file` / `DW_AT_call_line` attributes

### StepOut improvement

`StepOut` now uses CFI unwinding instead of `[rbp+8]`:

1. Call `cfi.UnwindFrame(proc, pc, regs)` to get the caller's register state
2. Read `rip` from the unwound registers — this is the return address
3. `RunUntilAddress(retAddr)` to run to the caller

This works with optimized code where `rbp` may be used as a general-purpose
register (e.g., with `-fomit-frame-pointer`).

### CLI commands

| Command | Action |
|---------|--------|
| `backtrace` / `bt` | Print the call stack |

---

## 29. Shared Library Support

When a dynamically-linked executable runs, the kernel loads the **dynamic linker**
(`ld-linux-x86-64.so.2`), which maps shared libraries into the process's address
space. The debugger needs to know about these libraries so it can set breakpoints,
look up symbols, read DWARF debug info, and unwind the stack across library boundaries.

### Why it matters

Without shared library tracking, the debugger can only see symbols and debug info
from the main binary. A breakpoint on `calc_check_threshold` — defined in
`libcalc.so` — would fail because the debugger wouldn't know that function exists.
Stack unwinding would also stop at the library boundary.

### The rendezvous structure

The dynamic linker maintains a **rendezvous structure** (`r_debug`) in the inferior's
address space. The debugger finds it through these steps:

```
.dynamic section (DT_DEBUG entry)
    ↓
r_debug struct
    ├── r_map  → linked list of loaded libraries (link_map)
    ├── r_brk  → address of _dl_debug_state function
    └── r_state → RT_CONSISTENT / RT_ADD / RT_DELETE
```

### Discovery algorithm

1. **Entry-point breakpoint** — At launch, set an internal breakpoint on the real
   program entry point (from `AT_ENTRY` in the auxiliary vector). The dynamic linker
   runs first; when it jumps to the entry point, the rendezvous structure is initialized.

2. **Read `.dynamic` section** — Find the `DT_DEBUG` entry, which the dynamic linker
   patches at runtime to point to `r_debug`.

3. **Walk the link map** — Read `r_debug.r_map`, a linked list of `link_map` entries.
   Each entry contains the load address and path of a shared library. Open each as
   a new `ELF` object with its own load bias, DWARF, and CFI.

4. **`_dl_debug_state` breakpoint** — Set an internal breakpoint on `r_debug.r_brk`.
   The dynamic linker calls this empty function whenever it loads or unloads a library.
   When it fires, re-walk the link map to discover new libraries.

5. **vDSO handling** — The virtual dynamic shared object (`linux-vdso.so.1`) doesn't
   exist on disk. It's dumped from process memory to a temp file so it can be parsed
   like any other ELF.

### Multi-ELF architecture

The `Target` type was refactored from a single `*ELF` to an `ELFCollection`:

```go
type Target struct {
    process        *Process
    elves          ELFCollection  // all loaded ELF objects
    mainELF        *ELF           // convenience pointer to main binary
    rendezvousAddr uint64         // r_debug address (0 until resolved)
}
```

All lookups — function name resolution, DWARF source mapping, CFI unwinding —
now search across all loaded ELFs. The `ELFContainingAddress(pc)` method finds
which ELF a virtual address belongs to by checking LOAD segment ranges.

### Breakpoint hit handlers

Breakpoint sites gained an `onHit` callback (`func() bool`). When a breakpoint
with a handler fires, the handler runs automatically. If it returns `true`,
the process resumes without involving the user; if it returns `false`, execution
stops normally. The `_dl_debug_state` breakpoint returns `true` (auto-resume
after re-walking the link map), while the entry-point breakpoint returns `false`
(stop at the program entry so `LaunchTarget` can return with libraries discovered).

### Backtrace across libraries

Stack frames now carry an `ELFFilename` field. The backtrace formatter qualifies
function names with the ELF filename using a backtick separator:

```
*[0]: 0x7f... libcalc.so`calc_check_threshold at libcalc.c:7
 [1]: 0x4004.. calc_client`main at calc_client.c:12
 [2]: 0x7f... libc.so.6`??
```

### Key files

| File | Role |
|------|------|
| `debugger/dynlib.go` | Rendezvous structure reading, link map walking, vDSO dumping |
| `debugger/elf_collection.go` | `ELFCollection` type: push, lookup by path/filename/address |
| `debugger/target.go` | Entry-point breakpoint, `resolveRendezvous` integration |
| `debugger/breakpoint_site.go` | Hit handler support (`InstallHitHandler`, `notifyHit`) |

---

## 30. Multithreading Support

### Mental Model

A process can contain multiple threads, each with its own registers, stack, and execution state, but sharing memory (code and data). From a debugger's perspective, the key challenge is **tracking all threads** and **coordinating their stops** so the user can inspect a consistent snapshot.

The debugger implements **all-stop mode**: when any thread hits a breakpoint or receives a reportable signal, *all* threads are stopped before control returns to the user. This is the same model used by GDB's default behavior and is simpler to reason about than non-stop mode.

### Thread Discovery

Threads are discovered through two mechanisms:

1. **`PTRACE_O_TRACECLONE`** — When enabled, the kernel notifies us whenever the tracee calls `clone()` to create a new thread. The parent thread stops with a `PTRACE_EVENT_CLONE` event, and the new child thread gets an initial `SIGSTOP`.

2. **`/proc/<pid>/task`** — When attaching to an already-running process, existing threads are enumerated by reading this directory. Each entry is seized and interrupted individually.

Thread tracking is a `Target`-level feature. Bare `Process` objects (used in simple tests) don't enable `PTRACE_O_TRACECLONE` to avoid interfering with programs that `clone()` without the debugger caring about thread lifecycle. `Target` calls `EnableThreadTracking()` after construction.

### Per-Thread State

```go
type ThreadState struct {
    TID                  int          // Linux thread ID (= PID for main thread)
    Regs                 *Registers   // per-thread register cache
    State                ProcessState // stopped/running/exited
    Reason               StopReason   // why it last stopped
    PendingSigstop       bool         // SIGSTOP queued but not yet consumed
    ExpectingSyscallExit bool         // for syscall entry/exit toggling
}
```

The `Process` struct holds a `map[int]*ThreadState` keyed by TID. The `currentThread` field determines which thread register reads, stepping, and other operations target.

### The Two-Pass Resume

`ResumeAllThreads()` uses a two-pass approach to avoid a race condition:

1. **Step over breakpoints** for all stopped threads (disable BP, single-step one instruction, re-enable).
2. **Send `PTRACE_CONT`** to all stopped threads.

If we did this in one pass (step-then-continue per thread), thread A might run past a breakpoint while it's temporarily disabled for thread B's step-over. The two-pass approach ensures all breakpoints are re-enabled before any thread starts running.

### Pending SIGSTOP

When stopping all running threads (all-stop mode), we send `SIGSTOP` via `tgkill()`. But a thread might stop for a *different* signal before the `SIGSTOP` is delivered. In that case:

- We process the actual signal that caused the stop
- We mark `PendingSigstop = true` on that thread
- Before any future single-step on that thread, `swallowPendingSigstop()` does a quick `PTRACE_CONT` + `wait4` to consume the queued `SIGSTOP`

Without this, `PTRACE_SINGLESTEP` would immediately stop due to the pending `SIGSTOP` rather than executing the instruction.

### Hardware Stoppoint Replication

Debug registers (DR0–DR7) are per-thread in the kernel. When a hardware breakpoint or watchpoint is set, the debugger replicates the DR values to all existing threads. New threads automatically inherit the current thread's debug register state.

### REPL Thread Commands

```
thread list              — show all threads and their states
thread select <tid>      — switch to a different thread
```

The `continue` command uses `ResumeAllThreads()` to resume all threads, not just the current one. Stop reasons include the TID when multiple threads exist.

### Wait Logic

`waitOnSignalFor()` calls `wait4(-1, __WALL)` to wait for any thread. The `__WALL` flag is essential for receiving events from threads created via `clone()`. Each stop event goes through `handleSignal()` which:

- Discovers new threads (creates `ThreadState`)
- Handles clone events transparently (resumes the parent)
- Augments SIGTRAP with the specific trap cause
- Filters syscalls based on the catch policy
- Consumes pending SIGSTOPs

When a reportable stop occurs, `stopRunningThreads()` sends `SIGSTOP` to all other running threads and waits for each to stop, implementing all-stop semantics.

---

## 31. DWARF Expressions and Variable Reading

DWARF uses a **stack-based bytecode language** to describe where variables live.
This is necessary because a variable's storage can change during execution — it
might start in a register, spill to the stack, or be optimized away entirely.

### The taxonomy of DWARF expressions

```
DWARF expression
├── Single location description
│   ├── Simple
│   │   ├── Address    — variable lives at a computed memory address
│   │   ├── Register   — variable lives in a register (DW_OP_reg0..31, DW_OP_regx)
│   │   ├── Implicit   — variable has no storage but a known value
│   │   └── Empty      — variable location is unknown (optimized out)
│   └── Composite      — variable is split across multiple locations (DW_OP_piece)
└── Location list      — location depends on program counter value
```

### How expressions are encoded

Single location descriptions use `DW_FORM_exprloc`: a ULEB128 length followed
by bytecode. Location lists use `DW_FORM_sec_offset`: an offset into the
`.debug_loc` section containing range-to-expression mappings.

### The stack machine (`dwarf_expr.go`)

The evaluator implements ~60 DWARF opcodes operating on a `[]uint64` stack:

| Category | Opcodes | Example |
|----------|---------|---------|
| **Literals** | `DW_OP_lit0..31`, `DW_OP_const*` | Push constants |
| **Registers** | `DW_OP_reg0..31`, `DW_OP_regx`, `DW_OP_breg*`, `DW_OP_bregx` | Read register values |
| **Memory** | `DW_OP_deref`, `DW_OP_deref_size` | Read from tracee memory |
| **Arithmetic** | `DW_OP_plus`, `DW_OP_minus`, `DW_OP_mul`, etc. | Stack arithmetic |
| **Control flow** | `DW_OP_skip`, `DW_OP_bra` | Branches (conditional/unconditional) |
| **Frame** | `DW_OP_fbreg`, `DW_OP_call_frame_cfa` | Frame base / CFA access |
| **Composite** | `DW_OP_piece`, `DW_OP_bit_piece` | Multi-location variables |
| **Implicit** | `DW_OP_stack_value`, `DW_OP_implicit_value` | Values without storage |

The `inFrameInfo` flag changes `DW_OP_reg*` semantics: in `.eh_frame` context,
register opcodes push the register's value (for CFA computation); in normal
context, they record a register location.

### Location lists (`dwarf_loc.go`)

A location list maps PC ranges to expressions. The evaluator:

1. Reads the current PC from the register state
2. Scans entries in `.debug_loc` until finding one whose range contains the PC
3. Evaluates that entry's expression

Base address selection entries (`first == 0xFFFFFFFFFFFFFFFF`) update the
base address for subsequent range calculations.

### Global variable index

The `DWARF` type lazily builds a global variable index by walking the DIE tree,
collecting `DW_TAG_variable` entries with `DW_AT_location` that aren't nested
inside any `DW_TAG_subprogram`. This enables `FindGlobalVariable("g_int")` to
quickly locate the DIE for a named global.

### CFI expression rules

The stack unwinder now handles three DWARF expression CFI instructions:

- `DW_CFA_def_cfa_expression` — compute CFA via a DWARF expression
- `DW_CFA_expression` — register's previous value is at address computed by expression
- `DW_CFA_val_expression` — register's previous value IS the expression result

For CFI expression rules, the CFA is pushed onto the stack before evaluation.

### Reading variable data

`ReadLocationData()` handles all location kinds:

- **Register**: reads the register and returns its bytes
- **Address**: reads N bytes from tracee memory
- **Literal/Data**: returns the inline value
- **Composite**: assembles data from multiple pieces, supporting bit-level granularity via `memcpyBits()`

### REPL command

The `variable` command supports typed reading, local variable listing, and
location queries:

```
(toydbg) var read g_int
Value: 42

(toydbg) var read debuggee.regs[0].name
Value: "rax"

(toydbg) var locals
argc: 1
argv: 0x7fffffffe2a8
i: 42

(toydbg) var location i
Address: 0x7fffffffe1ec
```

---

## 32. Types and Variables

Building on the DWARF expression infrastructure (Section 30), toydbg can now
read, visualize, and navigate typed variables — local, global, and compound.

### Why types matter

Raw bytes from `ReadLocationData` are meaningless without knowing their type.
DWARF records the type of every variable via `DW_AT_type`, which references a
chain of type DIEs (base types, pointers, arrays, structs, qualifiers, typedefs).
Rather than walking these DIEs manually, toydbg leverages Go's `debug/dwarf`
package, which provides `Data.Type(offset)` returning fully-resolved Go types.

### Go's DWARF type hierarchy

Go's `debug/dwarf` models DWARF type DIEs as concrete Go types:

| DWARF Tag | Go type | Notes |
|-----------|---------|-------|
| `DW_TAG_base_type` (signed) | `*dwarf.IntType` | Embeds `BasicType` |
| `DW_TAG_base_type` (unsigned) | `*dwarf.UintType` | Embeds `BasicType` |
| `DW_TAG_base_type` (char) | `*dwarf.CharType` / `*dwarf.UcharType` | |
| `DW_TAG_base_type` (float) | `*dwarf.FloatType` | |
| `DW_TAG_base_type` (bool) | `*dwarf.BoolType` | |
| `DW_TAG_pointer_type` | `*dwarf.PtrType` | `.Type` = pointee |
| `DW_TAG_array_type` | `*dwarf.ArrayType` | `.Count`, `.Type` = element |
| `DW_TAG_structure_type` / `DW_TAG_class_type` | `*dwarf.StructType` | `.Field` = members |
| `DW_TAG_const_type` / `DW_TAG_volatile_type` | `*dwarf.QualType` | `.Type` = inner |
| `DW_TAG_typedef` | `*dwarf.TypedefType` | `.Type` = aliased |
| `DW_TAG_enumeration_type` | `*dwarf.EnumType` | `.Val` = enumerators |

Because `*dwarf.IntType` is distinct from `*dwarf.BasicType` in Go's type system
(even though it embeds it), the `toBasicType()` helper extracts the embedded
`BasicType` from any of these subtypes for uniform visualization.

### TypedData

`TypedData` in `debugger/type.go` combines:

- `Data []byte` — raw variable bytes
- `Type dwarf.Type` — the resolved DWARF type
- `Address *uint64` — optional memory address (nil for register-resident values)

Key methods:

- `Visualize(proc, depth)` — pretty-prints the value as a string
- `DerefPointer(proc)` — reads the pointed-to value from memory
- `ReadMember(proc, name)` — extracts a struct member by name
- `Index(proc, i)` — reads element `i` from an array or pointer

### Visualization dispatch

`Visualize` strips qualifiers/typedefs, then dispatches by Go type:

- **Basic types**: formatted by name and size (int→decimal, float→%g, char→'c', bool→true/false)
- **Pointers**: hex address, or `"string"` if pointing to a char type
- **Arrays**: `[elem0, elem1, ...]` with recursive element visualization
- **Structs**: `{\n\tfield: value\n}` with depth-based indentation
- **Enums**: enumerator name if found, numeric fallback

### Bitfield support

DWARF encodes bitfields with `DW_AT_bit_size` and either `DW_AT_bit_offset`
(deprecated) or `DW_AT_data_bit_offset` (DWARF 4+). The `fieldByteOffset()`
helper computes the correct byte position, and `fixupBitfieldData()` uses
`memcpyBits()` to extract the relevant bits into a clean buffer before
visualization.

### Lexical scopes and local variables

Local variables live inside `DW_TAG_subprogram` DIEs (functions) and
`DW_TAG_lexical_block` DIEs (inner scopes like `if` or `for` blocks).
`ScopesAtAddress(addr)` walks the DIE tree recursively to find all
enclosing scopes at a given PC, deepest first. `FindLocalVariable(name, addr)`
searches these scopes for a matching `DW_TAG_variable` or
`DW_TAG_formal_parameter` DIE, returning the innermost match (correct
shadowing behavior).

### Frame base and CFA

Local variable locations often use `DW_OP_fbreg` (frame-base-relative offset).
The frame base itself is usually `DW_OP_call_frame_cfa`, which requires the
Canonical Frame Address from CFI. The `evalFrameBase()` function computes the
CFA via `UnwindFrame()` and passes it to the expression evaluator, enabling
correct resolution of stack-allocated locals.

### Compound name resolution

`ResolveIndirectName("info.regs[0].name")` parses the expression and
navigates the type hierarchy:

1. Find variable `info` → get its `TypedData`
2. `.regs` → `ReadMember(proc, "regs")` → pointer value
3. `[0]` → `Index(proc, 0)` → dereference pointer at offset
4. `.name` → `ReadMember(proc, "name")` → char pointer
5. `Visualize` → read null-terminated string → `"rax"`

Supported operators: `.` (member access), `->` (pointer dereference + member),
`[n]` (array/pointer indexing).

---

## 33. Expression Evaluation

Expression evaluation lets the user call functions inside the running process and
inspect the results. This is the most complex debugger feature because it bridges
multiple subsystems: DWARF lookup, ABI conventions, memory allocation, and process
control.

### Mental Model

When the user types `expr decode_signal(7)`, the debugger:

1. **Parses** the expression into a function name (`decode_signal`) and arguments (`7`)
2. **Locates** the function via DWARF info or the ELF symbol table
3. **Classifies** each argument per the **SysV x86-64 ABI** — integers go in
   `rdi, rsi, rdx, rcx, r8, r9`; floats go in `xmm0–xmm7`; large structs go
   on the stack
4. **Saves** all registers (so the call is invisible to the program)
5. **Sets RIP** to the function's entry point
6. **Pushes** a return address onto the stack — we reuse the program's `AT_ENTRY`
   address, since the entry-point breakpoint has already fired
7. **Resumes** the process; the function runs until it returns to the entry-point
   breakpoint
8. **Reads** the return value from `rax`/`rdx` (integer), `xmm0`/`xmm1` (float/SSE),
   or memory (large struct)
9. **Restores** the original registers

### Argument Types

The evaluator supports these literal argument types:

| Syntax | Type | ABI Class |
|--------|------|-----------|
| `42`, `-1` | Integer (int64) | INTEGER |
| `3.14` | Float (double) | SSE |
| `"hello"` | String (const char*) | INTEGER (pointer) |
| `'a'` | Character | INTEGER |
| `true`, `false` | Boolean | INTEGER |
| `my_var` | Variable name | depends on DWARF type |
| `$0` | Previous result | depends on stored type |

String arguments require allocating memory in the inferior (via `inferior_malloc`,
which calls `malloc()` inside the tracee). The string is written there and a pointer
is passed.

### Return Value Storage

Return values are dynamically allocated in the inferior via `inferior_malloc` so they
persist beyond the current stack frame. Users can reference previous results using
`$N` syntax (e.g., `$0`, `$1`). Re-reading a `$N` variable re-fetches from memory,
catching any mutations from subsequent calls.

### Key Types and Functions

```
debugger/expr.go:
  EvaluateExpression(expr string) → *ExpressionResult
  GetExpressionResult(index int) → *TypedData
  inferiorMalloc(size int) → uint64
  findCallableFunction(name string) → (addr, *dwarf.Entry)
  classifyType(typ, builtin) → [2]parameterClass
  setupArguments(args, funcDIE, regs, returnSlot, returnType)
  readReturnValue(funcDIE, returnType, returnSlot, regs) → *TypedData
```

### SysV x86-64 ABI Classification

The classification algorithm determines how each argument is passed:

- **INTEGER**: integer types, pointers, booleans → GPRs (rdi, rsi, rdx, rcx, r8, r9)
- **SSE**: float/double → XMM registers (xmm0–xmm7)
- **MEMORY**: structs > 16 bytes → stack
- **X87**: long double → stack (returned via st0)

For structs ≤ 16 bytes, each 8-byte chunk ("eightbyte") is classified independently
and the results are merged. If any eightbyte is MEMORY, the whole struct goes on the
stack.

---

## 34. File Reference

| File | Purpose |
|------|---------|
| `cmd/toydbg/main.go` | CLI entry point: argument parsing, REPL loop, command dispatch (continue, step, next, finish, stepi, list, backtrace, breakpoint, watchpoint, register, memory, disassemble, catchpoint, variable, expression, help, quit), SIGINT handler, source-level stop display |
| `debugger/debugger.go` | Package documentation |
| `debugger/elf.go` | ELF binary wrapper: symbol lookup by name and address, load bias, FunctionContainingAddress, DWARF loading, CFI loading, ContainsAddress, NotifyLoaded |
| `debugger/elf_collection.go` | ELFCollection type: multi-ELF container with lookup by path, filename, or address |
| `debugger/dynlib.go` | Dynamic linker integration: rendezvous structure (r_debug), link map walking, vDSO dumping, shared library discovery |
| `debugger/dwarf.go` | DWARF debug info wrapper: function lookup by address/name, line table index (GetEntryByAddress, GetEntriesByLine, AllLineEntries), source location mapping, InlineStackAtAddress, PrologueEndForRange, ScopesAtAddress, FindLocalVariable, LocalVariablesAtAddress, ReadDIE, ResolveType |
| `debugger/dwarf_expr.go` | DWARF expression evaluator: stack machine implementing ~60 DW_OP opcodes, result types (SimpleLocation, Piece, ExprResult), DW_OP_fbreg frame base lookup |
| `debugger/dwarf_loc.go` | Location attribute evaluation (exprloc and location lists), .debug_loc parsing, global variable index (FindGlobalVariable), ReadLocationData for all location kinds, bit-level memcpyBits |
| `debugger/cfi.go` | Call Frame Information parser: CIE/FDE parsing, pointer encoding, .eh_frame_hdr binary search, FDEForPC lookup |
| `debugger/auxv_linux.go` | Linux `/proc/<pid>/auxv` reader for AT_ENTRY (load bias computation) |
| `.devcontainer/devcontainer.json` | Dev container configuration (ptrace capabilities, VS Code extensions) |
| `.devcontainer/Dockerfile` | Dev container image (Go toolchain + gcc) |
| `debugger/type.go` | Typed variable support: TypedData struct, Visualize (base types, pointers, arrays, structs, enums), DerefPointer, ReadMember, Index, bitfield fixup, qualifier stripping, char type detection |
| `debugger/expr.go` | Expression evaluation engine: EvaluateExpression, inferior function calls, SysV ABI argument classification (INTEGER/SSE/MEMORY), argument setup, return value reading, inferior_malloc, expression result storage ($N references) |
| `debugger/target.go` | Target type: combines Process + ELFCollection for symbolic debugging, entry-point breakpoint for dynamic linker init, shared library discovery (LaunchTarget, AttachTarget), FindVariable, ResolveIndirectName, VariableLocation, LocalVariables, expression result storage |
| `debugger/unwind.go` | CFI instruction interpreter: executeCFIInstruction (including DW_CFA_expression, DW_CFA_val_expression, DW_CFA_def_cfa_expression), UnwindFrame, register rule types (including expression-based rules), unwind context |
| `debugger/stack.go` | Stack frame type and stack unwinding orchestration: UnwindStack, Backtrace, FormatBacktrace, inline frame handling |
| `debugger/stepping.go` | Source-level operations: StepIn, StepOver, StepOut (CFI-based), RunUntilAddress, SetBreakpointAtFunction, SetBreakpointAtLine, PrintSource, PrintSourceAtPC, SourceLocationAtPC, InlineDepthAtPC, DWARF (accessor) |
| `debugger/error.go` | Custom error type and constructors |
| `debugger/format.go` | `FormatRegisterValue` — display formatting for all register types |
| `debugger/parse.go` | `ParseRegisterValue` — CLI string → typed value conversion |
| `debugger/breakpoint_site.go` | BreakpointSite type (software and hardware, enable/disable, hit handlers) and breakpointSiteCollection |
| `debugger/watchpoint.go` | Watchpoint type (data read/write triggers via debug registers, data tracking) and watchpointCollection |
| `debugger/stoppoint_mode.go` | StoppointMode enum (Execute, Write, ReadWrite) — shared by hardware breakpoints and watchpoints |
| `debugger/process.go` | Process lifecycle: Launch, LaunchWithOptions, Attach, Resume, WaitOnSignal, GetPC, SetPC, breakpoint management, hardware stoppoint methods, watchpoint management, StepInstruction, ReadMemory, WriteMemory, ReadMemoryWithoutTraps, Close, TrapType, SyscallCatchPolicy, augmentStopReason, GetCurrentHardwareStoppoint |
| `debugger/syscalls.go` | Syscall name/ID mapping table (generated from unistd_64.h): SyscallIDToName, SyscallNameToID |
| `debugger/memory_linux.go` | Linux `process_vm_readv` wrapper for bulk memory reads |
| `debugger/disassembler.go` | Disassembler type: decodes x86-64 instructions via `x86asm` into AT&T syntax |
| `debugger/ptrace_linux.go` | Linux ptrace syscall wrappers (including GETSIGINFO, SETOPTIONS, SYSCALL) |
| `debugger/register_info.go` | Register metadata table (125 entries) and lookup functions |
| `debugger/registers_linux.go` | Register cache: read/write via ptrace |
| `test/debugger_test.go` | Integration tests (launch, attach, resume, register metadata, register I/O, assembly register tests, breakpoint tests, hardware breakpoint tests, watchpoint tests, memory read/write tests, syscall mapping tests, syscall catchpoint tests, ELF parsing tests, Target tests, DWARF tests, source-level stepping tests, source-level breakpoint tests, inline stack tests, source display tests, CFI tests, stack unwinding tests, shared library tracing tests, DWARF expression tests, global variable reading tests) |
| `test/targets/end_immediately/main.go` | Test target: exits immediately |
| `test/targets/run_endlessly/main.go` | Test target: infinite loop |
| `test/targets/reg_read.s` | Assembly test target: sets known register values and traps (no libc) |
| `test/targets/reg_write.s` | Assembly test target: prints debugger-written register values via printf |
| `test/targets/hello_toydbg.s` | Assembly test target: write + exit (no libc, non-PIE, used for breakpoint tests) |
| `test/targets/memory.s` | Assembly test target: stores known values and provides buffers for memory read/write tests |
| `test/targets/anti_debugger.c` | C test target: checksums its own function to detect software breakpoints (tests hardware breakpoint invisibility) |
| `test/targets/dwarf_target.c` | C test target: compiled with `-g` for DWARF debug info tests (function lookup, source location) |
| `test/targets/stepping_target.c` | C test target: compiled with `-g` for source-level stepping tests (StepIn, StepOver, StepOut, line/function breakpoints) |
| `test/targets/libcalc.c` | Shared library source: defines `calc_check_threshold()` for shared library tracing tests |
| `test/targets/calc_client.c` | Main program source: links against `libcalc.so`, used to test cross-library breakpoints and stack unwinding |
| `test/targets/multi_threaded.c` | C test target: spawns 10 pthreads calling `say_hi()`, used for multithreading tests |
| `test/targets/global_variable.c` | C test target: global variables with structs, bitfields, pointers, and arrays for typed variable reading tests |
| `test/targets/blocks.c` | C test target: nested lexical scopes redefining `i`, used for local variable scoping tests |
| `test/targets/expr_target.c` | C test target: various function signatures (int, double, string, char, struct, void, 6-arg) for expression evaluation tests |
| `doc.go` | Module-root package declaration |
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
