package launch

// qwen_keyless_remote_preserves_user_key_integrity_test.go — a remote with no
// credential at all blanked the user's OLLAMA_API_KEY (2026-09-27 audit, round
// 19).
//
// applyQwenOllamaKey preserves an existing value only on the daemon branch. On
// the remote branch it wrote the resolved token unconditionally, and a remote
// that needs NO credential — no api_key, no api_key_env, e.g. a localhost
// vLLM/llama server behind a tunnel — resolves to the empty string. So
// `oaica launch qwen box/some-model` overwrote whatever the user had in
// env.OLLAMA_API_KEY with "". That variable is also how ollama.com is
// reached: a launch that needs no credential consumed one, which is exactly
// the failure the daemon branch was fixed for (2026-09-26 audit, tenth round)
// — the same rule simply never reached the other branch.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestAKeylessRemoteQwenLaunchDoesNotBlankTheUsersKey(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	// Keyless on purpose: no api_key, no api_key_env — an endpoint that
	// genuinely needs no credential, like this fleet's own `oaica serve`.
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://127.0.0.1:30099/v1","tool_format":"tool_calls"}]}`)

	ep, ok := resolveRemoteEndpoint("box/big-model")
	if !ok {
		t.Fatal("premise: the box/big-model row does not resolve, so the launch would not take the remote branch")
	}
	if ep.Token != "" {
		t.Fatalf("premise: the remote resolved a token (%q), so this is not the keyless case", ep.Token)
	}

	configDir := filepath.Join(home, ".qwen")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	const real = "sk-ollama-cloud-REAL-KEY-1234567890"
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"),
		[]byte(`{"env":{"OLLAMA_API_KEY":"`+real+`"}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&Qwen{}).Configure("box/big-model"); err != nil {
		t.Fatalf("configure: %v", err)
	}

	cfg := qwenConfig(t, home)
	envCfg, _ := cfg["env"].(map[string]any)
	if envCfg[qwenOllamaEnvKey] != real {
		t.Errorf("a keyless-remote launch rewrote %s to %v — the endpoint was configured to need no credential, so the write can only destroy a credential someone else set (%s is how ollama.com is reached)",
			qwenOllamaEnvKey, envCfg[qwenOllamaEnvKey], qwenOllamaEnvKey)
	}

	// Control: the config really is the remote's, so the assertion above is
	// about the remote branch and not a launch that fell through to the daemon.
	providers, _ := cfg["modelProviders"].(map[string]any)
	openai, _ := providers["openai"].([]any)
	if len(openai) == 0 {
		t.Fatalf("no openai provider was written:\n%v", cfg)
	}
	first, _ := openai[0].(map[string]any)
	if got := toStr(first["baseUrl"]); got != "http://127.0.0.1:30099/v1" {
		t.Fatalf("provider baseUrl = %q, want the keyless remote", got)
	}
}

// The control for the other direction: a remote that DOES have a token still
// has to have it written, or the launch goes out with the wrong credential.
func TestARemoteQwenLaunchStillWritesItsToken(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-token","tool_format":"tool_calls"}]}`)

	if err := (&Qwen{}).Configure("box/big-model"); err != nil {
		t.Fatalf("configure: %v", err)
	}
	envCfg, _ := qwenConfig(t, home)["env"].(map[string]any)
	if envCfg[qwenOllamaEnvKey] != "sk-box-token" {
		t.Errorf("%s = %v, want the remote's token", qwenOllamaEnvKey, envCfg[qwenOllamaEnvKey])
	}
}
