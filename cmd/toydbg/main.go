package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/chzyer/readline"
	"golang.org/x/sys/unix"

	"x86-64-toy-debugger/debugger"
)

func main() {
	var pid int
	flag.IntVar(&pid, "p", 0, "attach to process by PID")
	flag.Usage = usage
	flag.Parse()

	if (pid == 0 && flag.NArg() != 1) || (pid != 0 && flag.NArg() != 0) {
		usage()
		os.Exit(2)
	}

	var (
		target *debugger.Target
		err    error
	)

	if pid != 0 {
		target, err = debugger.AttachTarget(pid)
	} else {
		target, err = debugger.LaunchTarget(flag.Arg(0))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
		os.Exit(1)
	}
	defer target.Close()

	proc := target.Process()

	// Install SIGINT handler: when the user presses Ctrl+C, send SIGSTOP
	// to the inferior instead of killing the debugger. Setpgid in Launch
	// isolates the inferior's process group, so Ctrl+C only reaches us.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT)
	go func() {
		for range sigCh {
			// Send SIGSTOP to the inferior to interrupt it.
			_ = syscall.Kill(proc.Pid(), syscall.SIGSTOP)
		}
	}()

	rl, err := readline.NewEx(&readline.Config{
		Prompt: "(toydbg) ",
	})
	if err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: REPL init failed: %v\n", err)
		os.Exit(1)
	}
	defer rl.Close()

	for {
		line, err := rl.Readline()
		if err == readline.ErrInterrupt {
			continue
		}
		if err == io.EOF {
			fmt.Println()
			return
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: REPL read failed: %v\n", err)
			return
		}

		fields := strings.Fields(line)
		if len(fields) == 0 {
			handleHelp(nil)
			continue
		}

		switch fields[0] {
		case "help", "h":
			handleHelp(fields[1:])
		case "continue", "c":
			if err := proc.ResumeAllThreads(); err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				continue
			}

			reason, err := proc.WaitOnSignal()
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				return
			}
			handleStop(target, reason)

			if reason.Reason == debugger.ProcessExited || reason.Reason == debugger.ProcessTerminated {
				return
			}
		case "breakpoint", "break":
			handleBreakpoint(target, fields[1:])
		case "stepi", "si":
			reason, err := proc.StepInstruction()
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				continue
			}
			handleStop(target, reason)

			if reason.Reason == debugger.ProcessExited || reason.Reason == debugger.ProcessTerminated {
				return
			}
		case "step", "s":
			reason, err := target.StepIn()
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				continue
			}
			handleStop(target, reason)

			if reason.Reason == debugger.ProcessExited || reason.Reason == debugger.ProcessTerminated {
				return
			}
		case "next", "n":
			reason, err := target.StepOver()
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				continue
			}
			handleStop(target, reason)

			if reason.Reason == debugger.ProcessExited || reason.Reason == debugger.ProcessTerminated {
				return
			}
		case "finish", "fin":
			reason, err := target.StepOut()
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				continue
			}
			handleStop(target, reason)

			if reason.Reason == debugger.ProcessExited || reason.Reason == debugger.ProcessTerminated {
				return
			}
		case "list", "l":
			if !target.PrintSourceAtPC(os.Stdout, 5) {
				fmt.Println("no source information available at current PC")
			}
		case "register", "reg":
			handleRegister(proc, fields[1:])
		case "memory", "mem":
			handleMemory(proc, fields[1:])
		case "watchpoint", "watch":
			handleWatchpoint(proc, fields[1:])
		case "disassemble", "disas":
			handleDisassemble(proc, fields[1:])
		case "catchpoint", "catch":
			handleCatchpoint(proc, fields[1:])
		case "backtrace", "bt":
			handleBacktrace(target)
		case "variable", "var":
			handleVariable(target, fields[1:])
		case "expression", "expr":
			handleExpression(target, line)
		case "thread", "t":
			handleThread(proc, fields[1:])
		case "quit", "q", "exit":
			return
		default:
			fmt.Printf("unknown command: %q\n", fields[0])
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  toydbg /path/to/program")
	fmt.Fprintln(os.Stderr, "  toydbg -p <pid>")
}

