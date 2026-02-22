package main

import (
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/chzyer/readline"

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
		proc *debugger.Process
		err  error
	)

	if pid != 0 {
		proc, err = debugger.Attach(pid)
	} else {
		proc, err = debugger.Launch(flag.Arg(0))
	}
	if err != nil {
		fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
		os.Exit(1)
	}
	defer proc.Close()

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
			if err := proc.Resume(); err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				continue
			}

			reason, err := proc.WaitOnSignal()
			if err != nil {
				fmt.Fprintf(os.Stderr, "toydbg: %v\n", err)
				return
			}
			printStopReason(proc, reason)

			if reason.Reason == debugger.ProcessExited || reason.Reason == debugger.ProcessTerminated {
				return
			}
		case "register", "reg":
			handleRegister(proc, fields[1:])
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
		}
	}
	fmt.Println("commands: continue (c), register (reg), quit (q), help (h)")
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

func printStopReason(proc *debugger.Process, reason debugger.StopReason) {
	switch reason.Reason {
	case debugger.ProcessStopped:
		pc, err := proc.GetPC()
		if err != nil {
			fmt.Printf("process stopped by signal %d\n", reason.Info)
		} else {
			fmt.Printf("process stopped by signal %d at 0x%x\n", reason.Info, pc)
		}
	case debugger.ProcessExited:
		fmt.Printf("process exited with code %d\n", reason.Info)
	case debugger.ProcessTerminated:
		fmt.Printf("process terminated by signal %d\n", reason.Info)
	case debugger.ProcessRunning:
		fmt.Println("process running")
	default:
		fmt.Println("process status changed")
	}
}
