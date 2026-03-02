//go:build !linux

package debugger

// readAuxv is not supported on non-Linux platforms.
// The auxiliary vector is a Linux-specific mechanism.
func readAuxv(pid int) (map[uint64]uint64, error) {
	return nil, newError("auxv: not supported on this platform")
}
