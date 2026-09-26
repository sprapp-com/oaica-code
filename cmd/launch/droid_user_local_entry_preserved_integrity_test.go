package launch

// droid_user_local_entry_preserved_integrity_test.go — a model the user added
// to Droid themselves, pointed at their own local Ollama server, was DELETED by
// the next launch (2026-09-27 audit, round 19).
//
// The merge recognised its own entries by apiKey == "ollama" alone. That is
// oaica's daemon marker, but it is also exactly what a user types when they
// point Droid at a server they run themselves — it is what the tool's own
// instructions suggest — and the id they pick is their own. So a hand-made
// entry read as ours: it was dropped from the foreign list, and since the
// launch did not select it, nothing rebuilt it either. The user's model was
// gone from ~/.factory/settings.json, with no message, on a launch that was
// not about it.
//
// The marker is now the pair this file actually writes: the id it generates
// and the model stored beside it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// droidUserLocalEntry is a model the user added by hand: their own id, their
// own server, and the apiKey the daemon marker shares.
const droidUserLocalEntry = `{
  "model": "my-local-llama",
  "displayName": "My local Llama",
  "baseUrl": "http://127.0.0.1:11434/v1",
  "apiKey": "ollama",
  "provider": "generic-chat-completion-api",
  "maxOutputTokens": 8192,
  "supportsImages": false,
  "id": "local-llama-i-added",
  "index": 4,
  "myOwnField": "keep me"
}`

func TestDroidEditKeepsTheUsersOwnLocalOllamaEntry(t *testing.T) {
	d := &Droid{}
	home := t.TempDir()
	setTestHome(t, home)
	stubBareIndex(t, map[string][]string{})

	dir := filepath.Join(home, ".factory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(dir, "settings.json")
	seed := `{"customModels":[` + droidUserLocalEntry + `],"theme":"dark"}`
	if err := os.WriteFile(settingsPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := d.Edit(testLaunchModels("llama3.2")); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	kept := droidEntryByID(droidCustomModels(t, settingsPath), "local-llama-i-added")
	if kept == nil {
		t.Fatalf("the user's own local model is gone from customModels — a launch that did not select it deleted it, because any entry with apiKey %q was treated as oaica's:\n%v", droidDaemonKey, droidCustomModels(t, settingsPath))
	}
	if kept["myOwnField"] != "keep me" {
		t.Errorf("the user's own fields were not preserved: %v", kept)
	}
	if kept["baseUrl"] != "http://127.0.0.1:11434/v1" {
		t.Errorf("the user's endpoint was rewritten: %v", kept)
	}

	// Control: the launch really did write its own entry beside it, so this is
	// not a case of Edit having done nothing at all.
	if len(droidEntriesFor(droidCustomModels(t, settingsPath), "llama3.2")) != 1 {
		t.Errorf("the launched model was not written:\n%v", droidCustomModels(t, settingsPath))
	}
}

// droidLegacyOllamaEntry is an entry the upstream `ollama config droid` wrote,
// in the shape it wrote them until 2026-01-23 (upstream 771d9280e): the same
// daemon apiKey, the model stored beside it, and an id carrying its own
// "-[Ollama]-" segment — the substring its ownership test looked for.
const droidLegacyOllamaEntry = `{
  "model": "gemma2",
  "displayName": "gemma2",
  "baseUrl": "http://localhost:11434/v1",
  "apiKey": "ollama",
  "provider": "generic-chat-completion-api",
  "maxOutputTokens": 64000,
  "supportsImages": false,
  "id": "custom:gemma2-[Ollama]-0",
  "index": 0
}`

// An entry written by that older shape must still read as oaica's own. Round
// 19 narrowed the marker to "the id this file writes", and droidPickerCandidates
// only stripped a trailing "-<digits>", so "custom:gemma2-[Ollama]-0" reversed
// to the picker name "gemma2-[Ollama]", which is not the model stored beside it
// — the entry read as the user's, was preserved, and a SECOND entry for the
// same model was appended: the model appears twice in Droid's picker, the stale
// row never updated or cleaned (2026-09-27 audit, round 20).
func TestDroidEditReplacesALegacyOllamaEntry(t *testing.T) {
	d := &Droid{}
	home := t.TempDir()
	setTestHome(t, home)
	stubBareIndex(t, map[string][]string{})

	dir := filepath.Join(home, ".factory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(dir, "settings.json")
	seed := `{"customModels":[` + droidLegacyOllamaEntry + `],"sessionDefaultSettings":{"model":"custom:gemma2-[Ollama]-0"}}`
	if err := os.WriteFile(settingsPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := d.Edit(testLaunchModels("gemma2")); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	entries := droidCustomModels(t, settingsPath)
	if got := len(droidEntriesFor(entries, "gemma2")); got != 1 {
		t.Errorf("Droid's settings hold %d entries for gemma2, want the one rebuilt entry: an entry written by the older id shape was read as the user's, kept, and a duplicate appended:\n%v", got, entries)
	}
	for _, e := range entries {
		if id, _ := e["id"].(string); strings.Contains(id, "-[Ollama]-") {
			t.Errorf("the superseded id shape is still in the file: %v", e)
		}
	}
	if got := (&Droid{}).Models(); len(got) != 1 || got[0] != "gemma2" {
		t.Errorf("Droid.Models() = %v, want [gemma2]: the picker name read back from the store decides whether the next launch rewrites it", got)
	}
}

// The other half: an entry oaica DID write is still recognised and replaced
// rather than duplicated, including one for a model this launch does not
// select — the pair the marker reads is what this file writes.
func TestDroidStillReplacesItsOwnDaemonEntryForAnUnselectedModel(t *testing.T) {
	d := &Droid{}
	home := t.TempDir()
	setTestHome(t, home)
	stubBareIndex(t, map[string][]string{})

	dir := filepath.Join(home, ".factory")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	settingsPath := filepath.Join(dir, "settings.json")
	seed := `{"customModels":[{"model":"gemma2","displayName":"gemma2","baseUrl":"http://127.0.0.1:11434/v1","apiKey":"ollama","provider":"generic-chat-completion-api","maxOutputTokens":64000,"supportsImages":false,"id":"custom:gemma2-0","index":0}]}`
	if err := os.WriteFile(settingsPath, []byte(seed), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := d.Edit(testLaunchModels("llama3.2")); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	for _, e := range droidCustomModels(t, settingsPath) {
		if e["model"] == "gemma2" {
			t.Errorf("oaica's own entry for a model this launch did not select was kept: a user moving between models collects one stale entry per model ever launched:\n%v", e)
		}
	}
}
