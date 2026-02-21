//go:build !linux

package debugger

import "syscall"

func ptraceCont(_ int) error {
	return syscall.ENOSYS
}