func handleHelp(args []string) {
	if len(args) > 0 {
		switch args[0] {
		case "register", "reg":
			fmt.Println("register subcommands:")
			fmt.Println("  register read             - read all GPRs")
			fmt.Println("  register read all          - read all registers")
			fmt.Println("  register read <name>       - read a single register")
			fmt.Println("  register write <name> <value> - write a register")
			return
		case "breakpoint", "break":
			fmt.Println("breakpoint subcommands:")
			fmt.Println("  breakpoint set <hex-addr>         - set breakpoint at address")
			fmt.Println("  breakpoint set <function>         - set breakpoint at function")
			fmt.Println("  breakpoint set <file>:<line>      - set breakpoint at source line")
			fmt.Println("  breakpoint set -h <location>      - set a hardware breakpoint")
			fmt.Println("  breakpoint list                   - list all breakpoints")
			fmt.Println("  breakpoint enable <id>            - enable a breakpoint")
			fmt.Println("  breakpoint disable <id>           - disable a breakpoint")
			fmt.Println("  breakpoint delete <id>            - delete a breakpoint")
			return
		case "watchpoint", "watch":
			fmt.Println("watchpoint subcommands:")
			fmt.Println("  watchpoint set <hex-addr> <mode> <size>  - set a watchpoint")
			fmt.Println("    modes: write, rw (read-write)")
			fmt.Println("    sizes: 1, 2, 4, 8")
			fmt.Println("  watchpoint list                          - list all watchpoints")
			fmt.Println("  watchpoint enable <id>                   - enable a watchpoint")
			fmt.Println("  watchpoint disable <id>                  - disable a watchpoint")
			fmt.Println("  watchpoint delete <id>                   - delete a watchpoint")
			return
		case "memory", "mem":
			fmt.Println("memory subcommands:")
			fmt.Println("  memory read <address>       - read 32 bytes at address")
			fmt.Println("  memory read <address> <n>   - read n bytes at address")
			fmt.Println("  memory write <address> [0xff,0x01,...] - write bytes at address")
			return
		case "disassemble", "disas":
			fmt.Println("disassemble options:")
			fmt.Println("  disassemble                - disassemble 5 instructions at PC")
			fmt.Println("  disassemble -c <n>         - disassemble n instructions at PC")
			fmt.Println("  disassemble -a <address>   - disassemble 5 instructions at address")
			fmt.Println("  disassemble -c <n> -a <address> - disassemble n instructions at address")
			return
		case "catchpoint", "catch":
			fmt.Println("catchpoint subcommands:")
			fmt.Println("  catchpoint syscall              - catch all syscalls")
			fmt.Println("  catchpoint syscall none         - stop catching syscalls")
			fmt.Println("  catchpoint syscall <list>       - catch specific syscalls (by name or number)")
			fmt.Println("  example: catchpoint syscall write,read,42")
			return
		case "variable", "var":
			fmt.Println("variable subcommands:")
			fmt.Println("  variable read <name>            - read a variable (supports name.field, ptr->field, arr[i])")
			fmt.Println("  variable locals                 - print all local variables in scope")
			fmt.Println("  variable location <name>        - print where a variable lives (register/address)")
			return
		case "expression", "expr":
			fmt.Println("expression evaluates a function call in the running process.")
			fmt.Println("  expression <func>(<args...>)    - call a function and display the return value")
			fmt.Println("  examples:")
			fmt.Println("    expr get_value(42)")
			fmt.Println("    expr format_name(\"test\")")
			fmt.Println("    expr compute(3.14)")
			fmt.Println("    expr process(my_var)")
			fmt.Println("    expr transform($0)            - pass a previous expression result")
			return
		}
	}
	fmt.Println("commands: continue (c), step (s), next (n), finish (fin), stepi (si), list (l), backtrace (bt), breakpoint (break), watchpoint (watch), register (reg), memory (mem), disassemble (disas), catchpoint (catch), variable (var), expression (expr), thread (t), quit (q), help (h)")
}

