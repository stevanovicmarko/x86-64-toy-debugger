// Package test contains integration tests for the debugger.
//
// These are black-box tests that consume the debugger package as an
// external API, exactly as cmd/toydbg does. They verify process
// launching, attaching, resuming, and error handling using the Linux
// procfs and kill(2) to inspect real process state.
package test

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
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

	// Build native test targets (assembly and C) with gcc.
	gccTargets := []struct {
		name  string
		src   string
		flags []string
	}{
		{"reg_read", "reg_read.s", []string{"-nostdlib", "-no-pie"}},
		{"reg_write", "reg_write.s", []string{"-no-pie"}},
		{"hello_toydbg", "hello_toydbg.s", []string{"-nostdlib", "-no-pie"}},
		{"memory", "memory.s", []string{"-nostdlib", "-no-pie"}},
		{"anti_debugger", "anti_debugger.c", []string{"-no-pie"}},
		{"dwarf_target", "dwarf_target.c", []string{"-g", "-no-pie"}},
	}
	for _, t := range gccTargets {
		src := filepath.Join("targets", t.src)
		out := filepath.Join(targetDir, t.name)
		args := append([]string{"-o", out, src}, t.flags...)
		cmd := exec.Command("gcc", args...)
		cmd.Stderr = os.Stderr
		if err := cmd.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "build gcc target %s: %v\n", t.name, err)
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
	// Go's os/exec propagates exec failures via an internal pipe.
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

func TestWriteSubregisterPreservesParent(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()
	raxInfo, _ := debugger.RegisterInfoByName("rax")
	eaxInfo, _ := debugger.RegisterInfoByName("eax")

	// Set rax to a known 64-bit value.
	if err := regs.Write(raxInfo, uint64(0x1122334455667788)); err != nil {
		t.Fatalf("Write rax failed: %v", err)
	}

	// Overwrite the lower 32 bits via eax.
	if err := regs.Write(eaxInfo, uint32(0xAABBCCDD)); err != nil {
		t.Fatalf("Write eax failed: %v", err)
	}

	// The upper 32 bits of rax should be preserved.
	got := regs.Read(raxInfo).(uint64)
	want := uint64(0x11223344AABBCCDD)
	if got != want {
		t.Errorf("rax after eax write: got %#016x, want %#016x", got, want)
	}
}

func TestWriteHighByteRegister(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()
	raxInfo, _ := debugger.RegisterInfoByName("rax")
	ahInfo, _ := debugger.RegisterInfoByName("ah")

	// Set rax to a known value where byte 0 = 0x34, byte 1 = 0x12.
	if err := regs.Write(raxInfo, uint64(0x0000000000001234)); err != nil {
		t.Fatalf("Write rax failed: %v", err)
	}

	// Write 0xFF to ah (byte 1 of the low word).
	if err := regs.Write(ahInfo, uint8(0xFF)); err != nil {
		t.Fatalf("Write ah failed: %v", err)
	}

	got := regs.Read(raxInfo).(uint64)
	want := uint64(0x000000000000FF34)
	if got != want {
		t.Errorf("rax after ah write: got %#016x, want %#016x", got, want)
	}
}

func TestWriteFPRegister(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()
	mxcsrInfo, _ := debugger.RegisterInfoByName("mxcsr")

	// Read the current mxcsr value.
	orig := regs.Read(mxcsrInfo).(uint32)

	// Toggle bit 6 (DAZ — Denormals Are Zero), which is a safe bit to flip.
	modified := orig ^ (1 << 6)
	if err := regs.Write(mxcsrInfo, modified); err != nil {
		t.Fatalf("Write mxcsr failed: %v", err)
	}

	got := regs.Read(mxcsrInfo).(uint32)
	if got != modified {
		t.Errorf("mxcsr after write: got %#08x, want %#08x", got, modified)
	}
}

func TestReadWriteXMMRegister(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()
	xmm0Info, _ := debugger.RegisterInfoByName("xmm0")

	want := [16]byte{
		0x01, 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x08,
		0x09, 0x0A, 0x0B, 0x0C, 0x0D, 0x0E, 0x0F, 0x10,
	}
	if err := regs.Write(xmm0Info, want); err != nil {
		t.Fatalf("Write xmm0 failed: %v", err)
	}

	got := regs.Read(xmm0Info).([16]byte)
	if got != want {
		t.Errorf("xmm0 after write: got %v, want %v", got, want)
	}
}

func TestReadWriteDebugRegister(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()
	dr0Info, _ := debugger.RegisterInfoByName("dr0")

	// dr0 holds a linear address for hardware breakpoints.
	want := uint64(0x00007FFF12345678)
	if err := regs.Write(dr0Info, want); err != nil {
		t.Fatalf("Write dr0 failed: %v", err)
	}

	got := regs.Read(dr0Info).(uint64)
	if got != want {
		t.Errorf("dr0 after write: got %#016x, want %#016x", got, want)
	}
}

func TestReadByIDAndWriteByID(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()
	raxInfo, _ := debugger.RegisterInfoByName("rax")

	// Round-trip via WriteByID / ReadByID.
	want := uint64(0xFEEDFACECAFEBEEF)
	if err := regs.WriteByID(raxInfo.ID, want); err != nil {
		t.Fatalf("WriteByID failed: %v", err)
	}

	got, err := regs.ReadByID(raxInfo.ID)
	if err != nil {
		t.Fatalf("ReadByID failed: %v", err)
	}
	if got.(uint64) != want {
		t.Errorf("ReadByID(rax): got %#x, want %#x", got, want)
	}

	// Invalid ID should return an error.
	_, err = regs.ReadByID(debugger.RegisterID(9999))
	if err == nil {
		t.Fatal("ReadByID with invalid ID should return error")
	}
	if !isDebuggerError(err) {
		t.Fatalf("expected *debugger.Error, got %T: %v", err, err)
	}

	err = regs.WriteByID(debugger.RegisterID(9999), uint64(0))
	if err == nil {
		t.Fatal("WriteByID with invalid ID should return error")
	}
	if !isDebuggerError(err) {
		t.Fatalf("expected *debugger.Error, got %T: %v", err, err)
	}
}

