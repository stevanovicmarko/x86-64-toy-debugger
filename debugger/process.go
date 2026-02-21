package debugger

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// Launch starts a new process with ptrace enabled and returns its PID.
func Launch(program string) (int, error) {
	cmd := exec.Command(program)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Ptrace: true,
	}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("exec failed: %w", err)
	}

	return cmd.Process.Pid, nil
}

// AttachPID attaches to a running process via PTRACE_ATTACH.
func AttachPID(pid int) error {
	if err := syscall.PtraceAttach(pid); err != nil {
		return fmt.Errorf("could not attach: %w", err)
	}
	return nil
}

// Resume continues a stopped tracee via PTRACE_CONT.
func Resume(pid int) error {
	if err := syscall.PtraceCont(pid, 0); err != nil {
		return fmt.Errorf("couldn't continue: %w", err)
	}
	return nil
}

// WaitOnSignal blocks until the tracee stops or terminates.
func WaitOnSignal(pid int) error {
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		return fmt.Errorf("waitpid failed: %w", err)
	}

	switch {
	case ws.Stopped():
		return nil
	case ws.Exited():
		return fmt.Errorf("exited with status %d", ws.ExitStatus())
	case ws.Signaled():
		return fmt.Errorf("killed by signal %d", ws.Signal())
	default:
		return fmt.Errorf("unknown wait status: %v", ws)
	}
}
