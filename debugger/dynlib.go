package debugger

import (
	"debug/elf"
	"encoding/binary"
	"os"
	"path/filepath"
	"unsafe"
)

// ---------------------------------------------------------------------------
// Dynamic Linker Rendezvous Structure
//
// When a dynamically-linked executable starts, the kernel loads the
// dynamic linker (ld-linux-x86-64.so.2), which maintains a "rendezvous
// structure" (r_debug) in the inferior's address space. This structure
// tells debuggers about all loaded shared libraries via a linked list
// of link_map entries.
//
// The debugger finds the rendezvous structure through the .dynamic
// section's DT_DEBUG entry, which the dynamic linker patches at
// runtime to point to r_debug.
// ---------------------------------------------------------------------------

// rDebug mirrors the Linux r_debug structure from <link.h>.
// r_version=1, r_map points to the loaded library linked list,
// r_brk is the address of _dl_debug_state (where we set a breakpoint),
// r_state indicates whether a library load/unload is in progress.
type rDebug struct {
	Version  int32
	_        [4]byte // padding
	Map      uint64  // pointer to first link_map
	Brk      uint64  // address of _dl_debug_state
	State    int32   // RT_CONSISTENT=0, RT_ADD=1, RT_DELETE=2
	_        [4]byte // padding
	LdBase   uint64  // load address of the dynamic linker
}

const (
	rtConsistent = 0
	// rtAdd     = 1
	// rtDelete  = 2
)

// linkMapEntry mirrors the first 5 fields of struct link_map from <link.h>.
// These are the only stable fields we should rely on.
type linkMapEntry struct {
	Addr uint64 // l_addr: load address
	Name uint64 // l_name: pointer to name string
	Ld   uint64 // l_ld: pointer to .dynamic section
	Next uint64 // l_next: next entry
	Prev uint64 // l_prev: previous entry
}

// Dynamic section entry tag for DT_DEBUG.
const dtDebug = 21

// resolveRendezvous locates the dynamic linker rendezvous structure
// by reading the .dynamic section from the inferior's memory and
// finding the DT_DEBUG entry. This entry is zero in the ELF file
// on disk but is patched by the dynamic linker at runtime.
func (t *Target) resolveRendezvous() {
	if t.rendezvousAddr != 0 {
		return // already resolved
	}
	if t.mainELF == nil {
		return
	}

	// Find the .dynamic section in the main ELF.
	dynSec := t.mainELF.file.Section(".dynamic")
	if dynSec == nil {
		return // statically linked — no dynamic section
	}

	// Read the .dynamic section from the inferior's memory.
	// The section's Addr field is a file address; add load bias
	// to get the virtual address in the running process.
	dynAddr := dynSec.Addr + t.mainELF.loadBias
	dynSize := dynSec.Size

	dynBytes, err := t.process.ReadMemory(dynAddr, int(dynSize))
	if err != nil {
		return
	}

	// Parse the dynamic entries. Each is {int64 tag, uint64 value}.
	entrySize := 16 // sizeof(Elf64_Dyn) = 16
	for i := 0; i+entrySize <= len(dynBytes); i += entrySize {
		tag := int64(binary.LittleEndian.Uint64(dynBytes[i : i+8]))
		val := binary.LittleEndian.Uint64(dynBytes[i+8 : i+16])

		if tag == 0 { // DT_NULL — end of list
			break
		}
		if tag == dtDebug {
			t.rendezvousAddr = val
			break
		}
	}

	if t.rendezvousAddr == 0 {
		return
	}

	// Read the rendezvous structure and set up library tracking.
	t.reloadDynamicLibraries()

	// Set a breakpoint on _dl_debug_state so we're notified
	// whenever the linker loads or unloads a shared library.
	rdebug, err := t.readRendezvous()
	if err != nil || rdebug.Brk == 0 {
		return
	}

	dlDebugBP, err := t.process.CreateBreakpointSite(rdebug.Brk, false, true)
	if err != nil {
		return
	}
	dlDebugBP.InstallHitHandler(func() bool {
		t.reloadDynamicLibraries()
		return true
	})
	dlDebugBP.Enable()
}