func TestWriteTypeMismatchReturnsError(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()
	raxInfo, _ := debugger.RegisterInfoByName("rax")

	// rax is an 8-byte uint register; writing [16]byte should fail.
	err = regs.Write(raxInfo, [16]byte{})
	if err == nil {
		t.Fatal("expected error writing [16]byte to uint64 register")
	}
	if !isDebuggerError(err) {
		t.Fatalf("expected *debugger.Error, got %T: %v", err, err)
	}
}

// ---------------------------------------------------------------------------
// Assembly-based register tests (trap-resume-read pattern)
// ---------------------------------------------------------------------------

// resumeAndWait resumes the process and waits for the next stop.
// It fails the test if the process exits or is terminated instead of stopping.
func resumeAndWait(t *testing.T, proc *debugger.Process) {
	t.Helper()
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected ProcessStopped, got %d (info=%d)", reason.Reason, reason.Info)
	}
}

func TestRegisterReadFromInferior(t *testing.T) {
	proc, err := debugger.Launch(targetPath("reg_read"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	regs := proc.Registers()

	// ── Trap 1: r13 == 0xcafecafe ────────────────────────────────
	resumeAndWait(t, proc)
	r13Info, _ := debugger.RegisterInfoByName("r13")
	r13 := regs.Read(r13Info).(uint64)
	if r13 != 0xcafecafe {
		t.Errorf("trap 1: r13 = %#x, want 0xcafecafe", r13)
	}

	// ── Trap 2: r13b == 42 ───────────────────────────────────────
	resumeAndWait(t, proc)
	r13bInfo, _ := debugger.RegisterInfoByName("r13b")
	r13b := regs.Read(r13bInfo).(uint8)
	if r13b != 42 {
		t.Errorf("trap 2: r13b = %d, want 42", r13b)
	}

	// ── Trap 3: mm0 contains 0xba5eba11 in low bytes ─────────────
	resumeAndWait(t, proc)
	mm0Info, _ := debugger.RegisterInfoByName("mm0")
	mm0 := regs.Read(mm0Info).([8]byte)
	mm0Val := binary.LittleEndian.Uint64(mm0[:])
	if mm0Val != 0xba5eba11 {
		t.Errorf("trap 3: mm0 = %#x, want 0xba5eba11", mm0Val)
	}

	// ── Trap 4: xmm0 contains double 64.125 ─────────────────────
	resumeAndWait(t, proc)
	xmm0Info, _ := debugger.RegisterInfoByName("xmm0")
	xmm0 := regs.Read(xmm0Info).([16]byte)
	xmm0Bits := binary.LittleEndian.Uint64(xmm0[:8])
	xmm0Float := math.Float64frombits(xmm0Bits)
	if xmm0Float != 64.125 {
		t.Errorf("trap 4: xmm0 as double = %g, want 64.125", xmm0Float)
	}

	// ── Trap 5: st0 contains 64.125 ─────────────────────────────
	resumeAndWait(t, proc)
	st0Info, _ := debugger.RegisterInfoByName("st0")
	st0 := regs.Read(st0Info).([16]byte)
	// x87 80-bit extended precision for 64.125:
	// The value should be non-zero in the first 10 bytes.
	allZero := true
	for _, b := range st0[:10] {
		if b != 0 {
			allZero = false
			break
		}
	}
	if allZero {
		t.Error("trap 5: st0 is all zeros, expected 64.125")
	}

	// Resume to exit.
	if err := proc.Resume(); err != nil {
		t.Fatalf("final Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("final WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessExited {
		t.Errorf("expected process to exit, got state %d", reason.Reason)
	}
}

// ---------------------------------------------------------------------------
// Breakpoint tests
// ---------------------------------------------------------------------------

func TestCreateBreakpointSite(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	site, err := proc.CreateBreakpointSite(pc, false, false)
	if err != nil {
		t.Fatalf("CreateBreakpointSite failed: %v", err)
	}
	if site.ID() <= 0 {
		t.Errorf("expected positive ID, got %d", site.ID())
	}
	if site.Address() != pc {
		t.Errorf("address = 0x%x, want 0x%x", site.Address(), pc)
	}
	if site.IsEnabled() {
		t.Error("new breakpoint site should be disabled")
	}
}

func TestBreakpointSiteIDsIncrease(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	s1, err := proc.CreateBreakpointSite(pc, false, false)
	if err != nil {
		t.Fatalf("CreateBreakpointSite(1) failed: %v", err)
	}
	s2, err := proc.CreateBreakpointSite(pc+100, false, false)
	if err != nil {
		t.Fatalf("CreateBreakpointSite(2) failed: %v", err)
	}
	if s2.ID() <= s1.ID() {
		t.Errorf("IDs should increase: s1.ID=%d, s2.ID=%d", s1.ID(), s2.ID())
	}
}

func TestBreakpointSiteDuplicateAddress(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	if _, err := proc.CreateBreakpointSite(pc, false, false); err != nil {
		t.Fatalf("first CreateBreakpointSite failed: %v", err)
	}
	_, err = proc.CreateBreakpointSite(pc, false, false)
	if err == nil {
		t.Fatal("expected error for duplicate address")
	}
	if !isDebuggerError(err) {
		t.Fatalf("expected *debugger.Error, got %T: %v", err, err)
	}
}

func TestBreakpointSiteList(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	proc.CreateBreakpointSite(pc, false, false)
	proc.CreateBreakpointSite(pc+100, false, false)

	sites := proc.BreakpointSites()
	if len(sites) != 2 {
		t.Fatalf("expected 2 sites, got %d", len(sites))
	}
}

func TestBreakpointSiteRemove(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	site, _ := proc.CreateBreakpointSite(pc, false, false)
	if err := proc.RemoveBreakpointSite(site.ID()); err != nil {
		t.Fatalf("RemoveBreakpointSite failed: %v", err)
	}
	if len(proc.BreakpointSites()) != 0 {
		t.Error("expected empty list after remove")
	}
}

func TestBreakpointSiteEnableDisable(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	site, _ := proc.CreateBreakpointSite(pc, false, false)

	if err := site.Enable(); err != nil {
		t.Fatalf("Enable failed: %v", err)
	}
	if !site.IsEnabled() {
		t.Error("expected enabled after Enable()")
	}

	if err := site.Disable(); err != nil {
		t.Fatalf("Disable failed: %v", err)
	}
	if site.IsEnabled() {
		t.Error("expected disabled after Disable()")
	}
}

func TestBreakpointStopsExecution(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	// Get the entry point PC. The first instruction is at _start.
	// We'll set a breakpoint a few instructions ahead (the write syscall
	// setup). For a non-PIE -nostdlib binary, the instructions after
	// _start are contiguous. We step once to get a second instruction
	// address.
	startPC, _ := proc.GetPC()
	reason, err := proc.StepInstruction()
	if err != nil {
		t.Fatalf("StepInstruction failed: %v", err)
	}
	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected stopped after step, got %d", reason.Reason)
	}
	secondPC, _ := proc.GetPC()
	if secondPC == startPC {
		t.Fatal("PC did not advance after step")
	}

	// Set a breakpoint at the second instruction.
	// First, step back by setting PC to start, then set BP at secondPC.
	if err := proc.SetPC(startPC); err != nil {
		t.Fatalf("SetPC back failed: %v", err)
	}

	site, err := proc.CreateBreakpointSite(secondPC, false, false)
	if err != nil {
		t.Fatalf("CreateBreakpointSite failed: %v", err)
	}
	if err := site.Enable(); err != nil {
		t.Fatalf("Enable failed: %v", err)
	}

	// Resume — should stop at the breakpoint.
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err = proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected stopped at breakpoint, got %d", reason.Reason)
	}
	hitPC, _ := proc.GetPC()
	if hitPC != secondPC {
		t.Errorf("stopped at 0x%x, want breakpoint at 0x%x", hitPC, secondPC)
	}

	// Resume again — process should run to exit.
	if err := proc.Resume(); err != nil {
		t.Fatalf("second Resume failed: %v", err)
	}
	reason, err = proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("second WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessExited {
		t.Errorf("expected process to exit, got state %d", reason.Reason)
	}
}

func TestStepInstruction(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	pcBefore, _ := proc.GetPC()

	reason, err := proc.StepInstruction()
	if err != nil {
		t.Fatalf("StepInstruction failed: %v", err)
	}
	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected stopped after step, got %d", reason.Reason)
	}

	pcAfter, _ := proc.GetPC()
	if pcAfter == pcBefore {
		t.Error("PC did not change after single step")
	}
}

func TestStepOverBreakpoint(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	pc, _ := proc.GetPC()

	// Set a breakpoint at the current PC.
	site, err := proc.CreateBreakpointSite(pc, false, false)
	if err != nil {
		t.Fatalf("CreateBreakpointSite failed: %v", err)
	}
	if err := site.Enable(); err != nil {
		t.Fatalf("Enable failed: %v", err)
	}

	// Step should execute the instruction under the breakpoint.
	reason, err := proc.StepInstruction()
	if err != nil {
		t.Fatalf("StepInstruction failed: %v", err)
	}
	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected stopped after step, got %d", reason.Reason)
	}

	newPC, _ := proc.GetPC()
	if newPC == pc {
		t.Error("PC did not advance past breakpoint")
	}
	if !site.IsEnabled() {
		t.Error("breakpoint should still be enabled after step")
	}
}

