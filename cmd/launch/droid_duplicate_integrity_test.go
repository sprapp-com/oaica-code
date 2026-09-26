package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// droidWriteRemotes writes a remotes.json holding one user remote ("ds" on a
// LAN host) whose inline api_key is token, and points OAICA_REMOTES_FILE at it.
// The token is a parameter because the merge has to survive a rotated one: the
// entry it wrote last time holds the OLD token, and that must not be what makes
// it unrecognisable.
func droidWriteRemotes(t *testing.T, path, token string) {
	t.Helper()
	content := fmt.Sprintf(`{"remotes":[{"name":"ds","base_url":"http://10.9.9.9:8088","api_key":%q}]}`, token)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

// droidCustomModels reads back the customModels array as written.
func droidCustomModels(t *testing.T, settingsPath string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("reading %s: %v", settingsPath, err)
	}
	var settings map[string]any
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings file is not valid JSON: %v", err)
	}
	raw, _ := settings["customModels"].([]any)
	entries := make([]map[string]any, 0, len(raw))
	for _, r := range raw {
		m, ok := r.(map[string]any)
		if !ok {
			t.Fatalf("customModels entry is not an object: %T (%v)", r, r)
		}
		entries = append(entries, m)
	}
	return entries
}

// droidEntriesFor counts the entries stored under an upstream model id.
func droidEntriesFor(entries []map[string]any, model string) []map[string]any {
	var found []map[string]any
	for _, e := range entries {
		if e["model"] == model {
			found = append(found, e)
		}
	}
	return found
}

// droidEntryByID returns the entry with the given id, or nil.
func droidEntryByID(entries []map[string]any, id string) map[string]any {
	for _, e := range entries {
		if e["id"] == id {
			return e
		}
	}
	return nil
}

// droidForeignEntry is a model the user added to ~/.factory/settings.json
// themselves, pointed at a service oaica knows nothing about. Nothing oaica
// writes may disturb it — extra fields included.
const droidForeignEntry = `{
  "model": "gpt-4",
  "displayName": "GPT-4",
  "baseUrl": "https://api.openai.com/v1",
  "apiKey": "sk-user",
  "provider": "openai",
  "maxOutputTokens": 4096,
  "supportsImages": true,
  "id": "user-gpt-4",
  "index": 7,
  "userField": "keep me"
}`

