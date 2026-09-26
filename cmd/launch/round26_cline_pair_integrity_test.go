package launch

// round26_cline_pair_integrity_test.go — the refusal arrived after half the pair
// had been published (2026-09-27 audit, round 26).
//
// Cline.Edit publishes TWO documents that describe one selection: providers.json
// and globalState.json, both under both locks, providers first. Round 25 added
// the legacy endpoint guard inside writeClineLegacyGlobalState — the SECOND
// writer — so a legacy document naming the user's own Ollama server refused the
// launch only after providers.json had already been written (or created), with
// the model id of a launch that never happened. The file's own comment states
// the rule this broke: "these two documents are one selection's two halves, so
// they are published together or not at all".

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClineRefusesBeforePublishingEitherHalfOfThePair(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	providersPath := clineProvidersPath(home)
	legacyPath := clineLegacyGlobalStatePath(home)
	if err := os.MkdirAll(filepath.Dir(providersPath), 0o755); err != nil {
		t.Fatal(err)
	}
	// The user's Cline is pointed at their own server, recorded in the legacy
	// document; providers.json holds a previous launch of oaica's own with a
	// different model.
	const providersSeed = `{"version":1,"lastUsedProvider":"ollama","providers":{"ollama":{"settings":{"provider":"ollama","model":"llama3.1","baseUrl":"http://127.0.0.1:11434/v1"}}}}`
	if err := os.WriteFile(providersPath, []byte(providersSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	const legacySeed = `{"ollamaBaseUrl":"https://lan.example:11434","actModeApiProvider":"ollama","actModeOllamaModelId":"their-lan-model","actModeOllamaBaseUrl":"https://lan.example:11434"}`
	if err := os.WriteFile(legacyPath, []byte(legacySeed), 0o600); err != nil {
		t.Fatal(err)
	}

	err := (&Cline{}).Edit([]LaunchModel{fallbackLaunchModel("llama3.2")})
	if err == nil {
		t.Fatal("Cline.Edit overwrote globalState.json's own ollama endpoint")
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
	if settings["model"] != "llama3.1" {
		t.Errorf("providers.json's model = %v after a REFUSED launch, want it left at llama3.1: the refusal runs in the legacy writer, which is the second of the pair, so the first half was published for a launch that never happened", settings["model"])
	}
	if strings.Contains(string(data), "llama3.2") {
		t.Errorf("providers.json carries the refused launch's model:\n%s", data)
	}
}

// The control: when the legacy document IS oaica's to repoint, both halves are
// still written, and the pair agrees.
func TestClineStillPublishesBothHalvesWhenTheLegacyEndpointIsOurs(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	legacyPath := clineLegacyGlobalStatePath(home)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(legacyPath, []byte(`{"ollamaBaseUrl":"http://127.0.0.1:11434","actModeApiProvider":"ollama"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (&Cline{}).Edit([]LaunchModel{fallbackLaunchModel("llama3.2")}); err != nil {
		t.Fatalf("Cline.Edit over a daemon-named legacy state: %v", err)
	}
	for _, path := range []string{clineProvidersPath(home), legacyPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read %s: %v", path, err)
		}
		if !strings.Contains(string(data), "llama3.2") {
			t.Errorf("%s does not name the launched model:\n%s", path, data)
		}
	}
}
