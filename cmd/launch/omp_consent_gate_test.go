package launch

// omp_consent_gate_test.go — the omp half of ENTERPRISE.md row 6's promise:
// "Decline the prompt, or install the agent yourself first, or from your own
// npm mirror — none of these installers run unprompted."
//
// `oaica launch omp` called `ensureOMPWebSearchPlugin`, which reached
// `omp plugin install @ollama/pi-web-search` — a package fetched from npm —
// with no prompt at all, for a plugin the user never asked for. Every other
// agent's installer (pi, cline, openclaw, opencode, kimi, dsh) asks first
// (2026-09-26 audit). The prompt is what makes the row's "decline" advice
// actionable, so the test drives the path with a DECLINING prompt and asserts
// the plugin was never installed.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// ompStub writes an `omp` executable that logs every argv, and returns the log
// path. `plugin list` prints nothing and succeeds, i.e. "no plugins installed".
func ompStub(t *testing.T, dir string) string {
	t.Helper()
	logPath := filepath.Join(dir, "omp.log")
	stub := "#!/bin/sh\n" +
		"echo \"$@\" >> " + logPath + "\n" +
		"if [ \"$1\" = \"plugin\" ] && [ \"$2\" = \"list\" ]; then exit 0; fi\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "omp"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	return logPath
}

func TestOMPPluginInstallIsBehindAConsentGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the omp stub is a POSIX shell script")
	}

	for _, tc := range []struct {
		name     string
		approve  bool
		wantVerb string
	}{
		{name: "declined", approve: false},
		{name: "approved", approve: true, wantVerb: "install"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			setLaunchTestHome(t, dir)
			t.Setenv("PATH", dir) // only the stub is on PATH
			logPath := ompStub(t, dir)

			prompts := 0
			old := DefaultConfirmPrompt
			DefaultConfirmPrompt = func(prompt string, _ ConfirmOptions) (bool, error) {
				prompts++
				return tc.approve, nil
			}
			t.Cleanup(func() { DefaultConfirmPrompt = old })

			ensureOMPWebSearchPlugin(filepath.Join(dir, "omp"))

			if prompts == 0 {
				t.Fatalf("the plugin install path ran with no prompt shown at all — ENTERPRISE.md row 6 promises 'none of these installers run unprompted'")
			}

			ran, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("the omp stub never ran, so this case proves nothing: %v", err)
			}
			installed := false
			for _, line := range strings.Split(string(ran), "\n") {
				if strings.Contains(line, "plugin install") {
					installed = true
				}
			}
			if !tc.approve && installed {
				t.Errorf("a DECLINED prompt still installed the plugin:\n%s", ran)
			}
			if tc.approve && !installed {
				t.Errorf("an APPROVED prompt did not install the plugin, so this test is not exercising the install path:\n%s", ran)
			}
		})
	}
}
