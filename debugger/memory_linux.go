//go:build linux

package debugger

import "golang.org/x/sys/unix"

// processVMReadv reads `amount` bytes starting at `addr` in the process
// identified by `pid`. It uses the process_vm_readv(2) syscall, which
// transfers data directly between address spaces without stopping at
// ptrace word boundaries.
//
// The read is split into page-aligned iovec chunks so that partial reads
// across page boundaries work correctly. Each remote iovec covers at most
// one page, while a single local iovec receives the entire result.
func processVMReadv(pid int, addr uint64, amount int) ([]byte, error) {
	buf := make([]byte, amount)

	localIov := []unix.Iovec{
		{Base: &buf[0], Len: uint64(amount)},
	}

	// Split the remote side into page-aligned chunks. This ensures
	// the syscall doesn't fail if the read straddles page boundaries
	// where one page is mapped and the next isn't (the kernel handles
	// each iovec independently and returns a short read rather than EFAULT).
	const pageSize = 4096
	var remoteIov []unix.RemoteIovec

	remaining := uint64(amount)
	cur := addr
	for remaining > 0 {
		// Bytes until the next page boundary (or remaining, whichever is smaller).
		pageOff := cur % pageSize
		chunk := pageSize - pageOff
		if chunk > remaining {
			chunk = remaining
		}
		remoteIov = append(remoteIov, unix.RemoteIovec{
			Base: uintptr(cur),
			Len:  int(chunk),
		})
		cur += chunk
		remaining -= chunk
	}

	n, err := unix.ProcessVMReadv(pid, localIov, remoteIov, 0)
	if err != nil {
		return nil, err
	}
	return buf[:n], nil
}
