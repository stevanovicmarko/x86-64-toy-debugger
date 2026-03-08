package debugger

import (
	"encoding/binary"
	"fmt"
	"io"
	"math/bits"
	"os"
	"os/exec"
	"runtime"
	"strconv"
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

// TrapType distinguishes the specific cause of a SIGTRAP signal.
// On x86-64, SIGTRAP is overloaded: software breakpoints (int3),
// single steps (EFLAGS.TF), hardware breakpoints (#DB), and
// syscall-stops all deliver the same signal number.
type TrapType int

const (
	TrapSingleStep    TrapType = iota // PTRACE_SINGLESTEP completed
	TrapSoftwareBreak                 // Executed int3 (0xCC)
	TrapHardwareBreak                 // Debug register match (#DB)
	TrapSyscall                       // Syscall entry or exit
	TrapClone                         // Thread created via clone()
	TrapUnknown                       // Unrecognized si_code
)

// SyscallInformation describes a syscall stop (entry or exit).
type SyscallInformation struct {
	ID    uint16     // Syscall number (from orig_rax)
	Entry bool       // true = entry, false = exit
	Args  [6]uint64  // Arguments (valid on entry only)
	Ret   int64      // Return value (valid on exit only)
}

// StopReason holds the reason a process stopped and additional info
// such as the exit code or signal number.
type StopReason struct {
	Reason      ProcessState
	Info        uint8
	TID         int                     // Thread that caused this stop (0 for single-threaded compat)
	TrapReason  *TrapType               // non-nil only for SIGTRAP stops
	SyscallInfo *SyscallInformation     // non-nil only for syscall traps
}

// newStopReason parses a syscall.WaitStatus into a StopReason,
// associating it with the thread that caused the stop.
func newStopReason(tid int, ws syscall.WaitStatus) StopReason {
	// Check for ptrace clone event before standard parsing.
	// When PTRACE_O_TRACECLONE is active, clone events arrive as
	// status>>8 == (SIGTRAP | (PTRACE_EVENT_CLONE << 8)).
	if ws.Stopped() && (ws>>8) == syscall.WaitStatus(uint32(syscall.SIGTRAP)|(ptraceEventClone<<8)) {
		trap := TrapClone
		return StopReason{
			Reason:     ProcessStopped,
			Info:       uint8(syscall.SIGTRAP),
			TID:        tid,
			TrapReason: &trap,
		}
	}

	switch {
	case ws.Exited():
		return StopReason{Reason: ProcessExited, Info: uint8(ws.ExitStatus()), TID: tid}
	case ws.Signaled():
		return StopReason{Reason: ProcessTerminated, Info: uint8(ws.Signal()), TID: tid}
	case ws.Stopped():
		return StopReason{Reason: ProcessStopped, Info: uint8(ws.StopSignal()), TID: tid}
	default:
		return StopReason{Reason: ProcessStopped, Info: 0, TID: tid}
	}
}

// si_code constants from the Linux kernel headers.
const (
	siKernel   = 0x80 // SI_KERNEL — software breakpoint (int3)
	trapTrace  = 2    // TRAP_TRACE — single step
	trapHWBkpt = 4    // TRAP_HWBKPT — hardware breakpoint/watchpoint
)

// augmentStopReasonForThread enriches a SIGTRAP stop with the specific
// trap cause by inspecting the signal info and register state for a
// particular thread.
func (p *Process) augmentStopReasonForThread(tid int, reason *StopReason) {
	if reason.Reason != ProcessStopped {
		return
	}

	thread := p.threads[tid]
	regs := thread.Regs

	// Check for syscall-stop: PTRACE_O_TRACESYSGOOD sets bit 7 on the
	// stop signal, so syscall stops arrive as SIGTRAP|0x80 = 133.
	if reason.Info == uint8(syscall.SIGTRAP)|0x80 {
		trap := TrapSyscall
		reason.TrapReason = &trap
		reason.Info = uint8(syscall.SIGTRAP) // normalize for display

		info := &SyscallInformation{}
		info.Entry = !thread.ExpectingSyscallExit
		thread.ExpectingSyscallExit = !thread.ExpectingSyscallExit

		// Read syscall number from orig_rax.
		if origRaxInfo, ok := RegisterInfoByName("orig_rax"); ok {
			info.ID = uint16(regs.Read(origRaxInfo).(uint64))
		}

		if info.Entry {
			// Read the 6 syscall arguments from registers.
			argRegs := [6]string{"rdi", "rsi", "rdx", "r10", "r8", "r9"}
			for i, name := range argRegs {
				if ri, ok := RegisterInfoByName(name); ok {
					info.Args[i] = regs.Read(ri).(uint64)
				}
			}
		} else {
			// Read return value from rax.
			if raxInfo, ok := RegisterInfoByName("rax"); ok {
				info.Ret = int64(regs.Read(raxInfo).(uint64))
			}
		}
		reason.SyscallInfo = info
		return
	}

	if reason.Info != uint8(syscall.SIGTRAP) {
		return
	}

	// Use PTRACE_GETSIGINFO to determine the specific SIGTRAP cause.
	si, err := ptraceGetSigInfo(tid)
	if err != nil {
		trap := TrapUnknown
		reason.TrapReason = &trap
		return
	}

	var trap TrapType
	switch si.Code {
	case siKernel:
		trap = TrapSoftwareBreak
	case trapTrace:
		trap = TrapSingleStep
	case trapHWBkpt:
		trap = TrapHardwareBreak
	default:
		trap = TrapUnknown
	}
	reason.TrapReason = &trap
}

// SyscallCatchPolicy controls which syscalls cause the debugger to stop.
type SyscallCatchPolicy struct {
	mode    int   // 0=none, 1=some, 2=all
	toCatch []int // only used when mode=1
}

// CatchNoSyscalls returns a policy that does not stop on any syscalls.
func CatchNoSyscalls() SyscallCatchPolicy {
	return SyscallCatchPolicy{mode: 0}
}

// CatchAllSyscalls returns a policy that stops on every syscall entry/exit.
func CatchAllSyscalls() SyscallCatchPolicy {
	return SyscallCatchPolicy{mode: 2}
}

// CatchSomeSyscalls returns a policy that stops only on the listed syscalls.
func CatchSomeSyscalls(ids []int) SyscallCatchPolicy {
	return SyscallCatchPolicy{mode: 1, toCatch: ids}
}

// IsNone returns true if no syscalls are being caught.
func (s SyscallCatchPolicy) IsNone() bool { return s.mode == 0 }

// contains reports whether the given syscall ID is in the catch list.
func (s SyscallCatchPolicy) contains(id int) bool {
	for _, c := range s.toCatch {
		if c == id {
			return true
		}
	}
	return false
}

// ---------------------------------------------------------------------------
// Thread State
// ---------------------------------------------------------------------------

// ThreadState holds the per-thread state for a traced thread.
// Each thread in a process has its own registers, stop reason,
// and execution state.
type ThreadState struct {
	TID                  int
	Regs                 *Registers
	State                ProcessState
	Reason               StopReason
	PendingSigstop       bool
	ExpectingSyscallExit bool
}

// Process represents a running process being debugged.
// Users must create one via Launch or Attach; direct construction is not possible
// because all fields are unexported.
type Process struct {
	pid                     int
	terminateOnEnd          bool
	isAttached              bool
	state                   ProcessState
	threads                 map[int]*ThreadState
	currentThread           int
	breakpointSites         breakpointSiteCollection
	watchpoints             watchpointCollection
	syscallCatchPolicy      SyscallCatchPolicy
	threadLifecycleCallback func(StopReason)
	// target is set by Target after construction so that handleSignal
	// can notify the target of thread lifecycle events.
	target *Target
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
			Ptrace:  true,
			Setpgid: true,
		}
	}

	if err := cmd.Start(); err != nil {
		if debug {
			runtime.UnlockOSThread()
		}
		return nil, newErrorf("exec failed: %v", err)
	}

	pid := cmd.Process.Pid
	proc := &Process{
		pid:            pid,
		terminateOnEnd: true,
		isAttached:     debug,
		state:          ProcessStopped,
		threads:        make(map[int]*ThreadState),
		currentThread:  pid,
	}

	if debug {
		// Wait for the initial SIGTRAP from exec.
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
			runtime.UnlockOSThread()
			return nil, newErrorf("waitpid failed: %v", err)
		}

		// Set ptrace options: TRACESYSGOOD for syscall identification.
		// TRACECLONE is added later by EnableThreadTracking() when
		// the caller wants thread discovery (via Target).
		if err := ptraceSetOptions(pid, ptraceOTraceSysGood); err != nil {
			proc.Close()
			return nil, newErrorf("set ptrace options: %v", err)
		}

		// Create the main thread state.
		mainThread := &ThreadState{
			TID:   pid,
			Regs:  &Registers{pid: pid},
			State: ProcessStopped,
		}
		if err := mainThread.Regs.readAll(); err != nil {
			proc.Close()
			return nil, err
		}
		proc.threads[pid] = mainThread
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
		threads:        make(map[int]*ThreadState),
		currentThread:  pid,
	}

	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(pid, &ws, 0, nil); err != nil {
		runtime.UnlockOSThread()
		return nil, newErrorf("waitpid failed: %v", err)
	}

	if err := ptraceSetOptions(pid, ptraceOTraceSysGood); err != nil {
		proc.Close()
		return nil, newErrorf("set ptrace options: %v", err)
	}

	// Create the main thread state.
	mainThread := &ThreadState{
		TID:   pid,
		Regs:  &Registers{pid: pid},
		State: ProcessStopped,
	}
	if err := mainThread.Regs.readAll(); err != nil {
		proc.Close()
		return nil, err
	}
	proc.threads[pid] = mainThread

	// Discover any existing threads from /proc/<pid>/task.
	proc.populateExistingThreads()

	return proc, nil
}