func handleBreakpoint(target *debugger.Target, args []string) {
	proc := target.Process()
	if len(args) == 0 {
		fmt.Println("usage: breakpoint <set|list|enable|disable|delete> [args...]")
		fmt.Println("  type 'help breakpoint' for details")
		return
	}

	switch args[0] {
	case "set":
		hardware := false
		addrArgs := args[1:]
		if len(addrArgs) > 0 && addrArgs[0] == "-h" {
			hardware = true
			addrArgs = addrArgs[1:]
		}
		if len(addrArgs) < 1 {
			fmt.Println("usage: breakpoint set [-h] <addr|func|file:line>")
			return
		}
		location := addrArgs[0]

		// Determine location type:
		// - starts with "0x": hex address
		// - contains ":": file:line
		// - otherwise: function name
		if strings.HasPrefix(location, "0x") {
			addr, err := strconv.ParseUint(strings.TrimPrefix(location, "0x"), 16, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: invalid address %q: %v\n", location, err)
				return
			}
			site, err := proc.CreateBreakpointSite(addr, hardware, false)
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				return
			}
			if err := site.Enable(); err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				return
			}
			kind := "software"
			if hardware {
				kind = "hardware"
			}
			fmt.Printf("%s breakpoint %d set at 0x%x\n", kind, site.ID(), site.Address())
		} else if strings.Contains(location, ":") {
			parts := strings.SplitN(location, ":", 2)
			line, err := strconv.Atoi(parts[1])
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: invalid line number %q: %v\n", parts[1], err)
				return
			}
			sites, err := target.SetBreakpointAtLine(parts[0], line, hardware)
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				return
			}
			for _, site := range sites {
				fmt.Printf("breakpoint %d set at 0x%x (%s:%d)\n", site.ID(), site.Address(), parts[0], line)
			}
		} else {
			sites, err := target.SetBreakpointAtFunction(location, hardware)
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				return
			}
			for _, site := range sites {
				fmt.Printf("breakpoint %d set at 0x%x (function %s)\n", site.ID(), site.Address(), location)
			}
		}

	case "list":
		sites := proc.BreakpointSites()
		if len(sites) == 0 {
			fmt.Println("no breakpoints")
			return
		}
		for _, s := range sites {
			status := "disabled"
			if s.IsEnabled() {
				status = "enabled"
			}
			kind := "sw"
			if s.IsHardware() {
				kind = "hw"
			}
			fmt.Printf("  %d: 0x%x [%s] (%s)\n", s.ID(), s.Address(), kind, status)
		}

	case "enable":
		if len(args) < 2 {
			fmt.Println("usage: breakpoint enable <id>")
			return
		}
		id, err := strconv.ParseInt(args[1], 10, 32)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid ID %q: %v\n", args[1], err)
			return
		}
		site, ok := proc.BreakpointSiteByID(int32(id))
		if !ok {
			fmt.Fprintf(os.Stderr, "toydbg: breakpoint %d not found\n", id)
			return
		}
		if err := site.Enable(); err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
			return
		}
		fmt.Printf("breakpoint %d enabled\n", id)

	case "disable":
		if len(args) < 2 {
			fmt.Println("usage: breakpoint disable <id>")
			return
		}
		id, err := strconv.ParseInt(args[1], 10, 32)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid ID %q: %v\n", args[1], err)
			return
		}
		site, ok := proc.BreakpointSiteByID(int32(id))
		if !ok {
			fmt.Fprintf(os.Stderr, "toydbg: breakpoint %d not found\n", id)
			return
		}
		if err := site.Disable(); err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
			return
		}
		fmt.Printf("breakpoint %d disabled\n", id)

	case "delete":
		if len(args) < 2 {
			fmt.Println("usage: breakpoint delete <id>")
			return
		}
		id, err := strconv.ParseInt(args[1], 10, 32)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid ID %q: %v\n", args[1], err)
			return
		}
		if err := proc.RemoveBreakpointSite(int32(id)); err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
			return
		}
		fmt.Printf("breakpoint %d deleted\n", id)

	default:
		fmt.Printf("unknown breakpoint subcommand: %q\n", args[0])
	}
}

