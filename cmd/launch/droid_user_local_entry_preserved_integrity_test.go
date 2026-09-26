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
