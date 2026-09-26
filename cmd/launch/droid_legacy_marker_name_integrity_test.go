package launch

// droid_legacy_marker_name_integrity_test.go — a model whose own name ends in
// the legacy id marker was duplicated on every launch (2026-09-27 audit, round
// 21).
//
// The id this file writes is "custom:<picker name>-<index>", and the round-20
// ownership test read it back by stripping the legacy segment upstream's
// `ollama config droid` used to put there ("-[Ollama]"). For a model actually
// named "foo-[Ollama]" the two readings are the same bytes: the launch writes
// "custom:foo-[Ollama]-0", the reverse strips it to "foo", and the daemon
// ownership test — picker must equal the model stored beside it — saw "foo"
// against "foo-[Ollama]" and concluded the entry was the user's. The entry was
// then preserved as foreign and a second copy appended, so launching the model
// twice left it in Droid's picker twice, with the first copy never updated and
// never cleaned. The marker is now a second READING of the id, chosen by the
// model the entry stores, which is the field that tells the readings apart.

import (
	"testing"
)

func TestDroidEdit_AModelNamedLikeTheLegacyMarkerIsNotDuplicated(t *testing.T) {
	d := &Droid{}
	home := t.TempDir()
	setTestHome(t, home)
	// The name resolves to nothing; the sweep is stubbed out of caution.
	stubBareIndex(t, map[string][]string{})

	t.Setenv("OAICA_REMOTES_FILE", withTempRemotesFile(t))
	settingsPath := droidSettingsPath(t, home)

	const model = "foo-[Ollama]"
	if err := d.Edit(testLaunchModels(model)); err != nil {
		t.Fatalf("first launch: %v", err)
	}
	if got := droidEntriesFor(droidCustomModels(t, settingsPath), model); len(got) != 1 {
		t.Fatalf("after the first launch: %d entries for %q, want 1", len(got), model)
	}

	if err := d.Edit(testLaunchModels(model)); err != nil {
		t.Fatalf("re-launch: %v", err)
	}

	entries := droidCustomModels(t, settingsPath)
	got := droidEntriesFor(entries, model)
	if len(got) != 1 {
		t.Fatalf("after the re-launch: %d entries for %q, want exactly 1 — the id this file writes for the name is byte-identical to the legacy shape, and the entry it owns must still be recognised as its own: %v", len(got), model, entries)
	}
	if id := got[0]["id"]; id != "custom:foo-[Ollama]-0" {
		t.Errorf("entry id = %v, want custom:foo-[Ollama]-0", id)
	}
	if key := got[0]["apiKey"]; key != droidDaemonKey {
		t.Errorf("entry apiKey = %v, want the daemon marker %q", key, droidDaemonKey)
	}
	if len(entries) != 2 {
		t.Errorf("customModels holds %d entries, want 2 (the launched model + the user's own): %v", len(entries), entries)
	}
	if droidEntryByID(entries, "user-gpt-4") == nil {
		t.Errorf("the user's own entry was not preserved: %v", entries)
	}
}
