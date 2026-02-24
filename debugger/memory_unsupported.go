//go:build !linux

package debugger

import "syscall"

func processVMReadv(pid int, addr uint64, amount int) ([]byte, error) {
	return nil, syscall.ENOSYS
}
