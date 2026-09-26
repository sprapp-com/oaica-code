package cmd

// interactive_router_list_print_integrity_test.go — the behavioural half of
// interactive_router_row_forgery_integrity_test.go: what /model list and
// /lora list actually print for a router answer that carries a newline.
//
// The arms are reached only from generateInteractive's readline loop, so these
// drive the two printers the arms now call, with a stubbed router answer — the
// shape interactive_oaica_show_integrity_test.go uses for /show info. Each
// test carries a control: an ordinary value must still print unquoted, so
// "quote everything" is not a pass.

import (
	"strings"
	"testing"
)

// A model whose description carries a newline is one row, quoted — not two.
func TestModelListDoesNotPrintAForgedRow(t *testing.T) {
	out := captureStdout(t, func() {
		oaicaPrintModelList([]oaicaModelListEntry{
			{ID: "kat-awq", Description: "fast general chat\n  other-model", Stars: 4},
			{ID: "glm-air", Stars: 3},
		})
	})

	lines := outputLines(out)
	if len(lines) != 4 { // header + 2 models + 1 description
		t.Errorf("/model list printed %d line(s) for 2 models and 1 description — a router-supplied string forged a row:\n%s", len(lines), out)
	}
	if !strings.Contains(out, `"fast general chat\n  other-model"`) {
		t.Errorf("the router's description is not shown in its quoted form, so the list reads as a model the router never published:\n%s", out)
	}
	// Controls: the ordinary rows keep their shape, star rating and all.
	if !strings.Contains(out, "  kat-awq") || !strings.Contains(out, starString(4)) {
		t.Errorf("the ordinary model row changed shape:\n%s", out)
	}
	if !strings.Contains(out, "  glm-air") || !strings.Contains(out, starString(3)) {
		t.Errorf("the star-only row (no description) changed shape:\n%s", out)
	}
}

// The id column is the router's too, and it is the one the user types back.
func TestModelListDoesNotPrintAForgedRowFromAnID(t *testing.T) {
	out := captureStdout(t, func() {
		oaicaPrintModelList([]oaicaModelListEntry{
			{ID: "kat-awq\n  fake-model", Stars: 1},
		})
	})
	lines := outputLines(out)
	if len(lines) != 2 { // header + the one model
		t.Errorf("/model list printed %d line(s) for 1 model — a router-supplied id forged a row:\n%s", len(lines), out)
	}
	if !strings.Contains(out, `"kat-awq\n  fake-model"`) {
		t.Errorf("the router's model id is not shown in its quoted form:\n%s", out)
	}
}

// /lora list prints a name, a backend model and a slot; two of the three are
// the router's strings.
func TestLoraListDoesNotPrintAForgedRow(t *testing.T) {
	out := captureStdout(t, func() {
		oaicaPrintLoraList([]oaicaLoraListEntry{
			{Name: "legal\n  adapter-fake  (model: x, slot: 99)", Model: "kat-awq", ID: 3},
			{Name: "malay", Model: "glm-air\n  fake-name", ID: 4},
		})
	})

	lines := outputLines(out)
	if len(lines) != 3 { // header + 2 adapters
		t.Errorf("/lora list printed %d line(s) for 2 adapters — a router-supplied string forged a row:\n%s", len(lines), out)
	}
	for _, want := range []string{`"legal\n  adapter-fake  (model: x, slot: 99)"`, `"glm-air\n  fake-name"`} {
		if !strings.Contains(out, want) {
			t.Errorf("/lora list does not show the router's value in its quoted form, want %s in:\n%s", want, out)
		}
	}
	// Control: the ordinary row keeps its exact shape, slot number included.
	if !strings.Contains(out, "  malay  (model: ") || !strings.Contains(out, "slot: 4)") {
		t.Errorf("the ordinary LoRA row changed shape:\n%s", out)
	}
}
