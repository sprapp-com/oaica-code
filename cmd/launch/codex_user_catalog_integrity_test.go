package launch

// codex_user_catalog_integrity_test.go — the CLI's model catalog was called
// ~/.codex/model.json, a name that is not oaica's, and both writers treated it
// as its own (2026-09-27 audit, round 21, F8).
//
// Configure overwrote whatever was at that path — a user's own model catalog,
// which Codex reads, is a file of exactly that shape — with a single-model
// document and no backup (WriteFileAtomic). Restore deleted the file whenever
// config.toml's model_catalog_json named some OTHER path, which is what it
// names when the user has a catalog of their own, and also when config.toml does
// not exist at all.
//
// The CLI's catalog now carries oaica's own name (the ChatGPT app's has always
// carried one), and a launch writes only that path.

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ollama/ollama/api"
)

// userCodexCatalog is a model catalog the user wrote into ~/.codex/model.json,
// in the shape Codex reads.
const userCodexCatalog = `{
  "models": [
    {
      "slug": "their-model",
      "display_name": "Their Model",
      "context_window": 128000,
      "shell_type": "default",
      "visibility": "list"
    }
  ],
  "their_own_key": true
}
`

func codexUserCatalogEnv(t *testing.T) (userCatalogPath, ourCatalogPath string) {
	t.Helper()
	home := t.TempDir()
	setTestHome(t, home)

	configPath := filepath.Join(home, ".codex", "config.toml")
	userCatalogPath = filepath.Join(home, ".codex", "model.json")
	ourCatalogPath = codexModelCatalogPathForConfig(configPath)
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(userCatalogPath, []byte(userCodexCatalog), 0o644); err != nil {
		t.Fatal(err)
	}
	return userCatalogPath, ourCatalogPath
}

func TestCodexLaunchLeavesTheUsersOwnModelCatalogAlone(t *testing.T) {
	userCatalogPath, ourCatalogPath := codexUserCatalogEnv(t)

	models := []LaunchModel{{Name: "llama3.2", Details: api.ModelDetails{Format: "gguf"}}}
	if err := ensureCodexConfig("llama3.2", models); err != nil {
		t.Fatalf("ensureCodexConfig: %v", err)
	}

	data, err := os.ReadFile(userCatalogPath)
	if err != nil {
		t.Fatalf("the user's own ~/.codex/model.json was deleted: %v", err)
	}
	if string(data) != userCodexCatalog {
		t.Errorf("the user's own ~/.codex/model.json was rewritten:\n%s", data)
	}
	if ourCatalogPath == userCatalogPath {
		t.Fatal("the CLI catalog is still the generic model.json: the file oaica owns must carry oaica's name")
	}
	if _, err := os.Stat(ourCatalogPath); err != nil {
		t.Errorf("the CLI model catalog was not written to oaica's own path %s: %v", ourCatalogPath, err)
	}
}

func TestCodexRestoreDoesNotDeleteTheUsersOwnModelCatalog(t *testing.T) {
	userCatalogPath, ourCatalogPath := codexUserCatalogEnv(t)

	models := []LaunchModel{{Name: "llama3.2", Details: api.ModelDetails{Format: "gguf"}}}
	if err := ensureCodexConfig("llama3.2", models); err != nil {
		t.Fatalf("ensureCodexConfig: %v", err)
	}

	if err := (&Codex{}).Restore(); err != nil {
		t.Fatalf("Restore: %v", err)
	}

	data, err := os.ReadFile(userCatalogPath)
	if err != nil {
		t.Fatalf("Restore deleted the user's own ~/.codex/model.json: %v", err)
	}
	if string(data) != userCodexCatalog {
		t.Errorf("Restore rewrote the user's own ~/.codex/model.json:\n%s", data)
	}
	// The catalog oaica owns is still removed when nothing references it.
	if _, err := os.Stat(ourCatalogPath); !os.IsNotExist(err) {
		t.Errorf("oaica's own CLI catalog should be removed by Restore when config.toml does not reference it, got err=%v", err)
	}
}
