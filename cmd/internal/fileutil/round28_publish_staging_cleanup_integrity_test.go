package fileutil

// round28_publish_staging_cleanup_integrity_test.go — a publish that could not
// be staged leaves nothing behind (2026-09-27 audit, round 28, F3).
//
// PublishAll stages every document before it renames any, so a failure while
// staging is the ordinary way a pair is refused. Some of those failures
// returned without calling discard, so an earlier document's staged temp — the
// config bytes, written 0600, in the user's own config directory — stayed on
// disk. The failure that makes staging likely is a read-only directory, which
// is also the one the user cannot clean up.

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAStagingFailureLeavesNoTempBehind: the second document cannot be staged,
// so the first is never published — and its temp must not survive.
func TestAStagingFailureLeavesNoTempBehind(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root ignores the directory mode this test uses to make staging fail")
	}
	dir := t.TempDir()
	good := filepath.Join(dir, "good")
	blocked := filepath.Join(dir, "blocked")
	for _, d := range []string{good, blocked} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(good, "a.json"), []byte(`{"old":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// Staging into this directory must fail: create-temp is a write.
	if err := os.Chmod(blocked, 0o555); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(blocked, 0o755) })

	err := PublishAll(
		PublishFile{Path: filepath.Join(good, "a.json"), Data: []byte(`{"new":1}`)},
		PublishFile{Path: filepath.Join(blocked, "b.json"), Data: []byte(`{"new":2}`)},
	)
	if err == nil {
		t.Fatal("PublishAll reported success while the second document could not be staged")
	}

	// The first document is untouched (nothing was renamed)...
	if b, rerr := os.ReadFile(filepath.Join(good, "a.json")); rerr != nil || string(b) != `{"old":1}` {
		t.Fatalf("a.json = %q, err %v; want the pre-publish content", b, rerr)
	}
	// ...and its staged temp is gone.
	entries, rerr := os.ReadDir(good)
	if rerr != nil {
		t.Fatal(rerr)
	}
	for _, e := range entries {
		if e.Name() != "a.json" {
			t.Errorf("staged temp %q left in %s after a publish that never became visible: it holds the config bytes of a write the caller was told failed, in a directory the user may not be able to clean", e.Name(), good)
		}
	}
}
