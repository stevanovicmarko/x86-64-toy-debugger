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

func ptracePeekData(_ int, _ uint64) (uint64, error) {
	return 0, syscall.ENOSYS
}

func ptracePokeData(_ int, _ uint64, _ uint64) error {
	return syscall.ENOSYS
}

func ptraceSingleStep(_ int) error {
	return syscall.ENOSYS
}

type siginfo struct {
	Signo int32
	Errno int32
	Code  int32
	_     [128 - 12]byte
}

func ptraceGetSigInfo(_ int) (siginfo, error) {
	return siginfo{}, syscall.ENOSYS
}

func ptraceSetOptions(_ int, _ int) error {
	return syscall.ENOSYS
}

func ptraceSyscall(_ int) error {
	return syscall.ENOSYS
}

func tgkill(_, _ int, _ syscall.Signal) error {
	return syscall.ENOSYS
}
