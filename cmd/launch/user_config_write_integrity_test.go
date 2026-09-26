package launch

// user_config_write_integrity_test.go — ~/.oaica/config.json was the one store
// written by load-mutate-write with no lock and no merge (2026-09-26 audit).
//
// Two consequences, one file. No lock: the wizard and `oaica config set` (or
// two `config set`s from a provisioning script) each load the same snapshot and
// the later rename publishes only its own field, so one setting silently
// reverts. No merge: the marshalled struct is a two-field view of the document,
// so a key written by a newer oaica — or by hand — is deleted by the next
// write. Every other store in this package goes through updateX + the merge for
// exactly these reasons; see store_document.go.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func configPathFor(t *testing.T, home string) string {
	t.Helper()
	return filepath.Join(home, ".oaica", "config.json")
}

func TestAConfigWriteKeepsMembersTheStructDoesNotModel(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	path := configPathFor(t, home)

	seed := []byte(`{
  "sonnet_model": "glm-5.3",
  "schema_note": "written by a newer oaica",
  "tiers": {"haiku": {"fallback": "glm-4.5-air"}}
}`)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := UserConfigSetHaikuModel("glm-4.5-air"); err != nil {
		t.Fatalf("UserConfigSetHaikuModel: %v", err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	for _, want := range []string{"schema_note", "written by a newer oaica", "fallback", `"glm-4.5-air"`} {
		if !strings.Contains(got, want) {
			t.Errorf("%q did not survive the write — oaica rewrote a document it only meant to set one key in:\n%s", want, got)
		}
	}
	if !strings.Contains(got, `"sonnet_model": "glm-5.3"`) {
		t.Errorf("the other standing preference was dropped:\n%s", got)
	}
}

// A write that changes nothing must not rewrite the file at all: the rewrite is
// the only way the unmodelled members above can be lost, and re-serialising a
// partial view over a document is exactly how the other stores lost them.
func TestAConfigWriteThatChangesNothingLeavesTheFileAlone(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	path := configPathFor(t, home)

	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	// Deliberately not oaica's own formatting: a rewrite would normalise it.
	seed := []byte("{\"haiku_model\":\"glm-4.5-air\",\"sonnet_model\":\"glm-5.3\",\"schema_note\":\"keep\"}")
	if err := os.WriteFile(path, seed, 0o600); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}

	if err := UserConfigSetHaikuModel("glm-4.5-air"); err != nil {
		t.Fatalf("UserConfigSetHaikuModel: %v", err)
	}

	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if !os.SameFile(before, after) {
		t.Errorf("setting a preference to the value it already had rewrote the file")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != string(seed) {
		t.Errorf("the file changed:\n%s", b)
	}
}

// Two processes setting two different standing preferences both have to survive.
// Real child processes, like the other store tests: the defect is between
// processes, and an in-process test would pass against a mutex.
func TestTwoConfigWritersDoNotLoseEachOther(t *testing.T) {
	home := t.TempDir()
	path := configPathFor(t, home)

	runStoreWriters(t, "config", path, []string{
		"sonnet-1", "haiku-1", "sonnet-2", "haiku-2",
		"sonnet-3", "haiku-3", "sonnet-4", "haiku-4",
	})

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no config.json was written: %v", err)
	}
	var c UserConfig
	if err := json.Unmarshal(b, &c); err != nil {
		t.Fatalf("config.json is not parseable: %v\n%s", err, b)
	}
	if c.SonnetModel == "" {
		t.Errorf("every writer set a sonnet tier and the file has none — a lost update:\n%s", b)
	}
	if c.HaikuModel == "" {
		t.Errorf("every writer set a haiku tier and the file has none — a lost update:\n%s", b)
	}
}
