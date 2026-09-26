package fileutil

// round27_unplaced_lock_visibility_integrity_test.go — a lock that could not
// be placed is said out loud (2026-09-27 audit, round 27, B-B).
//
// WithFileLock runs fn unlocked when the lock file cannot be created or cannot
// be acquired — deliberately, because refusing the write would refuse the
// credential the user just typed. The defect was the silence: forty concurrent
// registrations over an unplaceable lock left one row on disk and printed
// nothing, which is the lost update this file exists to prevent arriving as a
// success message.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAnUnplaceableLockSaysSoAndStillWrites: the write must still happen (the
// stance is unchanged) and it must be visible that exclusion was not held.
func TestAnUnplaceableLockSaysSoAndStillWrites(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")
	// A <path>.lock that is a DIRECTORY: OpenFile(O_CREATE|O_RDWR) fails on it,
	// which is the shape a read-only or otherwise unusable lock path takes.
	if err := os.MkdirAll(path+".lock", 0o755); err != nil {
		t.Fatal(err)
	}

	got := captureStderr(t, func() {
		if err := WithFileLock(path, func() error {
			return os.WriteFile(path, []byte("written"), 0o600)
		}); err != nil {
			t.Fatalf("WithFileLock: %v", err)
		}
	})

	if b, err := os.ReadFile(path); err != nil || string(b) != "written" {
		t.Fatalf("fn did not run: content %q, err %v", b, err)
	}
	if !strings.Contains(got, "could not lock") || !strings.Contains(got, path) {
		t.Errorf("stderr = %q, want a warning naming the path it could not lock: a write that proceeds without exclusion must not look like a write that held the lock", got)
	}
}

// TestAPlaceableLockIsSilent: the warning must not fire on the ordinary path,
// or it becomes noise nobody reads.
func TestAPlaceableLockIsSilent(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "auth.json")

	got := captureStderr(t, func() {
		if err := WithFileLock(path, func() error {
			return os.WriteFile(path, []byte("written"), 0o600)
		}); err != nil {
			t.Fatalf("WithFileLock: %v", err)
		}
	})
	if got != "" {
		t.Errorf("stderr = %q, want nothing when the lock is held", got)
	}
}

// captureStderr runs fn with os.Stderr pointed at a pipe and returns what was
// written.
func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = w
	defer func() { os.Stderr = saved }()

	fn()

	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 0, 256)
	tmp := make([]byte, 256)
	for {
		n, rerr := r.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if rerr != nil {
			break
		}
	}
	r.Close()
	return string(buf)
}