// droidSettingsPath creates ~/.factory/settings.json under the test HOME,
// pre-seeded with droidForeignEntry.
func droidSettingsPath(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".factory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "settings.json")
	seed := `{"customModels":[` + droidForeignEntry + `],"theme":"dark"}`
	if err := os.WriteFile(path, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestDroidEdit_RemoteModelReplacedNotDuplicated is the regression test for the
// 2026-09-26 audit finding: a user-remote-backed launch appended a fresh
// customModels entry on every launch instead of updating the one it owns.
//
// The entry oaica writes for a user remote carries the remote's token in
// apiKey (Droid sends that field as the request's bearer), and the merge
// recognised its own entries by apiKey == "ollama" alone — so the previous
// entry looked like a foreign one, was preserved, and the model ended up
// listed twice, then three times, with the credential going stale in all but
// the last copy.
func TestDroidEdit_RemoteModelReplacedNotDuplicated(t *testing.T) {
	d := &Droid{}
	home := t.TempDir()
	setTestHome(t, home)
	// The fixture remote is unreachable on purpose; nothing here resolves the
	// remote by a bare id, so the sweep is stubbed out of caution.
	stubBareIndex(t, map[string][]string{})

	remotesPath := filepath.Join(home, "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", remotesPath)
	droidWriteRemotes(t, remotesPath, "sk-first")

	settingsPath := droidSettingsPath(t, home)

	if err := d.Edit(testLaunchModels("ds/deepseek-chat")); err != nil {
		t.Fatalf("first launch: %v", err)
	}

	entries := droidCustomModels(t, settingsPath)
	if len(entries) != 2 {
		t.Fatalf("after the first launch customModels holds %d entries, want 2 (the remote model + the user's own): %v", len(entries), entries)
	}
	wrote := droidEntriesFor(entries, "deepseek-chat")
	if len(wrote) != 1 {
		t.Fatalf("after the first launch: %d entries for deepseek-chat, want 1: %v", len(wrote), entries)
	}
	if got := wrote[0]["baseUrl"]; got != "http://10.9.9.9:8088/v1" {
		t.Errorf("entry baseUrl = %v, want the remote's own /v1 (the launch must bypass the daemon)", got)
	}
	if got := wrote[0]["apiKey"]; got != "sk-first" {
		t.Errorf("entry apiKey = %v, want the remote's token", got)
	}

	// The remote's token rotates between launches: the stored entry is now both
	// stale and — for a merge that keys on apiKey — indistinguishable from a
	// foreign one.
	droidWriteRemotes(t, remotesPath, "sk-second")
	if err := d.Edit(testLaunchModels("ds/deepseek-chat")); err != nil {
		t.Fatalf("re-launch: %v", err)
	}

	entries = droidCustomModels(t, settingsPath)
	if got := len(droidEntriesFor(entries, "deepseek-chat")); got != 1 {
		t.Fatalf("after the re-launch: %d entries for deepseek-chat, want exactly 1 — the launch must update the entry it owns, not append another: %v", got, entries)
	}
	if len(entries) != 2 {
		t.Fatalf("after the re-launch customModels holds %d entries, want 2 (the remote model + the user's own): %v", len(entries), entries)
	}
	ours := droidEntriesFor(entries, "deepseek-chat")[0]
	if got := ours["apiKey"]; got != "sk-second" {
		t.Errorf("the surviving entry was not refreshed with the rotated token: apiKey = %v, want sk-second", got)
	}
	if got := ours["id"]; got != "custom:ds/deepseek-chat-0" {
		t.Errorf("surviving entry id = %v, want custom:ds/deepseek-chat-0", got)
	}
	if got := ours["index"]; got != float64(0) {
		t.Errorf("surviving entry index = %v, want 0", got)
	}

	// The user's own entry is untouched, in place and byte-for-byte intact.
	kept := droidEntryByID(entries, "user-gpt-4")
	if kept == nil {
		t.Fatalf("the user's own customModels entry was not preserved: %v", entries)
	}
	if kept["apiKey"] != "sk-user" || kept["userField"] != "keep me" || kept["index"] != float64(7) || kept["provider"] != "openai" {
		t.Errorf("the user's own entry was rewritten: %v", kept)
	}
	if entries[0]["id"] != "custom:ds/deepseek-chat-0" {
		t.Errorf("oaica's entries are no longer written first: %v", entries)
	}

	// The read path answers with the picker name the launch saved, so the
	// launcher can tell the live config is the one it wrote.
	if got := d.Models(); !slices.Equal(got, []string{"ds/deepseek-chat"}) {
		t.Errorf("Models() = %v, want [ds/deepseek-chat]", got)
	}
}

// TestDroidEdit_UnselectedModelsDoNotAccumulate pins the other half of the same
// ownership rule: an entry oaica wrote for a model this launch does NOT select
// is still oaica's, and goes — exactly as a daemon-shaped entry (apiKey
// "ollama") for an unselected model has always gone. Without it a user moving
// between remote models collects one stale entry per model ever launched, each
// holding a dead credential.
func TestDroidEdit_UnselectedModelsDoNotAccumulate(t *testing.T) {
	d := &Droid{}
	home := t.TempDir()
	setTestHome(t, home)
	// The daemon-model launches below name a bare id, which would otherwise
	// sweep this unreachable fixture remote (fetchRemoteModels' 2s timeout).
	stubBareIndex(t, map[string][]string{})

	remotesPath := filepath.Join(home, "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", remotesPath)
	droidWriteRemotes(t, remotesPath, "sk-ds")

	settingsPath := droidSettingsPath(t, home)

	if err := d.Edit(testLaunchModels("ds/deepseek-chat", "ds/deepseek-reasoner")); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if entries := droidCustomModels(t, settingsPath); len(entries) != 3 {
		t.Fatalf("after the first launch customModels holds %d entries, want 3: %v", len(entries), entries)
	}

	// Second launch drops one of the two remote models.
	if err := d.Edit(testLaunchModels("ds/deepseek-reasoner")); err != nil {
		t.Fatalf("second launch: %v", err)
	}
	entries := droidCustomModels(t, settingsPath)
	if got := len(droidEntriesFor(entries, "deepseek-chat")); got != 0 {
		t.Errorf("the unselected remote model is still configured (%d entries): %v", got, entries)
	}
	if got := len(droidEntriesFor(entries, "deepseek-reasoner")); got != 1 {
		t.Errorf("%d entries for the selected remote model, want 1: %v", got, entries)
	}
	if len(entries) != 2 {
		t.Errorf("customModels holds %d entries, want 2 (the selected model + the user's own): %v", len(entries), entries)
	}

	// Third launch goes back to a daemon model: the remote entry goes, and the
	// daemon entry for it is written once — the daemon path never duplicated,
	// and now neither does the remote one.
	if err := d.Edit(testLaunchModels("llama3.2")); err != nil {
		t.Fatalf("third launch: %v", err)
	}
	if err := d.Edit(testLaunchModels("llama3.2")); err != nil {
		t.Fatalf("fourth launch: %v", err)
	}
	entries = droidCustomModels(t, settingsPath)
	if got := len(droidEntriesFor(entries, "llama3.2")); got != 1 {
		t.Errorf("%d entries for the daemon model after two launches, want 1: %v", got, entries)
	}
	if len(droidEntriesFor(entries, "deepseek-reasoner")) != 0 {
		t.Errorf("the remote entry survived a daemon launch: %v", entries)
	}
	if len(entries) != 2 {
		t.Errorf("customModels holds %d entries, want 2 (the daemon model + the user's own): %v", len(entries), entries)
	}
	if droidEntryByID(entries, "user-gpt-4") == nil {
		t.Errorf("the user's own entry was not preserved: %v", entries)
	}
}