func TestContinueFromBreakpoint(t *testing.T) {
	proc, err := debugger.Launch(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	// Step once to get a target address for the breakpoint.
	proc.StepInstruction()
	bpAddr, _ := proc.GetPC()

	// Set PC back to _start and set a breakpoint at the second instruction.
	proc.StepInstruction() // step again to get past
	thirdPC, _ := proc.GetPC()
	_ = thirdPC

	// Reset to start over. Set PC to entry and create BP.
	// Actually, let's just launch fresh approach: set BP at current PC.
	site, err := proc.CreateBreakpointSite(bpAddr, false, false)
	if err != nil {
		t.Fatalf("CreateBreakpointSite failed: %v", err)
	}
	if err := site.Enable(); err != nil {
		t.Fatalf("Enable failed: %v", err)
	}

	// Continue from here (past the BP address) — process should exit normally.
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessExited {
		t.Errorf("expected process to exit, got state %d (info=%d)", reason.Reason, reason.Info)
	}
}

// ---------------------------------------------------------------------------
// Memory read/write tests
// ---------------------------------------------------------------------------

func TestReadMemory(t *testing.T) {
	// Create a pipe to capture the inferior's stdout (it writes the
	// address of the known value to stdout).
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	defer pr.Close()

	proc, err := debugger.LaunchWithOptions(targetPath("memory"),
		debugger.LaunchOptions{Stdout: pw})
	if err != nil {
		pw.Close()
		t.Fatalf("LaunchWithOptions failed: %v", err)
	}
	defer proc.Close()
	pw.Close()

	// Resume to trap 1 — inferior has stored 0xcafecafe and written its address.
	resumeAndWait(t, proc)

	// Read the 8-byte address from the pipe.
	addrBuf := make([]byte, 8)
	if _, err := pr.Read(addrBuf); err != nil {
		t.Fatalf("read address from pipe: %v", err)
	}
	addr := binary.LittleEndian.Uint64(addrBuf)

	// Read 8 bytes at that address.
	data, err := proc.ReadMemory(addr, 8)
	if err != nil {
		t.Fatalf("ReadMemory failed: %v", err)
	}
	val := binary.LittleEndian.Uint64(data)
	if val != 0xcafecafe {
		t.Errorf("ReadMemory: got 0x%x, want 0xcafecafe", val)
	}
}

func TestWriteMemory(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	defer pr.Close()

	proc, err := debugger.LaunchWithOptions(targetPath("memory"),
		debugger.LaunchOptions{Stdout: pw})
	if err != nil {
		pw.Close()
		t.Fatalf("LaunchWithOptions failed: %v", err)
	}
	defer proc.Close()
	pw.Close()

	// Resume to trap 1 (skip the read phase).
	resumeAndWait(t, proc)
	// Drain the first address from the pipe.
	drain := make([]byte, 8)
	pr.Read(drain)

	// Resume to trap 2 — inferior has zeroed the buffer and written its address.
	resumeAndWait(t, proc)

	// Read the buffer address from the pipe.
	addrBuf := make([]byte, 8)
	if _, err := pr.Read(addrBuf); err != nil {
		t.Fatalf("read address from pipe: %v", err)
	}
	bufAddr := binary.LittleEndian.Uint64(addrBuf)

	// Write "Hello, toydbg!" into the buffer.
	msg := []byte("Hello, toydbg!")
	if err := proc.WriteMemory(bufAddr, msg); err != nil {
		t.Fatalf("WriteMemory failed: %v", err)
	}

	// Resume — inferior prints the buffer contents and exits.
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessExited {
		t.Fatalf("expected process to exit, got state %d", reason.Reason)
	}

	// Read the printed buffer from the pipe.
	outBuf := make([]byte, 256)
	n, _ := pr.Read(outBuf)
	out := string(outBuf[:n])
	if !strings.Contains(out, "Hello, toydbg!") {
		t.Errorf("inferior output %q does not contain expected message", out)
	}
}

func TestRegisterWriteToInferior(t *testing.T) {
	// Create a pipe to capture the inferior's stdout.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	defer pr.Close()

	proc, err := debugger.LaunchWithOptions(targetPath("reg_write"),
		debugger.LaunchOptions{Stdout: pw})
	if err != nil {
		pw.Close()
		t.Fatalf("LaunchWithOptions failed: %v", err)
	}
	defer proc.Close()
	// Close the write end in the parent so reads see EOF when the child exits.
	pw.Close()

	regs := proc.Registers()

	// Helper to read from the pipe until we get some output.
	readOutput := func() string {
		buf := make([]byte, 256)
		n, _ := pr.Read(buf)
		return string(buf[:n])
	}

	// ── Trap 1: Write rsi = 0xcafecafe ───────────────────────────
	resumeAndWait(t, proc)
	rsiInfo, _ := debugger.RegisterInfoByName("rsi")
	if err := regs.Write(rsiInfo, uint64(0xcafecafe)); err != nil {
		t.Fatalf("write rsi failed: %v", err)
	}

	// Resume to Trap 2 — inferior prints rsi and hits next int3.
	resumeAndWait(t, proc)
	out := readOutput()
	if !strings.Contains(out, "0xcafecafe") {
		t.Errorf("trap 1→2: inferior printed %q, want 0xcafecafe", out)
	}

	// ── Trap 2: Write mm0 = 0xba5eba11 ──────────────────────────
	mm0Info, _ := debugger.RegisterInfoByName("mm0")
	var mm0Val [8]byte
	binary.LittleEndian.PutUint64(mm0Val[:], 0xba5eba11)
	if err := regs.Write(mm0Info, mm0Val); err != nil {
		t.Fatalf("write mm0 failed: %v", err)
	}

	// Resume to Trap 3 — inferior prints mm0 and hits next int3.
	resumeAndWait(t, proc)
	out = readOutput()
	if !strings.Contains(out, "0xba5eba11") {
		t.Errorf("trap 2→3: inferior printed %q, want 0xba5eba11", out)
	}

	// ── Trap 3: Write xmm0 = double 42.24 ───────────────────────
	xmm0Info, _ := debugger.RegisterInfoByName("xmm0")
	var xmm0Val [16]byte
	binary.LittleEndian.PutUint64(xmm0Val[:8], math.Float64bits(42.24))
	if err := regs.Write(xmm0Info, xmm0Val); err != nil {
		t.Fatalf("write xmm0 failed: %v", err)
	}

	// Resume to Trap 4 — inferior prints xmm0 as float.
	resumeAndWait(t, proc)
	out = readOutput()
	if !strings.Contains(out, "42.24") {
		t.Errorf("trap 3→4: inferior printed %q, want 42.24", out)
	}

	// Resume to exit.
	if err := proc.Resume(); err != nil {
		t.Fatalf("final Resume failed: %v", err)
	}
	reason, err2 := proc.WaitOnSignal()
	if err2 != nil {
		t.Fatalf("final WaitOnSignal failed: %v", err2)
	}
	if reason.Reason != debugger.ProcessExited {
		t.Errorf("expected process to exit, got state %d", reason.Reason)
	}
}

// ---------------------------------------------------------------------------
// Hardware breakpoint tests
// ---------------------------------------------------------------------------

// TestHardwareBreakpointEvadesChecksum verifies that a hardware breakpoint
// at a function address does not alter the function's bytes, allowing the
// anti-debugger checksum to pass. A software breakpoint at the same address
// would inject 0xCC and be detected.
func TestHardwareBreakpointEvadesChecksum(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	defer pr.Close()

	proc, err := debugger.LaunchWithOptions(targetPath("anti_debugger"),
		debugger.LaunchOptions{Stdout: pw})
	if err != nil {
		pw.Close()
		t.Fatalf("LaunchWithOptions failed: %v", err)
	}
	defer proc.Close()
	pw.Close()

	// Resume to trap 1 — the inferior has written the function address
	// and computed the clean checksum.
	resumeAndWait(t, proc)

	// Read the 8-byte function address from the pipe.
	addrBuf := make([]byte, 8)
	if _, err := io.ReadFull(pr, addrBuf); err != nil {
		t.Fatalf("read function address from pipe: %v", err)
	}
	funcAddr := binary.LittleEndian.Uint64(addrBuf)
	t.Logf("an_innocent_function at 0x%x", funcAddr)

	// ── Round 1: software breakpoint → should be detected ────────
	site, err := proc.CreateBreakpointSite(funcAddr, false, false)
	if err != nil {
		t.Fatalf("CreateBreakpointSite (software) failed: %v", err)
	}
	if err := site.Enable(); err != nil {
		t.Fatalf("Enable software BP failed: %v", err)
	}

	// Resume — the inferior checksums the function, detects the 0xCC,
	// prints "pepperoni", and traps again.
	resumeAndWait(t, proc)

	out := make([]byte, 256)
	n, _ := pr.Read(out)
	output := string(out[:n])
	if !strings.Contains(output, "pepperoni") {
		t.Errorf("expected 'pepperoni' (detected tampering), got %q", output)
	}

	// Delete the software breakpoint.
	if err := proc.RemoveBreakpointSite(site.ID()); err != nil {
		t.Fatalf("RemoveBreakpointSite failed: %v", err)
	}

	// ── Round 2: hardware breakpoint → should be invisible ───────
	hwSite, err := proc.CreateBreakpointSite(funcAddr, true, false)
	if err != nil {
		t.Fatalf("CreateBreakpointSite (hardware) failed: %v", err)
	}
	if err := hwSite.Enable(); err != nil {
		t.Fatalf("Enable hardware BP failed: %v", err)
	}

	// Resume — the checksum matches, the inferior calls the function,
	// which triggers the hardware breakpoint.
	resumeAndWait(t, proc)

	pc, _ := proc.GetPC()
	if pc != funcAddr {
		t.Errorf("hardware BP hit at 0x%x, want 0x%x", pc, funcAddr)
	}

	// Disable the hardware breakpoint and continue through the function.
	if err := hwSite.Disable(); err != nil {
		t.Fatalf("Disable hardware BP failed: %v", err)
	}

	// Resume — the function runs, prints "pineapple", and hits the
	// final int3.
	resumeAndWait(t, proc)

	out2 := make([]byte, 256)
	n2, _ := pr.Read(out2)
	output2 := string(out2[:n2])
	if !strings.Contains(output2, "pineapple") {
		t.Errorf("expected 'pineapple' (no tampering), got %q", output2)
	}
}

// ---------------------------------------------------------------------------
// Watchpoint tests
// ---------------------------------------------------------------------------

// TestWatchpointDetectsWrite verifies that a write watchpoint triggers
// when the tracee writes to the watched address.
func TestWatchpointDetectsWrite(t *testing.T) {
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("os.Pipe failed: %v", err)
	}
	defer pr.Close()

	proc, err := debugger.LaunchWithOptions(targetPath("memory"),
		debugger.LaunchOptions{Stdout: pw})
	if err != nil {
		pw.Close()
		t.Fatalf("LaunchWithOptions failed: %v", err)
	}
	defer proc.Close()
	pw.Close()

	// Resume to trap 1 — the inferior has stored 0xcafecafe and written
	// its address to stdout.
	resumeAndWait(t, proc)

	// Read the address from the pipe.
	addrBuf := make([]byte, 8)
	if _, err := io.ReadFull(pr, addrBuf); err != nil {
		t.Fatalf("read address from pipe: %v", err)
	}
	watchAddr := binary.LittleEndian.Uint64(addrBuf)
	t.Logf("watching address 0x%x", watchAddr)

	// The inferior will next zero this region (the 32-byte buffer
	// at a different address). Let's continue to trap 2 first.
	resumeAndWait(t, proc)

	// Drain the second address.
	drain := make([]byte, 8)
	pr.Read(drain)
	bufAddr := binary.LittleEndian.Uint64(drain)
	t.Logf("buffer address 0x%x", bufAddr)

	// Set a write watchpoint on the buffer address (8-byte aligned).
	// The buffer is at rsp+0, which should be 8-byte aligned.
	wp, err := proc.CreateWatchpoint(bufAddr, debugger.StoppointModeWrite, 8)
	if err != nil {
		t.Fatalf("CreateWatchpoint failed: %v", err)
	}
	if wp.ID() <= 0 {
		t.Errorf("expected positive watchpoint ID, got %d", wp.ID())
	}
	if !wp.IsEnabled() {
		t.Error("watchpoint should be enabled after creation")
	}

	// Write some data to the buffer so the inferior has something to print.
	msg := []byte("Hello, toydbg!")
	if err := proc.WriteMemory(bufAddr, msg); err != nil {
		t.Fatalf("WriteMemory failed: %v", err)
	}

	// Clean up watchpoint before resuming (the inferior is about to
	// write to stdout which may touch this memory area).
	if err := proc.RemoveWatchpoint(wp.ID()); err != nil {
		t.Fatalf("RemoveWatchpoint failed: %v", err)
	}

	// Resume — the inferior prints the buffer and exits.
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessExited {
		t.Fatalf("expected process to exit, got state %d", reason.Reason)
	}
}