// populateExistingThreads reads /proc/<pid>/task to discover threads
// that already exist when attaching to a running process.
func (p *Process) populateExistingThreads() {
	taskDir := fmt.Sprintf("/proc/%d/task", p.pid)
	entries, err := os.ReadDir(taskDir)
	if err != nil {
		return
	}
	for _, entry := range entries {
		tid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if _, exists := p.threads[tid]; exists {
			continue // already tracked (main thread)
		}
		// Seize and interrupt the additional thread.
		if err := ptraceSeizeProcess(tid); err != nil {
			continue
		}
		if err := ptraceInterruptProcess(tid); err != nil {
			continue
		}
		var ws syscall.WaitStatus
		if _, err := syscall.Wait4(tid, &ws, 0, nil); err != nil {
			continue
		}
		ptraceSetOptions(tid, ptraceOTraceSysGood)

		thread := &ThreadState{
			TID:   tid,
			Regs:  &Registers{pid: tid},
			State: ProcessStopped,
		}
		thread.Regs.readAll()
		p.threads[tid] = thread
	}
}

// Pid returns the OS process ID.
func (p *Process) Pid() int {
	return p.pid
}

// State returns the current process state.
func (p *Process) State() ProcessState {
	return p.state
}

// CurrentThread returns the TID of the currently active thread.
func (p *Process) CurrentThread() int {
	return p.currentThread
}

