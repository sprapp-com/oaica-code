package launch

// hermes_user_provider_integrity_test.go — a launch deleted the user's own
// Ollama entries from Hermes' config (2026-09-27 audit, round 21).
//
// Two writes did it. Providers: hermesManagedProviderEntry adopted the entry
// under the legacy key "ollama" as the base for oaica's own, and the writer then
// deleted that key — so a provider the user had keyed "ollama" (Hermes' own name
// for Ollama) was consumed and removed. custom_providers: any entry whose name
// was "Ollama", case-insensitively, was dropped, hand-written ones included.
// Both are now ownership tests on the recorded endpoint, the rule
// hermesManagedCurrentModel already uses: only an endpoint oaica writes (the
// daemon's, or a configured remote's) makes an entry ours. Entries that cannot
// be shown to be ours are left exactly as they are.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func TestHermesKeepsTheUsersOwnOllamaEntries(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	dir := filepath.Join(home, ".hermes")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	seed := `providers:
  ollama:
    name: Ollama
    api: https://lan.example:11434/v1
    api_key: their-lan-key
    default_model: their-model
    models:
      - their-model
model:
  provider: ollama
  default: their-model
custom_providers:
  - name: Ollama
    api: https://lan.example:11434/v1
    api_key: their-lan-key
`
	if err := os.WriteFile(configPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeHermesConfig(configPath, "llama3.2", []string{"llama3.2"}); err != nil {
		t.Fatalf("writeHermesConfig: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatalf("config.yaml is no longer YAML: %v\n%s", err, data)
	}

	providers, _ := doc["providers"].(map[string]any)
	user, _ := providers["ollama"].(map[string]any)
	if user == nil {
		t.Error("the user's own `ollama` provider was deleted")
	} else {
		if got, _ := user["api"].(string); got != "https://lan.example:11434/v1" {
			t.Errorf("the user's provider was rewritten: api = %v", got)
		}
		if got, _ := user["api_key"].(string); got != "their-lan-key" {
			t.Errorf("the user's provider key was destroyed: api_key = %v", got)
		}
	}

	if _, ok := providers[hermesProviderKey]; !ok {
		t.Error("oaica's own provider entry was not written")
	}

	for _, raw := range hermesCustomProviders(doc["custom_providers"]) {
		entry, _ := raw.(map[string]any)
		if entry == nil {
			continue
		}
		if name, _ := entry["name"].(string); strings.EqualFold(name, "Ollama") {
			if api, _ := entry["api"].(string); api == "https://lan.example:11434/v1" {
				return
			}
		}
	}
	t.Errorf("the user's hand-written custom provider was deleted: %v", doc["custom_providers"])
}