func handleWatchpoint(proc *debugger.Process, args []string) {
	if len(args) == 0 {
		fmt.Println("usage: watchpoint <set|list|enable|disable|delete> [args...]")
		fmt.Println("  type 'help watchpoint' for details")
		return
	}

	switch args[0] {
	case "set":
		if len(args) < 4 {
			fmt.Println("usage: watchpoint set <hex-addr> <write|rw> <1|2|4|8>")
			return
		}
		addr, err := strconv.ParseUint(strings.TrimPrefix(args[1], "0x"), 16, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid address %q: %v\n", args[1], err)
			return
		}
		var mode debugger.StoppointMode
		switch args[2] {
		case "write", "w":
			mode = debugger.StoppointModeWrite
		case "rw", "readwrite":
			mode = debugger.StoppointModeReadWrite
		default:
			fmt.Fprintf(os.Stderr, "toydbg: invalid mode %q (use 'write' or 'rw')\n", args[2])
			return
		}
		size, err := strconv.Atoi(args[3])
		if err != nil || (size != 1 && size != 2 && size != 4 && size != 8) {
			fmt.Fprintf(os.Stderr, "toydbg: invalid size %q (must be 1, 2, 4, or 8)\n", args[3])
			return
		}
		wp, err := proc.CreateWatchpoint(addr, mode, size)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
			return
		}
		fmt.Printf("watchpoint %d set at 0x%x (mode=%s, size=%d)\n",
			wp.ID(), wp.Address(), args[2], wp.Size())

	case "list":
		wps := proc.Watchpoints()
		if len(wps) == 0 {
			fmt.Println("no watchpoints")
			return
		}
		for _, wp := range wps {
			status := "disabled"
			if wp.IsEnabled() {
				status = "enabled"
			}
			modeStr := "write"
			if wp.Mode() == debugger.StoppointModeReadWrite {
				modeStr = "rw"
			}
			fmt.Printf("  %d: 0x%x (mode=%s, size=%d, %s)\n",
				wp.ID(), wp.Address(), modeStr, wp.Size(), status)
		}

	case "enable":
		if len(args) < 2 {
			fmt.Println("usage: watchpoint enable <id>")
			return
		}
		id, err := strconv.ParseInt(args[1], 10, 32)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid ID %q: %v\n", args[1], err)
			return
		}
		wp, ok := proc.WatchpointByID(int32(id))
		if !ok {
			fmt.Fprintf(os.Stderr, "toydbg: watchpoint %d not found\n", id)
			return
		}
		if err := wp.Enable(); err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
			return
		}
		fmt.Printf("watchpoint %d enabled\n", id)

	case "disable":
		if len(args) < 2 {
			fmt.Println("usage: watchpoint disable <id>")
			return
		}
		id, err := strconv.ParseInt(args[1], 10, 32)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid ID %q: %v\n", args[1], err)
			return
		}
		wp, ok := proc.WatchpointByID(int32(id))
		if !ok {
			fmt.Fprintf(os.Stderr, "toydbg: watchpoint %d not found\n", id)
			return
		}
		if err := wp.Disable(); err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
			return
		}
		fmt.Printf("watchpoint %d disabled\n", id)

	case "delete":
		if len(args) < 2 {
			fmt.Println("usage: watchpoint delete <id>")
			return
		}
		id, err := strconv.ParseInt(args[1], 10, 32)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid ID %q: %v\n", args[1], err)
			return
		}
		if err := proc.RemoveWatchpoint(int32(id)); err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
			return
		}
		fmt.Printf("watchpoint %d deleted\n", id)

	default:
		fmt.Printf("unknown watchpoint subcommand: %q\n", args[0])
	}
}

func handleCatchpoint(proc *debugger.Process, args []string) {
	if len(args) == 0 {
		fmt.Println("usage: catchpoint syscall [none|<name-or-number>,...]")
		fmt.Println("  type 'help catchpoint' for details")
		return
	}

	if args[0] != "syscall" {
		fmt.Printf("unknown catchpoint type: %q (only 'syscall' is supported)\n", args[0])
		return
	}

	if len(args) == 1 {
		// "catchpoint syscall" with no further args → catch all.
		proc.SetSyscallCatchPolicy(debugger.CatchAllSyscalls())
		fmt.Println("catching all syscalls")
		return
	}

	if args[1] == "none" {
		proc.SetSyscallCatchPolicy(debugger.CatchNoSyscalls())
		fmt.Println("stopped catching syscalls")
		return
	}

	// Parse comma-separated list of syscall names or numbers.
	parts := strings.Split(args[1], ",")
	var ids []int
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		// Try as a number first.
		if num, err := strconv.Atoi(p); err == nil {
			ids = append(ids, num)
			continue
		}
		// Try as a name.
		if id, ok := debugger.SyscallNameToID(p); ok {
			ids = append(ids, id)
			continue
		}
		fmt.Fprintf(os.Stderr, "toydbg: unknown syscall %q\n", p)
		return
	}

	if len(ids) == 0 {
		fmt.Println("no valid syscalls specified")
		return
	}

	proc.SetSyscallCatchPolicy(debugger.CatchSomeSyscalls(ids))
	names := make([]string, len(ids))
	for i, id := range ids {
		names[i] = debugger.SyscallIDToName(id)
	}
	fmt.Printf("catching syscalls: %s\n", strings.Join(names, ", "))
}

