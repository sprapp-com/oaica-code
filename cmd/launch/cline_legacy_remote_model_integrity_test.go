package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func readClineJSONFile(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse %s: %v", path, err)
	}
	return cfg
}

// The legacy globalState.json keys are read as "the Ollama server this model
// comes from". For a user-remote model, recording the daemon's root there told
// Cline to talk to 127.0.0.1 for a model only the remote serves (2026-09-26
// audit) — and recording the namespaced picker name told it the model is
// called something the remote does not know.
func TestClineLegacyGlobalStateRecordsRemoteModelTruth(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	t.Setenv("OAICA_REMOTES_FILE", writeDescriptorRemotesFile(t))
	t.Setenv("KAT_KEY", "sk-kat")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")
	// The bare-id sweep would otherwise probe the unreachable hosts in the
	// remotes file above on every non-remote name lookup.
	stubBareIndex(t, map[string][]string{})

	legacyPath := clineLegacyGlobalStatePath(home)
	providersPath := clineProvidersPath(home)

	t.Run("remote model does not claim the daemon serves it", func(t *testing.T) {
		os.RemoveAll(filepath.Join(home, ".cline"))

		if err := (&Cline{}).Edit(testLaunchModels("kat/kat-coder")); err != nil {
			t.Fatalf("Edit returned error: %v", err)
		}

		state := readClineJSONFile(t, legacyPath)
		for _, key := range []string{"ollamaBaseUrl", "actModeOllamaBaseUrl", "planModeOllamaBaseUrl"} {
			got, _ := state[key].(string)
			if strings.Contains(got, "127.0.0.1") {
				t.Fatalf("%s = %q, want the remote's base URL, not the daemon's", key, got)
			}
			if got != "http://192.168.0.50:8080" {
				t.Fatalf("%s = %q, want http://192.168.0.50:8080", key, got)
			}
		}
		for _, key := range []string{"actModeOllamaModelId", "planModeOllamaModelId"} {
			if got, _ := state[key].(string); got != "kat-coder" {
				t.Fatalf("%s = %q, want the remote's own model id kat-coder", key, got)
			}
		}
		if state["actModeApiProvider"] != clineLaunchProvider || state["planModeApiProvider"] != clineLaunchProvider {
			t.Fatalf("provider = %v/%v, want %s", state["actModeApiProvider"], state["planModeApiProvider"], clineLaunchProvider)
		}

		// The modern config already records the remote triple; the legacy file
		// must not disagree with it.
		providers := readClineJSONFile(t, providersPath)
		entries, _ := providers["providers"].(map[string]any)
		provider, _ := entries[clineLaunchProvider].(map[string]any)
		settings, _ := provider["settings"].(map[string]any)
		if settings["baseUrl"] != "http://192.168.0.50:8080/v1" || settings["model"] != "kat-coder" {
			t.Fatalf("providers.json = %v, want the remote base URL and bare model id", settings)
		}
	})

	t.Run("a later local launch restores the daemon shape", func(t *testing.T) {
		if err := (&Cline{}).Edit(testLaunchModels("kimi-k2.5:cloud")); err != nil {
			t.Fatalf("Edit returned error: %v", err)
		}

		state := readClineJSONFile(t, legacyPath)
		for _, key := range []string{"ollamaBaseUrl", "actModeOllamaBaseUrl", "planModeOllamaBaseUrl"} {
			if got, _ := state[key].(string); got != "http://127.0.0.1:11434" {
				t.Fatalf("%s = %q, want the daemon's root after a local launch", key, got)
			}
		}
		for _, key := range []string{"actModeOllamaModelId", "planModeOllamaModelId"} {
			if got, _ := state[key].(string); got != "kimi-k2.5:cloud" {
				t.Fatalf("%s = %q, want the picker name after a local launch", key, got)
			}
		}
	})
}
