package fileutil

// symlink_write_integrity_test.go — an atomic write to a path that is a SYMLINK
// replaced the link with a regular file, so the credential landed in a new file
// and the file the user was actually keeping (a git-managed or backed-up
// directory) kept its previous content while the client's own success message
// named the path they configured (2026-09-26 audit).
//
// The rename is the whole point of this writer — it is what stops a reader from
// ever seeing a truncated file — and a rename onto a symlink cannot be
// "atomic AND write-through": rename(2) replaces the link. So the symlink is
// followed first and the temp+rename happens at its target, which keeps both
// properties: the reader never sees a partial file, and the destination the user
// chose is the one that changes.

import (
	"os"
	"path/filepath"
	"testing"
)

func TestWriteFileAtomicWritesThroughASymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real", "auth.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(real, []byte(`{"providers":{"old":{"key":"sk-old"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "auth.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(link, []byte(`{"providers":{"new":{"key":"sk-new"}}}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}

	if fi, err := os.Lstat(link); err != nil {
		t.Fatalf("the configured path disappeared: %v", err)
	} else if fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the symlink at %s was replaced by a regular file — a user who links this store into a backed-up or git-managed directory gets an orphaned credential in a new file, while the file they keep retains the old one", link)
	}
	got, err := os.ReadFile(real)
	if err != nil {
		t.Fatalf("the symlink's target is unreadable: %v", err)
	}
	if string(got) != `{"providers":{"new":{"key":"sk-new"}}}` {
		t.Errorf("the symlink target still holds %s — the write went to a new file beside the link instead of to the file the user keeps", got)
	}
	if fi, err := os.Stat(real); err != nil {
		t.Fatalf("stat target: %v", err)
	} else if fi.Mode().Perm() != 0o600 {
		t.Errorf("target mode = %v, want 0600 — the credential file's permissions are asserted on the target", fi.Mode().Perm())
	}
}

// A symlink whose target does not exist yet is still the user's chosen
// destination: the write creates it there rather than at the link's path.
func TestWriteFileAtomicWritesThroughADanglingSymlink(t *testing.T) {
	dir := t.TempDir()
	real := filepath.Join(dir, "real", "auth.json")
	if err := os.MkdirAll(filepath.Dir(real), 0o700); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "auth.json")
	if err := os.Symlink(real, link); err != nil {
		t.Fatal(err)
	}

	if err := WriteFileAtomic(link, []byte(`{"providers":{}}`), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	if _, err := os.Stat(real); err != nil {
		t.Errorf("the symlink target was not created: %v", err)
	}
	if fi, err := os.Lstat(link); err != nil || fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("the dangling symlink was replaced instead of followed (err=%v)", err)
	}
}

// The control: an ordinary path is written atomically as before, with the mode
// asserted and no temp file left behind.
func TestWriteFileAtomicStillReplacesAPlainPath(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	if err := os.WriteFile(path, []byte("old"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := WriteFileAtomic(path, []byte("new"), 0o600); err != nil {
		t.Fatalf("WriteFileAtomic: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new" {
		t.Errorf("content = %q, want %q", got, "new")
	}
	if fi, _ := os.Stat(path); fi.Mode().Perm() != 0o600 {
		t.Errorf("mode = %v, want 0600", fi.Mode().Perm())
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if e.Name() != "state.json" {
			t.Errorf("temp file %s left behind", e.Name())
		}
	}
}