func handleRegister(proc *debugger.Process, args []string) {
	if len(args) == 0 {
		fmt.Println("usage: register <read|write> [args...]")
		fmt.Println("  type 'help register' for details")
		return
	}

	switch args[0] {
	case "read":
		handleRegisterRead(proc, args[1:])
	case "write":
		handleRegisterWrite(proc, args[1:])
	default:
		fmt.Printf("unknown register subcommand: %q\n", args[0])
	}
}

func handleRegisterRead(proc *debugger.Process, args []string) {
	regs := proc.Registers()
	if regs == nil {
		fmt.Fprintln(os.Stderr, "toydbg: registers not available")
		return
	}

	if len(args) == 0 {
		// Print all GPRs.
		for _, info := range debugger.AllRegisterInfos() {
			if info.Type == debugger.RegisterTypeGPR {
				val := regs.Read(info)
				fmt.Printf("%-12s %s\n", info.Name, debugger.FormatRegisterValue(info, val))
			}
		}
		return
	}

	if args[0] == "all" {
		for _, info := range debugger.AllRegisterInfos() {
			val := regs.Read(info)
			fmt.Printf("%-12s %s\n", info.Name, debugger.FormatRegisterValue(info, val))
		}
		return
	}

	// Single register by name.
	info, ok := debugger.RegisterInfoByName(args[0])
	if !ok {
		fmt.Fprintf(os.Stderr, "toydbg: unknown register %q\n", args[0])
		return
	}
	val := regs.Read(info)
	fmt.Printf("%-12s %s\n", info.Name, debugger.FormatRegisterValue(info, val))
}

func handleRegisterWrite(proc *debugger.Process, args []string) {
	if len(args) < 2 {
		fmt.Println("usage: register write <name> <value>")
		return
	}

	name := args[0]
	valueStr := strings.Join(args[1:], " ")

	info, ok := debugger.RegisterInfoByName(name)
	if !ok {
		fmt.Fprintf(os.Stderr, "toydbg: unknown register %q\n", name)
		return
	}

	val, err := debugger.ParseRegisterValue(info, valueStr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
		return
	}

	regs := proc.Registers()
	if regs == nil {
		fmt.Fprintln(os.Stderr, "toydbg: registers not available")
		return
	}

	if err := regs.Write(info, val); err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: write failed: %v\n", err)
		return
	}
	fmt.Printf("%-12s %s\n", info.Name, debugger.FormatRegisterValue(info, val))
}

// signalName returns the symbolic name of a signal (e.g., "SIGTRAP")
// or falls back to a numeric representation.
func signalName(sig uint8) string {
	name := unix.SignalName(syscall.Signal(sig))
	if name != "" {
		return name
	}
	return fmt.Sprintf("signal %d", sig)
}

