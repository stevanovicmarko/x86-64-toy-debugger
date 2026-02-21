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
