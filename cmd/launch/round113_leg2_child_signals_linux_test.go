package launch

// round113_leg2_child_signals_linux_test.go — leg 2, round 113 (2026-09-29 audit),
// F113-L2-2. Linux-only (the tests signal their own process).
//
// `oaica launch claude` ran its child with cmd.Run() and installed no signal handler, so a
// SIGTERM to oaica alone (a supervisor stop, `kill <pid>`, an ssh wrapper) ended oaica at once
// by Go's default and left the child running against a translation proxy that had died with
// its parent, on the same tty and session. cmd/oaica_pull_serve.go already does this properly
// for pull and serve. runChild is the one place the launch doors now start their child: it
// forwards SIGTERM and SIGHUP to it and waits, leaves SIGINT to the terminal (Ctrl-C reaches
// the child through the foreground process group, and oaica must not die before it), and
// kills a child that ignores the signal after a grace period.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func r113Child(t *testing.T, script string) (*exec.Cmd, string) {
	t.Helper()
	pidFile := filepath.Join(t.TempDir(), "pid")
	cmd := exec.Command("sh", "-c", "echo $$ > "+pidFile+"; "+script)
	return cmd, pidFile
}

func r113ReadPID(t *testing.T, path string) int {
	t.Helper()
	for i := 0; i < 300; i++ {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the child never wrote its pid")
	return 0
}

func r113Alive(pid int) bool { return syscall.Kill(pid, 0) == nil }

func TestMine113ATerminatedLauncherDoesNotOrphanItsChild(t *testing.T) {
	// A long grace, so that only the FORWARDED SIGTERM can end the child in time: the kill
	// after the grace is a different mechanism and is pinned by the next test.
	old := childTermGrace
	childTermGrace = 60 * time.Second
	t.Cleanup(func() { childTermGrace = old })
	cmd, pidFile := r113Child(t, "exec sleep 60")
	done := make(chan error, 1)
	go func() { done <- runChild(cmd) }()
	pid := r113ReadPID(t, pidFile)
	time.Sleep(100 * time.Millisecond) // runChild has its handler installed before it starts the child
	if err := syscall.Kill(os.Getpid(), syscall.SIGTERM); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("runChild did not return within 5s of SIGTERM — the signal was not forwarded to the child (2026-09-29 audit, round 113, F113-L2-2)")
	}
	for i := 0; i < 100 && r113Alive(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if r113Alive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("the child (pid %d) is still running after its launcher was sent SIGTERM — it is orphaned with a dead proxy (2026-09-29 audit, round 113, F113-L2-2)", pid)
	}
}

// A child that ignores SIGTERM is killed after the grace period rather than waited on for ever.
func TestMine113AChildThatIgnoresTermIsKilledAfterTheGrace(t *testing.T) {
	old := childTermGrace
	childTermGrace = 500 * time.Millisecond
	t.Cleanup(func() { childTermGrace = old })
	cmd, pidFile := r113Child(t, "trap '' TERM; while :; do sleep 1; done")
	done := make(chan error, 1)
	go func() { done <- runChild(cmd) }()
	pid := r113ReadPID(t, pidFile)
	time.Sleep(200 * time.Millisecond)
	syscall.Kill(os.Getpid(), syscall.SIGTERM)
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("runChild waited for ever on a child that ignores SIGTERM (2026-09-29 audit, round 113, F113-L2-2)")
	}
	for i := 0; i < 100 && r113Alive(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if r113Alive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("the child that ignored SIGTERM (pid %d) is still running (2026-09-29 audit, round 113, F113-L2-2)", pid)
	}
}

// SIGINT is left to the terminal: the launcher must neither die nor kill the child on it, because
// Ctrl-C already reaches the child through the foreground process group.
func TestMine113SigintDoesNotEndTheLauncher(t *testing.T) {
	old := runChildStdinIsTerminal
	runChildStdinIsTerminal = func(*exec.Cmd) bool { return true } // the terminal delivers Ctrl-C to the group itself
	t.Cleanup(func() { runChildStdinIsTerminal = old })
	cmd, pidFile := r113Child(t, "exec sleep 60")
	done := make(chan error, 1)
	go func() { done <- runChild(cmd) }()
	pid := r113ReadPID(t, pidFile)
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
		t.Errorf("runChild returned on SIGINT — the launcher would exit before the child that Ctrl-C also reached (2026-09-29 audit, round 113, F113-L2-2)")
	case <-time.After(500 * time.Millisecond):
	}
	if !r113Alive(pid) {
		t.Errorf("the child was killed by the launcher's SIGINT handling (2026-09-29 audit, round 113, F113-L2-2)")
	}
	syscall.Kill(pid, syscall.SIGKILL)
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("runChild did not return after the child exited")
	}
}

// The child's own exit status is what the caller sees.
func TestMine113RunChildReturnsTheChildsExitStatus(t *testing.T) {
	cmd, _ := r113Child(t, "exit 7")
	err := runChild(cmd)
	ee, ok := err.(*exec.ExitError)
	if !ok || ee.ExitCode() != 7 {
		t.Errorf("runChild returned %v, want the child's ExitError with code 7 (2026-09-29 audit, round 113, F113-L2-2)", err)
	}
}

// F114-L2-3: with no terminal (a supervisor, a systemd unit with KillSignal=SIGINT, `kill -INT <pid>`)
// nothing delivers SIGINT to the child but us, and absorbing it left the launcher and its child running
// until something else stopped them. Without a terminal SIGINT is forwarded like SIGTERM. With one it is
// still absorbed, because Ctrl-C already reached the child through the foreground process group and a
// second delivery would be a double Ctrl-C, which Claude Code reads as "exit".
func TestMine114ASigintWithoutATerminalReachesTheChild(t *testing.T) {
	old := runChildStdinIsTerminal
	runChildStdinIsTerminal = func(*exec.Cmd) bool { return false }
	t.Cleanup(func() { runChildStdinIsTerminal = old })
	oldGrace := childTermGrace
	childTermGrace = 60 * time.Second
	t.Cleanup(func() { childTermGrace = oldGrace })
	cmd, pidFile := r113Child(t, "exec sleep 60")
	done := make(chan error, 1)
	go func() { done <- runChild(cmd) }()
	pid := r113ReadPID(t, pidFile)
	time.Sleep(100 * time.Millisecond)
	if err := syscall.Kill(os.Getpid(), syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		syscall.Kill(pid, syscall.SIGKILL)
		t.Fatalf("a SIGINT sent to the launcher alone was swallowed: the child (pid %d) is still running and runChild still waiting (2026-09-29 audit, round 114, F114-L2-3)", pid)
	}
	for i := 0; i < 100 && r113Alive(pid); i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if r113Alive(pid) {
		syscall.Kill(pid, syscall.SIGKILL)
		t.Errorf("the child (pid %d) outlived a forwarded SIGINT (2026-09-29 audit, round 114, F114-L2-3)", pid)
	}
}
