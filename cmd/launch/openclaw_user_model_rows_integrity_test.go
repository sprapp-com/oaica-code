package launch

// openclaw_user_model_rows_integrity_test.go — a launch replaced every row in
// the Ollama provider's model list with its own selection (2026-09-27 audit,
// round 21).
//
// openclawEditConfig builds newModels from the launch selection alone and
// publishes `ollama["models"] = newModels`. The existing entries are read, but
// only to merge per-id fields into the rows being written; an entry whose id is
// not in this launch's selection is dropped from the file entirely. The Ollama
// provider is the one oaica writes, and it is also the one a user's own local
// models live under, so `oaica launch openclaw` silently deleted every model
// they had added there. OMP's writer already carries the other rows over
// (writeOMPModelsConfig); this is the same carry-over.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestOpenclawKeepsModelRowsItDidNotWrite(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	dir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "openclaw.json")
	seed := `{"models":{"providers":{"ollama":{"baseUrl":"http://127.0.0.1:11434","apiKey":"ollama-local","api":"ollama","models":[{"id":"user/private-model","name":"user/private-model"}]}}}}`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := openclawEditConfig(configPath, filepath.Join(home, ".clawdbot", "clawdbot.json"), []LaunchModel{fallbackLaunchModel("llama3.2")}); err != nil {
		t.Fatalf("openclawEditConfig: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("openclaw.json is no longer JSON: %v", err)
	}
	modelsSection, _ := doc["models"].(map[string]any)
	providers, _ := modelsSection["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	rows, _ := ollama["models"].([]any)

	ids := make(map[string]bool, len(rows))
	for _, raw := range rows {
		if entry, ok := raw.(map[string]any); ok {
			if id, _ := entry["id"].(string); id != "" {
				ids[id] = true
			}
		}
	}
	if !ids["user/private-model"] {
		t.Errorf("the model the user had added to the Ollama provider was deleted by the launch: %v", ids)
	}
	if !ids["llama3.2"] {
		t.Errorf("the launched model is not in the provider's list: %v", ids)
	}
}
