package launch

// round30_declaration_matches_the_store_shape_integrity_test.go — two more ways
// the drift question was asked about something other than what the writer
// leaves (2026-09-27 audit, round 30, A-F1/A-F2).
//
// Pi is the one integration whose Models() sorts (pi.go:884) and the one that
// never implemented DeclaresSelection, so it fell back to sameModelSelection,
// which compares index by index. The picker returns the checked models last-
// checked-first, and Pi's own store keeps file order with new entries appended,
// so the two agreed only by accident: a two-model launch whose save order did
// not happen to be sorted read as drift on every run and re-entered the
// configure path forever. The primary is not the list order in any case — it is
// settings.defaultModel — and the writer preserves entries it did not write, so
// the list is not a sequence at all: it is the set of entries the writer owns.
//
// selectionRows kept an empty name where the writer drops it, so a saved
// selection carrying one asked a store to declare a row no write would ever
// produce.

import (
	"path/filepath"
	"testing"
)

// TestAPiStoreHoldingTheSameModelsInAnotherOrderIsNotDrift is A-F1.
func TestAPiStoreHoldingTheSameModelsInAnotherOrderIsNotDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	// The list the picker returns when the user checks qwen3 first: the last
	// one checked comes first.
	selection := []LaunchModel{fallbackLaunchModel("qwen3"), fallbackLaunchModel("llama3.2")}
	if err := (&Pi{}).Edit(selection); err != nil {
		t.Fatalf("Pi.Edit: %v", err)
	}

	// The path the launcher takes. Pi had no declaration of its own, so this
	// fell back to comparing Pi.Models() — which sorts — against the selection
	// index by index, and the two agreed only when the save order happened to
	// be sorted.
	c := &launcherClient{}
	c.inventory = &modelInventory{loaded: true, models: []LaunchModel{{Name: "qwen3"}, {Name: "llama3.2"}}}
	if !c.liveEditorDeclaration(t.Context(), &Pi{}, []string{"qwen3", "llama3.2"}) {
		t.Error("Pi's store reads as drift for the very selection a write just left there: the launch re-resolves the inventory and re-runs the configure step on every run")
	}

	// The same set in the other order — a launch selecting them the other way
	// round, or a picker whose check order differs. The list is not a sequence:
	// Pi's primary is settings.defaultModel, and the writer keeps the entries it
	// finds where they are.
	if !(&Pi{}).DeclaresSelection([]LaunchModel{fallbackLaunchModel("llama3.2"), fallbackLaunchModel("qwen3")}) {
		t.Error("Pi's store reads as drift for the same two models in another order: the list carries no sequence a write leaves, so comparing it as one is drift on every run")
	}

	// It is not a rubber stamp: a model the store does not hold is drift, and
	// so is a store holding a model this launch did not select.
	if (&Pi{}).DeclaresSelection([]LaunchModel{fallbackLaunchModel("qwen3"), fallbackLaunchModel("gemma4")}) {
		t.Error("Pi's store reads as current for a selection naming a model it does not hold")
	}
	if err := (&Pi{}).Edit([]LaunchModel{fallbackLaunchModel("qwen3"), fallbackLaunchModel("llama3.2"), fallbackLaunchModel("gemma4")}); err != nil {
		t.Fatalf("Pi.Edit: %v", err)
	}
	if (&Pi{}).DeclaresSelection(selection) {
		t.Error("Pi's store reads as current while it holds a model this launch did not select: the launch would leave it registered")
	}
}

// TestAPiStoreHoldingTheUsersOwnEntriesIsNotDrift: the writer preserves an
// entry it did not write (no _launch marker), so its presence is not drift.
func TestAPiStoreHoldingTheUsersOwnEntriesIsNotDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	selection := []LaunchModel{fallbackLaunchModel("qwen3")}
	if err := (&Pi{}).Edit(selection); err != nil {
		t.Fatalf("Pi.Edit: %v", err)
	}

	path := filepath.Join(home, ".pi", "agent", "models.json")
	doc := readJSONMapForTest(t, path)
	providers, _ := doc["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	models, _ := ollama["models"].([]any)
	ollama["models"] = append(models, map[string]any{"id": "hand-added", "name": "hand-added"})
	writeJSONMapForTest(t, path, doc)

	if !(&Pi{}).DeclaresSelection(selection) {
		t.Error("Pi's store reads as drift because the user's own entry is listed beside oaica's: the launch re-runs the configure step on every run for a file the write would not change")
	}
}

// TestAnEmptyNameIsNotARowTheWriterWouldLeave is A-F2.
func TestAnEmptyNameIsNotARowTheWriterWouldLeave(t *testing.T) {
	rows := selectionRows(nil, []string{"", "qwen3"})
	if len(rows) != 1 || rows[0].Name != "qwen3" {
		t.Fatalf("selectionRows([\"\" qwen3]) = %v; want the one row a write would leave: the writer skips an empty name (launchModelNames), so a declaration asked about it is asked about a row that will never exist", rows)
	}
	if len(selectionRows(nil, []string{""})) != 0 {
		t.Error("selectionRows([\"\"]) kept a row the writer drops: an editor that narrows to one entry (Cline) would spend its single slot on it and be configured with nothing")
	}
}