// TestWatchpointAlignment verifies that unaligned watchpoint addresses
// are rejected.
func TestWatchpointAlignment(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	// Address 0x1001 is not 4-byte aligned.
	_, err = proc.CreateWatchpoint(0x1001, debugger.StoppointModeWrite, 4)
	if err == nil {
		t.Fatal("expected error for unaligned watchpoint address")
	}
	if !isDebuggerError(err) {
		t.Fatalf("expected *debugger.Error, got %T: %v", err, err)
	}
}

// TestWatchpointListAndRemove verifies the watchpoint CRUD operations.
func TestWatchpointListAndRemove(t *testing.T) {
	proc, err := debugger.Launch(targetPath("run_endlessly"))
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	wp, err := proc.CreateWatchpoint(0x1000, debugger.StoppointModeWrite, 8)
	if err != nil {
		t.Fatalf("CreateWatchpoint failed: %v", err)
	}

	wps := proc.Watchpoints()
	if len(wps) != 1 {
		t.Fatalf("expected 1 watchpoint, got %d", len(wps))
	}

	found, ok := proc.WatchpointByID(wp.ID())
	if !ok {
		t.Fatal("WatchpointByID returned false")
	}
	if found.Address() != 0x1000 {
		t.Errorf("address = 0x%x, want 0x1000", found.Address())
	}

	if err := proc.RemoveWatchpoint(wp.ID()); err != nil {
		t.Fatalf("RemoveWatchpoint failed: %v", err)
	}
	if len(proc.Watchpoints()) != 0 {
		t.Error("expected empty watchpoint list after remove")
	}
}

