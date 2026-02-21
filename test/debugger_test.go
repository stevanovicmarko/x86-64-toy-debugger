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
