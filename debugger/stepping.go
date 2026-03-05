package debugger

import (
	"bufio"
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/arch/x86/x86asm"
)

// RunUntilAddress resumes the process and runs until it reaches the
// given virtual address. It creates a temporary internal breakpoint
// at the target, resumes, waits for the stop, and cleans up.
func (t *Target) RunUntilAddress(addr uint64) (StopReason, error) {
	proc := t.process

	// Check if a breakpoint already exists at this address.
	existing := proc.breakpointSites.enabledAtAddress(addr)
	if existing != nil {
		// Breakpoint already there — just resume and wait.
		if err := proc.Resume(); err != nil {
			return StopReason{}, err
		}
		return proc.WaitOnSignal()
	}

	// Create a temporary internal breakpoint.
	site, err := proc.CreateBreakpointSite(addr, false, true)
	if err != nil {
		return StopReason{}, fmt.Errorf("RunUntilAddress: create temp BP at 0x%x: %w", addr, err)
	}
	if err := site.Enable(); err != nil {
		proc.breakpointSites.removeByID(site.ID())
		return StopReason{}, fmt.Errorf("RunUntilAddress: enable temp BP: %w", err)
	}

	// Resume and wait.
	if err := proc.Resume(); err != nil {
		site.Disable()
		proc.breakpointSites.removeByID(site.ID())
		return StopReason{}, err
	}
	reason, err := proc.WaitOnSignal()

	// Clean up the temporary breakpoint.
	site.Disable()
	proc.breakpointSites.removeByID(site.ID())

	if err != nil {
		return reason, err
	}

	// If we stopped at our temp BP, mark it as a single step.
	pc, pcErr := proc.GetPC()
	if pcErr == nil && pc == addr && reason.TrapReason != nil && *reason.TrapReason == TrapSoftwareBreak {
		trap := TrapSingleStep
		reason.TrapReason = &trap
	}

	return reason, nil
}

// StepIn performs a source-level step-in: it advances the process until
// the source line changes. If the new location is at the start of a
// function, the prologue is skipped so the user lands on the first
// real statement.
func (t *Target) StepIn() (StopReason, error) {
	proc := t.process
	dw := t.dwarf()

	// Get current source location.
	startLoc, hasStart := t.sourceLocationAtPC()

	if !hasStart || dw == nil {
		// No DWARF info — fall back to single instruction step.
		return proc.StepInstruction()
	}

	// Step instructions until the source line changes.
	for {
		reason, err := proc.StepInstruction()
		if err != nil {
			return reason, err
		}
		if reason.Reason != ProcessStopped {
			return reason, nil
		}

		loc, ok := t.sourceLocationAtPC()
		if !ok {
			continue // no source info at this PC, keep stepping
		}
		if loc.EndSequence {
			continue // end-of-sequence marker, not a real line
		}
		if loc.File == startLoc.File && loc.Line == startLoc.Line {
			continue // still on the same line
		}

		// We're on a new line. Check if we're at a function entry
		// and need to skip the prologue.
		if err := t.skipPrologueIfNeeded(); err != nil {
			return reason, err
		}

		trap := TrapSingleStep
		reason.TrapReason = &trap
		return reason, nil
	}
}

// StepOver performs a source-level step-over: it advances the process
// until the source line changes, but steps over CALL instructions by
// placing a temporary breakpoint at the return address.
func (t *Target) StepOver() (StopReason, error) {
	proc := t.process
	dw := t.dwarf()

	startLoc, hasStart := t.sourceLocationAtPC()
	if !hasStart || dw == nil {
		return proc.StepInstruction()
	}

	for {
		pc, err := proc.GetPC()
		if err != nil {
			return StopReason{}, err
		}

		// Check if the current instruction is a CALL.
		isCall, instrLen, err := isCallAt(proc, pc)
		if err != nil {
			return StopReason{}, err
		}

		var reason StopReason
		if isCall {
			// Set temp BP at the return address (PC + instruction length).
			reason, err = t.RunUntilAddress(pc + uint64(instrLen))
		} else {
			reason, err = proc.StepInstruction()
		}
		if err != nil {
			return reason, err
		}
		if reason.Reason != ProcessStopped {
			return reason, nil
		}

		loc, ok := t.sourceLocationAtPC()
		if !ok {
			continue
		}
		if loc.EndSequence {
			continue
		}
		if loc.File == startLoc.File && loc.Line == startLoc.Line {
			continue
		}

		trap := TrapSingleStep
		reason.TrapReason = &trap
		return reason, nil
	}
}

