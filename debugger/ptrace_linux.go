//go:build linux

package debugger

import "syscall"

const (
	// PTRACE_SEIZE attaches to a process without sending SIGSTOP.
	// Available since Linux 3.4.
	ptraceSeize = 0x4206

	// PTRACE_INTERRUPT stops a tracee that was seized (not attached).
	// Unlike SIGSTOP, it does not generate a signal-delivery-stop,
	// avoiding the double-SIGSTOP issue with PTRACE_ATTACH.
	ptraceInterrupt = 0x4207
)

func ptraceCont(pid int) error {
	return syscall.PtraceCont(pid, 0)
}

// ptraceSeizeProcess attaches to the given pid using PTRACE_SEIZE,
// which does not send SIGSTOP. The caller must use ptraceInterruptProcess
// to stop the tracee.
func ptraceSeizeProcess(pid int) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptraceSeize), uintptr(pid), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// ptraceInterruptProcess stops a seized tracee without delivering a signal.
func ptraceInterruptProcess(pid int) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptraceInterrupt), uintptr(pid), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
