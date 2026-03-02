package debugger

import (
	"fmt"
	"os"
)

// atEntry is the auxiliary vector tag for the program entry point.
// The kernel sets this to the actual virtual address where execution
// begins, which may differ from the ELF header's e_entry when ASLR
// relocates a PIE binary.
const atEntry = 9 // AT_ENTRY

// Target combines a traced Process with its ELF binary, providing the
// symbolic layer above raw process control. The CLI uses Target to
// resolve addresses to function names when displaying stop reasons.
type Target struct {
	process *Process
	elf     *ELF
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
	t.elf = e

	// Read the auxiliary vector to compute load bias.
	auxv, err := readAuxv(t.process.pid)
	if err != nil {
		return
	}

	if actualEntry, ok := auxv[atEntry]; ok {
		// Load bias = actual entry point (from kernel) - ELF entry point (from file).
		// For ET_EXEC (non-PIE) this is 0. For ET_DYN (PIE) this is the
		// ASLR base address.
		t.elf.SetLoadBias(actualEntry - t.elf.EntryPoint())
	}
}

// Process returns the underlying Process for direct ptrace operations.
func (t *Target) Process() *Process {
	return t.process
}

// ELF returns the ELF binary, or nil if ELF loading failed.
func (t *Target) ELF() *ELF {
	return t.elf
}

// Close releases both the process and the ELF file.
func (t *Target) Close() {
	if t.elf != nil {
		t.elf.Close()
	}
	t.process.Close()
}