// SetCurrentThread changes which thread is considered "current" for
// operations like register reads, stepping, etc.
func (p *Process) SetCurrentThread(tid int) {
	if _, ok := p.threads[tid]; ok {
		p.currentThread = tid
	}
}

// ThreadStates returns the map of all tracked thread states.
func (p *Process) ThreadStates() map[int]*ThreadState {
	return p.threads
}

// Registers returns the cached register state for the current thread.
// The cache is refreshed automatically by WaitOnSignal each time the
// process stops.
func (p *Process) Registers() *Registers {
	if ts, ok := p.threads[p.currentThread]; ok {
		return ts.Regs
	}
	return nil
}

// GetPC returns the current program counter (instruction pointer)
// for the current thread.
func (p *Process) GetPC() (uint64, error) {
	regs := p.Registers()
	if regs == nil {
		return 0, newError("registers not available")
	}
	info, ok := RegisterInfoByName("rip")
	if !ok {
		return 0, newError("rip register not found")
	}
	return regs.Read(info).(uint64), nil
}

// SetPC writes the program counter (instruction pointer) for the
// current thread.
func (p *Process) SetPC(addr uint64) error {
	regs := p.Registers()
	if regs == nil {
		return newError("registers not available")
	}
	info, ok := RegisterInfoByName("rip")
	if !ok {
		return newError("rip register not found")
	}
	return regs.Write(info, addr)
}

