package launch

// qwen_env_key_clobber_integrity_test.go — `oaica launch qwen` destroyed the
// user's OLLAMA_API_KEY, and grew a dead provider entry per remote launch
// (2026-09-26 audit, tenth round).
//
// applyQwenOllamaConfig wrote env.OLLAMA_API_KEY unconditionally. For a local
// model that value is the literal placeholder "ollama" (the daemon does not
// check it), so `oaica launch qwen llama3.2` overwrote whatever the user had
// there — and OLLAMA_API_KEY is also the variable someone sets to reach
// ollama.com, a real credential destroyed silently by a launch that needed no
// credential at all.
//
// The provider list had the mirror-image problem: qwenMergeOpenAIProviders
// drops the entries it owns by requiring baseUrl == the DAEMON's URL, so the
// entry written by a remote-backed launch (baseUrl = the remote) never matched
// and was never removed. Each remote launch appended another dead provider,
// all pointing at the remote, all carrying the same envKey.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// qwenConfig is the settings.json written by the last Configure.
func qwenConfig(t *testing.T, home string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".qwen", "settings.json"))
	if err != nil {
		t.Fatalf("read settings.json: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("parse settings.json: %v", err)
	}
	return cfg
}

// A launch that needs no credential must not overwrite one.
func TestALocalQwenLaunchDoesNotOverwriteTheUsersKey(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	stubDaemon(t)

	configDir := filepath.Join(home, ".qwen")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const real = "sk-ollama-cloud-REAL-KEY-1234567890"
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"),
		[]byte(`{"env":{"OLLAMA_API_KEY":"`+real+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&Qwen{}).Configure("gemma4"); err != nil {
		t.Fatalf("configure: %v", err)
	}

	envCfg, _ := qwenConfig(t, home)["env"].(map[string]any)
	if envCfg[qwenOllamaEnvKey] != real {
		t.Errorf("a local-model launch rewrote %s from the user's key to %v — the daemon does not check this value, so the only thing the write can do is destroy a credential someone else set (OLLAMA_API_KEY is how ollama.com is reached)",
			qwenOllamaEnvKey, envCfg[qwenOllamaEnvKey])
	}
	// And the base URL really is the daemon's: this is the no-credential case.
	if base, _ := envCfg["OLLAMA_HOST"].(string); strings.Contains(base, "example.com") {
		t.Fatalf("premise: the config points at a remote: %q", base)
	}
}

// The placeholder still has to be written when nothing is there, or qwen's
// provider has no key at all.
func TestALocalQwenLaunchStillWritesAPlaceholderWhenNoneExists(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	stubDaemon(t)

	if err := (&Qwen{}).Configure("gemma4"); err != nil {
		t.Fatalf("configure: %v", err)
	}
	envCfg, _ := qwenConfig(t, home)["env"].(map[string]any)
	if envCfg[qwenOllamaEnvKey] != "ollama" {
		t.Errorf("with no key configured, %s = %v, want the placeholder", qwenOllamaEnvKey, envCfg[qwenOllamaEnvKey])
	}
}

// One entry per remote launch, not one per launch.
func TestRemoteQwenLaunchesDoNotAccumulateDeadProviders(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"k","tool_format":"tool_calls"}]}`)

	for i := 0; i < 3; i++ {
		if err := (&Qwen{}).Configure("box/big-model"); err != nil {
			t.Fatalf("configure %d: %v", i, err)
		}
	}

	cfg := qwenConfig(t, home)
	providers, _ := cfg["modelProviders"].(map[string]any)
	openai, _ := providers["openai"].([]any)
	if len(openai) != 1 {
		var ids []string
		for _, p := range openai {
			m, _ := p.(map[string]any)
			ids = append(ids, toStr(m["id"])+":"+toStr(m["baseUrl"]))
		}
		t.Errorf("three launches of the same remote model left %d openai provider(s): %v — the merge drops an owned entry only when its baseUrl is the DAEMON's, so a remote-backed launch's entry is never recognised as ours and one dead copy is appended per launch", len(openai), ids)
	}
	envCfg, _ := cfg["env"].(map[string]any)
	if envCfg[qwenOllamaEnvKey] != "k" {
		t.Errorf("a remote launch must set the key to the remote's token, got %v", envCfg[qwenOllamaEnvKey])
	}
}

// The user's own unrelated provider entries still survive the merge.
func TestTheQwenMergeStillPreservesForeignProviders(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"k","tool_format":"tool_calls"}]}`)

	configDir := filepath.Join(home, ".qwen")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	body := `{"modelProviders":{"openai":[
		{"id":"mine","name":"My Own Endpoint","envKey":"MY_KEY","baseUrl":"http://10.0.0.20:11434/v1"},
		{"id":"openrouter/model","name":"OpenRouter Model","envKey":"OPENROUTER_API_KEY","baseUrl":"https://openrouter.ai/api/v1"}]}}`
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&Qwen{}).Configure("box/big-model"); err != nil {
		t.Fatalf("configure: %v", err)
	}

	providers, _ := qwenConfig(t, home)["modelProviders"].(map[string]any)
	openai, _ := providers["openai"].([]any)
	seen := map[string]bool{}
	for _, p := range openai {
		m, _ := p.(map[string]any)
		seen[toStr(m["id"])] = true
	}
	if !seen["mine"] || !seen["openrouter/model"] || !seen["big-model"] {
		t.Errorf("the merge dropped a provider: %v", seen)
	}
}

func toStr(v any) string {
	s, _ := v.(string)
	return s
}
