//go:build linux

package debugger

import (
	"syscall"
	"unsafe"
)

const (
	// PTRACE_SEIZE attaches to a process without sending SIGSTOP.
	// Available since Linux 3.4.
	ptraceSeize = 0x4206

	// PTRACE_INTERRUPT stops a tracee that was seized (not attached).
	// Unlike SIGSTOP, it does not generate a signal-delivery-stop,
	// avoiding the double-SIGSTOP issue with PTRACE_ATTACH.
	ptraceInterrupt = 0x4207

	// ptrace requests for register access.
	ptraceGetFPRegsReq = 0xe // PTRACE_GETFPREGS
	ptraceSetFPRegsReq = 0xf // PTRACE_SETFPREGS
	ptracePeekUserReq  = 0x3 // PTRACE_PEEKUSER
	ptracePokeUserReq  = 0x6 // PTRACE_POKEUSER
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

// ptraceGetRegs reads all general-purpose registers into regs.
func ptraceGetRegs(pid int, regs *syscall.PtraceRegs) error {
	return syscall.PtraceGetRegs(pid, regs)
}

// ptraceSetRegs writes all general-purpose registers from regs.
func ptraceSetRegs(pid int, regs *syscall.PtraceRegs) error {
	return syscall.PtraceSetRegs(pid, regs)
}

// ptraceGetFPRegs reads all floating-point registers into fprs.
func ptraceGetFPRegs(pid int, fprs *fpRegs) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptraceGetFPRegsReq), uintptr(pid), 0,
		uintptr(unsafe.Pointer(fprs)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// ptraceSetFPRegs writes all floating-point registers from fprs.
func ptraceSetFPRegs(pid int, fprs *fpRegs) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptraceSetFPRegsReq), uintptr(pid), 0,
		uintptr(unsafe.Pointer(fprs)), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// ptracePeekUser reads a single word from the user area at the given
// byte offset. The kernel writes the result to the data pointer (4th arg),
// so we must pass a valid address rather than NULL.
func ptracePeekUser(pid int, offset uintptr) (uint64, error) {
	var val uint64
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptracePeekUserReq), uintptr(pid), offset,
		uintptr(unsafe.Pointer(&val)), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return val, nil
}

// ptracePokeUser writes a single word to the user area at the given
// byte offset.
func ptracePokeUser(pid int, offset uintptr, data uint64) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptracePokeUserReq), uintptr(pid), offset,
		uintptr(data), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
