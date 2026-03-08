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

	// ptrace requests for memory access and single-stepping.
	ptracePeekDataReq   = 0x2 // PTRACE_PEEKDATA
	ptracePokeDataReq   = 0x5 // PTRACE_POKEDATA
	ptraceSingleStepReq = 0x9 // PTRACE_SINGLESTEP

	// ptrace requests for signal info, options, and syscall tracing.
	ptraceGetSigInfoReq  = 0x4202 // PTRACE_GETSIGINFO
	ptraceSetOptionsReq  = 0x4200 // PTRACE_SETOPTIONS
	ptraceSyscallReq     = 24     // PTRACE_SYSCALL

	// PTRACE_O_TRACESYSGOOD: set bit 7 on signal number for syscall stops,
	// turning SIGTRAP into SIGTRAP|0x80 so we can distinguish syscall stops
	// from other SIGTRAP causes without inspecting si_code.
	ptraceOTraceSysGood = 1

	// PTRACE_O_TRACECLONE: when a traced thread calls clone(), the kernel
	// sends SIGTRAP to the parent thread and SIGSTOP to the new child.
	// This lets the debugger discover newly created threads.
	ptraceOTraceClone = 0x8

	// PTRACE_EVENT_CLONE identifies a clone event in the wait status.
	// When status>>8 == (SIGTRAP | (PTRACE_EVENT_CLONE<<8)), a new thread
	// was created.
	ptraceEventClone = 3

	// __WALL flag for waitpid: wait on both processes and threads created
	// with clone(). Without this, waitpid only sees direct children.
	wall = 0x40000000
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

// ptracePeekData reads a single word from the tracee's address space.
func ptracePeekData(pid int, addr uint64) (uint64, error) {
	var val uint64
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptracePeekDataReq), uintptr(pid), uintptr(addr),
		uintptr(unsafe.Pointer(&val)), 0, 0)
	if errno != 0 {
		return 0, errno
	}
	return val, nil
}

// ptracePokeData writes a single word to the tracee's address space.
func ptracePokeData(pid int, addr uint64, data uint64) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptracePokeDataReq), uintptr(pid), uintptr(addr),
		uintptr(data), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// ptraceSingleStep resumes the tracee for exactly one instruction,
// then stops it again with SIGTRAP.
func ptraceSingleStep(pid int) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptraceSingleStepReq), uintptr(pid), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// siginfo is the Go representation of the kernel's siginfo_t structure.
// We only need the first three fields (si_signo, si_errno, si_code) for
// distinguishing trap causes; the rest is padded to match the kernel layout.
type siginfo struct {
	Signo int32
	Errno int32
	Code  int32
	_     [128 - 12]byte // pad to SI_MAX_SIZE (128 bytes)
}

// ptraceGetSigInfo retrieves the siginfo_t for the last signal delivered
// to the tracee. The si_code field distinguishes trap causes:
//   - SI_KERNEL (0x80)   → software breakpoint (int3)
//   - TRAP_TRACE (2)     → single step
//   - TRAP_HWBKPT (4)    → hardware breakpoint or watchpoint
func ptraceGetSigInfo(pid int) (siginfo, error) {
	var info siginfo
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptraceGetSigInfoReq), uintptr(pid), 0,
		uintptr(unsafe.Pointer(&info)), 0, 0)
	if errno != 0 {
		return info, errno
	}
	return info, nil
}

// ptraceSetOptions sets ptrace options for the tracee. The primary option
// we use is PTRACE_O_TRACESYSGOOD, which sets bit 7 on the stop signal
// for syscall-stops (SIGTRAP|0x80 = signal 133).
func ptraceSetOptions(pid int, options int) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptraceSetOptionsReq), uintptr(pid), 0,
		uintptr(options), 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}

// tgkill sends a signal to a specific thread within a thread group.
// Unlike kill(), which delivers to the process, tgkill targets a single
// thread by TID. This is essential for stopping specific threads in
// multithreaded debugging (sending SIGSTOP to individual threads).
func tgkill(tgid, tid int, sig syscall.Signal) error {
	_, _, errno := syscall.RawSyscall(syscall.SYS_TGKILL,
		uintptr(tgid), uintptr(tid), uintptr(sig))
	if errno != 0 {
		return errno
	}
	return nil
}

// ptraceSyscall resumes the tracee like PTRACE_CONT but stops it at the
// next syscall entry or exit boundary. Combined with PTRACE_O_TRACESYSGOOD,
// the resulting stop signal will be SIGTRAP|0x80.
func ptraceSyscall(pid int) error {
	_, _, errno := syscall.RawSyscall6(syscall.SYS_PTRACE,
		uintptr(ptraceSyscallReq), uintptr(pid), 0, 0, 0, 0)
	if errno != 0 {
		return errno
	}
	return nil
}
