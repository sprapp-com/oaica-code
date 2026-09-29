package server

// round109_leg1_request_log_replay_script_test.go — leg 1, round 109 (2026-09-29
// audit), F109-L1-2.
//
// OLLAMA_DEBUG_LOG_REQUESTS writes a curl replay script for each request. Its URL
// came from the client's Host header and its Content-Type header from the client's,
// both interpolated with Go's %q — which escapes a quote and a backslash and leaves
// `$(...)` and a backtick live inside the double quotes of a /bin/sh script — and
// the method, a valid HTTP token that may contain a backtick or `$`, was not
// quoted at all. Any client that could reach the port chose text the operator's
// shell ran when they replayed the script. Every client-supplied field is now
// single-quoted.

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

func TestMine109TheReplayScriptRunsNothingTheClientChose(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "PWNED")
	bin := t.TempDir()
	// A stub curl that records its arguments one per line.
	stub := "#!/bin/sh\nfor a in \"$@\"; do printf '%s\\n' \"$a\" >> \"" + filepath.Join(bin, "args") + "\"; done\n"
	if err := os.WriteFile(filepath.Join(bin, "curl"), []byte(stub), 0o755); err != nil {
		t.Fatal(err)
	}
	l := &inferenceRequestLogger{dir: dir}
	hostile := "$(touch " + marker + ")"
	l.log("/api/chat", "P`touch "+marker+"`", "http", "a"+hostile+"b", "text/plain"+hostile+"`touch "+marker+"`'; touch "+marker+"; '", []byte(`{}`))

	scripts, _ := filepath.Glob(filepath.Join(dir, "*_request.sh"))
	if len(scripts) != 1 {
		t.Fatalf("premise: %d replay scripts written", len(scripts))
	}
	cmd := exec.Command("sh", scripts[0])
	cmd.Env = append(os.Environ(), "PATH="+bin+":"+os.Getenv("PATH"))
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("premise: the replay script failed to run: %v\n%s", err, out)
	}
	if _, err := os.Stat(marker); err == nil {
		t.Errorf("replaying the script ran a command the client chose (2026-09-29 audit, round 109, F109-L1-2)")
	}
	got, _ := os.ReadFile(filepath.Join(bin, "args"))
	if !strings.Contains(string(got), "a"+hostile+"b") {
		t.Errorf("curl was handed %q, want the Host value passed through as text, unexpanded (2026-09-29 audit, round 109, F109-L1-2)", got)
	}
}
