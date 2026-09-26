package cmd

// model_report_row_forgery_integrity_test.go — `oaica model sync` and `oaica
// model scan` printed their report rows RAW (2026-09-26 audit, round 13).
//
// Every id these two commands print comes from somewhere oaica does not
// control: a fetched catalog, or the filesystem. cmd/launch has the rule for
// exactly this class and already applies it to every store-backed value it
// prints (manifestCell, and printableName for a remote), and package cmd uses
// it for its own listings — but the sync/scan report was written as a bare
// fmt.Printf("  + %s\n", id), so a catalog key of
//
//	"evil\n  + anthropic/claude-4.9-opus"
//
// painted two `+` rows: the user reads a diff that does not match what was
// installed. The scan's conflict rows name the raw FILE PATHS, which can carry
// a newline of their own.
//
// These tests drive the real commands through NewCLI and assert on the
// terminal output. Each carries a control: an ordinary id must still print
// unquoted and unaltered, so "quote everything" is not a pass.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const forgedModelID = "evil\n  + anthropic/claude-4.9-opus"

// A catalog key carrying a newline is one row, quoted — not two.
func TestModelSyncDoesNotPrintAForgedRow(t *testing.T) {
	t.Setenv("OAICA_MODELS_FILE", filepath.Join(t.TempDir(), "models.json"))

	catalog := filepath.Join(t.TempDir(), "catalog.json")
	body := `{"version":1,"models":{` +
		`"evil\n  + anthropic/claude-4.9-opus":{"id":"evil\n  + anthropic/claude-4.9-opus","engine":"vllm","context_window":4096},` +
		`"good-model":{"id":"good-model","engine":"vllm","context_window":4096}}}`
	if err := os.WriteFile(catalog, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out := runOAICA(t, "model", "sync", "--url", "file://"+catalog)
	lines := outputLines(out)
	if len(lines) != 3 { // the synced-from header + two added models
		t.Errorf("`model sync` printed %d line(s) for a header + 2 catalog entries — a catalog id forged a row:\n%s", len(lines), out)
	}
	if !strings.Contains(out, `"evil\n  + anthropic/claude-4.9-opus"`) {
		t.Errorf("the catalog id is not shown in its quoted form, so the report reads as two models where one was installed:\n%s", out)
	}
	// Control: the ordinary entry keeps its exact row shape.
	if !strings.Contains(out, "  + good-model\n") {
		t.Errorf("the ordinary report row changed shape — want \"  + good-model\" in:\n%s", out)
	}
}

// A "skipped" line is the other half of the same report, and it is built from
// the catalog's own bytes too (id + the validation error).
func TestModelSyncDoesNotPrintAForgedSkipRow(t *testing.T) {
	t.Setenv("OAICA_MODELS_FILE", filepath.Join(t.TempDir(), "models.json"))

	catalog := filepath.Join(t.TempDir(), "catalog.json")
	// No engine: these entries fail validation, so their ids land in
	// rep.Skipped — one nasty, one ordinary as the control.
	body := `{"version":1,"models":{` +
		`"evil\n  ! fake-model":{"id":"evil\n  ! fake-model","context_window":4096},` +
		`"plainbad":{"id":"plainbad","context_window":4096},` +
		`"good-model":{"id":"good-model","engine":"vllm","context_window":4096}}}`
	if err := os.WriteFile(catalog, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	out := runOAICA(t, "model", "sync", "--url", "file://"+catalog)
	lines := outputLines(out)
	if len(lines) != 4 { // header + one added + two skipped
		t.Errorf("`model sync` printed %d line(s) for a header + 1 added + 2 skipped — a catalog id forged a row:\n%s", len(lines), out)
	}
	if !strings.Contains(out, `"evil\n  ! fake-model`) {
		t.Errorf("the skipped catalog id is not shown in its quoted form:\n%s", out)
	}
	if !strings.Contains(out, "  ! plainbad: unknown engine") {
		t.Errorf("the ordinary skipped row changed shape:\n%s", out)
	}
}

// The scan's conflict row names two raw filesystem paths, and a path is a
// string the filesystem does not validate. A directory with a newline in its
// name must not forge a report row.
func TestModelScanDoesNotPrintAForgedRow(t *testing.T) {
	t.Setenv("OAICA_MODELS_FILE", filepath.Join(t.TempDir(), "models.json"))

	first := t.TempDir()
	if err := os.WriteFile(filepath.Join(first, "evil.gguf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Same id (sanitizeModelID maps the newline to "-" only in the ID, not in
	// the path), different path: the scan reports the refusal by naming BOTH
	// paths.
	second := filepath.Join(t.TempDir(), "we\nird dir")
	if err := os.MkdirAll(second, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(second, "evil.gguf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	if out := runOAICA(t, "model", "scan", first); !strings.Contains(out, "  + evil\n") {
		t.Fatalf("premise: the first scan did not register the model:\n%s", out)
	}

	out := runOAICA(t, "model", "scan", second)
	lines := outputLines(out)
	if len(lines) != 2 { // the scanned-N-dirs header + the one conflict line
		t.Errorf("`model scan` printed %d line(s) for a header + 1 conflict — a path in the message forged a row:\n%s", len(lines), out)
	}
	if !strings.Contains(out, `we\nird dir`) {
		t.Errorf("the conflict message does not show the newline-bearing path in quoted form:\n%s", out)
	}
}
