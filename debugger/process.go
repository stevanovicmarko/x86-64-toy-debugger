package debugger

import (
	"encoding/binary"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
)

// LaunchOptions configures optional behavior for LaunchWithOptions.
type LaunchOptions struct {
	Stdout io.Writer // If non-nil, the child's stdout is sent here instead of os.Stdout.
	Stderr io.Writer // If non-nil, the child's stderr is sent here instead of os.Stderr.
}

// ProcessState represents the running state of a traced process.
type ProcessState int

const (
	ProcessStopped    ProcessState = iota // Process is stopped by a signal
	ProcessRunning                        // Process is currently executing
	ProcessExited                         // Process exited normally
	ProcessTerminated                     // Process was killed by a signal
)

// StopReason holds the reason a process stopped and additional info
// such as the exit code or signal number.
type StopReason struct {
	Reason ProcessState
	Info   uint8
}

// newStopReason parses a syscall.WaitStatus into a StopReason.
func newStopReason(ws syscall.WaitStatus) StopReason {
	switch {
	case ws.Exited():
		return StopReason{Reason: ProcessExited, Info: uint8(ws.ExitStatus())}
	case ws.Signaled():
		return StopReason{Reason: ProcessTerminated, Info: uint8(ws.Signal())}
	case ws.Stopped():
		return StopReason{Reason: ProcessStopped, Info: uint8(ws.StopSignal())}
	default:
		return StopReason{Reason: ProcessStopped, Info: 0}
	}
}

// Process represents a running process being debugged.
// Users must create one via Launch or Attach; direct construction is not possible
// because all fields are unexported.
type Process struct {
	pid             int
	terminateOnEnd  bool
	isAttached      bool
	state           ProcessState
	regs            *Registers
	breakpointSites breakpointSiteCollection
}

// Launch starts a new process under ptrace and returns a Process that
// manages it. The process is stopped immediately after exec so the
// debugger can control it.
//
// The calling goroutine is pinned to its current OS thread for the
// lifetime of the returned Process (released by Close). This is
// required because Linux ptrace is per-thread: only the thread that
// forked the tracee may issue subsequent ptrace requests for it.
func Launch(program string, args ...string) (*Process, error) {
	return launchWithOpts(program, true, LaunchOptions{}, args)
}

// LaunchWithOptions starts a new process under ptrace with the given
// options. It behaves like Launch but allows redirecting the child's
// stdout/stderr — useful in tests that need to capture inferior output.
func LaunchWithOptions(program string, opts LaunchOptions, args ...string) (*Process, error) {
	return launchWithOpts(program, true, opts, args)
}

// LaunchNoDebug starts a new process without ptrace tracing and returns
// a Process that manages it. The process runs freely; use Attach to
// begin tracing it later. This is primarily useful in tests that need
// a running target to attach to separately.
func LaunchNoDebug(program string, args ...string) (*Process, error) {
	return launchWithOpts(program, false, LaunchOptions{}, args)
}

