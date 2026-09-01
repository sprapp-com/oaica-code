package launch

import (
	"context"
	"errors"
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

// stubAgentRoutingNetwork keeps ResolveAgentModel off every network path it
// would otherwise touch: the bare-id remote sweep (a bare model name goes
// through resolveBareRemoteModel), the inventory's own remote sweep
// (agentModelMeta → modelInventory.load → userRemoteLaunchModels; writeRemotesFile's
// "lan" host is unroutable, so each sweep costs fetchRemoteModels' full 6s
// timeout), the router catalog, and the local Ollama daemon.
func stubAgentRoutingNetwork(t *testing.T) {
	t.Helper()
	stubBareIndex(t, map[string][]string{})
	stubUserRemoteModels(t, nil, nil)
	stubCloudFetch(t, nil, nil)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
}

// TestResolveAgentModelRemote routes "<remote>/<model>" through a loopback
// translation proxy with the remote's own key, and returns the bare upstream
// model id.
func TestResolveAgentModelRemote(t *testing.T) {
	stubAgentRoutingNetwork(t)
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("OAICA_API_KEY", "unused-oaica-key")

	baseURL, token, upstreamModel, meta, err := ResolveAgentModel(context.Background(), "deepseek/deepseek-chat")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if !strings.HasPrefix(baseURL, "http://127.0.0.1:") {
		t.Errorf("baseURL = %q, want loopback proxy", baseURL)
	}
	// 2026-09-01 security audit H2: the caller gets the per-launch PROXY
	// token, never the remote's real key (which stays inside the proxy).
	if !strings.HasPrefix(token, "oaica-proxy-") {
		t.Errorf("token = %q, want per-launch proxy token", token)
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
	stubAgentRoutingNetwork(t)
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("LAN_KEY", "sk-from-env")

	_, token, upstreamModel, _, err := ResolveAgentModel(context.Background(), "lan/qwen3")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	// H2: proxy token, not the remote's real key (api_key_env still wins
	// INSIDE the proxy — asserted by TestProxyResolveKey*).
	if !strings.HasPrefix(token, "oaica-proxy-") {
		t.Errorf("token = %q, want per-launch proxy token", token)
	}
	if upstreamModel != "qwen3" {
		t.Errorf("upstreamModel = %q, want %q", upstreamModel, "qwen3")
	}
}

// TestResolveAgentModelLocalTag: ":local" is stripped and the result uses the
// OAICA key, never the remote key.
func TestResolveAgentModelLocalTag(t *testing.T) {
	stubAgentRoutingNetwork(t)
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

// TestApplyAgentModelMeta covers the pure decision helper. Tool capability is
// only known for local inventory entries; cloud/router and user-remote entries
// carry no capability metadata (ToolCapable zero value), so they are treated as
// tool-capable. No network access needed.
func TestApplyAgentModelMeta(t *testing.T) {
	defaults := AgentModelMeta{ToolCapable: true, ContextLength: 128000, MaxOutputTokens: 4096}

	t.Run("not found keeps defaults", func(t *testing.T) {
		got := applyAgentModelMeta(defaults, LaunchModel{}, false)
		if got != defaults {
			t.Errorf("applyAgentModelMeta = %+v, want defaults untouched %+v", got, defaults)
		}
	})

	t.Run("local entry with tools stays tool-capable", func(t *testing.T) {
		got := applyAgentModelMeta(defaults, LaunchModel{ToolCapable: true}, true)
		if !got.ToolCapable {
			t.Error("ToolCapable = false, want true")
		}
	})

	t.Run("remote entry treated as tool-capable", func(t *testing.T) {
		got := applyAgentModelMeta(defaults, LaunchModel{Remote: true}, true)
		if !got.ToolCapable {
			t.Error("ToolCapable = false for remote entry, want true (regression: router sends no capability metadata)")
		}
	})

	t.Run("local entry without tools is not tool-capable", func(t *testing.T) {
		got := applyAgentModelMeta(defaults, LaunchModel{}, true)
		if got.ToolCapable {
			t.Error("ToolCapable = true for local entry without tools, want false")
		}
	})

	t.Run("positive limits override, zero values keep defaults", func(t *testing.T) {
		got := applyAgentModelMeta(defaults, LaunchModel{Remote: true, ContextLength: 32000, MaxOutputTokens: 8000}, true)
		if got.ContextLength != 32000 || got.MaxOutputTokens != 8000 {
			t.Errorf("limits not overridden: %+v", got)
		}
		got = applyAgentModelMeta(defaults, LaunchModel{Remote: true}, true)
		if got.ContextLength != defaults.ContextLength || got.MaxOutputTokens != defaults.MaxOutputTokens {
			t.Errorf("zero limits should keep defaults: %+v", got)
		}
	})
}

// TestResolveAgentModelUnknownModelFallsBackToDefaults: an unknown model
// yields positive defaults and tool-capable=true.
func TestResolveAgentModelUnknownModelFallsBackToDefaults(t *testing.T) {
	stubAgentRoutingNetwork(t)
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

// TestResolveAgentModel_RouterSKUBareWhenCatalogUnreachable: a bare "oaica-*"
// id must resolve to the router even when the catalog fetch FAILS (launch
// with router down) and a user remote mirrors the id in its /models. The
// catalog-based guard alone flipped the sonnet tier onto the mirroring
// remote (opencode zen), which 401'd "Model ... is not supported"
// (2026-09-01 fleet, port 5929).
func TestResolveAgentModel_RouterSKUBareWhenCatalogUnreachable(t *testing.T) {
	stubBareIndex(t, map[string][]string{"oaica-35b-a3b-vision": {"opencode-go/oaica-35b-a3b-vision"}})
	stubUserRemoteModels(t, nil, nil)
	stubCloudFetch(t, nil, errors.New("router unreachable (test stub)"))
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
	t.Setenv("OAICA_REMOTES_FILE", writeRemotesFile(t))
	t.Setenv("OAICA_API_KEY", "oaica-key")
	t.Setenv("OAICA_HOST", "https://api.oaica.com/v1")

	baseURL, token, _, _, err := ResolveAgentModel(context.Background(), "oaica-35b-a3b-vision")
	if err != nil {
		t.Fatalf("ResolveAgentModel: %v", err)
	}
	if strings.Contains(baseURL, "127.0.0.1") {
		// A loopback bind is allowed only for the logging proxy in front of
		// the router host; the token must still be the OAICA key.
		if token != "oaica-key" {
			t.Errorf("token = %q, want OAICA key (router leg)", token)
		}
	} else if baseURL != "https://api.oaica.com/v1" {
		t.Errorf("baseURL = %q, want router host", baseURL)
	}
}