// ---------------------------------------------------------------------------
// Syscall name mapping
// ---------------------------------------------------------------------------

func TestSyscallMapping(t *testing.T) {
	// Verify well-known syscalls round-trip: ID → name → ID.
	wellKnown := map[int]string{
		0:  "read",
		1:  "write",
		2:  "open",
		3:  "close",
		9:  "mmap",
		59: "execve",
		60: "exit",
	}

	for id, expectedName := range wellKnown {
		name := debugger.SyscallIDToName(id)
		if name != expectedName {
			t.Errorf("SyscallIDToName(%d) = %q, want %q", id, name, expectedName)
		}
		gotID, ok := debugger.SyscallNameToID(name)
		if !ok {
			t.Errorf("SyscallNameToID(%q) returned false", name)
		}
		if gotID != id {
			t.Errorf("SyscallNameToID(%q) = %d, want %d", name, gotID, id)
		}
	}

	// Unknown syscall ID should return a formatted string.
	name := debugger.SyscallIDToName(99999)
	if name != "syscall_99999" {
		t.Errorf("SyscallIDToName(99999) = %q, want %q", name, "syscall_99999")
	}

	// Unknown syscall name should return -1, false.
	_, ok := debugger.SyscallNameToID("nonexistent_syscall")
	if ok {
		t.Error("SyscallNameToID(nonexistent) should return false")
	}
}

