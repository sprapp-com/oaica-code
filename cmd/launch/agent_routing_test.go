package launch

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeRemotesFile(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "remotes.json")
	content := `{
  "remotes": [
    { "name": "deepseek", "base_url": "https://api.deepseek.com", "api_key": "sk-static" },
    { "name": "lan",      "base_url": "http://192.168.1.50:8080", "api_key_env": "LAN_KEY" }
  ]
}`
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestResolveAgentModelRemote routes "<remote>/<model>" through a loopback
// translation proxy with the remote's own key, and returns the bare upstream
// model id.
func TestResolveAgentModelRemote(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("OAICA_API_KEY", "unused-oaica-key")

	baseURL, token, upstreamModel, meta, err := ResolveAgentModel(context.Background(), "deepseek/deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		t.Errorf("baseURL = %q, want loopback proxy", baseURL)
	}
	if token != "sk-static" {
		t.Errorf("token = %q, want remote's static key", token)
	}
	if upstreamModel != "deepseek-chat" {
		t.Errorf("upstreamModel = %q, want %q", upstreamModel, "deepseek-chat")
	}
	if !meta.ToolCapable {
		t.Error("remote models should default to tool-capable")
	}
	if meta.MaxOutputTokens <= 0 {
		t.Errorf("MaxOutputTokens = %d, want positive default", meta.MaxOutputTokens)
	}
}

// TestResolveAgentModelKeyPrecedence: api_key_env wins over api_key.
func TestResolveAgentModelKeyPrecedence(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("LAN_KEY", "sk-from-env")

	_, token, upstreamModel, _, err := ResolveAgentModel(context.Background(), "lan/qwen3")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if token != "sk-from-env" {
		t.Errorf("token = %q, want env-provided key (env beats file)", token)
	}
	if upstreamModel != "qwen3" {
		t.Errorf("upstreamModel = %q, want %q", upstreamModel, "qwen3")
	}
}

// TestResolveAgentModelLocalTag: ":local" is stripped and the result uses the
// OAICA key, never the remote key.
func TestResolveAgentModelLocalTag(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("OAICA_API_KEY", "oaica-key")
	t.Setenv("OAICA_HOST", "https://router.example.test")

	baseURL, token, upstreamModel, _, err := ResolveAgentModel(context.Background(), "llama3.1:8b:local")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if upstreamModel != "llama3.1:8b" {
		t.Errorf("upstreamModel = %q, want tag stripped to %q", upstreamModel, "llama3.1:8b")
	}
	if token != "oaica-key" {
		t.Errorf("token = %q, want OAICA key", token)
	}
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") && baseURL != "https://router.example.test" {
		t.Errorf("baseURL = %q, want loopback logging proxy or OAICA_HOST", baseURL)
	}
}

// TestResolveAgentModelUnknownModelFallsBackToDefaults: an unknown model
// yields positive defaults and tool-capable=true.
func TestResolveAgentModelUnknownModelFallsBackToDefaults(t *testing.T) {
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("OAICA_API_KEY", "oaica-key")
	t.Setenv("OAICA_HOST", "https://router.example.test")

	_, _, upstreamModel, meta, err := ResolveAgentModel(context.Background(), "made-up-model")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if upstreamModel != "made-up-model" {
		t.Errorf("upstreamModel = %q, want identity for cloud", upstreamModel)
	}
	if !meta.ToolCapable {
		t.Error("unknown models should default to tool-capable")
	}
	if meta.ContextLength <= 0 || meta.MaxOutputTokens <= 0 {
		t.Errorf("defaults not applied: ContextLength=%d MaxOutputTokens=%d", meta.ContextLength, meta.MaxOutputTokens)
	}
}
