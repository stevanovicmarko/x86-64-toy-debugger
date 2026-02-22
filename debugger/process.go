package debugger

import (
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// ProcessState represents the running state of a traced process.
type ProcessState int

const (
	ProcessStopped    ProcessState = iota // Process is stopped by a signal
	ProcessRunning                        // Process is currently executing
	ProcessExited                         // Process exited normally
	ProcessTerminated                     // Process was killed by a signal
)

// StopReason holds the reason a process stopped and additional info
// such as the exit code or signal number.
type StopReason struct {
	Reason ProcessState
	Info   uint8
}

// newStopReason parses a syscall.WaitStatus into a StopReason.
func newStopReason(ws syscall.WaitStatus) StopReason {
	switch {
	case ws.Exited():
		return StopReason{Reason: ProcessExited, Info: uint8(ws.ExitStatus())}
	case ws.Signaled():
		return StopReason{Reason: ProcessTerminated, Info: uint8(ws.Signal())}
	case ws.Stopped():
		return StopReason{Reason: ProcessStopped, Info: uint8(ws.StopSignal())}
	default:
		return StopReason{Reason: ProcessStopped, Info: 0}
	}
}

// Process represents a running process being debugged.
// Users must create one via Launch or Attach; direct construction is not possible
// because all fields are unexported.
type Process struct {
	pid            int
	terminateOnEnd bool
	isAttached     bool
	state          ProcessState
	regs           *Registers
}

// Launch starts a new process under ptrace and returns a Process that
// manages it. The process is stopped immediately after exec so the
// debugger can control it.
//
// The calling goroutine is pinned to its current OS thread for the
// lifetime of the returned Process (released by Close). This is
// required because Linux ptrace is per-thread: only the thread that
// forked the tracee may issue subsequent ptrace requests for it.
func Launch(program string, args ...string) (*Process, error) {
	return launch(program, true, args)
}

// LaunchNoDebug starts a new process without ptrace tracing and returns
// a Process that manages it. The process runs freely; use Attach to
// begin tracing it later. This is primarily useful in tests that need
// a running target to attach to separately.
func LaunchNoDebug(program string, args ...string) (*Process, error) {
	return launch(program, false, args)
}

func launch(program string, debug bool, args []string) (*Process, error) {
	if debug {
		runtime.LockOSThread()
	}

	cmd := exec.Command(program, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if debug {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Ptrace: true,
		}
	}

	if err := cmd.Start(); err != nil {
		if debug {
			runtime.UnlockOSThread()
		}
		return nil, newErrorf("exec failed: %v", err)
	}

	proc := &Process{
		pid:            cmd.Process.Pid,
		terminateOnEnd: true,
		isAttached:     debug,
		state:          ProcessStopped,
	}

	if debug {
		if _, err := proc.WaitOnSignal(); err != nil {
			runtime.UnlockOSThread()
			return nil, err
		}
		proc.regs = &Registers{pid: proc.pid}
		if err := proc.regs.readAll(); err != nil {
			proc.Close()
			return nil, err
		}
	} else {
		proc.state = ProcessRunning
	}

	return proc, nil
}

// Attach attaches to an existing process by PID and returns a Process
// that manages it.
//
// It uses PTRACE_SEIZE + PTRACE_INTERRUPT rather than the older
// PTRACE_ATTACH because the latter sends SIGSTOP, which can produce
// duplicate stop events and confuse subsequent Resume calls.
//
// Like Launch, the calling goroutine is pinned to its OS thread for
// the lifetime of the returned Process (released by Close).
func Attach(pid int) (*Process, error) {
	if pid == 0 {
		return nil, newError("invalid PID")
	}

	runtime.LockOSThread()

	if err := ptraceSeizeProcess(pid); err != nil {
		runtime.UnlockOSThread()
		return nil, newErrorf("could not attach: %v", err)
	}

	if err := ptraceInterruptProcess(pid); err != nil {
		runtime.UnlockOSThread()
		return nil, newErrorf("could not stop tracee: %v", err)
	}

	proc := &Process{
		pid:            pid,
		terminateOnEnd: false,
		isAttached:     true,
		state:          ProcessStopped,
	}

	if _, err := proc.WaitOnSignal(); err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}

	proc.regs = &Registers{pid: proc.pid}
	if err := proc.regs.readAll(); err != nil {
		proc.Close()
		return nil, err
	}

	return proc, nil
}

// Pid returns the OS process ID.
func (p *Process) Pid() int {
	return p.pid
}

// State returns the current process state.
func (p *Process) State() ProcessState {
	return p.state
}

// Registers returns the cached register state. The cache is refreshed
// automatically by WaitOnSignal each time the process stops.
func (p *Process) Registers() *Registers {
	return p.regs
}

// Resume continues a stopped process.
func (p *Process) Resume() error {
	if err := ptraceCont(p.pid); err != nil {
		return newErrorf("could not resume: %v", err)
	}
	p.state = ProcessRunning
	return nil
}

// WaitOnSignal blocks until the process stops or terminates and returns
// the reason for stopping. When the process is stopped and we are
// attached, the register cache is refreshed automatically.
func (p *Process) WaitOnSignal() (StopReason, error) {
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(p.pid, &ws, 0, nil); err != nil {
		return StopReason{}, newErrorf("waitpid failed: %v", err)
	}

	reason := newStopReason(ws)
	p.state = reason.Reason

	if p.isAttached && p.state == ProcessStopped && p.regs != nil {
		if err := p.regs.readAll(); err != nil {
			return reason, err
		}
	}

	return reason, nil
}

// Close detaches from the process and optionally terminates it.
// It also releases the OS-thread pin established by Launch or Attach.
func (p *Process) Close() {
	if p.pid == 0 {
		return
	}

	if p.isAttached {
		if p.state == ProcessRunning {
			_ = syscall.Kill(p.pid, syscall.SIGSTOP)
			var ws syscall.WaitStatus
			syscall.Wait4(p.pid, &ws, 0, nil)
		}

		syscall.PtraceDetach(p.pid)
		_ = syscall.Kill(p.pid, syscall.SIGCONT)
	}

	if p.terminateOnEnd {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		syscall.Wait4(p.pid, &ws, 0, nil)
	}

	if p.isAttached {
		runtime.UnlockOSThread()
	}
}
