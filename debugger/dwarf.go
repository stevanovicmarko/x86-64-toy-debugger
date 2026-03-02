package debugger

import (
	"debug/dwarf"
	"sort"
)

// FunctionEntry represents a function found in DWARF debug info.
// Each function has one or more address ranges — most have a single
// contiguous [low, high) range, but functions split by the linker
// or optimized with hot/cold partitioning can have multiple ranges.
type FunctionEntry struct {
	Name      string
	Ranges    [][2]uint64 // [low, high) in file-address space
	IsInlined bool
}

// SourceLocation maps a program counter to the source file and line
// that generated it. This comes from the DWARF .debug_line section,
// which encodes a state machine that maps addresses to source positions.
type SourceLocation struct {
	File   string
	Line   int
	Column int
}

// funcAddrEntry pairs a function with a single address range for
// sorted lookup. One FunctionEntry with N ranges produces N entries.
type funcAddrEntry struct {
	startPC uint64
	endPC   uint64
	fn      *FunctionEntry
}

// DWARF wraps Go's debug/dwarf.Data with pre-built indexes for fast
// function and source-location lookup. It mirrors how ELF wraps
// debug/elf — the stdlib handles parsing, we add debugger-focused
// query methods on top.
type DWARF struct {
	data        *dwarf.Data
	funcsByAddr []funcAddrEntry          // sorted by startPC for binary search
	funcsByName map[string][]*FunctionEntry
	loadBias    uint64
}

// newDWARF walks the DWARF DIE tree once, extracting all functions
// (TagSubprogram) and inlined subroutines (TagInlinedSubroutine),
// then sorts them by address for fast lookup.
func newDWARF(data *dwarf.Data) *DWARF {
	d := &DWARF{
		data:        data,
		funcsByName: make(map[string][]*FunctionEntry),
	}

	reader := data.Reader()
	for {
		entry, err := reader.Next()
		if err != nil || entry == nil {
			break
		}

		switch entry.Tag {
		case dwarf.TagSubprogram:
			d.addFunction(entry, reader, false)
		case dwarf.TagInlinedSubroutine:
			d.addFunction(entry, reader, true)
		default:
			// Skip children of non-interesting DIEs — but only
			// leaf nodes. Compile units and other containers
			// hold the subprograms we want.
		}
	}

	// Sort by start address for binary search.
	sort.Slice(d.funcsByAddr, func(i, j int) bool {
		return d.funcsByAddr[i].startPC < d.funcsByAddr[j].startPC
	})

	return d
}

// addFunction extracts a function or inlined subroutine from a DIE
// and adds it to the lookup indexes.
func (d *DWARF) addFunction(entry *dwarf.Entry, reader *dwarf.Reader, inlined bool) {
	name := entryName(entry)

	// For inlined subroutines, the name lives on the abstract origin DIE.
	if name == "" && inlined {
		if ref, ok := entry.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset); ok {
			name = d.resolveAbstractOriginName(ref)
		}
	}

	if name == "" {
		return
	}

	// Get address ranges. Data.Ranges handles all encodings:
	// simple lowpc/highpc pairs, .debug_ranges, and DWARF 5 rnglists.
	ranges, err := d.data.Ranges(entry)
	if err != nil || len(ranges) == 0 {
		return
	}

	fn := &FunctionEntry{
		Name:      name,
		Ranges:    ranges,
		IsInlined: inlined,
	}

	d.funcsByName[name] = append(d.funcsByName[name], fn)

	// Only index non-inlined functions by address. Inlined subroutines
	// are nested inside their callers and would confuse PC→function
	// lookup (the caller is the "real" function at that address).
	if !inlined {
		for _, r := range ranges {
			d.funcsByAddr = append(d.funcsByAddr, funcAddrEntry{
				startPC: r[0],
				endPC:   r[1],
				fn:      fn,
			})
		}
	}
}

// resolveAbstractOriginName looks up a DIE by offset and returns its
// name attribute. Inlined subroutines reference their "abstract origin"
// (the original function definition) for the name.
func (d *DWARF) resolveAbstractOriginName(off dwarf.Offset) string {
	reader := d.data.Reader()
	reader.Seek(off)
	entry, err := reader.Next()
	if err != nil || entry == nil {
		return ""
	}
	return entryName(entry)
}

// entryName returns the DW_AT_name attribute of a DIE, or "".
func entryName(entry *dwarf.Entry) string {
	name, _ := entry.Val(dwarf.AttrName).(string)
	return name
}

// FunctionContainingPC returns the function whose address range contains
// the given virtual address. The address is adjusted for load bias
// before searching (virtual address → file address).
func (d *DWARF) FunctionContainingPC(addr uint64) *FunctionEntry {
	fileAddr := addr - d.loadBias

	// Binary search: find the last entry with startPC <= fileAddr.
	n := len(d.funcsByAddr)
	if n == 0 {
		return nil
	}

	idx := sort.Search(n, func(i int) bool {
		return d.funcsByAddr[i].startPC > fileAddr
	})
	idx-- // back up to the last entry with startPC <= fileAddr

	if idx < 0 {
		return nil
	}

	entry := &d.funcsByAddr[idx]
	if fileAddr >= entry.startPC && fileAddr < entry.endPC {
		return entry.fn
	}
	return nil
}

// FunctionsByName returns all functions (including inlined instances)
// with the given name, or nil if none.
func (d *DWARF) FunctionsByName(name string) []*FunctionEntry {
	return d.funcsByName[name]
}

// PCToSourceLocation maps a virtual address to its source file and line
// using the DWARF line number program. It iterates compile units and
// uses LineReader.SeekPC to find the matching line entry.
//
// This is O(CUs) per call, which is fine for a toy debugger — real
// debuggers build an address-sorted index of CU ranges.
func (d *DWARF) PCToSourceLocation(addr uint64) (SourceLocation, bool) {
	fileAddr := addr - d.loadBias

	reader := d.data.Reader()
	for {
		entry, err := reader.Next()
		if err != nil || entry == nil {
			break
		}

		if entry.Tag != dwarf.TagCompileUnit {
			continue
		}

		lr, err := d.data.LineReader(entry)
		if err != nil || lr == nil {
			reader.SkipChildren()
			continue
		}

		var lineEntry dwarf.LineEntry
		if err := lr.SeekPC(fileAddr, &lineEntry); err != nil {
			reader.SkipChildren()
			continue
		}

		return SourceLocation{
			File:   lineEntry.File.Name,
			Line:   lineEntry.Line,
			Column: lineEntry.Column,
		}, true
	}

	return SourceLocation{}, false
}
