package launch

// codex_app_remote_refusal_integrity_test.go — the ChatGPT (Codex App) writer
// accepted a user-remote selection and wrote it as a daemon model
// (2026-09-26 audit, round 16).
//
// writeCodexAppConfig hardcodes the daemon's /v1 as the provider base_url and
// the picker name as the root model. The CLI codex path translates both —
// codexBaseURLFor, codexModelIDFor, codexWireFor, codexAPIKeyFor — but the App
// path has none of that, and it cannot: the credential on the CLI path is
// handed to the child process as OPENAI_API_KEY, and the App is a GUI the user
// starts themselves, so there is nothing oaica can set. A remote selection
// therefore made Codex App post the namespaced picker name ("box/big-model")
// to the local daemon, which does not resolve namespaced remotes: every
// request fails model-not-found after the user has already answered the key
// prompt. Same shape as muse's settings, and the same answer — refuse at the
// point the user can act on it (museRejectNonDaemonModels).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCodexAppRefusesARemoteModel(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")
	// The remote has to exist for the refusal to be about a remote at all: the
	// predicate is the endpoint the row routes to, so a row that resolves
	// nowhere is an ordinary name the daemon would be asked for
	// (2026-09-27 audit, round 20).
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET","tool_format":"tool_calls"}]}`)
	if _, ok := resolveRemoteEndpoint("box/big-model"); !ok {
		t.Fatal("premise: the box/big-model row does not resolve, so the refusal below would not be about a user remote")
	}

	remote := LaunchModel{Name: "box/big-model", Remote: true, Upstream: "big-model"}
	models := append([]LaunchModel{remote}, launchModelsFromNames([]string{"llama3.2"})...)

	app := &CodexApp{}
	err := app.ConfigureWithModels("box/big-model", models)
	if err == nil {
		t.Fatal("ConfigureWithModels accepted a user-remote model for the ChatGPT app, which cannot route to it: the config names the local daemon as the provider and the picker name as the model, so every request fails model-not-found")
	}
	if !strings.Contains(err.Error(), "box/big-model") {
		t.Errorf("the refusal does not name the model that cannot be used: %v", err)
	}

	// And it must refuse BEFORE writing: a config that has already been
	// rewritten is a worse outcome than the refusal, because the user's
	// working ChatGPT setup is gone.
	configPath := filepath.Join(tmpDir, ".codex", "config.toml")
	if b, rerr := os.ReadFile(configPath); rerr == nil && strings.Contains(string(b), "box/big-model") {
		t.Errorf("the refused model was written into %s anyway:\n%s", configPath, b)
	}
}

// The daemon case is unaffected: a local selection still configures.
func TestCodexAppStillConfiguresADaemonModel(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:9999")

	if err := (&CodexApp{}).ConfigureWithModels("llama3.2", testLaunchModels("llama3.2", "qwen3:8b")); err != nil {
		t.Fatalf("a daemon-backed selection was refused: %v", err)
	}
}
