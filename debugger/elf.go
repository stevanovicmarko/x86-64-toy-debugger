package debugger

import (
	"debug/elf"
	"path/filepath"
	"sort"
)

// addrSymbol pairs a symbol with its address for sorted lookup.
type addrSymbol struct {
	addr uint64
	sym  *elf.Symbol
}

// ELF wraps Go's debug/elf with pre-built symbol lookup structures.
// It provides name-based and address-based symbol resolution, which
// the debugger uses to map program counters to function names.
type ELF struct {
	path     string
	file     *elf.File
	loadBias uint64

	// addrLow and addrHigh define the file-address range of the ELF's
	// LOAD segments. Combined with loadBias, this lets us check whether
	// a virtual address belongs to this ELF.
	addrLow  uint64
	addrHigh uint64

	// symbolsByName maps symbol names to all symbols with that name.
	// Multiple symbols can share a name (e.g., static functions in
	// different compilation units).
	symbolsByName map[string][]*elf.Symbol

	// symbolsByAddr is sorted by address for binary search. Only
	// symbols with Size > 0 are included (we need the range to
	// answer "which function contains this address?").
	symbolsByAddr []addrSymbol

	// dwarf holds the DWARF debug info, or nil if the binary has
	// no DWARF sections. When present, it provides richer function
	// lookup and source-location mapping than the symbol table alone.
	dwarf *DWARF

	// cfi holds the parsed call frame information from .eh_frame,
	// or nil if the section is absent. CFI enables stack unwinding
	// without requiring frame pointers.
	cfi *CallFrameInformation
}

// OpenELF opens and parses an ELF binary, building the symbol lookup
// structures. The caller must call Close when done.
func OpenELF(path string) (*ELF, error) {
	f, err := elf.Open(path)
	if err != nil {
		return nil, newErrorf("open ELF %s: %v", path, err)
	}

	e := &ELF{
		path:          path,
		file:          f,
		symbolsByName: make(map[string][]*elf.Symbol),
	}

	// Compute file-address range from LOAD segments.
	e.computeAddressRange()

	// Read the symbol table (.symtab). This may not exist in
	// stripped binaries, which is fine — we just won't have symbols.
	symbols, err := f.Symbols()
	if err != nil {
		// No symbol table is not fatal — just means no symbol lookup.
		return e, nil
	}

	for i := range symbols {
		sym := &symbols[i]
		e.symbolsByName[sym.Name] = append(e.symbolsByName[sym.Name], sym)

		if sym.Size > 0 {
			e.symbolsByAddr = append(e.symbolsByAddr, addrSymbol{
				addr: sym.Value,
				sym:  sym,
			})
		}
	}

	// Sort by address for binary search in SymbolContainingAddress.
	sort.Slice(e.symbolsByAddr, func(i, j int) bool {
		return e.symbolsByAddr[i].addr < e.symbolsByAddr[j].addr
	})

	// Load DWARF debug info if present. This is non-fatal — the
	// debugger works without it, just with less detail.
	if dwarfData, err := f.DWARF(); err == nil {
		e.dwarf = newDWARF(dwarfData)
	}

	// Load call frame information (.eh_frame) if present. This
	// enables stack unwinding without frame pointers.
	e.loadCFI(f)

	return e, nil
}

// loadCFI reads the .eh_frame and .eh_frame_hdr sections and
// constructs the CallFrameInformation. Non-fatal if sections are absent.
func (e *ELF) loadCFI(f *elf.File) {
	ehFrameSec := f.Section(".eh_frame")
	if ehFrameSec == nil {
		return
	}
	ehFrameData, err := ehFrameSec.Data()
	if err != nil {
		return
	}

	var ehFrameHdrData []byte
	var ehFrameHdrAddr uint64
	if hdrSec := f.Section(".eh_frame_hdr"); hdrSec != nil {
		if data, err := hdrSec.Data(); err == nil {
			ehFrameHdrData = data
			ehFrameHdrAddr = hdrSec.Addr
		}
	}

	var textStart uint64
	if textSec := f.Section(".text"); textSec != nil {
		textStart = textSec.Addr
	}

	e.cfi = newCallFrameInformation(ehFrameData, ehFrameSec.Addr, ehFrameHdrData, ehFrameHdrAddr, textStart)
}

// Close releases the underlying ELF file handle.
func (e *ELF) Close() error {
	if e.file != nil {
		return e.file.Close()
	}
	return nil
}

// Header returns the ELF file header.
func (e *ELF) Header() elf.FileHeader {
	return e.file.FileHeader
}

