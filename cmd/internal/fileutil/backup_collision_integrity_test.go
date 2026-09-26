package fileutil

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// Two backups of the same file inside one wall-clock second used to collapse
// into one name: `<base>.<unix-seconds>` is not unique at that granularity, and
// the copy was written with O_TRUNC, so the second backup destroyed the first —
// the one holding the user's ORIGINAL pre-oaica content — and replaced it with
// oaica's own intermediate write.
//
// That is reachable from a single `oaica launch openclaw`: launchEditorIntegration
// runs Openclaw.Edit (openclaw.go:782) and then launchAfterConfiguration runs
// Openclaw.Run, whose configureOllamaWebSearch call (openclaw.go:102, writing at
// openclaw.go:995) rewrites the same ~/.openclaw/openclaw.json. On a launch that
// is not the first there is no subprocess between them, and the comment at
// openclaw.go:991-995 explicitly promises the user's config is kept.
//
// The clock is pinned so the collision is deterministic rather than dependent on
// how fast the test machine happens to be.
func TestWriteWithBackup_TwoBackupsInOneSecondBothSurvive(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := t.TempDir()
	path := filepath.Join(dir, "openclaw.json")

	original := []byte(`{"owner":"the user","token":"keep-me"}`)
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}

	frozen := time.Unix(1758888888, 0)
	prev := timeNow
	timeNow = func() time.Time { return frozen }
	defer func() { timeNow = prev }()

	// Write 1: Openclaw.Edit's rewrite. Write 2: configureOllamaWebSearch's,
	// moments later in the same launch.
	if err := WriteWithBackup(path, []byte(`{"owner":"oaica","step":"edit"}`), "openclaw"); err != nil {
		t.Fatalf("first write: %v", err)
	}
	if err := WriteWithBackup(path, []byte(`{"owner":"oaica","step":"websearch"}`), "openclaw"); err != nil {
		t.Fatalf("second write: %v", err)
	}

	backupDir := filepath.Join(BackupDir(), "openclaw")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}

	var backups []string
	for _, e := range entries {
		if e.IsDir() || !strings.HasPrefix(e.Name(), "openclaw.json.") {
			continue
		}
		backups = append(backups, e.Name())
	}
	if len(backups) != 2 {
		t.Fatalf("got %d backups %v, want 2 — the first backup was overwritten by the second", len(backups), backups)
	}

	haveOriginal, haveEdit := false, false
	for _, name := range backups {
		data, err := os.ReadFile(filepath.Join(backupDir, name))
		if err != nil {
			t.Fatalf("read backup %s: %v", name, err)
		}
		switch string(data) {
		case string(original):
			haveOriginal = true
		case `{"owner":"oaica","step":"edit"}`:
			haveEdit = true
		}
	}
	if !haveOriginal {
		t.Errorf("no backup holds the user's original bytes %s — it was destroyed", original)
	}
	if !haveEdit {
		t.Errorf("no backup holds the first write's bytes; the intermediate state is unrecoverable")
	}
}

// The "-N" names a second backup claims in the same second must still count as
// backups of that file, or pruneOldBackups would stop recognising them and the
// backup directory would grow without the maxBackupsPerFile bound.
func TestWriteWithBackup_PruneStillCountsSameSecondBackups(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)

	dir := t.TempDir()
	path := filepath.Join(dir, "rapid.json")
	if err := os.WriteFile(path, []byte(`{"v": 0}`), 0o600); err != nil {
		t.Fatal(err)
	}

	frozen := time.Unix(1758888888, 0)
	prev := timeNow
	timeNow = func() time.Time { return frozen }
	defer func() { timeNow = prev }()

	for i := 1; i <= maxBackupsPerFile+3; i++ {
		data := []byte(fmt.Sprintf(`{"v": %d}`, i))
		if err := WriteWithBackup(path, data, "rapid"); err != nil {
			t.Fatalf("write %d: %v", i, err)
		}
	}

	backupDir := filepath.Join(BackupDir(), "rapid")
	entries, err := os.ReadDir(backupDir)
	if err != nil {
		t.Fatalf("read backup dir: %v", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), "rapid.json.") {
			names = append(names, e.Name())
		}
	}
	if len(names) != maxBackupsPerFile {
		t.Fatalf("got %d backups %v, want the %d-backup bound held", len(names), names, maxBackupsPerFile)
	}

	// A backup holds the state that was about to be overwritten, so the newest
	// surviving backup is the second-to-last write, and the last write is what
	// the target file now holds.
	wantNewestBackup := fmt.Sprintf(`{"v": %d}`, maxBackupsPerFile+2)
	newest := false
	for _, n := range names {
		if data, err := os.ReadFile(filepath.Join(backupDir, n)); err == nil && string(data) == wantNewestBackup {
			newest = true
		}
	}
	if !newest {
		t.Errorf("no surviving backup holds %s, the most recent state pruning may keep", wantNewestBackup)
	}
	if data, err := os.ReadFile(path); err != nil || string(data) != fmt.Sprintf(`{"v": %d}`, maxBackupsPerFile+3) {
		t.Errorf("target holds %q (err %v), want the last write", data, err)
	}
}
