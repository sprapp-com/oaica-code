package launch

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestRemoteRoutingHelpers asserts the shared per-model resolver produces the
// remote's direct triple (baseURL incl. /v1, remote token, bare upstream id)
// and the local daemon triple, across the OpenAI integrations' pure helpers.
// Local launches must stay byte-identical to before (regression guard, spec §8).
func TestRemoteRoutingHelpers(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeDescriptorRemotesFile(t))
	t.Setenv("KAT_KEY", "sk-kat")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")
	// The "local" cases resolve a bare name, which sweeps every configured
	// remote's /v1/models (unroutable hosts above → 6s timeout per helper
	// call). The explicit "<remote>/<id>" cases never consult the index.
	stubBareIndex(t, map[string][]string{})

	const remoteModel = "kat/kat-coder"

	t.Run("kimi config uses remote triple", func(t *testing.T) {
		cfg, err := buildKimiInlineConfig(remoteModel, 32768)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(cfg, `"base_url":"http://192.168.0.50:8080/v1"`) {
			t.Errorf("kimi base_url missing remote base: %s", cfg)
		}
		if !strings.Contains(cfg, `"api_key":"sk-kat"`) {
			t.Errorf("kimi api_key missing remote token: %s", cfg)
		}
		if !strings.Contains(cfg, `"model":"kat-coder"`) {
			t.Errorf("kimi model missing bare upstream id: %s", cfg)
		}
	})

	t.Run("kimi config local is byte-identical", func(t *testing.T) {
		cfg, err := buildKimiInlineConfig("llama3.2", 32768)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(cfg, `"base_url":"http://127.0.0.1:11434/v1"`) {
			t.Errorf("kimi local base_url wrong: %s", cfg)
		}
		if !strings.Contains(cfg, `"api_key":"ollama"`) || !strings.Contains(cfg, `"model":"llama3.2"`) {
			t.Errorf("kimi local triple wrong: %s", cfg)
		}
	})

	t.Run("copilot env uses remote triple", func(t *testing.T) {
		c := &Copilot{}
		env := c.envVars(remoteModel)
		got := strings.Join(env, "\n")
		for _, want := range []string{
			"COPILOT_PROVIDER_BASE_URL=http://192.168.0.50:8080/v1",
			"COPILOT_PROVIDER_API_KEY=sk-kat",
			"COPILOT_PROVIDER_WIRE_API=chat",
			"COPILOT_MODEL=kat-coder",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("copilot env missing %q", want)
			}
		}
	})

	t.Run("copilot env local is byte-identical", func(t *testing.T) {
		c := &Copilot{}
		env := c.envVars("llama3.2")
		got := strings.Join(env, "\n")
		for _, want := range []string{
			"COPILOT_PROVIDER_BASE_URL=http://127.0.0.1:11434/v1",
			"COPILOT_PROVIDER_API_KEY=",
			"COPILOT_PROVIDER_WIRE_API=responses",
			"COPILOT_MODEL=llama3.2",
		} {
			if !strings.Contains(got, want) {
				t.Errorf("copilot local env missing %q", want)
			}
		}
	})

	t.Run("poolside args/env use remote triple", func(t *testing.T) {
		p := &Poolside{}
		args := p.args(remoteModel, nil)
		if len(args) != 2 || args[0] != "-m" || args[1] != "kat-coder" {
			t.Errorf("poolside args = %v, want -m kat-coder", args)
		}
		if got := poolsideBaseURLFor(remoteModel); got != "http://192.168.0.50:8080/v1" {
			t.Errorf("poolside baseURL = %q", got)
		}
		if got := poolsideKeyFor(remoteModel); got != "sk-kat" {
			t.Errorf("poolside key = %q", got)
		}
	})

	t.Run("poolside local is byte-identical", func(t *testing.T) {
		p := &Poolside{}
		if got := p.args("llama3.2", nil); len(got) != 2 || got[1] != "llama3.2" {
			t.Errorf("poolside local args = %v", got)
		}
		if got := poolsideBaseURLFor("llama3.2"); got != "http://127.0.0.1:11434/v1" {
			t.Errorf("poolside local baseURL = %q", got)
		}
		if got := poolsideKeyFor("llama3.2"); got != "ollama" {
			t.Errorf("poolside local key = %q", got)
		}
	})

	t.Run("codex helpers use remote triple with chat wire", func(t *testing.T) {
		if got := codexModelIDFor(remoteModel); got != "kat-coder" {
			t.Errorf("codex model = %q", got)
		}
		if got := codexBaseURLFor(remoteModel); got != "http://192.168.0.50:8080/v1/" {
			t.Errorf("codex baseURL = %q", got)
		}
		if got := codexWireFor(remoteModel); got != "chat" {
			t.Errorf("codex wire = %q, want chat (chat/completions for remote)", got)
		}
		if got := codexAPIKeyFor(remoteModel); got != "sk-kat" {
			t.Errorf("codex key = %q", got)
		}
	})

	t.Run("codex local is byte-identical", func(t *testing.T) {
		if got := codexModelIDFor("llama3.2"); got != "llama3.2" {
			t.Errorf("codex local model = %q", got)
		}
		if got := codexWireFor("llama3.2"); got != "responses" {
			t.Errorf("codex local wire = %q, want responses", got)
		}
		if got := codexAPIKeyFor("llama3.2"); got != "ollama" {
			t.Errorf("codex local key = %q", got)
		}
	})

	t.Run("qwen env/args use remote triple", func(t *testing.T) {
		args := qwenLaunchArgs(remoteModel, []string{"-n"})
		joined := strings.Join(args, " ")
		if !strings.Contains(joined, "--model kat-coder") {
			t.Errorf("qwen args missing bare model: %v", args)
		}
		env := strings.Join(qwenLaunchEnv(remoteModel), "\n")
		for _, want := range []string{
			"OPENAI_BASE_URL=http://192.168.0.50:8080/v1",
			"OPENAI_API_KEY=sk-kat",
			"OPENAI_MODEL=kat-coder",
		} {
			if !strings.Contains(env, want) {
				t.Errorf("qwen env missing %q", want)
			}
		}
	})

	t.Run("cline providers config uses remote triple", func(t *testing.T) {
		home := t.TempDir()
		path := filepath.Join(home, ".cline", "data", "settings", "providers.json")
		if err := writeClineProvidersConfig(path, map[string]any{}, remoteModel); err != nil {
			t.Fatal(err)
		}
		data, _ := os.ReadFile(path)
		var cfg map[string]any
		json.Unmarshal(data, &cfg)
		providers, _ := cfg["providers"].(map[string]any)
		provider, _ := providers["ollama"].(map[string]any)
		settings, _ := provider["settings"].(map[string]any)
		if settings["baseUrl"] != "http://192.168.0.50:8080/v1" {
			t.Errorf("cline baseUrl = %v", settings["baseUrl"])
		}
		if settings["model"] != "kat-coder" {
			t.Errorf("cline model = %v", settings["model"])
		}
		if settings["apiKey"] != "sk-kat" {
			t.Errorf("cline apiKey = %v, want remote token", settings["apiKey"])
		}
	})
}
