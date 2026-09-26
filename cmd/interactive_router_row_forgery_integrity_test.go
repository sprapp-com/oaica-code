package cmd

// interactive_router_row_forgery_integrity_test.go — /model list and /lora list
// printed the ROUTER's strings raw (2026-09-26 audit, round 13).
//
// Both lists come over HTTP from a router the user may not own — the model id
// and its "recommended for" description are set once through the router's
// admin API, and the LoRA name/model are whatever the router was configured
// with — so they are no more trusted than the catalog ids `oaica model sync`
// prints, which package cmd already puts through launch.PrintableCell
// (store_cell_forgery_integrity_test.go). A newline in any of them forged a row
// in the interactive picker: a model the router never published.
//
// The arms themselves are reached only from generateInteractive's readline
// loop, which needs a terminal, so the printing is now one function per list
// and the tests below drive THOSE with a stubbed router answer — the shape
// interactive_oaica_show_integrity_test.go uses for /show info. This file pins
// the arms to them, so the rule cannot be bypassed by printing raw in the arm
// again.

import (
	"strings"
	"testing"
)

// The arms must go through the printers, so the rule cannot be bypassed by
// printing raw in the arm again.
func TestTheListArmsUseThePrinters(t *testing.T) {
	src := interactiveSource(t)

	modelArm := slashCaseBlock(t, src, "/model")
	if !strings.Contains(modelArm, "oaicaPrintModelList(") {
		t.Errorf("/model no longer prints through oaicaPrintModelList — the rule lives there:\n%s", modelArm)
	}
	for _, forbidden := range []string{"m.Description", "m.ID"} {
		if strings.Contains(modelArm, forbidden) {
			t.Errorf("/model prints %s in the arm itself, outside the printer that applies launch.PrintableCell:\n%s", forbidden, modelArm)
		}
	}

	loraArm := slashCaseBlock(t, src, "/lora")
	if !strings.Contains(loraArm, "oaicaPrintLoraList(") {
		t.Errorf("/lora no longer prints through oaicaPrintLoraList — the rule lives there:\n%s", loraArm)
	}
	// l.Name alone is not forbidden: the arm also uses it as a LOOKUP key
	// (byName[l.Name] = l), which prints nothing.
	for _, forbidden := range []string{"l.Model", "l.ID"} {
		if strings.Contains(loraArm, forbidden) {
			t.Errorf("/lora prints %s in the arm itself, outside the printer that applies launch.PrintableCell:\n%s", forbidden, loraArm)
		}
	}
	// The unknown-name hint joins every configured LoRA onto ONE line, which is
	// what makes a newline in a name a forged row.
	if !strings.Contains(loraArm, "launch.PrintableCell(l.Name)") {
		t.Errorf("/lora's unknown-name hint joins the router's names without PrintableCell:\n%s", loraArm)
	}
}
