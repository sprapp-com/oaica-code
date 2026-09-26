package launch

// cline_user_ollama_provider_integrity_test.go — a launch repointed and
// stripped the user's own Cline Ollama provider (2026-09-27 audit, round 21).
//
// "ollama" is Cline's own provider id as well as the key oaica writes under, and
// writeClineProvidersConfig took that entry over whenever it was present: it
// overwrote settings.baseUrl with the local daemon and DELETED settings.apiKey.
// A user who had configured Cline's Ollama provider against their own server
// (a LAN ollama with OLLAMA_API_KEY, or ollama.com) lost the endpoint and the
// key to an unrelated `oaica launch cline`. Refusing instead, and saying which
// endpoint was found, keeps the launch honest: Cline has one provider under
// that name, so oaica cannot write its own beside it.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClineRefusesToTakeOverTheUsersOwnOllamaProvider(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	providersPath := clineProvidersPath(home)
	if err := os.MkdirAll(filepath.Dir(providersPath), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"version":1,"lastUsedProvider":"ollama","providers":{"ollama":{"settings":{"provider":"ollama","model":"llama3","baseUrl":"https://lan.example:11434/v1","apiKey":"their-lan-key"},"updatedAt":"2026-01-01T00:00:00Z","tokenSource":"manual"}}}`
	if err := os.WriteFile(providersPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	err := (&Cline{}).Edit([]LaunchModel{fallbackLaunchModel("llama3.2")})
	if err == nil {
		t.Fatal("Cline.Edit overwrote the user's own ollama provider: its base URL now names the local daemon and its API key is gone")
	}
	if !strings.Contains(err.Error(), "lan.example") {
		t.Errorf("the refusal does not say which endpoint it found: %v", err)
	}

	data, readErr := os.ReadFile(providersPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	providers, _ := doc["providers"].(map[string]any)
	provider, _ := providers["ollama"].(map[string]any)
	settings, _ := provider["settings"].(map[string]any)
	if settings["baseUrl"] != "https://lan.example:11434/v1" {
		t.Errorf("the provider's base URL was rewritten: %v", settings["baseUrl"])
	}
	if settings["apiKey"] != "their-lan-key" {
		t.Errorf("the provider's API key was destroyed: %v", settings["apiKey"])
	}
}

// Control: a provider entry naming the daemon — Cline's own default, and what
// oaica itself writes — is still taken over and configured.
func TestClineStillConfiguresItsOwnDaemonProvider(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	providersPath := clineProvidersPath(home)
	if err := os.MkdirAll(filepath.Dir(providersPath), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"version":1,"lastUsedProvider":"ollama","providers":{"ollama":{"settings":{"provider":"ollama","model":"llama3","baseUrl":"http://127.0.0.1:11434/v1","apiKey":"bad-migrated-key"},"tokenSource":"manual"}}}`
	if err := os.WriteFile(providersPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (&Cline{}).Edit([]LaunchModel{fallbackLaunchModel("llama3.2")}); err != nil {
		t.Fatalf("Cline.Edit: %v", err)
	}
}
