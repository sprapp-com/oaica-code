package launch

// pi_models_reports_picker_names_integrity_test.go — Pi.Models() answered with
// the upstream model id while the picker had handed out "<remote>/<id>"
// (2026-09-26 audit, thirteenth round).
//
// launch.go compares the live config against the saved selection with
// slices.Equal(editor.Models(), models), where models is the list of PICKER
// names the user chose. Pi stores, for a user-remote model, the bare upstream
// id (piModelIDFor), so its Models() answered with a name the launcher never
// saved: liveConfigMatches was false on every run, so every launch took the
// configure path and rewrote the config it had just read. The same defect was
// fixed for droid and hermes (which translate), and is pinned here for pi.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func setPiModelsConfig(t *testing.T, config map[string]any) {
	t.Helper()
	path := filepath.Join(os.Getenv("HOME"), ".pi", "agent", "models.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestPiModelsReportsThePickerNameForARemoteEntry(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	t.Setenv(ollamaCloudEnvKey, "")
	writeRemotes(t, `{"remotes":[{"name":"deepseek","base_url":"https://api.deepseek.com","api_key":"sk-static"}]}`)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

	// Premise: the picker name resolves to this remote, and Pi writes that
	// remote's endpoint as the provider base URL (piProviderBaseURL) with the
	// bare upstream id per entry (piModelIDFor).
	ep, ok := resolveRemoteEndpoint("deepseek/deepseek-chat")
	if !ok {
		t.Fatal("premise: deepseek/deepseek-chat must resolve to the configured remote")
	}
	setPiModelsConfig(t, map[string]any{
		"providers": map[string]any{
			"ollama": map[string]any{
				"api":     piProviderAPI,
				"baseUrl": ep.BaseURL,
				"models": []any{
					map[string]any{"id": "deepseek-chat", "_launch": true},
				},
			},
		},
	})

	got := (&Pi{}).Models()
	if len(got) != 1 || got[0] != "deepseek/deepseek-chat" {
		t.Errorf("Pi.Models() = %v, want [deepseek/deepseek-chat] — the entry stores the bare upstream id, which is a name nothing in the launcher saved, so liveConfigMatches is false on every run and each launch rewrites the config it just read", got)
	}
}

// TestPiEditKeepsARemoteEntry is the write half of the same translation: Edit
// decides which managed entries are still selected by comparing the stored id
// against the selection's PICKER names, and a remote entry stores the bare
// upstream id. The entry therefore never matched, was dropped, and was rebuilt
// from scratch by createConfig — losing every member oaica does not model, on
// every launch of a remote model.
func TestPiEditKeepsARemoteEntry(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/show" {
			fmt.Fprintf(w, `{"capabilities":[],"model_info":{}}`)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	t.Setenv(ollamaCloudEnvKey, "")

	home := t.TempDir()
	setLaunchTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"deepseek","base_url":"https://api.deepseek.com","api_key":"sk-static"}]}`)

	ep, ok := resolveRemoteEndpoint("deepseek/deepseek-chat")
	if !ok {
		t.Fatal("premise: deepseek/deepseek-chat must resolve to the configured remote")
	}

	const (
		upstreamID = "deepseek-chat"
		pickerName = "deepseek/deepseek-chat"
		seedKey    = "maxOutputTokens"
		seedVal    = "8192"
	)
	setPiModelsConfig(t, map[string]any{
		"providers": map[string]any{
			"ollama": map[string]any{
				"api":     piProviderAPI,
				"baseUrl": ep.BaseURL,
				"models": []any{
					map[string]any{"id": upstreamID, "_launch": true, "contextWindow": 200000, seedKey: 8192},
				},
			},
		},
	})

	if err := (&Pi{}).Edit(launchModelsFromNames([]string{pickerName})); err != nil {
		t.Fatalf("Edit(%q) error = %v, want nil", pickerName, err)
	}

	data, err := os.ReadFile(filepath.Join(home, ".pi", "agent", "models.json"))
	if err != nil {
		t.Fatal(err)
	}
	doc := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("models.json does not parse after Edit: %v\n%s", err, data)
	}
	providers, _ := doc["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	list, _ := ollama["models"].([]any)

	var entry map[string]any
	matches := 0
	for _, m := range list {
		obj, ok := m.(map[string]any)
		if !ok {
			continue
		}
		if id, _ := obj["id"].(string); id == upstreamID {
			entry = obj
			matches++
		}
	}
	if matches != 1 {
		t.Fatalf("Edit wrote %d entries with id %q, want exactly one — the selected remote entry did not match (the stored id is the bare upstream id, the selection is the picker name), so it was dropped and rebuilt:\n%s", matches, upstreamID, data)
	}
	if got := fmt.Sprint(entry["contextWindow"]); got != "200000" {
		t.Errorf("contextWindow = %v, want 200000 — the entry was rebuilt from scratch", entry["contextWindow"])
	}
	if got := fmt.Sprint(entry[seedKey]); got != seedVal {
		t.Errorf("%s = %v, want %s — the entry was rebuilt from scratch and every member oaica does not model was deleted", seedKey, entry[seedKey], seedVal)
	}
}

// Control: with the daemon as the provider endpoint, no entry may be turned
// into a remote name — that would report a model the remote does not serve.
func TestPiModelsLeavesDaemonEntriesAlone(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	t.Setenv(ollamaCloudEnvKey, "")
	writeRemotes(t, `{"remotes":[{"name":"deepseek","base_url":"https://api.deepseek.com","api_key":"sk-static"}]}`)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

	setPiModelsConfig(t, map[string]any{
		"providers": map[string]any{
			"ollama": map[string]any{
				"api":     piProviderAPI,
				"baseUrl": "http://127.0.0.1:11434/v1",
				"models":  []any{map[string]any{"id": "qwen3:8b", "_launch": true}},
			},
		},
	})

	got := (&Pi{}).Models()
	if len(got) != 1 || got[0] != "qwen3:8b" {
		t.Errorf("Pi.Models() = %v, want [qwen3:8b]", got)
	}
}
