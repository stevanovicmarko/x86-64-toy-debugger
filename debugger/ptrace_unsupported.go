//go:build !linux

package debugger

import "syscall"

func ptraceCont(_ int) error {
	return syscall.ENOSYS
}

func ptraceSeizeProcess(_ int) error {
	return syscall.ENOSYS
}

func ptraceInterruptProcess(_ int) error {
	return syscall.ENOSYS
}

func ptraceGetRegs(_ int, _ any) error {
	return syscall.ENOSYS
}

func ptraceSetRegs(_ int, _ any) error {
	return syscall.ENOSYS
}

func ptraceGetFPRegs(_ int, _ *fpRegs) error {
	return syscall.ENOSYS
}

func ptraceSetFPRegs(_ int, _ *fpRegs) error {
	return syscall.ENOSYS
}

func ptracePeekUser(_ int, _ uintptr) (uint64, error) {
	return 0, syscall.ENOSYS
}

func ptracePokeUser(_ int, _ uintptr, _ uint64) error {
	return syscall.ENOSYS
}
