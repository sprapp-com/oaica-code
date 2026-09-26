package launch

// round25_cline_legacy_endpoint_integrity_test.go — the second of Cline's two
// stores was repointed without the guard the first one has (2026-09-27 audit,
// round 25).
//
// "ollama" is Cline's own provider id as well as the key oaica writes under, and
// both of Cline's documents hold an endpoint for it. writeClineProvidersConfig
// refuses to take over an entry whose endpoint oaica did not write
// (clineEndpointWasOurs, round 21) — writeClineLegacyGlobalState did not, and
// rewrote globalState.json's ollamaBaseUrl, actMode/planModeOllamaBaseUrl and
// the model id unconditionally. A user whose Cline is pointed at their own
// Ollama server (a LAN box, or ollama.com with a key) had that endpoint and
// model id silently replaced by the local daemon's on an unrelated `oaica launch
// cline`, and the two documents then disagreed about a launch that had not
// happened.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClineRefusesToRepointTheLegacyStatesOwnOllamaEndpoint(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	legacyPath := clineLegacyGlobalStatePath(home)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := `{"ollamaBaseUrl":"https://lan.example:11434","actModeApiProvider":"ollama","actModeOllamaModelId":"their-lan-model","actModeOllamaBaseUrl":"https://lan.example:11434","planModeApiProvider":"ollama","planModeOllamaModelId":"their-lan-model","planModeOllamaBaseUrl":"https://lan.example:11434"}`
	if err := os.WriteFile(legacyPath, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	err := (&Cline{}).Edit([]LaunchModel{fallbackLaunchModel("llama3.2")})
	if err == nil {
		t.Fatal("Cline.Edit overwrote globalState.json's own ollama endpoint: the user's Cline now talks to the local daemon with a model id of oaica's choosing")
	}
	if !strings.Contains(err.Error(), "lan.example") {
		t.Errorf("the refusal does not say which endpoint it found: %v", err)
	}

	data, readErr := os.ReadFile(legacyPath)
	if readErr != nil {
		t.Fatal(readErr)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"ollamaBaseUrl", "actModeOllamaBaseUrl", "planModeOllamaBaseUrl"} {
		if doc[key] != "https://lan.example:11434" {
			t.Errorf("globalState %s = %v, want the user's own endpoint left alone", key, doc[key])
		}
	}
	for _, key := range []string{"actModeOllamaModelId", "planModeOllamaModelId"} {
		if doc[key] != "their-lan-model" {
			t.Errorf("globalState %s = %v, want the model id left alone", key, doc[key])
		}
	}
}

// Control: a legacy state naming an endpoint oaica itself writes — the daemon,
// including a since-moved host's documented default — is still configured, and
// so is one that records no endpoint at all (Cline's own default is the daemon).
func TestClineStillConfiguresItsOwnLegacyOllamaEndpoint(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	legacyPath := clineLegacyGlobalStatePath(home)
	if err := os.MkdirAll(filepath.Dir(legacyPath), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, seed := range []string{
		`{"ollamaBaseUrl":"http://127.0.0.1:11434","actModeApiProvider":"ollama","actModeOllamaBaseUrl":"http://127.0.0.1:11434"}`,
		`{"actModeApiProvider":"ollama","actModeOllamaModelId":"llama3"}`,
	} {
		if err := os.WriteFile(legacyPath, []byte(seed), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := (&Cline{}).Edit([]LaunchModel{fallbackLaunchModel("llama3.2")}); err != nil {
			t.Errorf("Cline.Edit over a daemon-named legacy state (%s) = %v, want it configured", seed, err)
			continue
		}
		data, err := os.ReadFile(legacyPath)
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]any
		if err := json.Unmarshal(data, &doc); err != nil {
			t.Fatal(err)
		}
		if doc["actModeOllamaModelId"] != "llama3.2" {
			t.Errorf("actModeOllamaModelId = %v, want llama3.2", doc["actModeOllamaModelId"])
		}
	}
}
