package debugger

import (
	"debug/dwarf"
	"fmt"
	"path/filepath"
)

// ---------------------------------------------------------------------------
// Stack Frame
//
// A StackFrame represents one logical frame in the call stack. Physical
// (non-inlined) frames have distinct register states; inlined frames
// share their parent's registers but have different source locations
// and function names.
// ---------------------------------------------------------------------------

// StackFrame represents one frame in the call stack.
type StackFrame struct {
	// Regs holds the register state for this frame. For inlined frames
	// this is the same as the containing physical frame's registers.
	Regs *Registers

	// PC is the program counter to report for this frame. For the
	// deepest frame, this is the actual PC. For caller frames, this
	// points to the call instruction (not the return address).
	PC uint64

	// FunctionName is the name of the function this frame belongs to.
	FunctionName string

	// Inlined is true if this frame represents an inlined function call.
	Inlined bool

	// Location is the source file and line for this frame.
	Location SourceLocation

	// CFA is the Canonical Frame Address for this frame (only valid
	// for physical frames, 0 for inlined frames).
	CFA uint64
}

// ---------------------------------------------------------------------------
// Stack Unwinding
//
// UnwindStack walks the call stack from the current PC, producing a
// slice of StackFrames ordered deepest-first (index 0 = current frame).
// It handles both physical frames (via CFI) and inlined frames (via
// DWARF inline stack).
// ---------------------------------------------------------------------------

// UnwindStack produces the call stack for the current process state.
// It uses DWARF call frame information for physical frame unwinding
// and DWARF inline stack information for logical inlined frames.
// The returned slice is ordered deepest-first (index 0 = innermost).
//
// maxFrames limits the number of physical frames to unwind (0 = unlimited).
// Even with a limit, all inlined frames at each physical frame are included.
func (t *Target) UnwindStack(maxFrames int) []StackFrame {
	if t.elf == nil {
		return nil
	}

	dw := t.dwarf()
	cfi := t.elf.CFI()
	if cfi == nil {
		return nil
	}

	proc := t.process
	pc, err := proc.GetPC()
	if err != nil {
		return nil
	}

	var frames []StackFrame
	regs := proc.Registers()
	physicalCount := 0

	for pc != 0 {
		if maxFrames > 0 && physicalCount >= maxFrames {
			break
		}

		// Check if the PC is within our ELF's address space.
		if dw == nil {
			break
		}
		fn := dw.FunctionContainingPC(pc)
		if fn == nil && physicalCount > 0 {
			break // PC is outside our ELF — stop unwinding
		}

		// Get the inline stack at this PC.
		inlineStack := dw.InlineStackAtAddress(pc)

		if len(inlineStack) > 1 {
			frames = append(frames, createInlineFrames(dw, regs, pc, inlineStack)...)
		} else {
			frame := createPhysicalFrame(dw, regs, pc, fn, physicalCount == 0)
			frames = append(frames, frame)
		}

		// Unwind one physical frame using CFI.
		unwoundRegs, frameCFA, err := cfi.UnwindFrame(proc, pc, regs)
		if err != nil {
			break // can't unwind further
		}

		// Record CFA on the last physical frame we added.
		for i := len(frames) - 1; i >= 0; i-- {
			if !frames[i].Inlined {
				frames[i].CFA = frameCFA
				break
			}
		}

		// The unwound PC is the return address. Subtract 1 to point
		// into the call instruction (for correct line table lookup).
		ripInfo, _ := RegisterInfoByName("rip")
		returnAddr := readRegisterUint64(unwoundRegs, ripInfo)
		if returnAddr == 0 {
			break
		}

		pc = returnAddr - 1
		regs = unwoundRegs
		physicalCount++
	}

	return frames
}

// createPhysicalFrame builds a StackFrame for a non-inlined function.
func createPhysicalFrame(dw *DWARF, regs *Registers, pc uint64, fn *FunctionEntry, isDeepest bool) StackFrame {
	frame := StackFrame{
		Regs: regs,
		PC:   pc,
	}

	if fn != nil {
		frame.FunctionName = fn.Name
	}

	if dw != nil {
		if entry, ok := dw.GetEntryByAddress(pc); ok {
			frame.Location = SourceLocation{
				File:   entry.File,
				Line:   entry.Line,
				Column: entry.Column,
			}
			if !isDeepest {
				// For caller frames, report the line table entry's
				// address (start of the call instruction).
				frame.PC = entry.Address
			}
		}
	}

	return frame
}

