//go:build linux

package debugger

import "syscall"

func ptraceCont(pid int) error {
	return syscall.PtraceCont(pid, 0)
}
