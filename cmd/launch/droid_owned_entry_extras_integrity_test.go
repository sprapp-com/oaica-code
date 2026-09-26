package launch

// droid_owned_entry_extras_integrity_test.go — rebuilding an owned entry
// deleted the fields Droid itself had added to it (2026-09-26 audit, round 16).
//
// updateDroidSettings keeps only the entries this integration did NOT write and
// rebuilds the rest from a fixed struct, and writeDroidSettings' own comment
// says why the document is carried as a map: "map preserves unknown fields for
// writing back (including extra fields in model entries)". That holds for a
// foreign entry and not for an owned one. A model oaica registered, and the
// user then tuned in Droid's UI — every field Droid writes into the entry that
// oaica does not model — came back as a bare struct on the next launch, with
// those settings gone.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestDroidKeepsFieldsDroidAddedToAnEntryItOwns(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	settingsPath := filepath.Join(home, ".factory", "settings.json")
	if err := os.MkdirAll(filepath.Dir(settingsPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// The entry oaica writes for a daemon model, plus the fields Droid adds
	// when the user edits that model in its UI. "custom:llama3.2-0" is the id
	// this integration writes, and apiKey "ollama" is its daemon marker, so
	// droidOwnedEntry claims it and the rebuild replaces it.
	seed := `{
	  "customModels": [
	    {
	      "model": "llama3.2",
	      "displayName": "llama3.2",
	      "baseUrl": "http://127.0.0.1:11434/v1",
	      "apiKey": "ollama",
	      "provider": "generic-chat-completion-api",
	      "maxOutputTokens": 64000,
	      "supportsImages": false,
	      "id": "custom:llama3.2-0",
	      "index": 0,
	      "temperature": 0.3,
	      "reasoningEffort": "high"
	    }
	  ],
	  "sessionDefaultSettings": {"model": "custom:llama3.2-0"}
	}`
	if err := os.WriteFile(settingsPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (&Droid{}).Edit(launchModelsFromNames([]string{"llama3.2"})); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}

	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("Edit wrote no settings: %v", err)
	}
	var cfg struct {
		CustomModels []map[string]any `json:"customModels"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("the settings Edit wrote are not JSON: %v", err)
	}
	if len(cfg.CustomModels) != 1 {
		t.Fatalf("expected the one model entry to be rebuilt in place, got %d entries:\n%s", len(cfg.CustomModels), data)
	}
	entry := cfg.CustomModels[0]
	for _, field := range []string{"temperature", "reasoningEffort"} {
		if _, ok := entry[field]; !ok {
			t.Errorf("the entry oaica owns lost %q, a setting the user made in Droid's own UI, on a re-launch:\n%s", field, data)
		}
	}
	// The rebuild still owns its own fields: a stale one must not survive.
	if entry["model"] != "llama3.2" || entry["apiKey"] != droidDaemonKey {
		t.Errorf("the rebuilt entry is not oaica's own: model=%v apiKey=%v\n%s", entry["model"], entry["apiKey"], data)
	}
}
