package cmd

// round27_registry_field_preservation_integrity_test.go — a registry rewrite
// hands back what it did not write (2026-09-27 audit, round 27, B-F).
//
// Every writer of ~/.oaica/local_servers.json replaces the file whole: register,
// unregister and drop all read the rows into a struct, mutate, and marshal. A
// field the struct does not model — one a user added by hand, or one a later
// build wrote — was therefore deleted by the next `oaica serve`, silently, in a
// file that also holds credentials.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestARegisterKeepsFieldsItDoesNotModel(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "local_servers.json")
	seed := `[
  {
    "model": "kat",
    "origin": "http://127.0.0.1:30001",
    "pid": 4242,
    "started_at": "2026-09-27T00:00:00Z",
    "api_key": "sk-kat",
    "note": "started by hand, keep me",
    "owner": {"team": "infra"}
  }
]
`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := oaicaRegisterLocalServer("qwen3", "http://127.0.0.1:30002", "sk-qwen"); err != nil {
		t.Fatalf("oaicaRegisterLocalServer: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var rows []map[string]any
	if err := json.Unmarshal(b, &rows); err != nil {
		t.Fatalf("registry after a register: %v", err)
	}
	var kat map[string]any
	for _, r := range rows {
		if r["model"] == "kat" {
			kat = r
		}
	}
	if kat == nil {
		t.Fatalf("the kat row is gone after registering another model: %s", b)
	}
	if kat["note"] != "started by hand, keep me" {
		t.Errorf("kat's own field after a register = %v, want it preserved: this write replaces the file whole and does not own that field", kat["note"])
	}
	owner, _ := kat["owner"].(map[string]any)
	if owner["team"] != "infra" {
		t.Errorf("kat's nested field after a register = %v, want it preserved", kat["owner"])
	}
}
