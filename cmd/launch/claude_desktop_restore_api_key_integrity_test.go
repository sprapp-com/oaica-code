package launch

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// claudeDesktopRestoreKeyEnv stands up the real write path and the real restore
// path on a throwaway home: darwin target paths, a validation stub that accepts
// any key, and process hooks that never touch a real app.
func claudeDesktopRestoreKeyEnv(t *testing.T) claudeDesktopPaths {
	t.Helper()
	setTestHome(t, t.TempDir())
	withClaudeDesktopPlatform(t, "darwin")
	withClaudeDesktopProcessHooks(t, func() bool { return false }, func() error { return nil }, func() error { return nil })
	withClaudeDesktopValidation(t, func(context.Context, string) error { return nil })

	paths, err := claudeDesktopConfigPaths()
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

// writeClaudeDesktopProfileKey lays down the Ollama third-party profile the way
// a user's own Claude Desktop install would have it before oaica runs.
func writeClaudeDesktopProfileKey(t *testing.T, path, key string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := map[string]any{"disableDeploymentModeChooser": false}
	if key != "" {
		cfg["inferenceGatewayApiKey"] = key
	}
	data, err := json.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func claudeDesktopProfileGatewayKey(t *testing.T, path string) (string, bool) {
	t.Helper()
	cfg := claudeDesktopReadJSON(t, path)
	key, ok := cfg["inferenceGatewayApiKey"].(string)
	return key, ok
}

// claudeDesktopRestoreStateMentions reports whether oaica's own restore state
// (its bookkeeping file, not Claude Desktop's) still holds this string.
func claudeDesktopRestoreStateMentions(t *testing.T, secret string) bool {
	t.Helper()
	data, err := os.ReadFile(claudeDesktopRestoreStatePath())
	if err != nil {
		if os.IsNotExist(err) {
			return false
		}
		t.Fatalf("read restore state: %v", err)
	}
	return bytes.Contains(data, []byte(secret))
}

// Restore is the user asking for their config back. The gateway credential
// oaica injects is a secret oaica wrote, so it must not survive --restore
// (2026-09-26 audit) — while a credential the user configured themselves must.
func TestClaudeDesktopRestoreRemovesOnlyTheGatewayKeyOaicaInjected(t *testing.T) {
	t.Run("removes the key oaica injected into an empty profile", func(t *testing.T) {
		paths := claudeDesktopRestoreKeyEnv(t)
		t.Setenv("OLLAMA_API_KEY", "oaica-injected-key")

		if err := (&ClaudeDesktop{}).ConfigureAutodiscovery(); err != nil {
			t.Fatalf("ConfigureAutodiscovery returned error: %v", err)
		}
		key, ok := claudeDesktopProfileGatewayKey(t, paths.profile)
		if !ok || key != "oaica-injected-key" {
			t.Fatalf("configured key = (%q, %v), want oaica-injected-key", key, ok)
		}

		if err := (&ClaudeDesktop{}).Restore(); err != nil {
			t.Fatalf("Restore returned error: %v", err)
		}

		key, ok = claudeDesktopProfileGatewayKey(t, paths.profile)
		if ok {
			t.Fatalf("restore left oaica's injected gateway API key on disk: %q", key)
		}
		if claudeDesktopRestoreStateMentions(t, "oaica-injected-key") {
			t.Fatal("restore kept oaica's injected gateway API key in its own restore state")
		}
	})

	t.Run("keeps a key the user already had", func(t *testing.T) {
		paths := claudeDesktopRestoreKeyEnv(t)
		t.Setenv("OLLAMA_API_KEY", "")
		writeClaudeDesktopProfileKey(t, paths.profile, "user-key")

		if err := (&ClaudeDesktop{}).ConfigureAutodiscovery(); err != nil {
			t.Fatalf("ConfigureAutodiscovery returned error: %v", err)
		}
		if claudeDesktopRestoreStateMentions(t, "user-key") {
			t.Fatal("oaica copied a gateway API key it did not replace into a second file")
		}
		if err := (&ClaudeDesktop{}).Restore(); err != nil {
			t.Fatalf("Restore returned error: %v", err)
		}

		key, ok := claudeDesktopProfileGatewayKey(t, paths.profile)
		if !ok || key != "user-key" {
			t.Fatalf("restore did not preserve the user's own gateway API key: (%q, %v)", key, ok)
		}
	})

	t.Run("restores the key oaica overwrote", func(t *testing.T) {
		paths := claudeDesktopRestoreKeyEnv(t)
		writeClaudeDesktopProfileKey(t, paths.profile, "user-key")
		t.Setenv("OLLAMA_API_KEY", "oaica-injected-key")

		if err := (&ClaudeDesktop{}).ConfigureAutodiscovery(); err != nil {
			t.Fatalf("ConfigureAutodiscovery returned error: %v", err)
		}
		key, _ := claudeDesktopProfileGatewayKey(t, paths.profile)
		if key != "oaica-injected-key" {
			t.Fatalf("configured key = %q, want oaica-injected-key", key)
		}

		if err := (&ClaudeDesktop{}).Restore(); err != nil {
			t.Fatalf("Restore returned error: %v", err)
		}

		key, ok := claudeDesktopProfileGatewayKey(t, paths.profile)
		if !ok || key != "user-key" {
			t.Fatalf("restore did not put the user's own gateway API key back: (%q, %v)", key, ok)
		}
	})

	t.Run("repeated configure does not make oaica's key the user's", func(t *testing.T) {
		paths := claudeDesktopRestoreKeyEnv(t)
		t.Setenv("OLLAMA_API_KEY", "oaica-injected-key")

		if err := (&ClaudeDesktop{}).ConfigureAutodiscovery(); err != nil {
			t.Fatalf("first ConfigureAutodiscovery returned error: %v", err)
		}
		t.Setenv("OLLAMA_API_KEY", "oaica-rotated-key")
		if err := (&ClaudeDesktop{}).ConfigureAutodiscovery(); err != nil {
			t.Fatalf("second ConfigureAutodiscovery returned error: %v", err)
		}

		if err := (&ClaudeDesktop{}).Restore(); err != nil {
			t.Fatalf("Restore returned error: %v", err)
		}

		key, ok := claudeDesktopProfileGatewayKey(t, paths.profile)
		if ok {
			t.Fatalf("restore kept a gateway API key oaica wrote across two configures: %q", key)
		}
	})
}
