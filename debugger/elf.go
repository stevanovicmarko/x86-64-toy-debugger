package debugger

import (
	"debug/elf"
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

	// symbolsByName maps symbol names to all symbols with that name.
	// Multiple symbols can share a name (e.g., static functions in
	// different compilation units).
	symbolsByName map[string][]*elf.Symbol

	// symbolsByAddr is sorted by address for binary search. Only
	// symbols with Size > 0 are included (we need the range to
	// answer "which function contains this address?").
	symbolsByAddr []addrSymbol
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

	return e, nil
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
