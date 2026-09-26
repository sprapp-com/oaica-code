package launch

// pi_corrupt_config_integrity_test.go — Pi.Edit read ~/.pi/agent/models.json
// and settings.json with `_ = json.Unmarshal(...)`, so a document it could not
// parse was treated as an empty one and replaced with only the keys oaica
// knows (providers.ollama, defaultProvider/defaultModel). Every other provider,
// every top-level key, and every setting the user had was deleted
// (2026-09-26 audit).
//
// Go inserts map keys as it decodes, so what came back was the half of the
// document that happened to precede the syntax error, not even the empty map
// the code reads as.
//
// Both files already have backups (WriteWithBackup), but a backup is not the
// contract: refusing leaves the live file exactly as the user wrote it. The
// two pi_test.go subtests that asserted the lossy behavior ("handles corrupt
// config gracefully", "handles corrupt settings.json gracefully") recorded the
// bug as a contract and were rewritten with this change.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func piAgentPath(t *testing.T, home, name string) string {
	t.Helper()
	dir := filepath.Join(home, ".pi", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, name)
}

func TestEditRefusesToRewriteAnUnreadablePiModelsFile(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	path := piAgentPath(t, home, "models.json")
	corrupt := `{"providers":{"anthropic":{"apiKey":"sk-ant-do-not-lose-me"},"ollama":`
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &Pi{}
	err := p.Edit(testLaunchModels("qwen3:8b"))
	if err == nil {
		t.Errorf("Edit returned nil for a models.json it could not parse — it then wrote a document holding only providers.ollama, so the anthropic provider above is gone")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != corrupt {
		t.Errorf("the unreadable models.json was overwritten:\n%s\nwant it left byte-identical at:\n%s", got, corrupt)
	}
}

func TestEditRefusesToRewriteAnUnreadablePiSettingsFile(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	modelsPath := piAgentPath(t, home, "models.json")
	readable := `{"providers":{"anthropic":{"apiKey":"sk-ant-keep-me"}}}`
	if err := os.WriteFile(modelsPath, []byte(readable), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsPath := piAgentPath(t, home, "settings.json")
	corrupt := `{"theme":"dark","trustedFolders":["/srv/work"],"defaultProvider":`
	if err := os.WriteFile(settingsPath, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &Pi{}
	err := p.Edit(testLaunchModels("qwen3:8b"))
	if err == nil {
		t.Errorf("Edit returned nil for a settings.json it could not parse — it then wrote a document holding only defaultProvider/defaultModel, so theme and trustedFolders above are gone")
	}
	got, rerr := os.ReadFile(settingsPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != corrupt {
		t.Errorf("the unreadable settings.json was overwritten:\n%s\nwant it left byte-identical at:\n%s", got, corrupt)
	}

	// The refusal must come before any write: models.json is readable, but it
	// belongs to the same update, and a half-applied update is its own bug.
	gotModels, merr := os.ReadFile(modelsPath)
	if merr != nil {
		t.Fatal(merr)
	}
	if string(gotModels) != readable {
		t.Errorf("models.json was rewritten even though the update as a whole was refused:\n%s\nwant:\n%s", gotModels, readable)
	}
}

// The control: readable files keep the members oaica does not model, including
// a number that cannot round-trip through float64.
func TestEditKeepsTheMembersPiOwns(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	modelsPath := piAgentPath(t, home, "models.json")
	fixture := `{
  "providers": {"anthropic": {"apiKey": "sk-ant-keep-me"}},
  "revision": 9007199254740993
}`
	if err := os.WriteFile(modelsPath, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsPath := piAgentPath(t, home, "settings.json")
	settings := `{"theme":"dark","trustedFolders":["/srv/work"]}`
	if err := os.WriteFile(settingsPath, []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}

	p := &Pi{}
	if err := p.Edit(testLaunchModels("qwen3:8b")); err != nil {
		t.Fatal(err)
	}

	for _, tc := range []struct{ path, want string }{
		{modelsPath, "sk-ant-keep-me"},
		{modelsPath, "9007199254740993"},
		{settingsPath, `"dark"`},
		{settingsPath, "/srv/work"},
	} {
		got, err := os.ReadFile(tc.path)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(got), tc.want) {
			t.Errorf("%s did not survive the update of %s:\n%s", tc.want, tc.path, got)
		}
	}
}