// StepOut steps out of the current function. For inlined functions it
// runs to the end of the inlined range. For regular functions it reads
// the return address from [rbp+8] and runs to that address.
func (t *Target) StepOut() (StopReason, error) {
	proc := t.process
	dw := t.dwarf()

	pc, err := proc.GetPC()
	if err != nil {
		return StopReason{}, err
	}

	// Check for inline stack — if deepest frame is inlined, run to
	// end of the inlined range.
	if dw != nil {
		stack := dw.InlineStackAtAddress(pc)
		if len(stack) > 1 {
			// The last entry is the deepest inlined function.
			deepest := stack[len(stack)-1]
			if deepest.IsInlined {
				// Find the max high address of this inlined range.
				var maxHigh uint64
				for _, r := range deepest.Ranges {
					if r[1] > maxHigh {
						maxHigh = r[1]
					}
				}
				// maxHigh is in file-address space; adjust by load bias.
				targetAddr := maxHigh + t.elf.LoadBias()
				return t.RunUntilAddress(targetAddr)
			}
		}
	}

	// Regular function: read return address from [rbp+8].
	rbpInfo, ok := RegisterInfoByName("rbp")
	if !ok {
		return StopReason{}, newError("rbp register not found")
	}
	rbp := proc.regs.Read(rbpInfo).(uint64)

	retAddrBytes, err := proc.ReadMemory(rbp+8, 8)
	if err != nil {
		return StopReason{}, fmt.Errorf("StepOut: read return address at 0x%x: %w", rbp+8, err)
	}
	retAddr := binary.LittleEndian.Uint64(retAddrBytes)

	return t.RunUntilAddress(retAddr)
}

// SetBreakpointAtFunction sets breakpoints on all instances of the
// named function, past their prologues. Returns the created sites.
func (t *Target) SetBreakpointAtFunction(name string, hardware bool) ([]*BreakpointSite, error) {
	proc := t.process
	dw := t.dwarf()
	var sites []*BreakpointSite

	if dw != nil {
		fns := dw.FunctionsByName(name)
		for _, fn := range fns {
			if fn.IsInlined {
				continue // skip inlined instances
			}
			for _, r := range fn.Ranges {
				lowPC := r[0]
				highPC := r[1]
				// Try to find prologue end.
				addr := lowPC
				if prologueEnd, ok := dw.PrologueEndForRange(lowPC, highPC); ok {
					addr = prologueEnd
				}
				// Adjust by load bias.
				addr += t.elf.LoadBias()

				site, err := proc.CreateBreakpointSite(addr, hardware, false)
				if err != nil {
					continue // skip duplicates
				}
				if err := site.Enable(); err != nil {
					proc.breakpointSites.removeByID(site.ID())
					continue
				}
				sites = append(sites, site)
			}
		}
	}

	// Fallback to ELF symbol table if no DWARF matches.
	if len(sites) == 0 && t.elf != nil {
		for _, sym := range t.elf.SymbolsByName(name) {
			addr := sym.Value + t.elf.LoadBias()
			site, err := proc.CreateBreakpointSite(addr, hardware, false)
			if err != nil {
				continue
			}
			if err := site.Enable(); err != nil {
				proc.breakpointSites.removeByID(site.ID())
				continue
			}
			sites = append(sites, site)
		}
	}

	if len(sites) == 0 {
		return nil, newErrorf("no function %q found", name)
	}
	return sites, nil
}

// SetBreakpointAtLine sets breakpoints at all addresses corresponding
// to the given file:line. Returns the created sites.
func (t *Target) SetBreakpointAtLine(file string, line int, hardware bool) ([]*BreakpointSite, error) {
	proc := t.process
	dw := t.dwarf()

	if dw == nil {
		return nil, newError("no DWARF info available for line breakpoints")
	}

	entries := dw.GetEntriesByLine(file, line)
	if len(entries) == 0 {
		return nil, newErrorf("no code at %s:%d", file, line)
	}

	var sites []*BreakpointSite
	for _, entry := range entries {
		addr := entry.Address

		// Check if this address is at a function start, and skip prologue.
		fn := dw.FunctionContainingPC(addr)
		if fn != nil && len(fn.Ranges) > 0 {
			lowPC := fn.Ranges[0][0] + dw.loadBias
			if addr == lowPC {
				if pe, ok := dw.PrologueEndForRange(fn.Ranges[0][0], fn.Ranges[0][1]); ok {
					addr = pe + dw.loadBias
				}
			}
		}

		site, err := proc.CreateBreakpointSite(addr, hardware, false)
		if err != nil {
			continue
		}
		if err := site.Enable(); err != nil {
			proc.breakpointSites.removeByID(site.ID())
			continue
		}
		sites = append(sites, site)
	}

	if len(sites) == 0 {
		return nil, newErrorf("could not set breakpoint at %s:%d", file, line)
	}
	return sites, nil
}

// PrintSource prints source lines around currentLine from the given file.
// The current line is marked with '>'. contextLines controls how many
// lines above and below to show.
func (t *Target) PrintSource(w io.Writer, file string, currentLine, contextLines int) error {
	f, err := os.Open(file)
	if err != nil {
		return fmt.Errorf("open source file: %w", err)
	}
	defer f.Close()

	startLine := currentLine - contextLines
	if startLine < 1 {
		startLine = 1
	}
	endLine := currentLine + contextLines

	scanner := bufio.NewScanner(f)
	lineNum := 0
	// Calculate width needed for line numbers.
	width := len(fmt.Sprintf("%d", endLine))

	for scanner.Scan() {
		lineNum++
		if lineNum < startLine {
			continue
		}
		if lineNum > endLine {
			break
		}

		marker := " "
		if lineNum == currentLine {
			marker = ">"
		}
		fmt.Fprintf(w, "%s %*d  %s\n", marker, width, lineNum, scanner.Text())
	}

	return scanner.Err()
}

