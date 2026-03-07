package debugger

import (
	"debug/elf"
	"fmt"
	"os"
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

// Close releases both the process and all ELF files.
func (t *Target) Close() {
	t.elves.Close()
	t.process.Close()
}
