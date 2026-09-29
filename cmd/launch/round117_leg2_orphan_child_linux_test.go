package launch

import (
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestRound117HelperRunDoor(t *testing.T) {
	door := os.Getenv("P117_DOOR")
	if door == "" {
		t.Skip("helper")
	}
	var err error
	switch door {
	case "droid":
		err = (&Droid{}).Run("", nil, nil)
	case "claude-runchild":
		err = runChild(exec.Command("droid"))
	}
	t.Logf("door returned %v", err)
}

func p117Door(t *testing.T, door string) (childAlive bool) {
	bin := t.TempDir()
	pidFile := filepath.Join(bin, "pid")
	os.WriteFile(filepath.Join(bin, "droid"), []byte("#!/bin/sh\necho $$ > "+pidFile+"\nexec sleep 30\n"), 0o755)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRound117HelperRunDoor$", "-test.v")
	cmd.Env = append(os.Environ(), "P117_DOOR="+door, "PATH="+bin+":"+os.Getenv("PATH"))
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var pid int
	for i := 0; i < 500 && pid == 0; i++ {
		if b, err := os.ReadFile(pidFile); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	if pid == 0 {
		cmd.Process.Kill()
		t.Fatalf("%s: child never started", door)
	}
	time.Sleep(100 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM) // a supervisor stop / kill <pid> on the launcher alone
	werr := cmd.Wait()
	time.Sleep(300 * time.Millisecond)
	childAlive = syscall.Kill(pid, 0) == nil
	t.Logf("%s: launcher exit=%v; child pid %d alive after launcher SIGTERM = %v", door, werr, pid, childAlive)
	syscall.Kill(pid, syscall.SIGKILL)
	return
}

func TestRound117OtherDoorsOrphanTheirChild(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	rc := p117Door(t, "claude-runchild")
	dr := p117Door(t, "droid")
	if !rc && dr {
		t.Errorf("RED: the runChild door ends its child on SIGTERM, `oaica launch droid` leaves it orphaned")
	}
}

// Every interactive launch door starts its child through runChild (F117-L2-4). The behavioural test
// above runs the one door a fake binary can stand in for; this pins the rest, where a `return
// cmd.Run()` is exactly the shape that orphans the child on SIGTERM.
func TestRound117LaunchDoorsStartTheirChildThroughRunChild(t *testing.T) {
	bare := regexp.MustCompile(`return\s+cmd\.Run\(\)`)
	for _, f := range []string{"droid.go", "opencode.go", "pi.go", "poolside.go", "omp.go", "muse.go", "deepseek_harness.go", "cline.go", "qwen.go"} {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for i, line := range strings.Split(string(b), "\n") {
			if bare.MatchString(line) {
				t.Errorf("%s:%d starts a launch child without runChild: %s", f, i+1, strings.TrimSpace(line))
			}
		}
	}
	// openclaw starts its launch children under other variable names (round 118, F118-L2-1):
	// its passthrough and its TUI must both go through runChild.
	ob, err := os.ReadFile("openclaw.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(ob), "runChild("); n != 2 || strings.Contains(string(ob), "tui.Run()") {
		t.Errorf("openclaw.go: %d runChild launches (want 2) or a bare tui.Run() remains", n)
	}
	// hermes' setup, update and gateway subcommands may stay attached to plain Run; its two
	// launch paths must not.
	b, err := os.ReadFile("hermes.go")
	if err != nil {
		t.Fatal(err)
	}
	if n := strings.Count(string(b), "return runChild(hermesAttachedCommand("); n != 2 {
		t.Errorf("hermes.go starts %d launch children through runChild, want 2", n)
	}
}