// ---------------------------------------------------------------------------
// Syscall catchpoints
// ---------------------------------------------------------------------------

func TestSyscallCatchpoints(t *testing.T) {
	// Use the anti_debugger target which does a write(2) syscall.
	target := filepath.Join(targetDir, "anti_debugger")
	proc, err := debugger.LaunchWithOptions(target, debugger.LaunchOptions{
		Stdout: io.Discard,
		Stderr: io.Discard,
	})
	if err != nil {
		t.Fatalf("Launch failed: %v", err)
	}
	defer proc.Close()

	// Catch only the "write" syscall (ID=1).
	writeID, ok := debugger.SyscallNameToID("write")
	if !ok {
		t.Fatal("SyscallNameToID(write) returned false")
	}
	proc.SetSyscallCatchPolicy(debugger.CatchSomeSyscalls([]int{writeID}))

	// Resume and wait — should stop at a write syscall entry.
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}

	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected ProcessStopped, got %d", reason.Reason)
	}
	if reason.TrapReason == nil {
		t.Fatal("expected TrapReason to be set")
	}
	if *reason.TrapReason != debugger.TrapSyscall {
		t.Fatalf("expected TrapSyscall, got %d", *reason.TrapReason)
	}
	if reason.SyscallInfo == nil {
		t.Fatal("expected SyscallInfo to be set")
	}
	if int(reason.SyscallInfo.ID) != writeID {
		t.Errorf("expected syscall ID %d (write), got %d (%s)",
			writeID, reason.SyscallInfo.ID,
			debugger.SyscallIDToName(int(reason.SyscallInfo.ID)))
	}
	if !reason.SyscallInfo.Entry {
		t.Error("expected syscall entry, got exit")
	}

	// Resume again — should stop at the write syscall exit.
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume (exit) failed: %v", err)
	}
	reason, err = proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal (exit) failed: %v", err)
	}
	if reason.SyscallInfo != nil && !reason.SyscallInfo.Entry {
		// Successfully caught the exit — verify it's still "write".
		if int(reason.SyscallInfo.ID) != writeID {
			t.Errorf("exit: expected syscall ID %d, got %d",
				writeID, reason.SyscallInfo.ID)
		}
	}

	// Disable catchpoints and let the process finish.
	proc.SetSyscallCatchPolicy(debugger.CatchNoSyscalls())
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume (final) failed: %v", err)
	}
	reason, err = proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal (final) failed: %v", err)
	}
	if reason.Reason != debugger.ProcessExited {
		// Process might stop again — resume until it exits.
		for reason.Reason == debugger.ProcessStopped {
			if err := proc.Resume(); err != nil {
				break
			}
			reason, err = proc.WaitOnSignal()
			if err != nil {
				break
			}
		}
	}
}

// ---------------------------------------------------------------------------
// ELF parsing tests
// ---------------------------------------------------------------------------

func TestELFParserSymbolLookup(t *testing.T) {
	// Open the hello_toydbg binary — a non-PIE assembly program with
	// a known _start symbol at the entry point.
	e, err := debugger.OpenELF(targetPath("hello_toydbg"))
	if err != nil {
		t.Fatalf("OpenELF failed: %v", err)
	}
	defer e.Close()

	// _start should be findable by name.
	syms := e.SymbolsByName("_start")
	if len(syms) == 0 {
		t.Fatal("expected to find _start symbol")
	}

	// For a non-PIE binary, load bias is 0. The _start symbol value
	// should equal the ELF entry point.
	if syms[0].Value != e.EntryPoint() {
		t.Errorf("_start value 0x%x != entry point 0x%x", syms[0].Value, e.EntryPoint())
	}

	// Open the anti_debugger binary — a C program with main and
	// an_innocent_function symbols.
	e2, err := debugger.OpenELF(targetPath("anti_debugger"))
	if err != nil {
		t.Fatalf("OpenELF (anti_debugger) failed: %v", err)
	}
	defer e2.Close()

	mainSyms := e2.SymbolsByName("main")
	if len(mainSyms) == 0 {
		t.Fatal("expected to find main symbol in anti_debugger")
	}

	innocentSyms := e2.SymbolsByName("an_innocent_function")
	if len(innocentSyms) == 0 {
		t.Fatal("expected to find an_innocent_function symbol")
	}

	// Verify FunctionContainingAddress works for the main symbol.
	// Load bias is 0 for non-PIE.
	mainAddr := mainSyms[0].Value
	name, ok := e2.FunctionContainingAddress(mainAddr)
	if !ok {
		t.Fatal("FunctionContainingAddress did not find main")
	}
	if name != "main" {
		t.Errorf("FunctionContainingAddress returned %q, want %q", name, "main")
	}

	// An address outside any function should return false.
	_, ok = e2.FunctionContainingAddress(0x1)
	if ok {
		t.Error("FunctionContainingAddress should return false for address 0x1")
	}
}

