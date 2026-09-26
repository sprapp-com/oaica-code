package launch

// qwen_remote_current_model_integrity_test.go — Qwen's CurrentModel reports the
// bare upstream id for a user-remote launch, while the launcher saves the picker
// name (2026-09-26 audit, tenth round).
//
// Configure writes qwenModelIDFor(model), which for a user remote is
// ep.UpstreamModel (qwen.go:359). CurrentModel returned model.name verbatim, so
// `oaica launch qwen` with remote box/big-model saved "box/big-model" and read
// back "big-model": the picker state never agreed with what the launch had
// written, a later picker run reconfigured the model it had just configured,
// and that reconfigure wrote the DAEMON's base URL over the remote one. Hermes
// and OMP already translate back (hermesManagedCurrentModel /
// hermesRemotePickerName, ompRemotePickerName); this is qwen's twin of
// hermes_remote_current_model_integrity_test.go.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestQwenCurrentModelUnderstandsARemoteLaunch(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"k","tool_format":"tool_calls"}]}`)

	q := &Qwen{}
	if err := q.Configure("box/big-model"); err != nil {
		t.Fatalf("configure remote model: %v", err)
	}

	// The config really does hold the remote shape this test is about: the
	// remote's own base URL, and the bare upstream id as the model name.
	data, err := os.ReadFile(filepath.Join(home, ".qwen", "settings.json"))
	if err != nil {
		t.Fatalf("read qwen settings: %v", err)
	}
	if !strings.Contains(string(data), "https://box.example/v1") {
		t.Fatalf("the remote endpoint was not written at all:\n%s", data)
	}
	if !strings.Contains(string(data), `"name": "big-model"`) {
		t.Fatalf("the config does not hold the bare upstream id this test is about:\n%s", data)
	}

	if got := q.CurrentModel(); got != "box/big-model" {
		t.Errorf("CurrentModel() = %q after configuring box/big-model, want the picker name the launch wrote — the config stores ep.UpstreamModel (qwenModelIDFor), so a bare id here is a value the launcher cannot match against what it saved", got)
	}

	// A later daemon launch must move the answer back with it.
	if err := q.Configure("gemma4"); err != nil {
		t.Fatalf("configure local model: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(home, ".qwen", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), qwenBaseURL()) {
		t.Fatalf("the daemon endpoint was not restored:\n%s", data)
	}
	if got := q.CurrentModel(); got != "gemma4" {
		t.Errorf("CurrentModel() = %q after a local launch, want %q", got, "gemma4")
	}
}

// A model name that no configured remote serves is the user's own and must come
// back untouched: the translation is a lookup, not a rewrite.
func TestQwenCurrentModelLeavesAForeignModelNameAlone(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"k","tool_format":"tool_calls"}]}`)

	configDir := filepath.Join(home, ".qwen")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"model":{"name":"llama3.2"},"modelProviders":{"openai":[{"id":"llama3.2","name":"llama3.2 (ollama)","baseUrl":"` + qwenBaseURL() + `","envKey":"OLLAMA_API_KEY"}]}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if got := (&Qwen{}).CurrentModel(); got != "llama3.2" {
		t.Errorf("CurrentModel() = %q for a local model name, want it unchanged", got)
	}
}
