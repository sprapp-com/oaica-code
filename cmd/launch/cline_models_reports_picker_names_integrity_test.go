package launch

// cline_models_reports_picker_names_integrity_test.go — Cline.Models() answered
// with the upstream model id while the picker had handed out "<remote>/<id>"
// (2026-09-26 audit, thirteenth round).
//
// launch.go compares the live config against the saved selection with
// slices.Equal(editor.Models(), models), where models is the list of PICKER
// names the user chose. Cline stores, for a user-remote model, the bare
// upstream id (clineModelIDFor) beside that remote's base URL
// (clineProviderBaseURLFor / clineLegacyBaseURLFor), so its Models() answered
// with a name the launcher never saved: liveConfigMatches was false on every
// run, and every launch reconfigured a config it had just read. Both of
// Cline's stores are covered — providers.json and the legacy globalState —
// because either can be the one answering.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// stubClineRemote registers one user remote and clears the built-in providers,
// so the only endpoint that can match a seeded base URL is this one.
func stubClineRemote(t *testing.T) {
	t.Helper()
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	t.Setenv(ollamaCloudEnvKey, "")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")
	writeRemotes(t, `{"remotes":[{"name":"deepseek","base_url":"https://api.deepseek.com","api_key":"sk-static"}]}`)
}

func writeClineStore(t *testing.T, home, rel string, doc map[string]any) {
	t.Helper()
	path := filepath.Join(home, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestClineModelsReportsThePickerNameFromTheProviderStore drives providers.json,
// the store Cline reads first.
func TestClineModelsReportsThePickerNameFromTheProviderStore(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	stubClineRemote(t)

	ep, ok := resolveRemoteEndpoint("deepseek/deepseek-chat")
	if !ok {
		t.Fatal("premise: deepseek/deepseek-chat must resolve to the configured remote")
	}
	writeClineStore(t, home, filepath.Join(".cline", "data", "settings", "providers.json"), map[string]any{
		"version":          1,
		"lastUsedProvider": clineLaunchProvider,
		"providers": map[string]any{
			clineLaunchProvider: map[string]any{
				"tokenSource": "manual",
				"settings": map[string]any{
					"provider": clineLaunchProvider,
					"model":    "deepseek-chat",
					"baseUrl":  ep.BaseURL,
				},
			},
		},
	})

	got := (&Cline{}).Models()
	if len(got) != 1 || got[0] != "deepseek/deepseek-chat" {
		t.Errorf("Cline.Models() = %v, want [deepseek/deepseek-chat] — the store holds the bare upstream id beside the remote's base URL, which is a name nothing in the launcher saved, so liveConfigMatches is false on every run and each launch rewrites the config it just read", got)
	}
}

// TestClineModelsReportsThePickerNameFromTheLegacyStore drives the legacy
// globalState, whose recorded base URL is the remote's ROOT (no /v1).
func TestClineModelsReportsThePickerNameFromTheLegacyStore(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	stubClineRemote(t)

	writeClineStore(t, home, filepath.Join(".cline", "data", "globalState.json"), map[string]any{
		"actModeApiProvider":   clineLaunchProvider,
		"actModeOllamaModelId": "deepseek-chat",
		"actModeOllamaBaseUrl": "https://api.deepseek.com",
	})

	got := (&Cline{}).Models()
	if len(got) != 1 || got[0] != "deepseek/deepseek-chat" {
		t.Errorf("Cline.Models() = %v, want [deepseek/deepseek-chat] — the legacy state holds the bare upstream id beside the remote's root URL", got)
	}
}

// Control: a daemon-backed entry is stored under the picker name already and
// must be reported verbatim — prefixing it would name a remote that is not
// serving it.
func TestClineModelsLeavesDaemonEntriesAlone(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	stubClineRemote(t)

	writeClineStore(t, home, filepath.Join(".cline", "data", "settings", "providers.json"), map[string]any{
		"version":          1,
		"lastUsedProvider": clineLaunchProvider,
		"providers": map[string]any{
			clineLaunchProvider: map[string]any{
				"tokenSource": "manual",
				"settings": map[string]any{
					"provider": clineLaunchProvider,
					"model":    "qwen3:8b",
					"baseUrl":  clineProviderBaseURL(),
				},
			},
		},
	})

	got := (&Cline{}).Models()
	if len(got) != 1 || got[0] != "qwen3:8b" {
		t.Errorf("Cline.Models() = %v, want [qwen3:8b]", got)
	}
}
