package launch

// claude_native_env_integrity_test.go — the native Claude Code paths handed the
// child an untouched environment, including the two variables that redirect
// Claude Code (2026-09-27 audit, round 21, F14).
//
// `oaica claude-login` and the `claude/<alias>` picker rows exist to run the
// real Claude Code against the user's OWN Anthropic account. A shell that
// exports ANTHROPIC_BASE_URL — to a router, a proxy, another tool — got that
// endpoint used instead, with ANTHROPIC_AUTH_TOKEN attached to it: the exact
// override these paths escape, applied behind the user's back. Both variables
// are now dropped (and named on stderr); ANTHROPIC_API_KEY is kept, because it
// is a native credential the help text promises.

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// claudeEnvDumpPath writes a fake `claude` on PATH that dumps its environment
// to a file and exits, and returns the dump path.
func claudeEnvDumpPath(t *testing.T) string {
	t.Helper()
	binDir := t.TempDir()
	dump := filepath.Join(t.TempDir(), "claude-env.txt")

	if runtime.GOOS == "windows" {
		t.Skip("the native claude paths exec a shell script in this test")
	}
	// PATH below holds only this directory, so the shim has to name env by an
	// absolute path: `env` itself must not be looked up on a PATH the test
	// replaced (and the real `claude`, which that PATH deliberately hides).
	envBin := ""
	for _, candidate := range []string{"/usr/bin/env", "/bin/env"} {
		if _, err := os.Stat(candidate); err == nil {
			envBin = candidate
			break
		}
	}
	if envBin == "" {
		t.Skip("no absolute env(1) to run the shim with")
	}
	name := "claude"
	script := "#!/bin/sh\n" + envBin + " > " + dump + "\n"
	if err := os.WriteFile(filepath.Join(binDir, name), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir)
	return dump
}

func claudeEnvDump(t *testing.T, dump string) map[string]string {
	t.Helper()
	data, err := os.ReadFile(dump)
	if err != nil {
		t.Fatalf("the fake claude did not run (no env dump): %v", err)
	}
	env := map[string]string{}
	for _, line := range strings.Split(string(data), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if ok {
			env[key] = value
		}
	}
	return env
}

func TestClaudeLoginDropsTheRedirectVariables(t *testing.T) {
	setTestHome(t, t.TempDir())
	dump := claudeEnvDumpPath(t)

	t.Setenv("ANTHROPIC_BASE_URL", "https://router.invalid")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-router-token")
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-user")

	if err := RunNative(nil); err != nil {
		t.Fatalf("RunNative: %v", err)
	}

	env := claudeEnvDump(t, dump)
	if got, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Errorf("the child inherited ANTHROPIC_BASE_URL=%q: the native path exists to reach the user's own Anthropic account, and this variable redirects it", got)
	}
	if got, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Errorf("the child inherited ANTHROPIC_AUTH_TOKEN=%q: a token for another endpoint would be sent to the account this path is meant to use", got)
	}
	if got := env["ANTHROPIC_API_KEY"]; got != "sk-ant-user" {
		t.Errorf("ANTHROPIC_API_KEY = %q, want the user's key kept: it is a native credential these paths promise to use", got)
	}
}

func TestClaudeNativePickerRowsDropTheRedirectVariables(t *testing.T) {
	setTestHome(t, t.TempDir())
	dump := claudeEnvDumpPath(t)

	t.Setenv("ANTHROPIC_BASE_URL", "https://router.invalid")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "sk-router-token")

	if err := (&Claude{}).runNative("sonnet", nil); err != nil {
		t.Fatalf("runNative: %v", err)
	}

	env := claudeEnvDump(t, dump)
	if _, ok := env["ANTHROPIC_BASE_URL"]; ok {
		t.Error("the claude/<alias> row's child inherited ANTHROPIC_BASE_URL, so the subscription path was redirected after all")
	}
	if _, ok := env["ANTHROPIC_AUTH_TOKEN"]; ok {
		t.Error("the claude/<alias> row's child inherited ANTHROPIC_AUTH_TOKEN")
	}
}
