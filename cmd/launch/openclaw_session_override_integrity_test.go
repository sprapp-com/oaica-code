package launch

// openclaw_session_override_integrity_test.go — an override deletion was thrown
// away and the sessions file left byte-identical (2026-09-26 audit, sixth
// round).
//
// clearSessionModelOverride exists for exactly one purpose: when `oaica launch
// openclaw` sets a new primary model, a per-session `modelOverride` cached in
// ~/.openclaw/agents/main/sessions/sessions.json would keep shadowing it on the
// next TUI launch, so the override is deleted. The delete branch never set
// `changed`, and `if !changed { return }` sits below it — so when a session's
// `model` ALREADY equalled the new primary (the other branch does not co-fire),
// the deletion was discarded in memory and the stale override survived on disk,
// shadowing the primary the user had just chosen.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// writeOpenclawSessions lays out the sessions file clearSessionModelOverride
// reads and returns HOME.
func writeOpenclawSessions(t *testing.T, sessions map[string]map[string]any) (string, string) {
	t.Helper()
	home := t.TempDir()
	t.Setenv("HOME", home)
	path := filepath.Join(home, ".openclaw", "agents", "main", "sessions", "sessions.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(sessions, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	return home, path
}

func readOpenclawSessions(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("the sessions file is gone: %v", err)
	}
	var out map[string]map[string]any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("the sessions file is no longer parseable: %v\n%s", err, b)
	}
	return out
}

// TestClearSessionModelOverrideRemovesAnOverrideAlone is the case the function
// exists for: the session's model is already the new primary, so the only thing
// to change is the cached override.
func TestClearSessionModelOverrideRemovesAnOverrideAlone(t *testing.T) {
	_, path := writeOpenclawSessions(t, map[string]map[string]any{
		"sess1": {
			"model":            "new-model",
			"modelOverride":    "old-model",
			"providerOverride": "ollama",
		},
	})

	clearSessionModelOverride("new-model")

	got := readOpenclawSessions(t, path)["sess1"]
	if v, ok := got["modelOverride"]; ok {
		t.Errorf("modelOverride = %q survived the call — the next TUI launch is shadowed by it, so the primary the user just selected does not take effect, which is the single thing this function is called at Edit time to prevent (file: %v)",
			v, got)
	}
	if v, ok := got["providerOverride"]; ok {
		t.Errorf("providerOverride = %q survived alongside the model override it belongs to (file: %v)", v, got)
	}
	if got["model"] != "new-model" {
		t.Errorf("model = %q, want the primary left alone", got["model"])
	}
}

// The control: the other branch still works, so the fix cannot be "always
// write" — a file rewritten when nothing changed is its own defect for a file
// the openclaw daemon also owns at runtime.
func TestClearSessionModelOverrideStillRewritesAStaleModel(t *testing.T) {
	_, path := writeOpenclawSessions(t, map[string]map[string]any{
		"sess1": {"model": "old-model"},
	})

	clearSessionModelOverride("new-model")

	if got := readOpenclawSessions(t, path)["sess1"]; got["model"] != "new-model" {
		t.Errorf("model = %q, want it moved to the primary", got["model"])
	}
}

// And a session that already matches the primary must not be rewritten at all.
func TestClearSessionModelOverrideLeavesAMatchingSessionUntouched(t *testing.T) {
	_, path := writeOpenclawSessions(t, map[string]map[string]any{
		"sess1": {"model": "new-model"},
	})
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	clearSessionModelOverride("new-model")

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if after.ModTime().After(before.ModTime()) {
		t.Errorf("the sessions file was rewritten for a session that already matched the primary (%s -> %s) — the openclaw daemon owns this file at runtime, so a needless write is a chance to clobber it", before.ModTime(), after.ModTime())
	}
}