// EntryPoint returns the ELF entry point address from the file header.
// For ET_EXEC binaries this is the actual virtual address; for ET_DYN
// (PIE) it is a relative offset that needs load bias adjustment.
func (e *ELF) EntryPoint() uint64 {
	return e.file.Entry
}

// SetLoadBias sets the load bias, which is the difference between the
// actual load address and the ELF file's expected load address. For
// PIE binaries under ASLR this is non-zero; for ET_EXEC it is 0.
func (e *ELF) SetLoadBias(bias uint64) {
	e.loadBias = bias
	if e.dwarf != nil {
		e.dwarf.loadBias = bias
	}
	if e.cfi != nil {
		e.cfi.loadBias = bias
	}
}

// DWARF returns the DWARF debug info, or nil if the binary has no
// DWARF sections. Use this for source-location and rich function lookup.
func (e *ELF) DWARF() *DWARF {
	return e.dwarf
}

// CFI returns the call frame information, or nil if the binary has no
// .eh_frame section. CFI enables stack unwinding without frame pointers.
func (e *ELF) CFI() *CallFrameInformation {
	return e.cfi
}

// LoadBias returns the current load bias.
func (e *ELF) LoadBias() uint64 {
	return e.loadBias
}

// SymbolsByName returns all symbols with the given name, or nil if none.
func (e *ELF) SymbolsByName(name string) []*elf.Symbol {
	return e.symbolsByName[name]
}

// SymbolAtAddress returns the symbol whose Value (adjusted for load bias)
// exactly matches the given virtual address.
func (e *ELF) SymbolAtAddress(addr uint64) (*elf.Symbol, bool) {
	// Convert virtual address to file address.
	fileAddr := addr - e.loadBias
	syms := e.symbolsByName
	for _, symList := range syms {
		for _, sym := range symList {
			if sym.Value == fileAddr {
				return sym, true
			}
		}
	}
	return nil, false
}

// SymbolContainingAddress returns the symbol whose range
// [Value, Value+Size) contains the given virtual address.
// Uses binary search on the sorted address list.
func (e *ELF) SymbolContainingAddress(addr uint64) (*elf.Symbol, bool) {
	fileAddr := addr - e.loadBias

	// Binary search: find the last symbol with addr <= fileAddr.
	n := len(e.symbolsByAddr)
	if n == 0 {
		return nil, false
	}

	// sort.Search finds the first index where symbolsByAddr[i].addr > fileAddr.
	// The candidate is one before that.
	idx := sort.Search(n, func(i int) bool {
		return e.symbolsByAddr[i].addr > fileAddr
	})
	idx-- // back up to the last symbol with addr <= fileAddr

	if idx < 0 {
		return nil, false
	}

	sym := e.symbolsByAddr[idx].sym
	if fileAddr >= sym.Value && fileAddr < sym.Value+sym.Size {
		return sym, true
	}
	return nil, false
}

// FunctionContainingAddress returns the name of the STT_FUNC symbol
// whose range contains the given virtual address. This is the primary
// method used by the CLI to annotate stop messages with function names.
func (e *ELF) FunctionContainingAddress(addr uint64) (string, bool) {
	sym, ok := e.SymbolContainingAddress(addr)
	if !ok {
		return "", false
	}
	if elf.ST_TYPE(sym.Info) != elf.STT_FUNC {
		return "", false
	}
	return sym.Name, true
}

// Path returns the filesystem path of this ELF binary.
func (e *ELF) Path() string {
	return e.path
}

// Filename returns just the basename of the ELF path.
func (e *ELF) Filename() string {
	return filepath.Base(e.path)
}

// ContainsAddress reports whether the given virtual address falls
// within this ELF's loaded address range.
func (e *ELF) ContainsAddress(addr uint64) bool {
	if e.addrHigh == 0 {
		return false
	}
	fileAddr := addr - e.loadBias
	return fileAddr >= e.addrLow && fileAddr < e.addrHigh
}

// computeAddressRange scans the ELF LOAD program headers to determine
// the file-address range that this binary occupies.
func (e *ELF) computeAddressRange() {
	first := true
	for _, prog := range e.file.Progs {
		if prog.Type != elf.PT_LOAD {
			continue
		}
		if first {
			e.addrLow = prog.Vaddr
			first = false
		}
		end := prog.Vaddr + prog.Memsz
		if end > e.addrHigh {
			e.addrHigh = end
		}
	}
}

// NotifyLoaded sets the load bias from the known load address.
// This is used for shared libraries where the load address comes
// from the dynamic linker's link map, not from the auxiliary vector.
func (e *ELF) NotifyLoaded(loadAddr uint64) {
	// For shared libraries (ET_DYN), the load address is the base
	// at which the lowest LOAD segment was mapped.
	e.SetLoadBias(loadAddr)
}
