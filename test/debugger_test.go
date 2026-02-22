// Package test contains integration tests for the debugger.
//
// These are black-box tests that consume the debugger package as an
// external API, exactly as cmd/toydbg does. They verify process
// launching, attaching, resuming, and error handling using the Linux
// procfs and kill(2) to inspect real process state.
package test

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"x86-64-toy-debugger/debugger"
)

// targetDir holds the path to the compiled test target binaries.
// It is populated by TestMain before any tests run.
var targetDir string

func TestMain(m *testing.M) {
	// Build test target programs into a temp directory.
	var err error
	targetDir, err = os.MkdirTemp("", "toydbg-test-targets-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "create target dir: %v\n", err)
		os.Exit(1)
	}
	defer os.RemoveAll(targetDir)

	for _, target := range []string{"run_endlessly", "end_immediately"} {
		src := filepath.Join("targets", target)
		out := filepath.Join(targetDir, target)
		cmd := exec.Command("go", "build", "-o", out, "./"+src)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "build target %s: %v\n", target, err)
			os.Exit(1)
		}
	}

	os.Exit(m.Run())
}

// targetPath returns the absolute path to a compiled test target binary.
func targetPath(name string) string {
	return filepath.Join(targetDir, name)
}

// ---------------------------------------------------------------------------
// Test helpers
// ---------------------------------------------------------------------------

// processExists checks whether a process with the given PID is alive.
// It uses kill(pid, 0), which performs existence and permission checks
// without actually delivering a signal.
func processExists(pid int) bool {
	err := syscall.Kill(pid, 0)
	return err == nil || err != syscall.ESRCH
}

// getProcessStatus reads /proc/<pid>/stat and returns the single-character
// process state indicator (e.g. 'R' running, 'S' sleeping, 't' tracing stop).
//
// The stat file format is: <pid> (<comm>) <state> ...
// Because the executable name (comm) can contain spaces and unmatched
// parentheses, we locate the *last* ')' and read the character two
// positions to the right.
func getProcessStatus(pid int) (byte, error) {
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid))
	if err != nil {
		return 0, fmt.Errorf("read procfs stat: %w", err)
	}

	line := string(data)
	lastParen := strings.LastIndex(line, ")")
	if lastParen == -1 || lastParen+2 >= len(line) {
		return 0, fmt.Errorf("unexpected stat format: %q", line)
	}
	return line[lastParen+2], nil
}

// isDebuggerError reports whether err is a *debugger.Error.
func isDebuggerError(err error) bool {
	var dbgErr *debugger.Error
	return errors.As(err, &dbgErr)
}

// ---------------------------------------------------------------------------
// Launch tests
// ---------------------------------------------------------------------------

func TestLaunchSuccess(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	if !processExists(proc.Pid()) {
		t.Fatalf("expected process %d to exist", proc.Pid())
	}
}

