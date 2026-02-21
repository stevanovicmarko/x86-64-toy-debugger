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

		switch strings.TrimSpace(line) {
		case "", "help", "h":
			fmt.Println("commands: continue (c), quit (q), help (h)")
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
			printStopReason(reason)

			if reason.Reason == debugger.ProcessExited || reason.Reason == debugger.ProcessTerminated {
				return
			}
		case "quit", "q", "exit":
			return
		default:
			fmt.Printf("unknown command: %q\n", strings.TrimSpace(line))
		}
	}
}

func usage() {
	fmt.Fprintln(os.Stderr, "Usage:")
	fmt.Fprintln(os.Stderr, "  toydbg /path/to/program")
	fmt.Fprintln(os.Stderr, "  toydbg -p <pid>")
}

func printStopReason(reason debugger.StopReason) {
	switch reason.Reason {
	case debugger.ProcessStopped:
		fmt.Printf("process stopped by signal %d\n", reason.Info)
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
