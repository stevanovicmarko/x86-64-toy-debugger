//go:build !linux

package debugger

import "syscall"

// fpRegs is a stub for non-Linux platforms.
type fpRegs struct{}

// Registers is a stub for non-Linux platforms.
type Registers struct {
	pid int
}

func (r *Registers) readAll() error                        { return syscall.ENOSYS }
func (r *Registers) Read(_ RegisterInfo) any               { return nil }
func (r *Registers) ReadByID(_ RegisterID) (any, error)    { return nil, syscall.ENOSYS }
func (r *Registers) Write(_ RegisterInfo, _ any) error     { return syscall.ENOSYS }
func (r *Registers) WriteByID(_ RegisterID, _ any) error   { return syscall.ENOSYS }
func (r *Registers) WriteFPRs() error                      { return syscall.ENOSYS }
func (r *Registers) WriteGPRs() error                      { return syscall.ENOSYS }