// getPCForThread returns the PC for a specific thread.
func (p *Process) getPCForThread(tid int) (uint64, error) {
	thread, ok := p.threads[tid]
	if !ok {
		return 0, newErrorf("thread %d not found", tid)
	}
	info, ok := RegisterInfoByName("rip")
	if !ok {
		return 0, newError("rip register not found")
	}
	return thread.Regs.Read(info).(uint64), nil
}

// setPCForThread writes the PC for a specific thread.
func (p *Process) setPCForThread(tid int, addr uint64) error {
	thread, ok := p.threads[tid]
	if !ok {
		return newErrorf("thread %d not found", tid)
	}
	info, ok := RegisterInfoByName("rip")
	if !ok {
		return newError("rip register not found")
	}
	return thread.Regs.Write(info, addr)
}

// EnableThreadTracking enables PTRACE_O_TRACECLONE on all tracked
// threads so the kernel notifies us when clone() creates new threads.
// This is called by Target after construction; bare Process objects
// don't track thread creation to avoid interfering with simple usage
// patterns (e.g., Resume without WaitOnSignal).
func (p *Process) EnableThreadTracking() error {
	for tid := range p.threads {
		if err := ptraceSetOptions(tid, ptraceOTraceSysGood|ptraceOTraceClone); err != nil {
			return newErrorf("enable thread tracking on tid %d: %v", tid, err)
		}
	}
	return nil
}

// InstallThreadLifecycleCallback sets a callback that is invoked
// whenever a thread is created or exits. The stop reason describes
// the event: ProcessStopped = created, ProcessExited/Terminated = ended.
func (p *Process) InstallThreadLifecycleCallback(callback func(StopReason)) {
	p.threadLifecycleCallback = callback
}

