package debugger

import (
	"debug/dwarf"
	"debug/elf"
	"fmt"
	"os"
	"strconv"
	"strings"
)

// atEntry is the auxiliary vector tag for the program entry point.
// The kernel sets this to the actual virtual address where execution
// begins, which may differ from the ELF header's e_entry when ASLR
// relocates a PIE binary.
const atEntry = 9 // AT_ENTRY

// Target combines a traced Process with its ELF binaries, providing the
// symbolic layer above raw process control. It manages multiple ELF
// objects — the main executable plus any shared libraries loaded by
// the dynamic linker.
type Target struct {
	process *Process
	elves   ELFCollection // all loaded ELF objects
	mainELF *ELF          // convenience pointer to the main executable

	// rendezvousAddr is the virtual address of the dynamic linker's
	// r_debug structure. Zero until resolved via the entry-point
	// breakpoint callback.
	rendezvousAddr uint64
}

// LaunchTarget starts a new process under ptrace, opens its ELF binary,
// and computes the load bias from the auxiliary vector. This is the
// primary entry point for the CLI.
func LaunchTarget(program string, args ...string) (*Target, error) {
	return launchTargetWithOpts(program, LaunchOptions{}, args)
}

// LaunchTargetWithOptions is like LaunchTarget but allows configuring
// child stdout/stderr redirection.
func LaunchTargetWithOptions(program string, opts LaunchOptions, args ...string) (*Target, error) {
	return launchTargetWithOpts(program, opts, args)
}

func launchTargetWithOpts(program string, opts LaunchOptions, args []string) (*Target, error) {
	proc, err := launchWithOpts(program, true, opts, args)
	if err != nil {
		return nil, err
	}

	t := &Target{process: proc}
	proc.target = t
	proc.EnableThreadTracking()
	t.loadELF(program)

	// Set an internal breakpoint on the real entry point so that
	// when it's hit, the dynamic linker has finished initialization
	// and we can read the rendezvous structure.
	if t.installEntryPointBreakpoint() {
		// Resume to the entry-point breakpoint so the dynamic linker
		// runs and initializes. The breakpoint handler will discover
		// shared libraries and auto-resume, leaving the process
		// stopped at the entry point.
		if err := proc.Resume(); err != nil {
			return nil, err
		}
		if _, err := proc.WaitOnSignal(); err != nil {
			return nil, err
		}
	}

	return t, nil
}

// AttachTarget attaches to an existing process and opens its ELF binary
// via /proc/<pid>/exe.
func AttachTarget(pid int) (*Target, error) {
	proc, err := Attach(pid)
	if err != nil {
		return nil, err
	}

	t := &Target{process: proc}
	proc.target = t
	proc.EnableThreadTracking()

	// Resolve the executable path from /proc/<pid>/exe.
	exePath := fmt.Sprintf("/proc/%d/exe", pid)
	resolved, err := os.Readlink(exePath)
	if err == nil {
		t.loadELF(resolved)
	}

	// When attaching, the dynamic linker is already initialized,
	// so resolve the rendezvous structure immediately.
	t.resolveRendezvous()

	return t, nil
}

// loadELF opens the ELF binary and computes the load bias. Errors are
// non-fatal — the Target works without symbols, just without name
// resolution.
func (t *Target) loadELF(path string) {
	e, err := OpenELF(path)
	if err != nil {
		return
	}
	t.mainELF = e
	t.elves.Push(e)

	// Read the auxiliary vector to compute load bias.
	auxv, err := readAuxv(t.process.pid)
	if err != nil {
		return
	}

	if actualEntry, ok := auxv[atEntry]; ok {
		// Load bias = actual entry point (from kernel) - ELF entry point (from file).
		// For ET_EXEC (non-PIE) this is 0. For ET_DYN (PIE) this is the
		// ASLR base address.
		t.mainELF.SetLoadBias(actualEntry - t.mainELF.EntryPoint())
	}
}

// installEntryPointBreakpoint sets an internal breakpoint at the
// program's real entry point (from AT_ENTRY in the auxiliary vector).
// When the dynamic linker finishes and jumps to the entry point,
// this breakpoint fires and we resolve the rendezvous structure.
// Returns true if the breakpoint was installed (i.e., the program
// has a dynamic linker).
func (t *Target) installEntryPointBreakpoint() bool {
	if t.mainELF == nil {
		return false
	}

	// Check if the program has an INTERP segment (dynamic linker).
	hasDynLinker := false
	for _, prog := range t.mainELF.file.Progs {
		if prog.Type == elf.PT_INTERP {
			hasDynLinker = true
			break
		}
	}
	if !hasDynLinker {
		return false // statically linked — no entry-point dance needed
	}

	auxv, err := readAuxv(t.process.pid)
	if err != nil {
		return false
	}
	entryAddr, ok := auxv[atEntry]
	if !ok {
		return false
	}

	bp, err := t.process.CreateBreakpointSite(entryAddr, false, true)
	if err != nil {
		return false
	}
	bp.InstallHitHandler(func() bool {
		t.resolveRendezvous()
		// Disable and remove the entry-point breakpoint — it's a
		// one-shot breakpoint that we no longer need.
		bp.Disable()
		t.process.breakpointSites.removeByAddress(entryAddr)
		// Return false — stop here so LaunchTarget can return with
		// the process stopped and all shared libraries discovered.
		return false
	})
	bp.Enable()
	return true
}

