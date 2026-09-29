// round118_leg2_openclaw_linux_test.go — F118-L2-1: oaica launch openclaw forwards SIGTERM to its children (2026-09-29 audit, round 118).

package launch

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

func TestRound118Helper(t *testing.T) {
	door := os.Getenv("P118_DOOR")
	if door == "" {
		t.Skip("helper")
	}
	setLaunchTestHome(t, os.Getenv("P118_HOME"))
	var err error
	switch door {
	case "openclaw":
		err = (&Openclaw{}).Run("llama3", nil, []string{"agent", "--x"})
	case "openclaw-tui":
		err = (&Openclaw{}).Run("llama3", nil, nil)
	case "runchild":
		err = runChild(exec.Command("openclaw"))
	}
	t.Logf("door returned %v", err)
}

func round118Door(t *testing.T, door string) bool {
	bin := t.TempDir()
	home := t.TempDir()
	os.MkdirAll(filepath.Join(home, ".openclaw"), 0o755)
	os.WriteFile(filepath.Join(home, ".openclaw", "openclaw.json"), []byte(`{"wizard":{"lastRunAt":"2026-01-01"}}`), 0o644)
	pidFile := filepath.Join(bin, "pid")
	os.WriteFile(filepath.Join(bin, "openclaw"), []byte("#!/bin/sh\necho \"$$ $*\" >> "+pidFile+".log\ncase \"$1\" in agent|'') echo $$ > "+pidFile+"; exec sleep 30;; esac\nexit 0\n"), 0o755)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRound118Helper$", "-test.v")
	cmd.Env = append(os.Environ(), "P118_DOOR="+door, "P118_HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
	out, _ := os.Create(filepath.Join(bin, "out"))
	cmd.Stdout, cmd.Stderr = out, out
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
		cmd.Wait()
		t.Fatalf("%s: child never started", door)
	}
	time.Sleep(100 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM)
	werr := cmd.Wait()
	time.Sleep(300 * time.Millisecond)
	alive := syscall.Kill(pid, 0) == nil
	b, _ := os.ReadFile(pidFile + ".log")
	t.Logf("calls: %q", b)
	t.Logf("%s: launcher exit=%v; child %d alive after launcher SIGTERM = %v", door, werr, pid, alive)
	syscall.Kill(pid, syscall.SIGKILL)
	return alive
}

func TestRound118OpenclawOrphans(t *testing.T) {
	rc := round118Door(t, "runchild")
	oc := round118Door(t, "openclaw")
	if !rc && oc {
		t.Errorf("RED: runChild ends its child on SIGTERM; `oaica launch openclaw <args>` orphans it")
	}
}

func TestRound118OpenclawTUIOrphans(t *testing.T) {
	bin := t.TempDir()
	home := t.TempDir()
	port := "48617"
	os.MkdirAll(filepath.Join(home, ".openclaw"), 0o755)
	os.WriteFile(filepath.Join(home, ".openclaw", "openclaw.json"), []byte(`{"wizard":{"lastRunAt":"2026-01-01"},"gateway":{"port":`+port+`}}`), 0o644)
	os.WriteFile(filepath.Join(bin, "openclaw"), []byte("#!/bin/sh\necho \"$$ $*\" >> "+bin+"/calls\ncase \"$1\" in gateway) echo $$ > "+bin+"/gwpid; exec python3 -m http.server "+port+" --bind 127.0.0.1 >/dev/null 2>&1;; tui) echo $$ > "+bin+"/pid; exec sleep 30;; esac\nexit 0\n"), 0o755)
	cmd := exec.Command(os.Args[0], "-test.run=^TestRound118Helper$", "-test.v")
	cmd.Env = append(os.Environ(), "P118_DOOR=openclaw-tui", "P118_HOME="+home, "PATH="+bin+":"+os.Getenv("PATH"))
	out, _ := os.Create(filepath.Join(bin, "out"))
	cmd.Stdout, cmd.Stderr = out, out
	cmd.Start()
	var pid int
	for i := 0; i < 4000 && pid == 0; i++ {
		if b, err := os.ReadFile(filepath.Join(bin, "pid")); err == nil {
			pid, _ = strconv.Atoi(strings.TrimSpace(string(b)))
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(filepath.Join(bin, "gwpid"))
	gw, _ := strconv.Atoi(strings.TrimSpace(string(b)))
	if pid == 0 {
		cmd.Process.Kill()
		cmd.Wait()
		o, _ := os.ReadFile(filepath.Join(bin, "out"))
		t.Fatalf("tui never started: %s", o)
	}
	time.Sleep(100 * time.Millisecond)
	cmd.Process.Signal(syscall.SIGTERM)
	werr := cmd.Wait()
	time.Sleep(300 * time.Millisecond)
	c, _ := os.ReadFile(filepath.Join(bin, "calls"))
	t.Logf("calls: %q", c)
	tuiAlive := syscall.Kill(pid, 0) == nil
	gwAlive := gw != 0 && syscall.Kill(gw, 0) == nil
	t.Logf("launcher exit=%v; tui %d alive=%v; foreground gateway %d alive=%v", werr, pid, tuiAlive, gw, gwAlive)
	syscall.Kill(pid, syscall.SIGKILL)
	if gw != 0 {
		syscall.Kill(gw, syscall.SIGKILL)
	}
	if tuiAlive || gwAlive {
		t.Errorf("RED: SIGTERM to `oaica launch openclaw` leaves its children running")
	}
}
