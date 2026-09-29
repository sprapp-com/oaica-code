// round118_leg2_kimi_test.go — F118-L2-2: the archived kimi-cli door scrubs like the env-config door (2026-09-29 audit, round 118).

package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func round118Kimi(t *testing.T, help string) string {
	bin := t.TempDir()
	envOut := filepath.Join(bin, "env")
	os.WriteFile(filepath.Join(bin, "kimi"), []byte("#!/bin/sh\nif [ \"$1\" = --help ]; then echo '"+help+"'; exit 0; fi\nenv > "+envOut+"\n"), 0o755)
	t.Setenv("PATH", bin+":"+os.Getenv("PATH"))
	t.Setenv("OAICA_API_KEY", "ROUTER-SECRET-p118")
	t.Setenv("OPENROUTER_API_KEY", "OPENROUTER-SECRET-p118")
	if err := (&Kimi{}).Run("qwen3:8b", nil, nil); err != nil {
		t.Logf("run err: %v", err)
	}
	b, _ := os.ReadFile(envOut)
	t.Logf("child env bytes=%d, KIMI_MODEL_API_KEY present=%v", len(b), strings.Contains(string(b), "KIMI_MODEL_API_KEY="))
	var leaked []string
	for _, l := range strings.Split(string(b), "\n") {
		if strings.Contains(l, "SECRET-p118") {
			leaked = append(leaked, l)
		}
	}
	return strings.Join(leaked, " ")
}

func TestRound118KimiArchivedEnv(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	cur := round118Kimi(t, "Usage: kimi [--model]")
	leg := round118Kimi(t, "Usage: kimi --config-file PATH --config JSON")
	t.Logf("kimi code CLI (env-config) child sees: %q", cur)
	t.Logf("archived kimi-cli (legacy) child sees: %q", leg)
	if cur == "" && leg != "" {
		t.Errorf("RED: the env-config door scrubs other credentials; the archived-CLI door hands them to the child")
	}
}