// Process returns the underlying Process for direct ptrace operations.
func (t *Target) Process() *Process {
	return t.process
}

// ELF returns the main ELF binary, or nil if ELF loading failed.
// For multi-library lookups, use ELFs() instead.
func (t *Target) ELF() *ELF {
	return t.mainELF
}

// ELFs returns the collection of all loaded ELF objects (main binary
// + shared libraries).
func (t *Target) ELFs() *ELFCollection {
	return &t.elves
}

// ELFContainingPC returns the ELF object whose address range contains
// the current program counter, or the main ELF as fallback.
func (t *Target) ELFContainingPC() *ELF {
	pc, err := t.process.GetPC()
	if err != nil {
		return t.mainELF
	}
	if e := t.elves.ELFContainingAddress(pc); e != nil {
		return e
	}
	return t.mainELF
}

// notifyThreadLifecycle is called by Process.reportThreadLifecycle when
// a thread is created or exits. Currently a no-op hook for future use
// (e.g., replicating debug register state to new threads).
func (t *Target) notifyThreadLifecycle(reason StopReason) {
	// Future: replicate hardware breakpoints/watchpoints to new threads,
	// update per-thread DWARF state, etc.
}

// ---------------------------------------------------------------------------
// Variable Lookup and Resolution
//
// FindVariable searches for a variable by name: first as a local
// variable in the current scope, then as a global across all ELFs.
//
// ResolveIndirectName handles compound names like "info.regs[0].name"
// by parsing operators (., ->, []) and navigating the type hierarchy.
// ---------------------------------------------------------------------------

// FindVariable looks up a variable by name at the current PC.
// It checks local variables first (lexical scopes), then globals
// across all loaded ELF files. Returns the DIE offset, the DWARF
// instance that owns it, and whether it was found.
func (t *Target) FindVariable(name string) (dwarf.Offset, *DWARF, bool) {
	pc, err := t.process.GetPC()
	if err != nil {
		return 0, nil, false
	}

	// Try local variables in the ELF containing the current PC.
	if dw := t.dwarf(); dw != nil {
		if off, found := dw.FindLocalVariable(name, pc); found {
			return off, dw, true
		}
	}

	// Try global variables across all loaded ELFs.
	var foundOff dwarf.Offset
	var foundDW *DWARF
	found := false
	t.elves.ForEach(func(e *ELF) {
		if found {
			return
		}
		dw := e.DWARF()
		if dw == nil {
			return
		}
		if off, ok := dw.FindGlobalVariable(name); ok {
			foundOff = off
			foundDW = dw
			found = true
		}
	})
	return foundOff, foundDW, found
}

// ResolveIndirectName resolves a compound variable name like
// "info.regs[0].name" at the current PC. It supports:
//   - "." for struct member access
//   - "->" for pointer dereference + member access
//   - "[n]" for array/pointer indexing
func (t *Target) ResolveIndirectName(name string) (*TypedData, error) {
	// Split off the initial variable name from operators.
	opPos := strings.IndexAny(name, ".-[")
	varName := name
	if opPos >= 0 {
		varName = name[:opPos]
	}

	data, err := t.getInitialVariableData(varName)
	if err != nil {
		return nil, err
	}

	// Process remaining operators.
	rest := ""
	if opPos >= 0 {
		rest = name[opPos:]
	}

	for len(rest) > 0 {
		switch {
		case rest[0] == '-':
			// -> operator: dereference pointer, then read member.
			if len(rest) < 2 || rest[1] != '>' {
				return nil, fmt.Errorf("invalid operator at %q", rest)
			}
			data, err = data.DerefPointer(t.process)
			if err != nil {
				return nil, err
			}
			rest = rest[2:]
			// Fall through to read the member name (handled by '.' or '>' case next iteration).
			// Actually, after '->', we need to read the member name now.
			memberEnd := strings.IndexAny(rest, ".-[")
			memberName := rest
			if memberEnd >= 0 {
				memberName = rest[:memberEnd]
				rest = rest[memberEnd:]
			} else {
				rest = ""
			}
			data, err = data.ReadMember(t.process, memberName)
			if err != nil {
				return nil, err
			}

		case rest[0] == '.':
			rest = rest[1:]
			memberEnd := strings.IndexAny(rest, ".-[")
			memberName := rest
			if memberEnd >= 0 {
				memberName = rest[:memberEnd]
				rest = rest[memberEnd:]
			} else {
				rest = ""
			}
			data, err = data.ReadMember(t.process, memberName)
			if err != nil {
				return nil, err
			}

		case rest[0] == '[':
			closeIdx := strings.Index(rest, "]")
			if closeIdx < 0 {
				return nil, fmt.Errorf("missing ']' in %q", rest)
			}
			indexStr := rest[1:closeIdx]
			index, parseErr := strconv.Atoi(indexStr)
			if parseErr != nil {
				return nil, fmt.Errorf("invalid index %q", indexStr)
			}
			data, err = data.Index(t.process, index)
			if err != nil {
				return nil, err
			}
			rest = rest[closeIdx+1:]

		default:
			return nil, fmt.Errorf("unexpected character %q in name", string(rest[0]))
		}
	}

	return data, nil
}

