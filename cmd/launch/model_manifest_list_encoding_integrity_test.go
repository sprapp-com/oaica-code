package launch

// model_manifest_list_encoding_integrity_test.go — `oaica model list` printed a
// manifest id and its notes raw (2026-09-26 audit, tenth round, finding 22).
//
// The manifest is a hand-editable store: models.json can be edited, merged or
// synced from a catalog, so an id with a control character in it reaches the
// listing regardless of what `model add` validates today. Printed with %s into
// a table cell, a newline in that id paints a second, entirely fabricated row —
// a model a reader takes as configured. And the notes cell is truncated with
// notes[:57], a BYTE cut: a note ending in a multi-byte rune is cut inside one
// and the table ships invalid UTF-8 to every pipe consumer and log shipper.
//
// Both are the class printableName (remote_cli.go) already covers for remote
// names — with the addition the manifest needs, that a value that is not valid
// UTF-8 must be sanitised rather than printed through.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"unicode/utf8"
)

// writeModelsFile points models.json at a throwaway path holding body, the way
// a hand edit (or a catalog merge) leaves it.
func writeModelsFile(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "models.json")
	t.Setenv("OAICA_MODELS_FILE", path)
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// A newline in an id and a byte-cut multi-byte note must neither forge a row
// nor corrupt the byte stream, and the entry must still be readable.
func TestModelListDoesNotForgeRowsOrEmitInvalidUTF8(t *testing.T) {
	// é is two bytes, so a 57-byte cut of a run of them lands inside one.
	multibyte := quoteJSON(t, strings.Repeat("é", 40))
	body := `{"version":1,"models":{` +
		`"forged":{"id":"evil\nid-2  vllm  awq-w4a16  262144  forged","engine":"vllm","quant":"awq-w4a16","context_window":262144},` +
		`"multibyte":{"id":"cjk-notes","engine":"vllm","notes":` + multibyte + `}}}`
	writeModelsFile(t, body)

	var buf bytes.Buffer
	if err := WriteModelList(&buf); err != nil {
		t.Fatalf("WriteModelList: %v", err)
	}
	out := buf.String()

	if !utf8.ValidString(out) {
		t.Errorf("`oaica model list` emitted invalid UTF-8 (%q) — the notes cell is cut mid-rune, so every pipe consumer sees corrupt bytes:\n%s", out, out)
	}

	lines := 0
	for _, l := range strings.Split(strings.TrimSpace(out), "\n") {
		if strings.TrimSpace(l) != "" {
			lines++
		}
	}
	if lines != 3 {
		t.Errorf("`oaica model list` printed %d line(s) for two manifest entries, want 3 (header + one row each) — a newline in the id forged a row:\n%s", lines, out)
	}
	if strings.Contains(out, "\nid-2") {
		t.Errorf("the forged row is in the output:\n%s", out)
	}

	// Readable form: each entry is still identifiable in the table.
	if !strings.Contains(out, "evil") {
		t.Errorf("the entry whose id carries a newline vanished from the listing instead of being shown escaped:\n%s", out)
	}
	if !strings.Contains(out, "cjk-notes") {
		t.Errorf("the entry with the multi-byte note vanished from the listing:\n%s", out)
	}
}

// The same printer class one verb over: `oaica model show` prints the id on a
// line of its own, so a newline in it forges a field line there.
func TestModelShowDoesNotForgeAFieldLine(t *testing.T) {
	// The manifest is a map keyed by ID, so the forged entry is keyed by the
	// same string it declares as its id.
	forged := "evil\nengine:           vllm"
	body := `{"version":1,"models":{` + quoteJSON(t, forged) + `:{"id":` + quoteJSON(t, forged) + `,"engine":"vllm"}}}`
	writeModelsFile(t, body)

	var buf bytes.Buffer
	if err := WriteModelShow(&buf, forged); err != nil {
		t.Fatalf("WriteModelShow: %v", err)
	}
	// A line that STARTS with a field name is a field line; the same text
	// inside a quoted id is part of the id's own value.
	fieldLines := 0
	for _, l := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if strings.HasPrefix(l, "engine:") {
			fieldLines++
		}
	}
	if fieldLines != 1 {
		t.Errorf("`oaica model show` printed %d engine field line(s), want exactly 1 — the id forged another:\n%s", fieldLines, buf.String())
	}
}