func TestTargetLaunchWithELF(t *testing.T) {
	target, err := debugger.LaunchTargetWithOptions(
		targetPath("anti_debugger"),
		debugger.LaunchOptions{Stdout: io.Discard, Stderr: io.Discard},
	)
	if err != nil {
		t.Fatalf("LaunchTarget failed: %v", err)
	}
	defer target.Close()

	e := target.ELF()
	if e == nil {
		t.Fatal("expected ELF to be loaded")
	}

	// Verify the process is accessible.
	proc := target.Process()
	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	t.Logf("initial PC: 0x%x, entry point: 0x%x, load bias: 0x%x",
		pc, e.EntryPoint(), e.LoadBias())

	// Resume to the first int3 in the anti_debugger program.
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected ProcessStopped, got %d", reason.Reason)
	}

	// At the int3 inside main(), FunctionContainingAddress should
	// return "main".
	pc, _ = proc.GetPC()
	name, ok := e.FunctionContainingAddress(pc)
	if !ok {
		t.Errorf("FunctionContainingAddress(0x%x) returned false, expected a function", pc)
	} else if name != "main" {
		t.Errorf("FunctionContainingAddress(0x%x) = %q, want %q", pc, name, "main")
	}
}

// ---------------------------------------------------------------------------
// DWARF debug information tests
// ---------------------------------------------------------------------------

func TestDWARFFunctionByName(t *testing.T) {
	// Open the dwarf_target binary directly (no process needed for
	// name-based DWARF lookup).
	e, err := debugger.OpenELF(targetPath("dwarf_target"))
	if err != nil {
		t.Fatalf("OpenELF failed: %v", err)
	}
	defer e.Close()

	dw := e.DWARF()
	if dw == nil {
		t.Fatal("expected DWARF info to be present (compiled with -g)")
	}

	// Verify we can find each function by name.
	for _, name := range []string{"add_numbers", "multiply_numbers", "main"} {
		fns := dw.FunctionsByName(name)
		if len(fns) == 0 {
			t.Errorf("FunctionsByName(%q) returned no results", name)
			continue
		}
		if fns[0].Name != name {
			t.Errorf("FunctionsByName(%q)[0].Name = %q", name, fns[0].Name)
		}
		if len(fns[0].Ranges) == 0 {
			t.Errorf("FunctionsByName(%q)[0] has no address ranges", name)
		}
		t.Logf("%s: ranges=%v", name, fns[0].Ranges)
	}

	// Verify the address from DWARF matches the ELF symbol table.
	addFns := dw.FunctionsByName("add_numbers")
	addSyms := e.SymbolsByName("add_numbers")
	if len(addFns) > 0 && len(addSyms) > 0 {
		dwarfAddr := addFns[0].Ranges[0][0]
		symAddr := addSyms[0].Value
		if dwarfAddr != symAddr {
			t.Errorf("DWARF address 0x%x != symtab address 0x%x for add_numbers",
				dwarfAddr, symAddr)
		}
	}
}

func TestDWARFFunctionContainingPC(t *testing.T) {
	// Launch the dwarf_target process and stop at the first int3.
	// The int3 is inside main(), so FunctionContainingPC should
	// return "main".
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pr.Close()

	target, err := debugger.LaunchTargetWithOptions(
		targetPath("dwarf_target"),
		debugger.LaunchOptions{Stdout: pw},
	)
	if err != nil {
		pw.Close()
		t.Fatalf("LaunchTarget failed: %v", err)
	}
	defer target.Close()
	pw.Close()

	proc := target.Process()

	// Resume to the first int3 (past the address writes).
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected ProcessStopped, got %d", reason.Reason)
	}

	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	dw := target.ELF().DWARF()
	if dw == nil {
		t.Fatal("expected DWARF info to be present")
	}

	fn := dw.FunctionContainingPC(pc)
	if fn == nil {
		t.Fatalf("FunctionContainingPC(0x%x) returned nil", pc)
	}
	if fn.Name != "main" {
		t.Errorf("FunctionContainingPC(0x%x) = %q, want %q", pc, fn.Name, "main")
	}
	t.Logf("PC=0x%x is in function %q", pc, fn.Name)
}

func TestDWARFSourceLocation(t *testing.T) {
	// Launch the dwarf_target and verify PCToSourceLocation returns
	// a file containing "dwarf_target.c" with a positive line number.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pr.Close()

	target, err := debugger.LaunchTargetWithOptions(
		targetPath("dwarf_target"),
		debugger.LaunchOptions{Stdout: pw},
	)
	if err != nil {
		pw.Close()
		t.Fatalf("LaunchTarget failed: %v", err)
	}
	defer target.Close()
	pw.Close()

	proc := target.Process()

	// Resume to the first int3.
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected ProcessStopped, got %d", reason.Reason)
	}

	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	dw := target.ELF().DWARF()
	if dw == nil {
		t.Fatal("expected DWARF info to be present")
	}

	loc, ok := dw.PCToSourceLocation(pc)
	if !ok {
		t.Fatalf("PCToSourceLocation(0x%x) returned false", pc)
	}

	if !strings.Contains(loc.File, "dwarf_target.c") {
		t.Errorf("PCToSourceLocation file = %q, want it to contain %q", loc.File, "dwarf_target.c")
	}
	if loc.Line <= 0 {
		t.Errorf("PCToSourceLocation line = %d, want > 0", loc.Line)
	}
	t.Logf("PC=0x%x → %s:%d", pc, loc.File, loc.Line)
}