// getSigtrapInfo returns extra context about a SIGTRAP stop, such as
// which breakpoint or watchpoint triggered it, or syscall details.
func getSigtrapInfo(proc *debugger.Process, reason debugger.StopReason) string {
	if reason.TrapReason == nil {
		return ""
	}

	switch *reason.TrapReason {
	case debugger.TrapSingleStep:
		return " (single step)"

	case debugger.TrapSoftwareBreak:
		pc, err := proc.GetPC()
		if err != nil {
			return " (software breakpoint)"
		}
		for _, bp := range proc.BreakpointSites() {
			if bp.Address() == pc && !bp.IsHardware() {
				return fmt.Sprintf(" (breakpoint %d)", bp.ID())
			}
		}
		return " (software breakpoint)"

	case debugger.TrapHardwareBreak:
		hs, err := proc.GetCurrentHardwareStoppoint()
		if err != nil {
			return " (hardware breakpoint)"
		}
		if hs.IsWatchpoint {
			wp, ok := proc.WatchpointByID(hs.WatchpointID)
			if ok {
				return fmt.Sprintf(" (watchpoint %d)\nOld value: 0x%x\nNew value: 0x%x",
					wp.ID(), wp.PreviousData(), wp.Data())
			}
			return fmt.Sprintf(" (watchpoint %d)", hs.WatchpointID)
		}
		return fmt.Sprintf(" (breakpoint %d)", hs.BreakpointID)

	case debugger.TrapSyscall:
		if reason.SyscallInfo == nil {
			return " (syscall)"
		}
		info := reason.SyscallInfo
		name := debugger.SyscallIDToName(int(info.ID))
		if info.Entry {
			return fmt.Sprintf(" (syscall entry)\nsyscall: %s(0x%x, 0x%x, 0x%x, 0x%x, 0x%x, 0x%x)",
				name, info.Args[0], info.Args[1], info.Args[2],
				info.Args[3], info.Args[4], info.Args[5])
		}
		return fmt.Sprintf(" (syscall exit)\nsyscall: %s returned %d",
			name, info.Ret)
	}

	return ""
}

func printStopReason(target *debugger.Target, reason debugger.StopReason) {
	proc := target.Process()
	threadPrefix := ""
	if len(proc.ThreadStates()) > 1 && reason.TID != 0 {
		threadPrefix = fmt.Sprintf("thread %d: ", reason.TID)
	}
	switch reason.Reason {
	case debugger.ProcessStopped:
		sigName := signalName(reason.Info)
		trapInfo := getSigtrapInfo(proc, reason)
		pc, err := proc.GetPC()
		if err != nil {
			fmt.Printf("%sprocess stopped by %s%s\n", threadPrefix, sigName, trapInfo)
		} else {
			funcInfo := formatFunctionInfo(target, pc)
			fmt.Printf("%sprocess stopped by %s at 0x%x%s%s\n", threadPrefix, sigName, pc, funcInfo, trapInfo)
		}
	case debugger.ProcessExited:
		fmt.Printf("process exited with code %d\n", reason.Info)
	case debugger.ProcessTerminated:
		fmt.Printf("process terminated by %s\n", signalName(reason.Info))
	case debugger.ProcessRunning:
		fmt.Println("process running")
	default:
		fmt.Println("process status changed")
	}
}

// formatFunctionInfo returns a parenthesized annotation for a stop address.
// It searches all loaded ELFs (main binary + shared libraries) and qualifies
// the function name with the ELF filename when in a shared library.
//
//	Main binary DWARF:  " (main at dwarf_target.c:30)"
//	Shared library:     " (libcalc.so`func at libcalc.c:4)"
//	Symtab only:        " (main)"
//	No symbols:         ""
func formatFunctionInfo(target *debugger.Target, pc uint64) string {
	elves := target.ELFs()
	if elves == nil {
		return ""
	}

	e := elves.ELFContainingAddress(pc)
	if e == nil {
		e = target.ELF()
	}
	if e == nil {
		return ""
	}

	prefix := ""
	if e != target.ELF() {
		prefix = e.Filename() + "`"
	}

	// Try DWARF first — it gives us function name + source location.
	if dw := e.DWARF(); dw != nil {
		if fn := dw.FunctionContainingPC(pc); fn != nil {
			if loc, ok := dw.PCToSourceLocation(pc); ok {
				return fmt.Sprintf(" (%s%s at %s:%d)", prefix, fn.Name, filepath.Base(loc.File), loc.Line)
			}
			return fmt.Sprintf(" (%s%s)", prefix, fn.Name)
		}
	}

	// Fall back to the ELF symbol table.
	if name, ok := e.FunctionContainingAddress(pc); ok {
		return fmt.Sprintf(" (%s%s)", prefix, name)
	}

	return ""
}

// handleStop prints the stop reason and, when the process is stopped,
// shows source context if DWARF info is available, otherwise falls
// back to disassembly.
func handleStop(target *debugger.Target, reason debugger.StopReason) {
	printStopReason(target, reason)
	if reason.Reason == debugger.ProcessStopped {
		if !target.PrintSourceAtPC(os.Stdout, 2) {
			printDisassembly(target.Process(), 5, nil)
		}
	}
}

