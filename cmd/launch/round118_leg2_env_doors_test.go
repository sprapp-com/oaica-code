// round118_leg2_env_doors_test.go — F118-L2-3: poolside hands its child one key and scrubs every other credential (2026-09-29 audit, round 118).

package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func round118EnvDoor(t *testing.T, name string, run func() error) string {
	bin := t.TempDir()
	envOut := filepath.Join(bin, "env")
	os.WriteFile(filepath.Join(bin, name), []byte("#!/bin/sh\nif [ \"$1\" = --help ] || [ \"$1\" = --version ]; then echo 'Usage'; exit 0; fi\nenv > "+envOut+"\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("OAICA_API_KEY", "ROUTER-SECRET-p118")
	t.Setenv("OPENROUTER_API_KEY", "OPENROUTER-SECRET-p118")
	if err := run(); err != nil {
		t.Logf("%s run err: %v", name, err)
	}
	b, _ := os.ReadFile(envOut)
	var leaked []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "SECRET-p118") {
			leaked = append(leaked, l)
		}
	}
	t.Logf("%s: child env bytes=%d leaked=%q", name, len(b), leaked)
	return strings.Join(leaked, " ")
}

func TestRound118PoolsideScrubsOtherCredentials(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	cp := round118EnvDoor(t, "copilot", func() error { return (&Copilot{}).Run("qwen3:8b", nil, nil) })
	ps := round118EnvDoor(t, "pool", func() error { return (&Poolside{}).Run("qwen3:8b", nil, nil) })
	if cp == "" && ps != "" {
		t.Errorf("RED: copilot scrubs other credentials; poolside hands them to the child")
	}
}
