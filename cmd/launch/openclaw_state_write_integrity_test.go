package launch

// openclaw_state_write_integrity_test.go — patchDeviceScopes and
// clearSessionModelOverride are the two openclaw writers that used
// fileutil.WriteFileAtomic with no backup, and that decoded through float64
// (2026-09-26 audit). Both files list state the program does not own:
// paired.json is the gateway's record of every paired device and its operator
// tokens, sessions.json is the TUI's session state.
//
// Two consequences, both silent:
//
//   - A number in either document came back rounded. paired.json's tokens and
//     sessions.json's sessions carry timestamps; anything past 2^53 (an
//     expiry in nanoseconds, an id) is changed by the round-trip.
//   - There was no copy of the previous file, on the two writers whose whole
//     job is to rewrite somebody else's document. Every other openclaw write
//     (Openclaw.Edit, configureOllamaWebSearch) keeps one.
//
// Both functions are best-effort and return without reporting, so neither
// failure has any symptom at all.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

func writeOpenclawFile(t *testing.T, home, rel, body string) string {
	t.Helper()
	path := filepath.Join(home, ".openclaw", rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestPatchDeviceScopesKeepsNumbersAndABackup(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	writeOpenclawFile(t, home, filepath.Join("identity", "device-auth.json"), `{"deviceId":"dev-1"}`)
	// The scopes are absent, so the patch fires; expiry and token id are
	// values a float64 cannot hold.
	fixture := `{
  "dev-1": {
    "deviceId": "dev-1",
    "tokens": {"operator": {"expiresAt": 9007199254740993, "tokenId": 9007199254740995}},
    "scopes": []
  }
}`
	path := writeOpenclawFile(t, home, filepath.Join("devices", "paired.json"), fixture)

	patchDeviceScopes()

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"9007199254740993", "9007199254740995", "operator.read"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("%s not found after the scope patch — it was rounded away by the float64 round-trip:\n%s", want, got)
		}
	}

	backups, _ := filepath.Glob(filepath.Join(fileutil.BackupDir(), "openclaw", "paired.json.*"))
	found := false
	for _, b := range backups {
		if data, err := os.ReadFile(b); err == nil && string(data) == fixture {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("paired.json was rewritten with no backup in %s — it is the gateway's record of every paired device, and this writer has no way to report a bad write", filepath.Join(fileutil.BackupDir(), "openclaw"))
	}
}

func TestClearSessionModelOverrideKeepsNumbersAndABackup(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	fixture := `{
  "sess-1": {"model": "old-model", "modelOverride": "old-model", "createdAt": 9007199254740993}
}`
	path := writeOpenclawFile(t, home, filepath.Join("agents", "main", "sessions", "sessions.json"), fixture)

	clearSessionModelOverride("new-model")

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(got), "9007199254740993") {
		t.Errorf("createdAt was rounded away by the float64 round-trip:\n%s", got)
	}
	if strings.Contains(string(got), "old-model") {
		t.Errorf("the stale override survived:\n%s", got)
	}

	backups, _ := filepath.Glob(filepath.Join(fileutil.BackupDir(), "openclaw", "sessions.json.*"))
	found := false
	for _, b := range backups {
		if data, err := os.ReadFile(b); err == nil && string(data) == fixture {
			found = true
			break
		}
	}
	if !found {
		t.Errorf("sessions.json was rewritten with no backup in %s — the previous session state is gone and this writer cannot report it", filepath.Join(fileutil.BackupDir(), "openclaw"))
	}
}

// The controls: neither writer fires when there is nothing to change, so
// "always backs up" cannot be passed by writing unconditionally.
func TestTheOpenclawStateWritersStayQuietWhenConverged(t *testing.T) {
	t.Run("paired", func(t *testing.T) {
		home := t.TempDir()
		setTestHome(t, home)
		writeOpenclawFile(t, home, filepath.Join("identity", "device-auth.json"), `{"deviceId":"dev-1"}`)
		fixture := `{"dev-1":{"scopes":["operator.read","operator.admin","operator.approvals","operator.pairing"],"approvedScopes":["operator.read","operator.admin","operator.approvals","operator.pairing"]}}`
		path := writeOpenclawFile(t, home, filepath.Join("devices", "paired.json"), fixture)

		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		patchDeviceScopes()
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if before.ModTime() != after.ModTime() {
			t.Errorf("paired.json was rewritten although every required scope was already present")
		}
	})

	t.Run("sessions", func(t *testing.T) {
		home := t.TempDir()
		setTestHome(t, home)
		path := writeOpenclawFile(t, home, filepath.Join("agents", "main", "sessions", "sessions.json"), `{"sess-1":{"model":"new-model"}}`)

		before, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		clearSessionModelOverride("new-model")
		after, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if before.ModTime() != after.ModTime() {
			t.Errorf("sessions.json was rewritten although no session overrode the model")
		}
	})
}
