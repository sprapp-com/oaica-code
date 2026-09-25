package launch

// pi_consent_gate_test.go — docs/ENTERPRISE.md's network table, row 6, gives
// the user this promise for the npm-backed agents:
//
//	"Decline the prompt, or install the agent yourself first, or from your own
//	 npm mirror — none of these installers run unprompted."
//
// `oaica launch pi` had three npm-writing paths and only the last prompted:
// the legacy-package migration and the "official package present, `pi` is
// merely not on PATH" reinstall both ran `npm install -g` with no prompt, so
// declining a prompt the user was never shown could not keep npm out
// (2026-09-26 audit). This test drives every path with a DECLINING prompt and
// asserts npm was never written to — the promise, executable rather than
// asserted in prose.

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// piConsentCase is one way `oaica launch pi` can find the machine, i.e. one
// path through ensurePiInstalled.
type piConsentCase struct {
	name string
	// installed lists the packages `npm ls -g <pkg>` reports as present.
	installed []string
	// piOnPath puts a `pi` stub on PATH, which is what selects the branch
	// that runs BEFORE the "pi is not installed" one.
	piOnPath bool
}

func TestPiNpmMutationsAreBehindAConsentGate(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the npm stub is a POSIX shell script")
	}

	cases := []piConsentCase{
		{name: "legacy package, pi on PATH", installed: []string{piLegacyNpmPackage}, piOnPath: true},
		{name: "legacy package, pi not on PATH", installed: []string{piLegacyNpmPackage}},
		{name: "official package, pi not on PATH", installed: []string{piNpmPackage}},
		{name: "nothing installed", installed: nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			setLaunchTestHome(t, dir)
			t.Setenv("PATH", dir) // only the stubs below are on PATH

			logPath := filepath.Join(dir, "npm.log")
			// The packages this case's `npm ls -g <pkg>` reports as present.
			// A `case` and not a `grep` against a list file: PATH is the temp
			// dir, so the stub can only use shell builtins.
			var arms strings.Builder
			for _, pkg := range tc.installed {
				fmt.Fprintf(&arms, "    %s) printf '{\"name\":\"lib\",\"dependencies\":{\"%%s\":{\"version\":\"1.0.0\"}}}\n' \"$3\"; exit 0 ;;\n", pkg)
			}
			// One stub serves every case: it records every argv, and answers
			// the package probes from the arm list rather than from the real
			// npm (which may not even be installed on the test host).
			stub := fmt.Sprintf(`#!/bin/sh
echo "$@" >> %q
if [ "$1" = "ls" ] && [ "$2" = "-g" ]; then
  case "$3" in
%s  esac
  printf '{"name":"lib"}\n'
  exit 1
fi
exit 0
`, logPath, arms.String())
			if err := os.WriteFile(filepath.Join(dir, "npm"), []byte(stub), 0o755); err != nil {
				t.Fatal(err)
			}
			if tc.piOnPath {
				if err := os.WriteFile(filepath.Join(dir, "pi"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
					t.Fatal(err)
				}
			}

			// The user is asked and says NO. Every npm write must then be
			// impossible — not merely unconfirmed.
			var prompts []string
			old := DefaultConfirmPrompt
			DefaultConfirmPrompt = func(prompt string, _ ConfirmOptions) (bool, error) {
				prompts = append(prompts, prompt)
				return false, nil
			}
			t.Cleanup(func() { DefaultConfirmPrompt = old })

			if _, err := ensurePiInstalled(); err == nil {
				t.Error("a declined install must stop the launch: `oaica launch pi` cannot run without pi, so returning nil here means the flow fell through to a misleading 'not found on PATH'")
			}
			if len(prompts) == 0 {
				t.Error("npm was reachable with no prompt shown at all — ENTERPRISE.md row 6 promises 'none of these installers run unprompted'")
			}

			ran, err := os.ReadFile(logPath)
			if err != nil {
				t.Fatalf("the npm stub never ran, so this case proves nothing: %v", err)
			}
			for _, line := range strings.Split(string(ran), "\n") {
				if strings.Contains(line, "install -g") || strings.Contains(line, "uninstall -g") {
					t.Errorf("a DECLINED prompt still wrote to npm: %q\nfull npm log:\n%s", line, ran)
				}
			}
		})
	}
}