// readRendezvous reads the r_debug structure from the inferior.
func (t *Target) readRendezvous() (rDebug, error) {
	size := int(unsafe.Sizeof(rDebug{}))
	data, err := t.process.ReadMemory(t.rendezvousAddr, size)
	if err != nil {
		return rDebug{}, err
	}

	var rd rDebug
	rd.Version = int32(binary.LittleEndian.Uint32(data[0:4]))
	// data[4:8] is padding
	rd.Map = binary.LittleEndian.Uint64(data[8:16])
	rd.Brk = binary.LittleEndian.Uint64(data[16:24])
	rd.State = int32(binary.LittleEndian.Uint32(data[24:28]))
	// data[28:32] is padding
	rd.LdBase = binary.LittleEndian.Uint64(data[32:40])
	return rd, nil
}

// reloadDynamicLibraries walks the link_map linked list in the
// rendezvous structure and opens ELF files for any new libraries.
func (t *Target) reloadDynamicLibraries() {
	rdebug, err := t.readRendezvous()
	if err != nil {
		return
	}

	entryPtr := rdebug.Map
	for entryPtr != 0 {
		// Read the link_map entry from the inferior.
		lmSize := int(unsafe.Sizeof(linkMapEntry{}))
		lmBytes, err := t.process.ReadMemory(entryPtr, lmSize)
		if err != nil {
			break
		}

		var lm linkMapEntry
		lm.Addr = binary.LittleEndian.Uint64(lmBytes[0:8])
		lm.Name = binary.LittleEndian.Uint64(lmBytes[8:16])
		lm.Ld = binary.LittleEndian.Uint64(lmBytes[16:24])
		lm.Next = binary.LittleEndian.Uint64(lmBytes[24:32])
		lm.Prev = binary.LittleEndian.Uint64(lmBytes[32:40])

		entryPtr = lm.Next

		// Read the library name (null-terminated string).
		if lm.Name == 0 {
			continue
		}
		nameBytes, err := t.process.ReadMemory(lm.Name, 4096)
		if err != nil {
			continue
		}
		// Find the null terminator.
		nameLen := 0
		for nameLen < len(nameBytes) && nameBytes[nameLen] != 0 {
			nameLen++
		}
		name := string(nameBytes[:nameLen])

		// Empty name = the main executable itself — skip it.
		if name == "" {
			continue
		}

		// Check if we already have this library loaded.
		const vdsoName = "linux-vdso.so.1"
		var found *ELF
		if name == vdsoName {
			found = t.elves.ELFByFilename(vdsoName)
		} else {
			found = t.elves.ELFByPath(name)
		}

		if found != nil {
			continue // already loaded
		}

		// For the vDSO, dump it from memory to a temp file.
		elfPath := name
		if name == vdsoName {
			dumped, err := dumpVDSO(t.process, lm.Addr)
			if err != nil {
				continue
			}
			elfPath = dumped
		}

		newELF, err := OpenELF(elfPath)
		if err != nil {
			continue
		}
		newELF.NotifyLoaded(lm.Addr)
		t.elves.Push(newELF)
	}
}

// dumpVDSO reads the virtual dynamic shared object from process memory
// and writes it to a temporary file so we can parse it like any other ELF.
// The vDSO doesn't exist on disk — it's mapped directly by the kernel.
func dumpVDSO(proc *Process, addr uint64) (string, error) {
	// Read the ELF header to determine the full size.
	headerSize := int(unsafe.Sizeof(elf.Header64{}))
	headerBytes, err := proc.ReadMemory(addr, headerSize)
	if err != nil {
		return "", err
	}

	// Parse enough of the header to compute full file size.
	shOff := binary.LittleEndian.Uint64(headerBytes[40:48])
	shEntSize := binary.LittleEndian.Uint16(headerBytes[58:60])
	shNum := binary.LittleEndian.Uint16(headerBytes[60:62])

	totalSize := shOff + uint64(shEntSize)*uint64(shNum)

	vdsoBytes, err := proc.ReadMemory(addr, int(totalSize))
	if err != nil {
		return "", err
	}

	tmpDir, err := os.MkdirTemp("", "toydbg-vdso-*")
	if err != nil {
		return "", err
	}

	dumpPath := filepath.Join(tmpDir, "linux-vdso.so.1")
	if err := os.WriteFile(dumpPath, vdsoBytes, 0644); err != nil {
		return "", err
	}

	return dumpPath, nil
}
