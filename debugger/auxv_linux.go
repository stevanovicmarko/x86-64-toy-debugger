//go:build linux

package debugger

import (
	"encoding/binary"
	"fmt"
	"os"
)

// readAuxv reads the auxiliary vector from /proc/<pid>/auxv and returns
// it as a map of tag→value pairs. The auxiliary vector is an array of
// {uint64 tag, uint64 value} pairs terminated by AT_NULL (tag 0).
//
// The key value we need is AT_ENTRY: the actual entry point address
// chosen by the kernel's ELF loader. Comparing this with the ELF
// header's e_entry gives us the load bias for PIE binaries.
func readAuxv(pid int) (map[uint64]uint64, error) {
	path := fmt.Sprintf("/proc/%d/auxv", pid)
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, newErrorf("read auxv: %v", err)
	}

	result := make(map[uint64]uint64)
	// Each entry is two uint64s (16 bytes) in native byte order.
	for i := 0; i+16 <= len(data); i += 16 {
		tag := binary.LittleEndian.Uint64(data[i : i+8])
		val := binary.LittleEndian.Uint64(data[i+8 : i+16])
		if tag == 0 { // AT_NULL — end of vector
			break
		}
		result[tag] = val
	}

	return result, nil
}
