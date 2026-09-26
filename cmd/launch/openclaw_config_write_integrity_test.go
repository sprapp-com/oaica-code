package launch

// openclaw_config_write_integrity_test.go — configureOllamaWebSearch rewrote
// ~/.openclaw/openclaw.json with a plain overwrite every single time launch
// ran, whether or not anything had changed (2026-09-26 audit). Three separate
// consequences, all of them on a file this program does not own:
//
//   - The rewrite happened even when the config was already converged, so
//     every launch replaced the file's inode and moved its mtime. The
//     OpenClaw daemon owns that file; an unconditional rewrite is a window in
//     which its own concurrent edit (or an editor's save) is overwritten by a
//     copy read moments earlier.
//   - The whole document round-tripped through map[string]any, i.e. through
//     float64, so a large integer in the config came back changed. This one
//     also had no backup (Openclaw.Edit writes through
//     fileutil.WriteWithBackup), so the write that ran on every launch was
//     also the one with nothing to fall back on — as did
//     patchDeviceScopes/clearSessionModelOverride, fixed alongside it in
//     openclaw_state_write_integrity_test.go.
//
// The fix keeps the document intact: numbers decode as json.Number (verbatim
// text, no float64), the write is skipped when the modified document is
// semantically the same as the one on disk, and a real change goes through
// WriteWithBackup so the previous copy is kept.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

// convergedOpenclawConfig is what configureOllamaWebSearch is trying to
// produce: the bundled provider enabled, nothing stale to migrate.
const convergedOpenclawConfig = `{
  "plugins": {
    "entries": {
      "ollama": {
        "enabled": true
      }
    }
  },
  "tools": {
    "web": {
      "search": {
        "enabled": true,
        "provider": "ollama"
      }
    }
  }
}
`

func openclawConfigPath(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "openclaw.json")
}

func TestAConvergedOpenclawConfigIsNotRewritten(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	path := openclawConfigPath(t, home)
	if err := os.WriteFile(path, []byte(convergedOpenclawConfig), 0o600); err != nil {
		t.Fatal(err)
	}

	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(20 * time.Millisecond) // mtime resolution, and enough to see a write land

	configureOllamaWebSearch()

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Errorf("a launch rewrote an already-converged %s (mtime %s -> %s) — the OpenClaw daemon owns that file, and a rewrite nothing asked for is a window in which its own edit is replaced by a copy read a moment earlier",
			path, before.ModTime(), after.ModTime())
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != convergedOpenclawConfig {
		t.Errorf("the converged config was reformatted:\n%s", got)
	}
}

// A number the config holds must come back with the same digits. Decoding into
// map[string]any turns every number into a float64, and 9007199254740993 is
// the first integer that cannot survive that trip.
func TestABigIntegerInTheOpenclawConfigSurvives(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	path := openclawConfigPath(t, home)
	fixture := `{"tools":{"web":{"search":{"provider":"ollama","enabled":true}}},"plugins":{"entries":{"ollama":{"enabled":true}}},"requestIdFloor":9007199254740993}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	configureOllamaWebSearch()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "9007199254740993") {
		t.Errorf("the config's integer was rewritten (%s) — the document round-trips through float64, which cannot represent it, so a launch silently changes a value it never meant to touch:\n%s", "…"+string(got)+"…", got)
	}
	var parsed map[string]any
	if err := json.Unmarshal(got, &parsed); err != nil {
		t.Fatalf("the config is no longer valid JSON: %v\n%s", err, got)
	}
}

// The control: a config that DOES need the migration is still written, and —
// like the rest of this integration — the previous copy is kept.
func TestARealOpenclawChangeIsWrittenWithABackup(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	path := openclawConfigPath(t, home)
	fixture := `{"plugins":{"allow":["openclaw-web-search"],"entries":{"openclaw-web-search":{"enabled":true}}},"tools":{"alsoAllow":["ollama_web_search"],"web":{"search":{"provider":"openclaw-web-search"}}}}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	configureOllamaWebSearch()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(got), "openclaw-web-search") {
		t.Errorf("the stale plugin is still in the config:\n%s", got)
	}
	backups, _ := filepath.Glob(filepath.Join(fileutil.BackupDir(), "openclaw", "openclaw.json.*"))
	found := false
	for _, b := range backups {
		if data, err := os.ReadFile(b); err == nil && string(data) == fixture {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("the previous config was overwritten with no backup in %s — this is a file oaica does not own", filepath.Join(fileutil.BackupDir(), "openclaw"))
	}
}
