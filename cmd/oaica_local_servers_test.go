package cmd

import (
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

// local_servers.json is written by `oaica serve` and holds the --api-key that
// server was started with, so it is a credential file. os.WriteFile's mode
// only applies when it creates the file: a hand-created or copied one keeps
// its own mode, and the key sits in a world-readable file. The other three
// credential writers re-assert with os.Chmod; this one did not, and
// `oaica doctor --report` described it as non-sensitive and never scanned it
// (2026-09-26 audit).
func TestRegisterLocalServer_Reasserts0600(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "local_servers.json")
	if err := os.WriteFile(path, []byte("[]"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}

	const key = "sk-serve-apikey-0123456789"
	if err := oaicaRegisterLocalServer("kat", "http://127.0.0.1:9/v1", key); err != nil {
		t.Fatalf("register: %v", err)
	}

	fi, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if got := fi.Mode().Perm(); got != 0o600 {
		t.Errorf("local_servers.json is %o after a write, want 0600 — it holds the server's --api-key", got)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(b, []byte(key)) {
		t.Errorf("expected the entry to record the api key so the test is about a real file: %s", b)
	}
}