// getInitialVariableData locates a variable by name and reads its data.
func (t *Target) getInitialVariableData(name string) (*TypedData, error) {
	off, dw, found := t.FindVariable(name)
	if !found {
		return nil, fmt.Errorf("variable %q not found", name)
	}

	varType, err := dw.ResolveType(off)
	if err != nil {
		return nil, fmt.Errorf("resolve type for %q: %w", name, err)
	}

	regs := t.process.Registers()
	if regs == nil {
		return nil, fmt.Errorf("registers not available")
	}

	result, err := dw.EvalLocationAttr(t.process, regs, off, false)
	if err != nil {
		return nil, fmt.Errorf("evaluate location of %q: %w", name, err)
	}

	size := varType.Size()
	if size <= 0 {
		size = 8
	}
	data, err := ReadLocationData(t.process, regs, result, int(size))
	if err != nil {
		return nil, fmt.Errorf("read data for %q: %w", name, err)
	}

	// Extract address if the location is a simple memory address.
	var addr *uint64
	if !result.IsComposite && result.Location.Kind == LocAddress {
		a := result.Location.Address
		addr = &a
	}

	return &TypedData{Data: data, Type: varType, Address: addr}, nil
}

// VariableLocation returns a description of where a variable lives
// (register, memory address, or composite).
func (t *Target) VariableLocation(name string) (ExprResult, error) {
	off, dw, found := t.FindVariable(name)
	if !found {
		return ExprResult{}, fmt.Errorf("variable %q not found", name)
	}

	regs := t.process.Registers()
	if regs == nil {
		return ExprResult{}, fmt.Errorf("registers not available")
	}

	return dw.EvalLocationAttr(t.process, regs, off, false)
}

// LocalVariables returns typed data for all local variables visible
// at the current PC. Variables in inner scopes shadow outer ones.
func (t *Target) LocalVariables() ([]struct {
	Name string
	Data *TypedData
}, error) {
	pc, err := t.process.GetPC()
	if err != nil {
		return nil, err
	}

	dw := t.dwarf()
	if dw == nil {
		return nil, fmt.Errorf("no DWARF info available")
	}

	offsets := dw.LocalVariablesAtAddress(pc)
	regs := t.process.Registers()
	if regs == nil {
		return nil, fmt.Errorf("registers not available")
	}

	type varEntry struct {
		Name string
		Data *TypedData
	}
	var result []varEntry

	for _, off := range offsets {
		entry, err := dw.ReadDIE(off)
		if err != nil {
			continue
		}
		varName := entryName(entry)
		if varName == "" {
			continue
		}

		varType, err := dw.ResolveType(off)
		if err != nil {
			continue
		}

		locResult, err := dw.EvalLocationAttr(t.process, regs, off, false)
		if err != nil {
			continue
		}

		size := varType.Size()
		if size <= 0 {
			size = 8
		}
		data, err := ReadLocationData(t.process, regs, locResult, int(size))
		if err != nil {
			continue
		}

		var addr *uint64
		if !locResult.IsComposite && locResult.Location.Kind == LocAddress {
			a := locResult.Location.Address
			addr = &a
		}

		result = append(result, varEntry{
			Name: varName,
			Data: &TypedData{Data: data, Type: varType, Address: addr},
		})
	}

	// Convert to exported type.
	out := make([]struct {
		Name string
		Data *TypedData
	}, len(result))
	for i, v := range result {
		out[i].Name = v.Name
		out[i].Data = v.Data
	}
	return out, nil
}

// Close releases both the process and all ELF files.
func (t *Target) Close() {
	t.elves.Close()
	t.process.Close()
}