// SourceLocationAtPC returns the source location for the current PC,
// or false if no DWARF info is available.
func (t *Target) SourceLocationAtPC() (SourceLocation, bool) {
	loc, ok := t.sourceLocationAtPC()
	if !ok {
		return SourceLocation{}, false
	}
	return SourceLocation{
		File:   loc.File,
		Line:   loc.Line,
		Column: loc.Column,
	}, true
}

// sourceLocationAtPC is the internal version that returns the full
// LineEntry (including EndSequence) for stepping logic.
func (t *Target) sourceLocationAtPC() (LineEntry, bool) {
	dw := t.dwarf()
	if dw == nil {
		return LineEntry{}, false
	}
	pc, err := t.process.GetPC()
	if err != nil {
		return LineEntry{}, false
	}
	return dw.GetEntryByAddress(pc)
}

// dwarf returns the DWARF info from the ELF, or nil.
func (t *Target) dwarf() *DWARF {
	if t.elf == nil {
		return nil
	}
	return t.elf.DWARF()
}

// skipPrologueIfNeeded checks if the current PC is at the start of a
// function and, if so, runs past the prologue.
func (t *Target) skipPrologueIfNeeded() error {
	dw := t.dwarf()
	if dw == nil {
		return nil
	}

	pc, err := t.process.GetPC()
	if err != nil {
		return nil
	}

	fn := dw.FunctionContainingPC(pc)
	if fn == nil {
		return nil
	}

	// Check if PC is at the function's low PC (start of function).
	for _, r := range fn.Ranges {
		lowPC := r[0] + dw.loadBias
		if pc == lowPC {
			if pe, ok := dw.PrologueEndForRange(r[0], r[1]); ok {
				peVirt := pe + dw.loadBias
				if peVirt > pc {
					_, err := t.RunUntilAddress(peVirt)
					return err
				}
			}
			break
		}
	}

	return nil
}

// isCallAt reads the instruction at addr and reports whether it is a
// CALL instruction. Returns the instruction length for computing the
// return address.
func isCallAt(proc *Process, addr uint64) (bool, int, error) {
	// x86-64 instructions are at most 15 bytes.
	data, err := proc.ReadMemoryWithoutTraps(addr, 15)
	if err != nil {
		return false, 0, err
	}
	inst, err := x86asm.Decode(data, 64)
	if err != nil {
		return false, 1, nil // can't decode → not a call
	}
	return inst.Op == x86asm.CALL, inst.Len, nil
}

// PrintSourceAtPC is a convenience that resolves the current PC to a
// source location and prints context lines. Returns false if no source
// info is available.
func (t *Target) PrintSourceAtPC(w io.Writer, contextLines int) bool {
	loc, ok := t.SourceLocationAtPC()
	if !ok {
		return false
	}
	// Try the file path as-is first, then try to find it relative
	// to the binary's directory.
	file := loc.File
	if _, err := os.Stat(file); err != nil {
		// Try relative to binary path.
		if t.elf != nil {
			dir := filepath.Dir(t.elf.path)
			candidate := filepath.Join(dir, filepath.Base(file))
			if _, err := os.Stat(candidate); err == nil {
				file = candidate
			}
		}
	}

	fn := ""
	if dw := t.dwarf(); dw != nil {
		if f := dw.FunctionContainingPC(t.currentPC()); f != nil {
			fn = f.Name
		}
	}

	if fn != "" {
		fmt.Fprintf(w, "%s at %s:%d\n", fn, filepath.Base(loc.File), loc.Line)
	} else {
		fmt.Fprintf(w, "%s:%d\n", filepath.Base(loc.File), loc.Line)
	}

	if err := t.PrintSource(w, file, loc.Line, contextLines); err != nil {
		// Source file not available — silently skip.
		return false
	}
	return true
}

// currentPC returns the current program counter, or 0 on error.
func (t *Target) currentPC() uint64 {
	pc, _ := t.process.GetPC()
	return pc
}

// DWARF returns the DWARF debug info, or nil. This is a convenience
// accessor so callers don't need to go through ELF().DWARF().
func (t *Target) DWARF() *DWARF {
	return t.dwarf()
}

// InlineDepthAtPC returns how deep the inline stack is at the current
// PC. Returns 0 if no inlining or no DWARF info.
func (t *Target) InlineDepthAtPC() int {
	dw := t.dwarf()
	if dw == nil {
		return 0
	}
	pc := t.currentPC()
	stack := dw.InlineStackAtAddress(pc)
	if len(stack) <= 1 {
		return 0
	}
	return len(stack) - 1 // exclude the outer function
}
