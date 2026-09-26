package cmd

// site_sections_row_forgery_integrity_test.go — `oaica site sections` printed a
// file-held title raw, so a newline in it painted a second, invented section
// row (2026-09-26 audit, tenth round).
//
// site.json is on-disk state, written by the model's plan and hand-editable
// afterwards — the same class of input `oaica usage` (PrintableCell), `model
// list` (manifestCell) and `remote list` (printableName) already quote at their
// print sites. normalizeSpec only TrimSpaces a title, so an inner newline
// survives every read path, and the section row it was printed into came out
// byte-identical in shape to a real one — a name, a kind and a summary the spec
// never held, exit 0.
//
// The test drives the output the way the command does: through Load, off a
// stored site.json, with the printing rule the whole tree shares.

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"github.com/ollama/ollama/internal/sitebuilder"
)

func TestSiteSectionsDoesNotPrintAForgedRow(t *testing.T) {
	dir := t.TempDir()
	stateDir := filepath.Join(dir, sitebuilder.StateDir)
	if err := os.MkdirAll(filepath.Join(stateDir, "sections"), 0o755); err != nil {
		t.Fatal(err)
	}
	// One real section, and one whose title carries a newline plus a row's
	// worth of columns — what a model reply or a hand edit can leave in the
	// file, and what normalizeSpec's TrimSpace does not remove.
	forged := "Prices\n  forged-99        custom       a section the spec never had"
	spec := `{"prompt":"p","model":"kat-awq","spec":{"title":"Tea Shop","tagline":"Same-day booking","language":"en",` +
		`"sections":[{"id":"hero","kind":"hero","title":"Hero"},` +
		`{"id":"pricing","kind":"pricing","title":` + strconv.Quote(forged) + `}]}}`
	if err := os.WriteFile(filepath.Join(stateDir, "site.json"), []byte(spec), 0o644); err != nil {
		t.Fatal(err)
	}

	site, err := sitebuilder.Load(dir)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(site.Spec.Sections) != 2 {
		t.Fatalf("premise: the loaded site has %d section(s), want 2", len(site.Spec.Sections))
	}
	if !strings.Contains(site.Spec.Sections[1].Title, "\n") {
		t.Fatalf("premise: the stored title lost its newline: %q", site.Spec.Sections[1].Title)
	}

	var out bytes.Buffer
	writeSiteSections(&out, site)

	// The listing is a header plus one line per section. A title that carries
	// a newline must not be able to add to that count.
	lines := strings.Split(strings.TrimSuffix(out.String(), "\n"), "\n")
	if want := 1 + len(site.Spec.Sections); len(lines) != want {
		t.Errorf("`site sections` printed %d line(s) for a 1-line header and %d section(s), want %d:\n%s\na title with a newline in it forged a section row — the file's own text was printed as a row nothing in the spec describes",
			len(lines), len(site.Spec.Sections), want, out.String())
	}
	if strings.Contains(out.String(), "\n  forged-99") {
		t.Errorf("the newline in the stored title was printed as a line break:\n%s", out.String())
	}
	// The value is still shown, in the form every other listing uses for a
	// value it cannot print on one line.
	if !strings.Contains(out.String(), strconv.Quote(forged)) {
		t.Errorf("the title is gone from the listing instead of being quoted:\n%s", out.String())
	}
}