// createInlineFrames builds StackFrames for a PC with inlined functions.
// The inline stack is ordered outermost→innermost.
// We return frames ordered innermost-first (matching the desired output).
func createInlineFrames(dw *DWARF, regs *Registers, pc uint64, inlineStack []*FunctionEntry) []StackFrame {
	var frames []StackFrame

	for i := len(inlineStack) - 1; i >= 0; i-- {
		fn := inlineStack[i]
		frame := StackFrame{
			Regs:         regs,
			FunctionName: fn.Name,
			Inlined:      fn.IsInlined,
		}

		if i == len(inlineStack)-1 {
			// Innermost: use the actual PC and line table.
			frame.PC = pc
			if entry, ok := dw.GetEntryByAddress(pc); ok {
				frame.Location = SourceLocation{
					File:   entry.File,
					Line:   entry.Line,
					Column: entry.Column,
				}
			}
		} else {
			// Outer frame: the PC should be the low_pc of the inner
			// inlined subroutine (the call site), and the location
			// comes from the inlined subroutine's DW_AT_call_* attributes.
			inner := inlineStack[i+1]
			if len(inner.Ranges) > 0 {
				frame.PC = inner.Ranges[0][0] + dw.loadBias
			}
			callLoc := dw.getInlineCallLocation(inner.DIEOffset)
			if callLoc.File != "" {
				frame.Location = callLoc
			}
		}

		frames = append(frames, frame)
	}

	return frames
}

// getInlineCallLocation reads the DW_AT_call_file, DW_AT_call_line,
// and DW_AT_call_column attributes from an inlined subroutine DIE.
func (d *DWARF) getInlineCallLocation(dieOffset dwarf.Offset) SourceLocation {
	reader := d.data.Reader()
	reader.Seek(dieOffset)
	entry, err := reader.Next()
	if err != nil || entry == nil {
		return SourceLocation{}
	}

	loc := SourceLocation{}

	if line, ok := entry.Val(dwarf.AttrCallLine).(int64); ok {
		loc.Line = int(line)
	}
	if col, ok := entry.Val(dwarf.AttrCallColumn).(int64); ok {
		loc.Column = int(col)
	}

	// Resolve DW_AT_call_file to a file path by finding the compile
	// unit that contains this DIE and looking up its line table's
	// file table.
	if fileIdx, ok := entry.Val(dwarf.AttrCallFile).(int64); ok {
		loc.File = d.resolveCallFile(dieOffset, int(fileIdx))
	}

	return loc
}

// resolveCallFile resolves a DW_AT_call_file index to a file path.
// It finds the compile unit containing the given DIE and uses the
// line reader's file table to look up the index.
func (d *DWARF) resolveCallFile(dieOffset dwarf.Offset, fileIdx int) string {
	// Walk the top-level entries to find the compile unit that
	// contains this DIE offset.
	reader := d.data.Reader()
	for {
		entry, err := reader.Next()
		if err != nil || entry == nil {
			break
		}
		if entry.Tag != dwarf.TagCompileUnit {
			reader.SkipChildren()
			continue
		}

		// Check if this CU contains the target DIE offset.
		// The CU's children extend until the next CU or EOF.
		// We can check by reading the next top-level entry.
		cuOffset := entry.Offset

		// Save our position and try to get a line reader.
		lr, err := d.data.LineReader(entry)
		if err != nil || lr == nil {
			continue
		}

		// Check if the DIE offset is within this CU's range.
		// We do this by peeking at the next CU.
		nextReader := d.data.Reader()
		nextReader.Seek(cuOffset)
		nextReader.Next()          // skip the CU entry itself
		nextReader.SkipChildren()  // skip all children
		nextEntry, _ := nextReader.Next()

		inThisCU := false
		if nextEntry == nil {
			// Last CU — the DIE must be here.
			inThisCU = dieOffset >= cuOffset
		} else {
			inThisCU = dieOffset >= cuOffset && dieOffset < nextEntry.Offset
		}

		if !inThisCU {
			continue
		}

		// Found the CU. Look up the file index.
		files := lr.Files()
		if fileIdx >= 0 && fileIdx < len(files) && files[fileIdx] != nil {
			return files[fileIdx].Name
		}
		return ""
	}

	return ""
}

// Backtrace returns the call stack frames.
func (t *Target) Backtrace(maxFrames int) []StackFrame {
	return t.UnwindStack(maxFrames)
}

// FormatBacktrace returns a human-readable backtrace string.
func FormatBacktrace(frames []StackFrame) string {
	if len(frames) == 0 {
		return "no stack frames available\n"
	}

	var result string
	for i, frame := range frames {
		marker := " "
		if i == 0 {
			marker = "*"
		}

		loc := ""
		if frame.Location.File != "" {
			loc = fmt.Sprintf(" at %s:%d", filepath.Base(frame.Location.File), frame.Location.Line)
		}

		funcName := frame.FunctionName
		if funcName == "" {
			funcName = "??"
		}

		inlined := ""
		if frame.Inlined {
			inlined = " [inlined]"
		}

		result += fmt.Sprintf("%s[%d]: 0x%x %s%s%s\n", marker, i, frame.PC, funcName, inlined, loc)
	}

	return result
}