func launchWithOpts(program string, debug bool, opts LaunchOptions, args []string) (*Process, error) {
	if debug {
		runtime.LockOSThread()
	}

	cmd := exec.Command(program, args...)
	if opts.Stdout != nil {
		cmd.Stdout = opts.Stdout
	} else {
		cmd.Stdout = os.Stdout
	}
	if opts.Stderr != nil {
		cmd.Stderr = opts.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	if debug {
		cmd.SysProcAttr = &syscall.SysProcAttr{
			Ptrace: true,
		}
	}

	if err := cmd.Start(); err != nil {
		if debug {
			runtime.UnlockOSThread()
		}
		return nil, newErrorf("exec failed: %v", err)
	}

	proc := &Process{
		pid:            cmd.Process.Pid,
		terminateOnEnd: true,
		isAttached:     debug,
		state:          ProcessStopped,
	}

	if debug {
		if _, err := proc.WaitOnSignal(); err != nil {
			runtime.UnlockOSThread()
			return nil, err
		}
		proc.regs = &Registers{pid: proc.pid}
		if err := proc.regs.readAll(); err != nil {
			proc.Close()
			return nil, err
		}
	} else {
		proc.state = ProcessRunning
	}

	return proc, nil
}

// Attach attaches to an existing process by PID and returns a Process
// that manages it.
//
// It uses PTRACE_SEIZE + PTRACE_INTERRUPT rather than the older
// PTRACE_ATTACH because the latter sends SIGSTOP, which can produce
// duplicate stop events and confuse subsequent Resume calls.
//
// Like Launch, the calling goroutine is pinned to its OS thread for
// the lifetime of the returned Process (released by Close).
func Attach(pid int) (*Process, error) {
	if pid == 0 {
		return nil, newError("invalid PID")
	}

	runtime.LockOSThread()

	if err := ptraceSeizeProcess(pid); err != nil {
		runtime.UnlockOSThread()
		return nil, newErrorf("could not attach: %v", err)
	}

	if err := ptraceInterruptProcess(pid); err != nil {
		runtime.UnlockOSThread()
		return nil, newErrorf("could not stop tracee: %v", err)
	}

	proc := &Process{
		pid:            pid,
		terminateOnEnd: false,
		isAttached:     true,
		state:          ProcessStopped,
	}

	if _, err := proc.WaitOnSignal(); err != nil {
		runtime.UnlockOSThread()
		return nil, err
	}

	proc.regs = &Registers{pid: proc.pid}
	if err := proc.regs.readAll(); err != nil {
		proc.Close()
		return nil, err
	}

	return proc, nil
}

// Pid returns the OS process ID.
func (p *Process) Pid() int {
	return p.pid
}

// State returns the current process state.
func (p *Process) State() ProcessState {
	return p.state
}

// Registers returns the cached register state. The cache is refreshed
// automatically by WaitOnSignal each time the process stops.
func (p *Process) Registers() *Registers {
	return p.regs
}

// GetPC returns the current program counter (instruction pointer).
func (p *Process) GetPC() (uint64, error) {
	info, ok := RegisterInfoByName("rip")
	if !ok {
		return 0, newError("rip register not found")
	}
	return p.regs.Read(info).(uint64), nil
}

// SetPC writes the program counter (instruction pointer).
func (p *Process) SetPC(addr uint64) error {
	info, ok := RegisterInfoByName("rip")
	if !ok {
		return newError("rip register not found")
	}
	return p.regs.Write(info, addr)
}

// CreateBreakpointSite creates a new (disabled) breakpoint site at the given
// address. The caller must call Enable() on the returned site to activate it.
// Returns an error if a breakpoint already exists at the address.
func (p *Process) CreateBreakpointSite(addr uint64) (*BreakpointSite, error) {
	if p.breakpointSites.containsAddress(addr) {
		return nil, newErrorf("breakpoint already exists at 0x%x", addr)
	}
	site := &BreakpointSite{
		id:      nextBreakpointID(),
		pid:     p.pid,
		address: addr,
	}
	p.breakpointSites.push(site)
	return site, nil
}

// BreakpointSites returns a snapshot of all breakpoint sites.
func (p *Process) BreakpointSites() []*BreakpointSite {
	result := make([]*BreakpointSite, p.breakpointSites.size())
	copy(result, p.breakpointSites.sites)
	return result
}

// BreakpointSiteByID returns the breakpoint site with the given ID.
func (p *Process) BreakpointSiteByID(id int32) (*BreakpointSite, bool) {
	return p.breakpointSites.getByID(id)
}

// RemoveBreakpointSite removes a breakpoint site by ID, disabling it first
// if it is enabled.
func (p *Process) RemoveBreakpointSite(id int32) error {
	site, ok := p.breakpointSites.getByID(id)
	if !ok {
		return newErrorf("breakpoint %d not found", id)
	}
	if site.isEnabled {
		if err := site.Disable(); err != nil {
			return err
		}
	}
	p.breakpointSites.removeByID(id)
	return nil
}

// stepOverBreakpoint handles the case where the process is stopped at an
// enabled breakpoint: disable it, single-step one instruction, re-enable it.
func (p *Process) stepOverBreakpoint() error {
	pc, err := p.GetPC()
	if err != nil {
		return err
	}
	site := p.breakpointSites.enabledAtAddress(pc)
	if site == nil {
		return nil
	}
	if err := site.Disable(); err != nil {
		return err
	}
	if err := ptraceSingleStep(p.pid); err != nil {
		// Re-enable on failure so state stays consistent.
		site.Enable()
		return newErrorf("single step failed: %v", err)
	}
	// Wait for the single-step stop.
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(p.pid, &ws, 0, nil); err != nil {
		site.Enable()
		return newErrorf("wait after single step failed: %v", err)
	}
	if err := site.Enable(); err != nil {
		return err
	}
	return nil
}

// StepInstruction executes a single machine instruction. If the process
// is stopped at an enabled breakpoint, the breakpoint is temporarily
// disabled for the step and then re-enabled.
func (p *Process) StepInstruction() (StopReason, error) {
	pc, err := p.GetPC()
	if err != nil {
		return StopReason{}, err
	}

	bpAtPC := p.breakpointSites.enabledAtAddress(pc)
	if bpAtPC != nil {
		if err := bpAtPC.Disable(); err != nil {
			return StopReason{}, err
		}
	}

	if err := ptraceSingleStep(p.pid); err != nil {
		if bpAtPC != nil {
			bpAtPC.Enable()
		}
		return StopReason{}, newErrorf("single step failed: %v", err)
	}
	p.state = ProcessRunning

	reason, err := p.WaitOnSignal()
	if err != nil {
		if bpAtPC != nil {
			bpAtPC.Enable()
		}
		return reason, err
	}

	if bpAtPC != nil {
		if err := bpAtPC.Enable(); err != nil {
			return reason, err
		}
	}

	return reason, nil
}

// Resume continues a stopped process. If stopped at an enabled breakpoint,
// it first steps over the breakpoint before continuing.
func (p *Process) Resume() error {
	if err := p.stepOverBreakpoint(); err != nil {
		return err
	}
	if err := ptraceCont(p.pid); err != nil {
		return newErrorf("could not resume: %v", err)
	}
	p.state = ProcessRunning
	return nil
}

// WaitOnSignal blocks until the process stops or terminates and returns
// the reason for stopping. When the process is stopped and we are
// attached, the register cache is refreshed automatically.
//
// If the stop is a SIGTRAP and an enabled breakpoint exists at PC-1,
// the program counter is adjusted back to the breakpoint address. This
// is necessary because x86-64 increments RIP past the 0xCC byte before
// delivering the trap.
func (p *Process) WaitOnSignal() (StopReason, error) {
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(p.pid, &ws, 0, nil); err != nil {
		return StopReason{}, newErrorf("waitpid failed: %v", err)
	}

	reason := newStopReason(ws)
	p.state = reason.Reason

	if p.isAttached && p.state == ProcessStopped && p.regs != nil {
		if err := p.regs.readAll(); err != nil {
			return reason, err
		}

		// If we stopped due to SIGTRAP and there is an enabled breakpoint
		// at PC-1, adjust the PC back to the breakpoint address.
		if reason.Info == uint8(syscall.SIGTRAP) {
			pc, err := p.GetPC()
			if err == nil {
				if site := p.breakpointSites.enabledAtAddress(pc - 1); site != nil {
					if err := p.SetPC(pc - 1); err != nil {
						return reason, err
					}
				}
			}
		}
	}

	return reason, nil
}

// ReadMemory reads `amount` bytes from the tracee's address space starting
// at `addr`. It uses process_vm_readv for efficient bulk reads.
func (p *Process) ReadMemory(addr uint64, amount int) ([]byte, error) {
	return processVMReadv(p.pid, addr, amount)
}

// WriteMemory writes `data` to the tracee's address space starting at
// `addr`. It uses PTRACE_POKEDATA one word at a time. Partial words at
// the start and end are handled via read-modify-write to preserve
// surrounding bytes.
func (p *Process) WriteMemory(addr uint64, data []byte) error {
	const wordSize = 8
	remaining := len(data)
	offset := 0

	for remaining > 0 {
		curAddr := addr + uint64(offset)
		aligned := curAddr & ^uint64(wordSize - 1)

		if curAddr != aligned || remaining < wordSize {
			// Partial word: read the existing word, patch our bytes in.
			existing, err := ptracePeekData(p.pid, aligned)
			if err != nil {
				return newErrorf("memory write: peek at 0x%x failed: %v", aligned, err)
			}
			var word [wordSize]byte
			binary.LittleEndian.PutUint64(word[:], existing)

			byteOff := int(curAddr - aligned)
			n := wordSize - byteOff
			if n > remaining {
				n = remaining
			}
			copy(word[byteOff:byteOff+n], data[offset:offset+n])

			val := binary.LittleEndian.Uint64(word[:])
			if err := ptracePokeData(p.pid, aligned, val); err != nil {
				return newErrorf("memory write: poke at 0x%x failed: %v", aligned, err)
			}
			offset += n
			remaining -= n
		} else {
			// Full aligned word.
			val := binary.LittleEndian.Uint64(data[offset : offset+wordSize])
			if err := ptracePokeData(p.pid, curAddr, val); err != nil {
				return newErrorf("memory write: poke at 0x%x failed: %v", curAddr, err)
			}
			offset += wordSize
			remaining -= wordSize
		}
	}
	return nil
}

// ReadMemoryWithoutTraps reads memory and patches out int3 (0xCC) bytes
// from any enabled breakpoints in the read region. This returns the
// "original" memory contents as they would appear without breakpoints,
// which is essential for disassembly.
func (p *Process) ReadMemoryWithoutTraps(addr uint64, amount int) ([]byte, error) {
	data, err := p.ReadMemory(addr, amount)
	if err != nil {
		return nil, err
	}

	// Find enabled breakpoints in [addr, addr+amount) and patch out 0xCC.
	for _, site := range p.breakpointSites.getInRegion(addr, addr+uint64(len(data))) {
		idx := site.address - addr
		data[idx] = site.savedData
	}
	return data, nil
}

// Close detaches from the process and optionally terminates it.
// It also releases the OS-thread pin established by Launch or Attach.
func (p *Process) Close() {
	if p.pid == 0 {
		return
	}

	if p.isAttached {
		if p.state == ProcessRunning {
			_ = syscall.Kill(p.pid, syscall.SIGSTOP)
			var ws syscall.WaitStatus
			syscall.Wait4(p.pid, &ws, 0, nil)
		}

		syscall.PtraceDetach(p.pid)
		_ = syscall.Kill(p.pid, syscall.SIGCONT)
	}

	if p.terminateOnEnd {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
		var ws syscall.WaitStatus
		syscall.Wait4(p.pid, &ws, 0, nil)
	}

	if p.isAttached {
		runtime.UnlockOSThread()
	}
}