// reportThreadLifecycle notifies both the callback and the target
// about a thread lifecycle event (creation or exit).
func (p *Process) reportThreadLifecycle(reason StopReason) {
	if p.threadLifecycleCallback != nil {
		p.threadLifecycleCallback(reason)
	}
	if p.target != nil {
		p.target.notifyThreadLifecycle(reason)
	}
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

// ---------------------------------------------------------------------------
// Stepping and resuming
// ---------------------------------------------------------------------------

// stepOverBreakpointForThread handles the case where a thread is stopped at
// an enabled breakpoint: disable it, single-step one instruction, re-enable.
func (p *Process) stepOverBreakpointForThread(tid int) error {
	pc, err := p.getPCForThread(tid)
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
	p.swallowPendingSigstop(tid)
	if err := ptraceSingleStep(tid); err != nil {
		site.Enable()
		return newErrorf("single step failed: %v", err)
	}
	// Wait for the single-step stop.
	var ws syscall.WaitStatus
	if _, err := syscall.Wait4(tid, &ws, wall, nil); err != nil {
		site.Enable()
		return newErrorf("wait after single step failed: %v", err)
	}
	if err := site.Enable(); err != nil {
		return err
	}
	return nil
}

// sendContinue issues the appropriate continue ptrace request for a thread.
func (p *Process) sendContinue(tid int) error {
	if !p.syscallCatchPolicy.IsNone() {
		if err := ptraceSyscall(tid); err != nil {
			return newErrorf("could not resume thread %d (syscall): %v", tid, err)
		}
	} else {
		if err := ptraceCont(tid); err != nil {
			return newErrorf("could not resume thread %d: %v", tid, err)
		}
	}
	if thread, ok := p.threads[tid]; ok {
		thread.State = ProcessRunning
	}
	p.state = ProcessRunning
	return nil
}

// StepInstruction executes a single machine instruction on the current
// thread. If the thread is stopped at an enabled breakpoint, the
// breakpoint is temporarily disabled for the step and then re-enabled.
func (p *Process) StepInstruction() (StopReason, error) {
	tid := p.currentThread

	pc, err := p.getPCForThread(tid)
	if err != nil {
		return StopReason{}, err
	}

	bpAtPC := p.breakpointSites.enabledAtAddress(pc)
	if bpAtPC != nil {
		if err := bpAtPC.Disable(); err != nil {
			return StopReason{}, err
		}
	}

	p.swallowPendingSigstop(tid)
	if err := ptraceSingleStep(tid); err != nil {
		if bpAtPC != nil {
			bpAtPC.Enable()
		}
		return StopReason{}, newErrorf("single step failed: %v", err)
	}
	if thread, ok := p.threads[tid]; ok {
		thread.State = ProcessRunning
	}
	p.state = ProcessRunning

	reason, err := p.waitOnSignalFor(tid)
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

// SetSyscallCatchPolicy configures which syscalls cause the debugger to
// stop. When a non-"none" policy is active, Resume uses PTRACE_SYSCALL
// instead of PTRACE_CONT so the kernel stops the tracee at syscall
// boundaries.
func (p *Process) SetSyscallCatchPolicy(policy SyscallCatchPolicy) {
	p.syscallCatchPolicy = policy
	// Reset syscall exit tracking for all threads.
	for _, ts := range p.threads {
		ts.ExpectingSyscallExit = false
	}
}

// Resume continues the current thread. If stopped at an enabled
// breakpoint, it first steps over the breakpoint before continuing.
//
// When a syscall catch policy is active, PTRACE_SYSCALL is used instead
// of PTRACE_CONT so the tracee stops at each syscall entry/exit.
func (p *Process) Resume() error {
	tid := p.currentThread
	if err := p.stepOverBreakpointForThread(tid); err != nil {
		return err
	}
	return p.sendContinue(tid)
}

// ResumeAllThreads resumes all stopped threads. Each thread steps over
// any breakpoint it's sitting on before all are continued. The two-pass
// approach prevents a race where one thread runs past a breakpoint while
// it's disabled for another thread's step-over.
func (p *Process) ResumeAllThreads() error {
	// Pass 1: step over breakpoints for all stopped threads.
	for tid, thread := range p.threads {
		if thread.State == ProcessStopped {
			if err := p.stepOverBreakpointForThread(tid); err != nil {
				return err
			}
		}
	}
	// Pass 2: send continue to all stopped threads.
	for tid, thread := range p.threads {
		if thread.State == ProcessStopped {
			if err := p.sendContinue(tid); err != nil {
				return err
			}
		}
	}
	return nil
}

// shouldResumeFromSyscall checks whether a syscall stop should be
// reported to the user. Returns true if the thread should be resumed.
func (p *Process) shouldResumeFromSyscall(reason StopReason) bool {
	if reason.SyscallInfo == nil {
		return false
	}
	id := int(reason.SyscallInfo.ID)
	if p.syscallCatchPolicy.mode == 2 {
		return false // catch all → report
	}
	if p.syscallCatchPolicy.mode == 1 && p.syscallCatchPolicy.contains(id) {
		return false // this one is in the list → report
	}
	return true // not interested → resume
}

// ---------------------------------------------------------------------------
// Multithreaded signal handling
// ---------------------------------------------------------------------------

// WaitOnSignal blocks until any thread in the process stops or terminates
// and returns the reason for stopping. In all-stop mode, when a
// reportable stop occurs, all other running threads are also stopped
// before returning control to the caller.
func (p *Process) WaitOnSignal() (StopReason, error) {
	return p.waitOnSignalFor(-1)
}

// waitOnSignalFor blocks until a specific thread (or any thread if
// toAwait is -1) stops. This is the core of the multithreaded wait
// logic.
func (p *Process) waitOnSignalFor(toAwait int) (StopReason, error) {
	var ws syscall.WaitStatus
	options := wall
	tid, err := syscall.Wait4(toAwait, &ws, options, nil)
	if err != nil {
		return StopReason{}, newErrorf("waitpid failed: %v", err)
	}

	reason := newStopReason(tid, ws)

	// handleSignal may return nil, meaning the thread should be
	// resumed and we should wait again.
	finalReason := p.handleSignal(reason, true)
	if finalReason == nil {
		p.sendContinue(tid)
		return p.waitOnSignalFor(toAwait)
	}

	reason = *finalReason

	// Update the thread's state.
	if thread, ok := p.threads[tid]; ok {
		thread.Reason = reason
		thread.State = reason.Reason
	}

	// If the thread exited or was terminated:
	if reason.Reason == ProcessExited || reason.Reason == ProcessTerminated {
		p.reportThreadLifecycle(reason)
		if tid == p.pid {
			// Main thread exited → process is done.
			p.state = reason.Reason
			return reason, nil
		}
		// Non-main thread exited → remove it and keep waiting.
		delete(p.threads, tid)
		return p.waitOnSignalFor(-1)
	}

	// We have a signal to report. Stop all other running threads
	// (all-stop mode).
	p.stopRunningThreads(tid)

	// Clean up any threads that exited during the stopping process.
	if mainExitReason := p.cleanupExitedThreads(tid); mainExitReason != nil {
		reason = *mainExitReason
	}

	p.state = reason.Reason
	p.currentThread = tid
	return reason, nil
}

// handleSignal processes a stop event and determines whether it should
// be reported to the user. Returns nil if the thread should be resumed
// (e.g., clone events, new thread SIGSTOPs, filtered syscalls).
func (p *Process) handleSignal(reason StopReason, isMainStop bool) *StopReason {
	tid := reason.TID

	// Clone event from parent thread → don't report, just resume.
	if reason.TrapReason != nil && *reason.TrapReason == TrapClone && isMainStop {
		return nil
	}

	// Check if this is a signal from a thread we don't know about yet.
	if p.isAttached && reason.Reason == ProcessStopped {
		if _, exists := p.threads[tid]; !exists {
			// New thread discovered! Create state for it.
			thread := &ThreadState{
				TID:   tid,
				Regs:  &Registers{pid: tid},
				State: ProcessStopped,
			}
			p.threads[tid] = thread

			// Set ptrace options on the new thread.
			ptraceSetOptions(tid, ptraceOTraceSysGood|ptraceOTraceClone)

			p.reportThreadLifecycle(reason)
			if isMainStop {
				return nil // Don't report thread creation as a stop
			}
		}

		// Check for pending SIGSTOP consumption.
		if thread, ok := p.threads[tid]; ok {
			if thread.PendingSigstop && reason.Info == uint8(syscall.SIGSTOP) {
				thread.PendingSigstop = false
				return nil
			}
		}
	}

	// For stopped threads that we're tracking, read registers and
	// augment the stop reason.
	if reason.Reason == ProcessStopped && p.isAttached {
		if thread, ok := p.threads[tid]; ok && thread.Regs != nil {
			thread.Regs.readAll()
			p.augmentStopReasonForThread(tid, &reason)

			if reason.TrapReason != nil {
				switch *reason.TrapReason {
				case TrapSoftwareBreak:
					pc, err := p.getPCForThread(tid)
					if err == nil {
						if site := p.breakpointSites.enabledSoftwareAtAddress(pc - 1); site != nil {
							p.setPCForThread(tid, pc-1)
							if site.notifyHit() && isMainStop {
								return nil // hit handler says resume
							}
						}
					}

				case TrapHardwareBreak:
					regs := thread.Regs
					dr6Info, _ := RegisterInfoByName("dr6")
					dr6 := regs.Read(dr6Info).(uint64)
					triggeredBits := dr6 & 0xF
					if triggeredBits != 0 {
						index := bits.TrailingZeros64(triggeredBits)
						drNames := [4]string{"dr0", "dr1", "dr2", "dr3"}
						drInfo, _ := RegisterInfoByName(drNames[index])
						addr := regs.Read(drInfo).(uint64)
						for _, wp := range p.watchpoints.points {
							if wp.address == addr && wp.hardwareRegisterIndex == index {
								wp.UpdateData()
								break
							}
						}
					}

				case TrapSyscall:
					if isMainStop && p.shouldResumeFromSyscall(reason) {
						return nil
					}
				}
			}
		}
	}

	return &reason
}

// stopRunningThreads sends SIGSTOP to all running threads except the
// one that caused the original stop, waits for each to stop, and
// records their states.
func (p *Process) stopRunningThreads(excludeTID int) {
	for tid, thread := range p.threads {
		if tid == excludeTID {
			continue
		}
		if thread.State != ProcessRunning {
			continue
		}

		// Send SIGSTOP unless one is already pending.
		if !thread.PendingSigstop {
			tgkill(p.pid, tid, syscall.SIGSTOP)
		}

		var ws syscall.WaitStatus
		waitedTid, err := syscall.Wait4(tid, &ws, wall, nil)
		if err != nil {
			continue
		}

		threadReason := newStopReason(waitedTid, ws)

		if threadReason.Reason == ProcessStopped {
			if threadReason.Info != uint8(syscall.SIGSTOP) {
				// We intercepted a real signal, not our SIGSTOP.
				// There's now a pending SIGSTOP queued for this thread.
				thread.PendingSigstop = true
			} else if thread.PendingSigstop {
				// This was the pending SIGSTOP we expected.
				thread.PendingSigstop = false
			}
		}

		// Process the signal through handleSignal (non-main stop).
		augmented := p.handleSignal(threadReason, false)
		if augmented == nil {
			augmented = &threadReason
		}

		thread.Reason = *augmented
		thread.State = augmented.Reason
	}
}

// cleanupExitedThreads removes threads that exited during the stopping
// process. Returns a non-nil StopReason if the main thread exited.
func (p *Process) cleanupExitedThreads(mainStopTID int) *StopReason {
	var toRemove []int
	var mainExitReason *StopReason

	for tid, thread := range p.threads {
		if tid == mainStopTID {
			continue
		}
		if thread.State == ProcessExited || thread.State == ProcessTerminated {
			p.reportThreadLifecycle(thread.Reason)
			toRemove = append(toRemove, tid)
			if tid == p.pid {
				reason := thread.Reason
				mainExitReason = &reason
			}
		}
	}

	for _, tid := range toRemove {
		delete(p.threads, tid)
	}

	return mainExitReason
}

// swallowPendingSigstop consumes a pending SIGSTOP on a thread before
// single-stepping it. Without this, PTRACE_SINGLESTEP would immediately
// stop due to the queued SIGSTOP.
func (p *Process) swallowPendingSigstop(tid int) {
	thread, ok := p.threads[tid]
	if !ok || !thread.PendingSigstop {
		return
	}
	ptraceCont(tid)
	var ws syscall.WaitStatus
	syscall.Wait4(tid, &ws, wall, nil)
	thread.PendingSigstop = false
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
// (0–3) and disables its DR7 enable/condition bits. In multithreaded
// mode, the change is replicated to all threads.
func (p *Process) ClearHardwareStoppoint(index int) error {
	if index < 0 || index > 3 {
		return newErrorf("invalid debug register index %d", index)
	}

	regs := p.Registers()
	if regs == nil {
		return newError("registers not available")
	}

	// Read current DR7.
	dr7Info, _ := RegisterInfoByName("dr7")
	control := regs.Read(dr7Info).(uint64)

	// Clear the local enable bit for this register (bit 2*index).
	control &^= 1 << (2 * uint(index))

	// Clear the condition (RW) and length (LEN) bits (4 bits at 16+4*index).
	control &^= 0xF << (16 + 4*uint(index))

	if err := regs.Write(dr7Info, control); err != nil {
		return newErrorf("clear hardware stoppoint: write DR7 failed: %v", err)
	}

	// Clear the address register.
	drName := [4]string{"dr0", "dr1", "dr2", "dr3"}
	drInfo, _ := RegisterInfoByName(drName[index])
	if err := regs.Write(drInfo, uint64(0)); err != nil {
		return newErrorf("clear hardware stoppoint: write %s failed: %v", drName[index], err)
	}

	// Replicate to all other threads.
	for tid, thread := range p.threads {
		if tid == p.currentThread {
			continue
		}
		thread.Regs.Write(drInfo, uint64(0))
		thread.Regs.Write(dr7Info, control)
	}

	return nil
}

// HardwareStoppoint identifies which hardware stoppoint (breakpoint or
// watchpoint) triggered a #DB exception.
type HardwareStoppoint struct {
	IsWatchpoint bool
	BreakpointID int32 // valid when !IsWatchpoint
	WatchpointID int32 // valid when IsWatchpoint
}

// GetCurrentHardwareStoppoint reads DR6 to determine which debug register
// triggered the most recent #DB exception, then looks up the address in
// the breakpoint/watchpoint collections to identify the stoppoint.
//
// DR6 status bits: bit 0 = DR0 triggered, bit 1 = DR1, etc.
// We use TrailingZeros to find the lowest set bit (Go equivalent of
// __builtin_ctzll from the C++ implementation).
func (p *Process) GetCurrentHardwareStoppoint() (HardwareStoppoint, error) {
	regs := p.Registers()
	if regs == nil {
		return HardwareStoppoint{}, newError("registers not available")
	}

	dr6Info, _ := RegisterInfoByName("dr6")
	dr6 := regs.Read(dr6Info).(uint64)

	// Only the low 4 bits of DR6 indicate which register triggered.
	triggeredBits := dr6 & 0xF
	if triggeredBits == 0 {
		return HardwareStoppoint{}, newError("no hardware stoppoint triggered (DR6 low bits are clear)")
	}

	index := bits.TrailingZeros64(triggeredBits)
	drNames := [4]string{"dr0", "dr1", "dr2", "dr3"}
	drInfo, _ := RegisterInfoByName(drNames[index])
	addr := regs.Read(drInfo).(uint64)

	// Check watchpoints first (they share debug registers with HW breakpoints).
	for _, wp := range p.watchpoints.points {
		if wp.address == addr && wp.hardwareRegisterIndex == index {
			return HardwareStoppoint{IsWatchpoint: true, WatchpointID: wp.id}, nil
		}
	}

	// Check hardware breakpoints.
	for _, bp := range p.breakpointSites.sites {
		if bp.isHardware && bp.address == addr && bp.hardwareRegisterIndex == index {
			return HardwareStoppoint{IsWatchpoint: false, BreakpointID: bp.id}, nil
		}
	}

	return HardwareStoppoint{}, newErrorf("hardware stoppoint at DR%d (0x%x) not found in collections", index, addr)
}

// setHardwareStoppoint is the core implementation for programming a
// debug register. It finds a free slot, writes the address to DR0–DR3,
// and configures DR7 with the mode and size. In multithreaded mode,
// the change is replicated to all threads.
func (p *Process) setHardwareStoppoint(addr uint64, mode StoppointMode, size int) (int, error) {
	regs := p.Registers()
	if regs == nil {
		return -1, newError("registers not available")
	}

	// Read current DR7 control register.
	dr7Info, _ := RegisterInfoByName("dr7")
	control := regs.Read(dr7Info).(uint64)

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
	if err := regs.Write(drInfo, addr); err != nil {
		return -1, newErrorf("set hardware stoppoint: write %s failed: %v", drNames[index], err)
	}

	// Set the local enable bit (bit 2*index).
	control |= 1 << (2 * uint(index))

	// Set condition (RW) and length (LEN) in the upper half of DR7.
	shift := 16 + 4*uint(index)
	control &^= 0xF << shift // clear the 4 bits first
	control |= (modeEnc | (sizeEnc << 2)) << shift

	if err := regs.Write(dr7Info, control); err != nil {
		return -1, newErrorf("set hardware stoppoint: write DR7 failed: %v", err)
	}

	// Replicate to all other threads.
	for tid, thread := range p.threads {
		if tid == p.currentThread {
			continue
		}
		thread.Regs.Write(drInfo, addr)
		thread.Regs.Write(dr7Info, control)
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
	// Capture the initial value at the watched address.
	if err := wp.UpdateData(); err != nil {
		return wp, nil // non-fatal: watchpoint is still usable
	}
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
		// Stop any running threads before detaching.
		for tid, thread := range p.threads {
			if thread.State == ProcessRunning {
				_ = tgkill(p.pid, tid, syscall.SIGSTOP)
				var ws syscall.WaitStatus
				syscall.Wait4(tid, &ws, wall, nil)
			}
		}

		// Detach from all threads.
		for tid := range p.threads {
			syscall.PtraceDetach(tid)
		}
		_ = syscall.Kill(p.pid, syscall.SIGCONT)
	}

	if p.terminateOnEnd {
		_ = syscall.Kill(p.pid, syscall.SIGKILL)
		// Reap all threads.
		for {
			var ws syscall.WaitStatus
			pid, err := syscall.Wait4(-1, &ws, wall|syscall.WNOHANG, nil)
			if err != nil || pid <= 0 {
				break
			}
		}
	}

	if p.isAttached {
		runtime.UnlockOSThread()
	}
}