func TestLaunchNoSuchProgram(t *testing.T) {
	// Go's os/exec propagates exec failures via an internal pipe,
	// so unlike the C++ version we don't need our own pipe wrapper.
	_, err := debugger.Launch("you_do_not_have_to_be_good")
	if err == nil {
		t.Fatal("expected an error for nonexistent program")
	}
	if !isDebuggerError(err) {
		t.Fatalf("expected *debugger.Error, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// Attach tests
// ---------------------------------------------------------------------------

func TestAttachSuccess(t *testing.T) {
	target, err := debugger.LaunchNoDebug(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("LaunchNoDebug failed: %v", err)
	}
	defer target.Close()

	proc, err := debugger.Attach(target.Pid())
	if err != nil {
		t.Fatalf("Attach failed: %v", err)
	}
	defer proc.Close()

	status, err := getProcessStatus(target.Pid())
	if err != nil {
		t.Fatalf("getProcessStatus: %v", err)
	}
	if status != 't' {
		t.Fatalf("expected process status 't' (tracing stop), got %q", status)
	}
}

func TestAttachInvalidPID(t *testing.T) {
	_, err := debugger.Attach(0)
	if err == nil {
		t.Fatal("expected an error for PID 0")
	}
	if !isDebuggerError(err) {
		t.Fatalf("expected *debugger.Error, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// Resume tests
// ---------------------------------------------------------------------------

func TestResumeAttached(t *testing.T) {
	target, err := debugger.LaunchNoDebug(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("LaunchNoDebug failed: %v", err)
	}
	defer target.Close()

	proc, err := debugger.Attach(target.Pid())
	if err != nil {
		t.Fatalf("Attach failed: %v", err)
	}
	defer proc.Close()

	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	// Give the kernel a moment to schedule the resumed process.
	time.Sleep(10 * time.Millisecond)

	status, err := getProcessStatus(proc.Pid())
	if err != nil {
		t.Fatalf("getProcessStatus: %v", err)
	}
	if status != 'R' && status != 'S' {
		t.Fatalf("expected status 'R' or 'S', got %q", status)
	}
}

func TestResumeLaunched(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	// Give the kernel a moment to schedule the resumed process.
	time.Sleep(10 * time.Millisecond)

	status, err := getProcessStatus(proc.Pid())
	if err != nil {
		t.Fatalf("getProcessStatus: %v", err)
	}
	if status != 'R' && status != 'S' {
		t.Fatalf("expected status 'R' or 'S', got %q", status)
	}
}

func TestResumeAlreadyTerminated(t *testing.T) {
	proc, err := debugger.Launch(targetPath("end_immediately"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	if err := proc.Resume(); err != nil {
		t.Fatalf("first Resume failed: %v", err)
	}

	if _, err := proc.WaitOnSignal(); err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}

	// Process has exited; Resume should fail.
	err = proc.Resume()
	if err == nil {
		t.Fatal("expected an error when resuming a terminated process")
	}
	if !isDebuggerError(err) {
		t.Fatalf("expected *debugger.Error, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// Register metadata tests
// ---------------------------------------------------------------------------

func TestRegisterInfoCount(t *testing.T) {
	// 25 64-bit GPRs + 16×32-bit + 16×16-bit + 4×high-8 + 16×low-8
	// + 8 FP control/status + 8 ST + 8 MM + 16 XMM + 8 DR = 125
	all := debugger.AllRegisterInfos()
	if got := len(all); got != 125 {
		t.Fatalf("expected 125 registers, got %d", got)
	}
}

func TestRegisterIDsMatchIndex(t *testing.T) {
	for i, info := range debugger.AllRegisterInfos() {
		if int(info.ID) != i {
			t.Errorf("register %q: ID=%d, want index %d", info.Name, info.ID, i)
		}
	}
}

func TestRegisterNamesUnique(t *testing.T) {
	seen := make(map[string]bool)
	for _, info := range debugger.AllRegisterInfos() {
		if seen[info.Name] {
			t.Errorf("duplicate register name: %q", info.Name)
		}
		seen[info.Name] = true
	}
}

func TestDwarfIDsUnique(t *testing.T) {
	seen := make(map[int]string)
	for _, info := range debugger.AllRegisterInfos() {
		if info.DwarfID < 0 {
			continue
		}
		if prev, ok := seen[info.DwarfID]; ok {
			t.Errorf("DWARF ID %d used by both %q and %q", info.DwarfID, prev, info.Name)
		}
		seen[info.DwarfID] = info.Name
	}
}

func TestLookupByName(t *testing.T) {
	for _, name := range []string{"rax", "rip", "eax", "ah", "al", "xmm0", "st0", "mm0", "dr0", "fcw"} {
		info, ok := debugger.RegisterInfoByName(name)
		if !ok {
			t.Errorf("RegisterInfoByName(%q) not found", name)
			continue
		}
		if info.Name != name {
			t.Errorf("RegisterInfoByName(%q).Name = %q", name, info.Name)
		}
	}
}

func TestLookupByDwarf(t *testing.T) {
	tests := []struct {
		dwarf int
		name  string
	}{
		{0, "rax"},
		{16, "rip"},
		{17, "xmm0"},
		{33, "st0"},
		{41, "mm0"},
		{64, "mxcsr"},
		{65, "fcw"},
	}
	for _, tt := range tests {
		info, ok := debugger.RegisterInfoByDwarf(tt.dwarf)
		if !ok {
			t.Errorf("RegisterInfoByDwarf(%d) not found", tt.dwarf)
			continue
		}
		if info.Name != tt.name {
			t.Errorf("RegisterInfoByDwarf(%d).Name = %q, want %q", tt.dwarf, info.Name, tt.name)
		}
	}
}

func TestLookupByDwarfNegativeReturnsNotFound(t *testing.T) {
	_, ok := debugger.RegisterInfoByDwarf(-1)
	if ok {
		t.Error("RegisterInfoByDwarf(-1) should return false")
	}
}

func TestRegisterOffsets(t *testing.T) {
	checks := []struct {
		name   string
		offset int
	}{
		{"r15", 0},
		{"rax", 80},
		{"rip", 128},
		{"rsp", 152},
		{"gs", 208},
		{"orig_rax", 120},
		{"fcw", 224},
		{"mxcsr", 248},
		{"st0", 256},
		{"mm0", 256},
		{"xmm0", 384},
		{"xmm15", 384 + 15*16},
		{"dr0", 848},
		{"dr7", 848 + 7*8},
		{"ah", 81},
		{"bh", 41},
		{"ch", 89},
		{"dh", 97},
	}
	for _, tt := range checks {
		info, ok := debugger.RegisterInfoByName(tt.name)
		if !ok {
			t.Errorf("register %q not found", tt.name)
			continue
		}
		if info.Offset != tt.offset {
			t.Errorf("register %q: offset=%d, want %d", tt.name, info.Offset, tt.offset)
		}
	}
}

// ---------------------------------------------------------------------------
// Register read/write tests
// ---------------------------------------------------------------------------

func TestReadRegistersAfterLaunch(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()
	if regs == nil {
		t.Fatal("Registers() returned nil")
	}

	// RIP should be non-zero — the process is stopped at its entry point.
	ripInfo, ok := debugger.RegisterInfoByName("rip")
	if !ok {
		t.Fatal("rip register not found")
	}
	val := regs.Read(ripInfo)
	rip, ok := val.(uint64)
	if !ok {
		t.Fatalf("rip value is %T, want uint64", val)
	}
	if rip == 0 {
		t.Fatal("rip is 0, expected non-zero after launch")
	}

	// RSP should also be non-zero (stack is set up by the kernel).
	rspInfo, _ := debugger.RegisterInfoByName("rsp")
	rsp := regs.Read(rspInfo).(uint64)
	if rsp == 0 {
		t.Fatal("rsp is 0, expected non-zero")
	}
}

func TestReadSubregisters(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()

	// Read the full 64-bit rax, then verify eax/ax/al are consistent.
	raxInfo, _ := debugger.RegisterInfoByName("rax")
	rax := regs.Read(raxInfo).(uint64)

	eaxInfo, _ := debugger.RegisterInfoByName("eax")
	eax := regs.Read(eaxInfo).(uint32)
	if eax != uint32(rax) {
		t.Errorf("eax=%#x, want low 32 of rax=%#x", eax, rax)
	}

	axInfo, _ := debugger.RegisterInfoByName("ax")
	ax := regs.Read(axInfo).(uint16)
	if ax != uint16(rax) {
		t.Errorf("ax=%#x, want low 16 of rax=%#x", ax, rax)
	}

	alInfo, _ := debugger.RegisterInfoByName("al")
	al := regs.Read(alInfo).(uint8)
	if al != uint8(rax) {
		t.Errorf("al=%#x, want low 8 of rax=%#x", al, rax)
	}
}

func TestWriteRegister(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()

	// Write a known value to rax and read it back.
	raxInfo, _ := debugger.RegisterInfoByName("rax")
	want := uint64(0xDEADBEEFCAFEBABE)
	if err := regs.Write(raxInfo, want); err != nil {
		t.Fatalf("Write rax failed: %v", err)
	}
	got := regs.Read(raxInfo).(uint64)
	if got != want {
		t.Errorf("rax after write: got %#x, want %#x", got, want)
	}
}

func TestReadRegistersAfterContinue(t *testing.T) {
	proc, err := debugger.Launch(targetPath("end_immediately"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	// Record RIP at launch.
	ripInfo, _ := debugger.RegisterInfoByName("rip")
	ripBefore := proc.Registers().Read(ripInfo).(uint64)

	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}

	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}

	// The process should have exited. Registers are only refreshed on
	// stop, not exit, so we just verify the flow didn't error.
	if reason.Reason != debugger.ProcessExited {
		// If it stopped instead of exiting, RIP should have changed.
		ripAfter := proc.Registers().Read(ripInfo).(uint64)
		if ripAfter == ripBefore {
			t.Error("rip did not change after continue")
		}
	}
}
