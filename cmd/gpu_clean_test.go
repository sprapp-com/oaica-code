package cmd

import (
	"os"
	"strings"
	"testing"
)

func TestMatchesOrphanPattern(t *testing.T) {
	cases := []struct {
		cmd  string
		want bool
	}{
		{"VLLM::EngineCore", true},
		{"python3 -c VLLM::EngineCore extra args", true},
		{"python3 -m vllm.entrypoints.openai.api_server --model x", false},
		{"bash -c ls", false},
		{"", false},
	}
	for _, c := range cases {
		if got := matchesOrphanPattern(c.cmd); got != c.want {
			t.Errorf("matchesOrphanPattern(%q) = %v, want %v", c.cmd, got, c.want)
		}
	}
}

func TestProcInfo_SelfProcess(t *testing.T) {
	// The test binary itself is a live, well-known process — proves the
	// /proc/PID/stat and /proc/PID/cmdline parsing works against a real
	// process without needing a GPU or fuser present.
	pid := os.Getpid()
	ppid, cmdLine, ok := procInfo(pid)
	if !ok {
		t.Fatalf("procInfo(%d) reported not-found for the running test process", pid)
	}
	if ppid <= 0 {
		t.Errorf("procInfo(%d) ppid = %d, want > 0", pid, ppid)
	}
	if cmdLine == "" {
		t.Errorf("procInfo(%d) cmdLine is empty", pid)
	}
}

func TestProcInfo_NonexistentPID(t *testing.T) {
	// PID 1 << 30 is never a real process; a huge unlikely-to-exist pid
	// exercises the "process already exited" path deterministically.
	const impossiblePID = 1<<30 - 1
	if _, _, ok := procInfo(impossiblePID); ok {
		t.Errorf("procInfo(%d) reported found for a PID that should not exist", impossiblePID)
	}
}

func TestFindNvidiaDevices_NoPanicWithoutGPU(t *testing.T) {
	// This test box may or may not have /dev/nvidia* nodes — the only
	// contract worth asserting is that it never panics and any returned
	// path actually looks like an nvidia device node.
	devices, err := findNvidiaDevices()
	if err != nil {
		t.Fatalf("findNvidiaDevices: %v", err)
	}
	for _, d := range devices {
		if !strings.HasPrefix(d, "/dev/nvidia") {
			t.Errorf("findNvidiaDevices returned non-device path: %q", d)
		}
	}
}

func TestNvidiaComputeAppsMemory_NoPanicWithoutNvidiaSmi(t *testing.T) {
	// Best-effort: must return an empty map, not error/panic, when
	// nvidia-smi is missing or reports nothing (the common case on a
	// non-GPU dev/CI box).
	m := nvidiaComputeAppsMemory()
	if m == nil {
		t.Error("nvidiaComputeAppsMemory returned nil, want a non-nil (possibly empty) map")
	}
}
