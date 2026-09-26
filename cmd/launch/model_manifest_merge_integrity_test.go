package launch

// model_manifest_merge_integrity_test.go — modelManifest.save marshalled the
// struct straight over ~/.oaica/models.json, so any top-level member the
// struct does not model was deleted by the next real write (2026-09-26 audit).
//
// models.json is a store the user also edits, and modelManifest is a partial
// view of it on purpose. The other four stores in this package go through
// storeDocumentMerge for exactly this reason; this one did not, so
// `oaica model add` — the command whose whole job is to add one entry — was
// the one that dropped the rest of the document.

import (
	"os"
	"strings"
	"testing"
)

func TestARealManifestWriteKeepsUnmodelledMembers(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/models.json"
	t.Setenv("OAICA_MODELS_FILE", path)

	// A member written by a newer client (or by hand) plus a top-level key
	// this version has no field for.
	fixture := `{
  "version": 1,
  "schemaNote": "written by a newer oaica",
  "models": {
    "existing": {"id": "existing", "engine": "vllm", "model_path": "/m/existing", "notes": "keep me"}
  }
}`
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ModelAdd(ModelAddOptions{ID: "added", Engine: "vllm", ModelPath: "/m/added"}); err != nil {
		t.Fatalf("ModelAdd: %v", err)
	}

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"schemaNote", "written by a newer oaica", "keep me", `"added"`} {
		if !strings.Contains(string(got), want) {
			t.Errorf("%s did not survive the write:\n%s", want, got)
		}
	}

	// And the entry is actually usable, not just present in the text.
	m, err := loadModelManifest()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := m.Get("added"); !ok {
		t.Errorf("the added entry is not in the manifest:\n%s", got)
	}
	if _, ok := m.Get("existing"); !ok {
		t.Errorf("the pre-existing entry is gone:\n%s", got)
	}
}

// The control: a manifest written from nothing still gets the struct's own
// members, so "merge" cannot be passed by never writing the file at all.
func TestAManifestWriteFromNothingStillWrites(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/models.json"
	t.Setenv("OAICA_MODELS_FILE", path)

	if _, err := ModelAdd(ModelAddOptions{ID: "added", Engine: "vllm", ModelPath: "/m/added"}); err != nil {
		t.Fatalf("ModelAdd: %v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"version"`, `"added"`, "/m/added"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("%s is missing from the new manifest:\n%s", want, got)
		}
	}
}