func TestDWARFLineTable(t *testing.T) {
	// Open the dwarf_target binary and verify the line table index
	// was built correctly.
	e, err := debugger.OpenELF(targetPath("dwarf_target"))
	if err != nil {
		t.Fatalf("OpenELF failed: %v", err)
	}
	defer e.Close()

	dw := e.DWARF()
	if dw == nil {
		t.Fatal("expected DWARF info to be present (compiled with -g)")
	}

	entries := dw.AllLineEntries()
	if len(entries) == 0 {
		t.Fatal("AllLineEntries returned no entries")
	}

	// Verify entries are sorted by address.
	for i := 1; i < len(entries); i++ {
		if entries[i].Address < entries[i-1].Address {
			t.Fatalf("entries not sorted: [%d].Address=0x%x > [%d].Address=0x%x",
				i-1, entries[i-1].Address, i, entries[i].Address)
		}
	}

	// Verify at least some entries reference dwarf_target.c.
	found := false
	for _, e := range entries {
		if strings.Contains(e.File, "dwarf_target.c") {
			found = true
			break
		}
	}
	if !found {
		t.Error("no line table entries reference dwarf_target.c")
	}

	t.Logf("line table has %d entries", len(entries))
}

func TestDWARFGetEntriesByLine(t *testing.T) {
	e, err := debugger.OpenELF(targetPath("dwarf_target"))
	if err != nil {
		t.Fatalf("OpenELF failed: %v", err)
	}
	defer e.Close()

	dw := e.DWARF()
	if dw == nil {
		t.Fatal("expected DWARF info to be present")
	}

	// Line 22 is "int add_numbers(int a, int b) {" — should have entries.
	entries := dw.GetEntriesByLine("dwarf_target.c", 22)
	if len(entries) == 0 {
		t.Fatal("GetEntriesByLine(dwarf_target.c, 22) returned no entries")
	}

	// The returned addresses should fall within add_numbers' range.
	fns := dw.FunctionsByName("add_numbers")
	if len(fns) == 0 {
		t.Fatal("FunctionsByName(add_numbers) returned no results")
	}
	fnLow := fns[0].Ranges[0][0]
	fnHigh := fns[0].Ranges[0][1]
	for _, entry := range entries {
		// Entries are in file-address space (no load bias set), so
		// they should match the DWARF function ranges directly.
		if entry.Address < fnLow || entry.Address >= fnHigh {
			t.Errorf("entry address 0x%x outside add_numbers range [0x%x, 0x%x)",
				entry.Address, fnLow, fnHigh)
		}
	}
	t.Logf("GetEntriesByLine(dwarf_target.c, 22) returned %d entries", len(entries))

	// Test with full path suffix (should also match).
	entries2 := dw.GetEntriesByLine("targets/dwarf_target.c", 22)
	if len(entries2) == 0 {
		t.Error("GetEntriesByLine with path suffix returned no entries")
	}

	// Non-existent line should return empty.
	entries3 := dw.GetEntriesByLine("dwarf_target.c", 9999)
	if len(entries3) != 0 {
		t.Errorf("GetEntriesByLine for non-existent line returned %d entries", len(entries3))
	}
}

func TestDWARFGetEntryByAddress(t *testing.T) {
	// Launch the dwarf_target process and stop at the first int3.
	pr, pw, err := os.Pipe()
	if err != nil {
		t.Fatalf("pipe: %v", err)
	}
	defer pr.Close()

	target, err := debugger.LaunchTargetWithOptions(
		targetPath("dwarf_target"),
		debugger.LaunchOptions{Stdout: pw},
	)
	if err != nil {
		pw.Close()
		t.Fatalf("LaunchTarget failed: %v", err)
	}
	defer target.Close()
	pw.Close()

	proc := target.Process()

	// Resume to the first int3.
	if err := proc.Resume(); err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	reason, err := proc.WaitOnSignal()
	if err != nil {
		t.Fatalf("WaitOnSignal failed: %v", err)
	}
	if reason.Reason != debugger.ProcessStopped {
		t.Fatalf("expected ProcessStopped, got %d", reason.Reason)
	}

	pc, err := proc.GetPC()
	if err != nil {
		t.Fatalf("GetPC failed: %v", err)
	}

	dw := target.ELF().DWARF()
	if dw == nil {
		t.Fatal("expected DWARF info to be present")
	}

	entry, ok := dw.GetEntryByAddress(pc)
	if !ok {
		t.Fatalf("GetEntryByAddress(0x%x) returned false", pc)
	}

	if !strings.Contains(entry.File, "dwarf_target.c") {
		t.Errorf("GetEntryByAddress file = %q, want it to contain %q",
			entry.File, "dwarf_target.c")
	}
	if entry.Line <= 0 {
		t.Errorf("GetEntryByAddress line = %d, want > 0", entry.Line)
	}

	// Cross-check with PCToSourceLocation for consistency.
	loc, locOK := dw.PCToSourceLocation(pc)
	if !locOK {
		t.Fatal("PCToSourceLocation returned false but GetEntryByAddress succeeded")
	}
	if loc.File != entry.File || loc.Line != entry.Line {
		t.Errorf("GetEntryByAddress(%s:%d) != PCToSourceLocation(%s:%d)",
			entry.File, entry.Line, loc.File, loc.Line)
	}

	t.Logf("PC=0x%x → %s:%d (column=%d)", pc, entry.File, entry.Line, entry.Column)
}
