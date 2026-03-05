package debugger

import (
	"debug/dwarf"
	"path/filepath"
	"sort"
	"strings"
)

// FunctionEntry represents a function found in DWARF debug info.
// Each function has one or more address ranges — most have a single
// contiguous [low, high) range, but functions split by the linker
// or optimized with hot/cold partitioning can have multiple ranges.
type FunctionEntry struct {
	Name      string
	Ranges    [][2]uint64  // [low, high) in file-address space
	IsInlined bool
	DIEOffset dwarf.Offset // offset of this DIE in .debug_info
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

// LineEntry represents a single row in the DWARF line table. It wraps
// the fields from Go's dwarf.LineEntry with File resolved to a plain
// string (the stdlib's *LineFile pointer is only valid during iteration).
// All addresses are in file-address space (before ASLR relocation).
type LineEntry struct {
	Address       uint64
	File          string
	Line          int
	Column        int
	IsStmt        bool
	BasicBlock    bool
	EndSequence   bool
	PrologueEnd   bool
	EpilogueBegin bool
	Discriminator int
}

// fileLineKey indexes entries by source file and line number.
type fileLineKey struct {
	file string
	line int
}

// lineIndex holds a pre-built index of all DWARF line table entries.
// Entries are sorted by address for binary search; byFileLine provides
// O(1) lookup by source location for "break at file:line" commands.
type lineIndex struct {
	entries    []LineEntry            // all entries, sorted by address
	byFileLine map[fileLineKey][]int // file:line → indices into entries
}

// DWARF wraps Go's debug/dwarf.Data with pre-built indexes for fast
// function and source-location lookup. It mirrors how ELF wraps
// debug/elf — the stdlib handles parsing, we add debugger-focused
// query methods on top.
type DWARF struct {
	data        *dwarf.Data
	funcsByAddr []funcAddrEntry          // sorted by startPC for binary search
	funcsByName map[string][]*FunctionEntry
	lines       lineIndex
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

	d.buildLineIndex()

	return d
}

// buildLineIndex iterates all compile units and their line number
// programs, collecting every line table entry into a sorted slice
// with a secondary file:line index.
func (d *DWARF) buildLineIndex() {
	d.lines.byFileLine = make(map[fileLineKey][]int)

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

		var le dwarf.LineEntry
		for {
			if err := lr.Next(&le); err != nil {
				break
			}
			fileName := ""
			if le.File != nil {
				fileName = le.File.Name
			}
			d.lines.entries = append(d.lines.entries, LineEntry{
				Address:       le.Address,
				File:          fileName,
				Line:          le.Line,
				Column:        le.Column,
				IsStmt:        le.IsStmt,
				BasicBlock:    le.BasicBlock,
				EndSequence:   le.EndSequence,
				PrologueEnd:   le.PrologueEnd,
				EpilogueBegin: le.EpilogueBegin,
				Discriminator: le.Discriminator,
			})
		}
	}

	// Sort by address for binary search.
	sort.Slice(d.lines.entries, func(i, j int) bool {
		return d.lines.entries[i].Address < d.lines.entries[j].Address
	})

	// Build the file:line → index map.
	for i := range d.lines.entries {
		e := &d.lines.entries[i]
		if e.EndSequence {
			continue
		}
		key := fileLineKey{file: e.File, line: e.Line}
		d.lines.byFileLine[key] = append(d.lines.byFileLine[key], i)
	}
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
		DIEOffset: entry.Offset,
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

// PCToSourceLocation maps a virtual address to its source file and line.
// It delegates to GetEntryByAddress and returns the simplified
// SourceLocation type for backward compatibility.
func (d *DWARF) PCToSourceLocation(addr uint64) (SourceLocation, bool) {
	entry, ok := d.GetEntryByAddress(addr)
	if !ok {
		return SourceLocation{}, false
	}
	return SourceLocation{
		File:   entry.File,
		Line:   entry.Line,
		Column: entry.Column,
	}, true
}

// GetEntryByAddress returns the line table entry covering the given
// virtual address. It binary-searches the pre-built sorted index,
// finding the last entry whose address is ≤ the target. EndSequence
// markers are skipped because they represent range terminators, not
// real source positions.
func (d *DWARF) GetEntryByAddress(addr uint64) (LineEntry, bool) {
	fileAddr := addr - d.loadBias

	n := len(d.lines.entries)
	if n == 0 {
		return LineEntry{}, false
	}

	// Find the last entry with Address <= fileAddr.
	idx := sort.Search(n, func(i int) bool {
		return d.lines.entries[i].Address > fileAddr
	})
	idx--

	// Walk backward past any EndSequence markers.
	for idx >= 0 && d.lines.entries[idx].EndSequence {
		idx--
	}

	if idx < 0 {
		return LineEntry{}, false
	}

	entry := d.lines.entries[idx]
	entry.Address += d.loadBias // return virtual address
	return entry, true
}

// GetEntriesByLine returns all line table entries matching the given
// file path and line number. The path can be a basename or partial
// suffix — it is matched against full DWARF paths using pathEndsWith.
// Returned addresses are virtual (adjusted for load bias).
func (d *DWARF) GetEntriesByLine(path string, line int) []LineEntry {
	var results []LineEntry

	for key, indices := range d.lines.byFileLine {
		if key.line != line {
			continue
		}
		if !pathEndsWith(key.file, path) {
			continue
		}
		for _, i := range indices {
			entry := d.lines.entries[i]
			entry.Address += d.loadBias
			results = append(results, entry)
		}
	}

	// Sort by address for deterministic output.
	sort.Slice(results, func(i, j int) bool {
		return results[i].Address < results[j].Address
	})

	return results
}

// AllLineEntries returns a copy of all line table entries in file-address
// space (no load bias adjustment). This is useful for testing and
// inspection.
func (d *DWARF) AllLineEntries() []LineEntry {
	result := make([]LineEntry, len(d.lines.entries))
	copy(result, d.lines.entries)
	return result
}

// InlineStackAtAddress returns the inline call stack at a virtual address,
// ordered outermost → innermost. The outermost entry is the containing
// non-inlined function; deeper entries are inlined subroutines whose
// ranges enclose the address. Returns nil if no DWARF info is available.
func (d *DWARF) InlineStackAtAddress(addr uint64) []*FunctionEntry {
	fileAddr := addr - d.loadBias

	// Find the non-inlined function containing this address.
	outerFn := d.FunctionContainingPC(addr)
	if outerFn == nil {
		return nil
	}

	stack := []*FunctionEntry{outerFn}
	d.walkInlineChildren(outerFn.DIEOffset, fileAddr, &stack)
	return stack
}

// walkInlineChildren recursively walks children of a DIE, looking for
// TagInlinedSubroutine entries whose ranges contain fileAddr. Matching
// entries are appended to stack and their children are recursed into.
// TagLexicalBlock entries are also recursed through since they can
// contain inlined subroutines.
func (d *DWARF) walkInlineChildren(parentOffset dwarf.Offset, fileAddr uint64, stack *[]*FunctionEntry) {
	reader := d.data.Reader()
	reader.Seek(parentOffset)

	// Read the parent entry itself and skip past it.
	parent, err := reader.Next()
	if err != nil || parent == nil || !parent.Children {
		return
	}

	for {
		child, err := reader.Next()
		if err != nil || child == nil || child.Tag == 0 {
			break
		}

		switch child.Tag {
		case dwarf.TagInlinedSubroutine:
			ranges, err := d.data.Ranges(child)
			if err != nil || !rangesContain(ranges, fileAddr) {
				reader.SkipChildren()
				continue
			}
			// Resolve the name via abstract origin.
			name := entryName(child)
			if name == "" {
				if ref, ok := child.Val(dwarf.AttrAbstractOrigin).(dwarf.Offset); ok {
					name = d.resolveAbstractOriginName(ref)
				}
			}
			fn := &FunctionEntry{
				Name:      name,
				Ranges:    ranges,
				IsInlined: true,
				DIEOffset: child.Offset,
			}
			*stack = append(*stack, fn)
			// Recurse into this inlined subroutine's children.
			d.walkInlineChildren(child.Offset, fileAddr, stack)
			// After recursion we've already consumed children via the
			// recursive seek, so skip them here. But since we already
			// recursed with a fresh reader from Seek, the current
			// reader's position is stale. We need to skip children
			// in the current reader.
			reader.SkipChildren()

		case dwarf.TagLexDwarfBlock:
			ranges, err := d.data.Ranges(child)
			if err != nil || !rangesContain(ranges, fileAddr) {
				reader.SkipChildren()
				continue
			}
			// Lexical blocks can contain inlined subroutines — recurse.
			d.walkInlineChildren(child.Offset, fileAddr, stack)
			reader.SkipChildren()

		default:
			reader.SkipChildren()
		}
	}
}

// rangesContain reports whether any [low, high) range contains addr.
func rangesContain(ranges [][2]uint64, addr uint64) bool {
	for _, r := range ranges {
		if addr >= r[0] && addr < r[1] {
			return true
		}
	}
	return false
}

// PrologueEndForRange scans the line table for the first entry with
// PrologueEnd==true in [lowPC, highPC). If none is found, it falls
// back to the address of the second line entry in the range (a common
// heuristic: the first entry is the prologue, the second is the body).
// All addresses are in file-address space.
func (d *DWARF) PrologueEndForRange(lowPC, highPC uint64) (uint64, bool) {
	entries := d.lines.entries
	n := len(entries)
	if n == 0 {
		return 0, false
	}

	// Binary search for the first entry with Address >= lowPC.
	start := sort.Search(n, func(i int) bool {
		return entries[i].Address >= lowPC
	})

	// First pass: look for an explicit PrologueEnd marker.
	for i := start; i < n && entries[i].Address < highPC; i++ {
		if entries[i].PrologueEnd && !entries[i].EndSequence {
			return entries[i].Address, true
		}
	}

	// Fallback: return the second non-EndSequence entry in the range.
	count := 0
	for i := start; i < n && entries[i].Address < highPC; i++ {
		if entries[i].EndSequence {
			continue
		}
		count++
		if count == 2 {
			return entries[i].Address, true
		}
	}

	return 0, false
}

// pathEndsWith checks whether fullPath ends with suffix at a path
// separator boundary. This allows matching "dwarf_target.c" against
// "/home/user/project/test/targets/dwarf_target.c".
func pathEndsWith(fullPath, suffix string) bool {
	if fullPath == suffix {
		return true
	}
	// Clean both paths to normalize separators.
	full := filepath.ToSlash(fullPath)
	sfx := filepath.ToSlash(suffix)

	if !strings.HasSuffix(full, sfx) {
		return false
	}
	// Ensure the match is at a path separator boundary.
	preceding := len(full) - len(sfx)
	return preceding > 0 && full[preceding-1] == '/'
}