func handleMemory(proc *debugger.Process, args []string) {
	if len(args) == 0 {
		fmt.Println("usage: memory <read|write> [args...]")
		fmt.Println("  type 'help memory' for details")
		return
	}

	switch args[0] {
	case "read":
		if len(args) < 2 {
			fmt.Println("usage: memory read <address> [n]")
			return
		}
		addr, err := strconv.ParseUint(strings.TrimPrefix(args[1], "0x"), 16, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid address %q: %v\n", args[1], err)
			return
		}
		amount := 32
		if len(args) >= 3 {
			n, err := strconv.Atoi(args[2])
			if err != nil || n <= 0 {
				fmt.Fprintf(os.Stderr, "toydbg: invalid byte count %q\n", args[2])
				return
			}
			amount = n
		}
		data, err := proc.ReadMemory(addr, amount)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: memory read failed: %v\n", err)
			return
		}
		// Print in rows of 16 bytes.
		for i := 0; i < len(data); i += 16 {
			fmt.Printf("  0x%x: ", addr+uint64(i))
			end := i + 16
			if end > len(data) {
				end = len(data)
			}
			for j := i; j < end; j++ {
				fmt.Printf("%02x ", data[j])
			}
			fmt.Println()
		}

	case "write":
		if len(args) < 3 {
			fmt.Println("usage: memory write <address> [0xff,0x01,...]")
			return
		}
		addr, err := strconv.ParseUint(strings.TrimPrefix(args[1], "0x"), 16, 64)
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid address %q: %v\n", args[1], err)
			return
		}
		data, err := parseByteList(args[2])
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
			return
		}
		if err := proc.WriteMemory(addr, data); err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: memory write failed: %v\n", err)
			return
		}
		fmt.Printf("wrote %d bytes at 0x%x\n", len(data), addr)

	default:
		fmt.Printf("unknown memory subcommand: %q\n", args[0])
	}
}

// parseByteList parses a bracket-delimited comma-separated list of hex
// bytes: "[0xff,0x01,0x02]".
func parseByteList(s string) ([]byte, error) {
	s = strings.TrimPrefix(s, "[")
	s = strings.TrimSuffix(s, "]")
	parts := strings.Split(s, ",")
	var result []byte
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		val, err := strconv.ParseUint(strings.TrimPrefix(p, "0x"), 16, 8)
		if err != nil {
			return nil, fmt.Errorf("invalid byte %q: %v", p, err)
		}
		result = append(result, byte(val))
	}
	if len(result) == 0 {
		return nil, fmt.Errorf("empty byte list")
	}
	return result, nil
}

func handleDisassemble(proc *debugger.Process, args []string) {
	count := 5
	var addr *uint64

	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c":
			if i+1 >= len(args) {
				fmt.Println("usage: disassemble [-c count] [-a address]")
				return
			}
			i++
			n, err := strconv.Atoi(args[i])
			if err != nil || n <= 0 {
				fmt.Fprintf(os.Stderr, "toydbg: invalid count %q\n", args[i])
				return
			}
			count = n
		case "-a":
			if i+1 >= len(args) {
				fmt.Println("usage: disassemble [-c count] [-a address]")
				return
			}
			i++
			a, err := strconv.ParseUint(strings.TrimPrefix(args[i], "0x"), 16, 64)
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: invalid address %q: %v\n", args[i], err)
				return
			}
			addr = &a
		default:
			fmt.Fprintf(os.Stderr, "toydbg: unknown disassemble flag %q\n", args[i])
			return
		}
	}

	printDisassembly(proc, count, addr)
}

func handleThread(proc *debugger.Process, args []string) {
	if len(args) == 0 {
		fmt.Println("usage: thread <list|select> [tid]")
		return
	}

	switch args[0] {
	case "list":
		threads := proc.ThreadStates()
		current := proc.CurrentThread()
		for tid, ts := range threads {
			marker := " "
			if tid == current {
				marker = "*"
			}
			stateStr := "stopped"
			switch ts.State {
			case debugger.ProcessRunning:
				stateStr = "running"
			case debugger.ProcessExited:
				stateStr = "exited"
			case debugger.ProcessTerminated:
				stateStr = "terminated"
			}
			fmt.Printf("%s thread %d (%s)\n", marker, tid, stateStr)
		}

	case "select":
		if len(args) < 2 {
			fmt.Println("usage: thread select <tid>")
			return
		}
		tid, err := strconv.Atoi(args[1])
		if err != nil {
			fmt.Fprintf(os.Stderr, "toydbg: invalid TID %q: %v\n", args[1], err)
			return
		}
		if _, ok := proc.ThreadStates()[tid]; !ok {
			fmt.Fprintf(os.Stderr, "toydbg: thread %d not found\n", tid)
			return
		}
		proc.SetCurrentThread(tid)
		fmt.Printf("switched to thread %d\n", tid)

	default:
		fmt.Printf("unknown thread subcommand: %q\n", args[0])
	}
}

