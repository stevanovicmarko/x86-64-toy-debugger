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
	watchpoints     watchpointCollection
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
//
// If hardware is true, the breakpoint will use a debug register (DR0–DR3)
// instead of int3 injection. Hardware breakpoints are invisible to the
// tracee's memory.
//
// If internal is true, the breakpoint is assigned ID -1 and is hidden from
// user-facing listings. Internal breakpoints are used by the debugger itself
// (e.g., for step-over logic).
func (p *Process) CreateBreakpointSite(addr uint64, hardware, internal bool) (*BreakpointSite, error) {
	if p.breakpointSites.containsAddress(addr) {
		return nil, newErrorf("breakpoint already exists at 0x%x", addr)
	}
	id := nextBreakpointID()
	if internal {
		id = -1
	}
	site := &BreakpointSite{
		id:                    id,
		pid:                   p.pid,
		proc:                  p,
		address:               addr,
		isHardware:            hardware,
		isInternal:            internal,
		hardwareRegisterIndex: -1,
	}
	p.breakpointSites.push(site)
	return site, nil
}

// BreakpointSites returns a snapshot of all user-visible breakpoint sites.
// Internal breakpoints (used by the debugger itself) are excluded.
func (p *Process) BreakpointSites() []*BreakpointSite {
	var result []*BreakpointSite
	for _, s := range p.breakpointSites.sites {
		if !s.isInternal {
			result = append(result, s)
		}
	}
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

		// If we stopped due to SIGTRAP and there is an enabled *software*
		// breakpoint at PC-1, adjust the PC back to the breakpoint address.
		// Hardware breakpoints stop AT the instruction (PC is already
		// correct), so we only adjust for software breakpoints.
		if reason.Info == uint8(syscall.SIGTRAP) {
			pc, err := p.GetPC()
			if err == nil {
				if site := p.breakpointSites.enabledSoftwareAtAddress(pc - 1); site != nil {
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

// ---------------------------------------------------------------------------
// Hardware stoppoints (debug registers)
// ---------------------------------------------------------------------------

// SetHardwareBreakpoint programs a free debug register to break on
// instruction execution at addr. It returns the debug register index
// (0–3) used, or an error if all slots are occupied.
func (p *Process) SetHardwareBreakpoint(addr uint64) (int, error) {
	return p.setHardwareStoppoint(addr, StoppointModeExecute, 1)
}

// ClearHardwareStoppoint clears the debug register at the given index
// (0–3) and disables its DR7 enable/condition bits.
func (p *Process) ClearHardwareStoppoint(index int) error {
	if index < 0 || index > 3 {
		return newErrorf("invalid debug register index %d", index)
	}

	// Read current DR7.
	dr7Info, _ := RegisterInfoByName("dr7")
	control := p.regs.Read(dr7Info).(uint64)

	// Clear the local enable bit for this register (bit 2*index).
	control &^= 1 << (2 * uint(index))

	// Clear the condition (RW) and length (LEN) bits (4 bits at 16+4*index).
	control &^= 0xF << (16 + 4*uint(index))

	if err := p.regs.Write(dr7Info, control); err != nil {
		return newErrorf("clear hardware stoppoint: write DR7 failed: %v", err)
	}

	// Clear the address register.
	drName := [4]string{"dr0", "dr1", "dr2", "dr3"}
	drInfo, _ := RegisterInfoByName(drName[index])
	if err := p.regs.Write(drInfo, uint64(0)); err != nil {
		return newErrorf("clear hardware stoppoint: write %s failed: %v", drName[index], err)
	}

	return nil
}

// setHardwareStoppoint is the core implementation for programming a
// debug register. It finds a free slot, writes the address to DR0–DR3,
// and configures DR7 with the mode and size.
func (p *Process) setHardwareStoppoint(addr uint64, mode StoppointMode, size int) (int, error) {
	// Read current DR7 control register.
	dr7Info, _ := RegisterInfoByName("dr7")
	control := p.regs.Read(dr7Info).(uint64)

	// Find a free debug register.
	index, err := findFreeStoppointRegister(control)
	if err != nil {
		return -1, err
	}

	// Encode mode and size.
	modeEnc := encodeHardwareStoppointMode(mode)
	sizeEnc, err := encodeHardwareStoppointSize(size)
	if err != nil {
		return -1, err
	}

	// Write the address to DR<index>.
	drNames := [4]string{"dr0", "dr1", "dr2", "dr3"}
	drInfo, _ := RegisterInfoByName(drNames[index])
	if err := p.regs.Write(drInfo, addr); err != nil {
		return -1, newErrorf("set hardware stoppoint: write %s failed: %v", drNames[index], err)
	}

	// Set the local enable bit (bit 2*index).
	control |= 1 << (2 * uint(index))

	// Set condition (RW) and length (LEN) in the upper half of DR7.
	// Bits 16+4*index..16+4*index+1 = condition (RW)
	// Bits 18+4*index..18+4*index+1 = length (LEN)
	shift := 16 + 4*uint(index)
	control &^= 0xF << shift // clear the 4 bits first
	control |= (modeEnc | (sizeEnc << 2)) << shift

	if err := p.regs.Write(dr7Info, control); err != nil {
		return -1, newErrorf("set hardware stoppoint: write DR7 failed: %v", err)
	}

	return index, nil
}

// findFreeStoppointRegister scans the DR7 control register for an
// unused debug register slot (0–3). A slot is free if its local enable
// bit is clear.
func findFreeStoppointRegister(control uint64) (int, error) {
	for i := 0; i < 4; i++ {
		if control&(1<<(2*uint(i))) == 0 {
			return i, nil
		}
	}
	return -1, newError("no free debug registers (all 4 hardware breakpoint slots are in use)")
}

// encodeHardwareStoppointMode maps a StoppointMode to the 2-bit DR7
// condition field encoding.
func encodeHardwareStoppointMode(mode StoppointMode) uint64 {
	switch mode {
	case StoppointModeExecute:
		return 0b00
	case StoppointModeWrite:
		return 0b01
	case StoppointModeReadWrite:
		return 0b11
	default:
		return 0b00
	}
}

// encodeHardwareStoppointSize maps a byte size (1, 2, 4, 8) to the
// 2-bit DR7 length field encoding.
func encodeHardwareStoppointSize(size int) (uint64, error) {
	switch size {
	case 1:
		return 0b00, nil
	case 2:
		return 0b01, nil
	case 4:
		return 0b11, nil
	case 8:
		return 0b10, nil
	default:
		return 0, newErrorf("unsupported hardware stoppoint size %d (must be 1, 2, 4, or 8)", size)
	}
}

// ---------------------------------------------------------------------------
// Watchpoints
// ---------------------------------------------------------------------------

// SetWatchpoint programs a debug register to trigger on data access at
// addr with the given mode and size. Returns the debug register index.
func (p *Process) SetWatchpoint(addr uint64, mode StoppointMode, size int) (int, error) {
	if mode == StoppointModeExecute {
		return -1, newError("use SetHardwareBreakpoint for execution watchpoints")
	}
	return p.setHardwareStoppoint(addr, mode, size)
}

// CreateWatchpoint creates a new watchpoint at the given address.
// The watchpoint is immediately enabled. The address must be aligned
// to the given size.
func (p *Process) CreateWatchpoint(addr uint64, mode StoppointMode, size int) (*Watchpoint, error) {
	if addr&uint64(size-1) != 0 {
		return nil, newErrorf("watchpoint address 0x%x is not %d-byte aligned", addr, size)
	}
	if p.watchpoints.containsAddress(addr) {
		return nil, newErrorf("watchpoint already exists at 0x%x", addr)
	}

	idx, err := p.SetWatchpoint(addr, mode, size)
	if err != nil {
		return nil, err
	}

	wp := &Watchpoint{
		id:                    nextWatchpointID(),
		proc:                  p,
		address:               addr,
		mode:                  mode,
		size:                  size,
		isEnabled:             true,
		hardwareRegisterIndex: idx,
	}
	p.watchpoints.push(wp)
	return wp, nil
}

// Watchpoints returns a snapshot of all watchpoints.
func (p *Process) Watchpoints() []*Watchpoint {
	result := make([]*Watchpoint, p.watchpoints.size())
	copy(result, p.watchpoints.points)
	return result
}

// WatchpointByID returns the watchpoint with the given ID.
func (p *Process) WatchpointByID(id int32) (*Watchpoint, bool) {
	return p.watchpoints.getByID(id)
}

// RemoveWatchpoint removes a watchpoint by ID, disabling it first
// if it is enabled.
func (p *Process) RemoveWatchpoint(id int32) error {
	wp, ok := p.watchpoints.getByID(id)
	if !ok {
		return newErrorf("watchpoint %d not found", id)
	}
	if wp.isEnabled {
		if err := wp.Disable(); err != nil {
			return err
		}
	}
	p.watchpoints.removeByID(id)
	return nil
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