func handleVariable(target *debugger.Target, args []string) {
	if len(args) == 0 {
		fmt.Println("usage: variable <read|locals|location> [args...]")
		fmt.Println("  type 'help variable' for details")
		return
	}

	switch args[0] {
	case "locals":
		handleVariableLocals(target)

	case "read":
		if len(args) < 2 {
			fmt.Println("usage: variable read <name>")
			return
		}
		handleVariableRead(target, args[1])

	case "location":
		if len(args) < 2 {
			fmt.Println("usage: variable location <name>")
			return
		}
		handleVariableLocation(target, args[1])

	default:
		fmt.Printf("unknown variable subcommand: %q\n", args[0])
	}
}

func handleVariableRead(target *debugger.Target, name string) {
	data, err := target.ResolveIndirectName(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
		return
	}
	str := data.Visualize(target.Process(), 0)
	fmt.Printf("Value: %s\n", str)
}

func handleVariableLocals(target *debugger.Target) {
	vars, err := target.LocalVariables()
	if err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
		return
	}
	if len(vars) == 0 {
		fmt.Println("no local variables in scope")
		return
	}
	for _, v := range vars {
		str := v.Data.Visualize(target.Process(), 0)
		fmt.Printf("%s: %s\n", v.Name, str)
	}
}

func handleVariableLocation(target *debugger.Target, name string) {
	result, err := target.VariableLocation(name)
	if err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
		return
	}

	printSimpleLoc := func(loc debugger.SimpleLocation) {
		switch loc.Kind {
		case debugger.LocRegister:
			info, ok := debugger.RegisterInfoByDwarf(int(loc.RegNum))
			if ok {
				fmt.Printf("Register: %s\n", info.Name)
			} else {
				fmt.Printf("Register: dwarf_%d\n", loc.RegNum)
			}
		case debugger.LocAddress:
			fmt.Printf("Address: 0x%x\n", loc.Address)
		case debugger.LocLiteral:
			fmt.Printf("Literal: %d\n", loc.Value)
		case debugger.LocEmpty:
			fmt.Println("Optimized out")
		default:
			fmt.Printf("Location kind: %d\n", loc.Kind)
		}
	}

	if !result.IsComposite {
		printSimpleLoc(result.Location)
	} else {
		for i, piece := range result.Pieces {
			fmt.Printf("Piece %d: offset=%d, bit_size=%d, location=", i, piece.Offset, piece.BitSize)
			printSimpleLoc(piece.Location)
		}
	}
}

func handleExpression(target *debugger.Target, line string) {
	// Extract the expression after the command name.
	spaceIdx := strings.Index(line, " ")
	if spaceIdx < 0 {
		fmt.Println("usage: expression <function-call>")
		fmt.Println("  example: expr print_value(42)")
		return
	}
	expr := strings.TrimSpace(line[spaceIdx+1:])
	if expr == "" {
		fmt.Println("usage: expression <function-call>")
		return
	}

	result, err := target.EvaluateExpression(expr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
		return
	}

	if result != nil && result.Value != nil {
		str := result.Value.Visualize(target.Process(), 0)
		fmt.Printf("$%d: %s\n", result.ID, str)
	}
}

func handleBacktrace(target *debugger.Target) {
	frames := target.Backtrace(0)
	fmt.Print(debugger.FormatBacktrace(frames))
}

func printDisassembly(proc *debugger.Process, count int, addr *uint64) {
	disasm := debugger.NewDisassembler(proc)
	instructions, err := disasm.Disassemble(count, addr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: disassemble failed: %v\n", err)
		return
	}
	for _, inst := range instructions {
		fmt.Printf("  0x%x: %s\n", inst.Address, inst.Text)
	}
}
