package launch

// round29_declaration_matches_the_writer_integrity_test.go — a declaration must
// describe what the WRITER leaves, field for field (2026-09-27 audit, round 29,
// A-F1/A-F2/A-F3/A-F4).
//
// A declaration that reads less than its writer writes answers "already
// current" for a store a write would change. Cline's providers.json carries the
// remote credential (settings.apiKey, set from the live remote token and
// deleted when there is none) and the declaration read only the id and the base
// URL, so a store holding a rotated-away key read as current and every later
// launch skipped the rewrite — Cline kept dialling the remote with the old
// key. OpenClaw's writer sets the provider's api and apiKey unconditionally and
// the declaration read neither.
//
// Reading MORE than the writer writes is the mirror image, and it fails the
// other way: the declaration can never be satisfied, so the launch re-enters
// the configure path forever. Two shapes of that here — `selectionRows` built
// one row per NAME while the writer resolves and dedupes by the row (so
// "ollama/qwen3" beside the saved "qwen3" asked the store to declare two rows
// it was never given), and opencode's writer caps `recent` at ten entries while
// the declaration required the whole selection at its head.

import (
	"os"
	"testing"
)

// TestAClineStoreHoldingARotatedAwayRemoteKeyIsDrift is A-F1.
func TestAClineStoreHoldingARotatedAwayRemoteKeyIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"ds","base_url":"https://api.deepseek.invalid/v1","api_key":"KEY-OLD","tool_format":"tool_calls"}]}`)

	row := LaunchModel{Name: "ds/deepseek-chat"}
	if err := (&Cline{}).Edit([]LaunchModel{row}); err != nil {
		t.Fatalf("Cline.Edit: %v", err)
	}
	if !(&Cline{}).DeclaresSelection([]LaunchModel{row}) {
		t.Fatal("control: the pair Cline.Edit just wrote does not read as declared")
	}

	// The key is rotated. providers.json still holds the old one, and a write
	// now would replace it — Cline passes neither model nor key to its child,
	// so that file is the only credential Cline has for this remote.
	writeRemotes(t, `{"remotes":[{"name":"ds","base_url":"https://api.deepseek.invalid/v1","api_key":"KEY-NEW","tool_format":"tool_calls"}]}`)

	if (&Cline{}).DeclaresSelection([]LaunchModel{row}) {
		t.Error("Cline's store reads as current while its ollama provider holds a rotated-away key: every later launch skips the rewrite and Cline keeps asking the remote with the old credential")
	}

	// Control: the write repairs it, and then the store IS current.
	if err := (&Cline{}).Edit([]LaunchModel{row}); err != nil {
		t.Fatalf("Cline.Edit: %v", err)
	}
	if !(&Cline{}).DeclaresSelection([]LaunchModel{row}) {
		t.Error("control: the pair Cline.Edit just wrote with the new key does not read as declared")
	}
}

// TestAClineStoreCarryingAKeyItShouldNotHaveIsDrift is A-F1's other direction:
// a local model's write deletes the credential field, so a store still holding
// one is not what a write leaves.
func TestAClineStoreCarryingAKeyItShouldNotHaveIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	row := LaunchModel{Name: "llama3.2"}
	if err := (&Cline{}).Edit([]LaunchModel{row}); err != nil {
		t.Fatalf("Cline.Edit: %v", err)
	}
	if !(&Cline{}).DeclaresSelection([]LaunchModel{row}) {
		t.Fatal("control: the pair Cline.Edit just wrote does not read as declared")
	}

	path := clineProvidersPath(home)
	doc := readJSONMapForTest(t, path)
	providers, _ := doc["providers"].(map[string]any)
	provider, _ := providers[clineLaunchProvider].(map[string]any)
	settings, _ := provider["settings"].(map[string]any)
	settings["apiKey"] = "a-key-the-user-pasted-in"
	writeJSONMapForTest(t, path, doc)

	if (&Cline{}).DeclaresSelection([]LaunchModel{row}) {
		t.Error("Cline's store reads as current while its provider carries an apiKey the write would delete: the launch skips the write that removes it")
	}
}

// TestAnOpenclawProviderRewrittenByHandIsDrift is A-F2: the writer sets the
// provider's api and apiKey on every write, so a store holding other values is
// not what a write leaves.
func TestAnOpenclawProviderRewrittenByHandIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")

	row := []LaunchModel{fallbackLaunchModel("llama3.2")}
	if err := (&Openclaw{}).Edit(row); err != nil {
		t.Fatalf("Openclaw.Edit: %v", err)
	}
	if !(&Openclaw{}).DeclaresSelection(row) {
		t.Fatal("control: the config Openclaw.Edit just wrote does not read as declared")
	}

	for _, tc := range []struct{ field, value string }{
		{"api", "openai"},
		{"apiKey", "a-key-the-user-set"},
	} {
		t.Run(tc.field, func(t *testing.T) {
			path := openclawConfigPath(t, home)
			doc := readJSONMapForTest(t, path)
			modelsSection, _ := doc["models"].(map[string]any)
			providers, _ := modelsSection["providers"].(map[string]any)
			ollama, _ := providers["ollama"].(map[string]any)
			ollama[tc.field] = tc.value
			writeJSONMapForTest(t, path, doc)

			if (&Openclaw{}).DeclaresSelection(row) {
				t.Errorf("OpenClaw's config reads as current while models.providers.ollama.%s is %q and the write would set it back: the launch skips a write the store needs", tc.field, tc.value)
			}

			// Restore for the next case, and prove the control still holds.
			if err := (&Openclaw{}).Edit(row); err != nil {
				t.Fatalf("Openclaw.Edit: %v", err)
			}
		})
	}
}

// TestTwoSpellingsOfOneModelAreOneStoredRow is A-F4: the writer resolves the
// selection and keeps one row per model Name, so the declaration must be asked
// about the same rows.
func TestTwoSpellingsOfOneModelAreOneStoredRow(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

	// The store is written for the model, then the next launch is asked about
	// the same model under both of its spellings — the prefixed picker name and
	// the bare override, which resolve to ONE row.
	if err := (&OpenCode{}).Edit([]LaunchModel{fallbackLaunchModel("qwen3")}); err != nil {
		t.Fatalf("OpenCode.Edit: %v", err)
	}

	c := &launcherClient{}
	c.inventory = &modelInventory{loaded: true, models: []LaunchModel{{Name: "qwen3"}}}
	if !c.liveEditorDeclaration(t.Context(), &OpenCode{}, []string{"ollama/qwen3", "qwen3"}) {
		t.Error("liveEditorDeclaration says the store is not current for two spellings of the one model it holds: the writer resolves them to a single row, so the launch re-enters the configure path on every run")
	}
}

// TestAnElevenModelSelectionIsNarrowedNotDroppedInSilence is A-F3: opencode's
// store holds ten entries, so the selection it is asked about is ten.
func TestAnElevenModelSelectionIsNarrowedNotDroppedInSilence(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

	names := make([]string, 0, 11)
	for i := 0; i < 11; i++ {
		names = append(names, string(rune('a'+i))+"-model")
	}

	stored, dropped := editorStoredModels(&OpenCode{}, names)
	if len(stored) != 10 || len(dropped) != 1 || dropped[0] != "k-model" {
		t.Fatalf("editorStoredModels(11 models) = %d stored, dropped %v; want 10 stored and [k-model]: the store holds ten and the extra one is dropped in silence", len(stored), dropped)
	}

	rows := make([]LaunchModel, 0, len(stored))
	for _, n := range stored {
		rows = append(rows, fallbackLaunchModel(n))
	}
	if err := (&OpenCode{}).Edit(rows); err != nil {
		t.Fatalf("OpenCode.Edit: %v", err)
	}
	if !(&OpenCode{}).DeclaresSelection(rows) {
		t.Error("the state OpenCode.Edit just wrote for the narrowed selection does not read as declared: every launch re-enters the configure path for a store that cannot hold more")
	}
}

// A control on the test above: the file Edit writes really is capped at ten.
func TestOpencodeStateIsCappedAtTenForTheNarrowingTest(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

	var rows []LaunchModel
	for i := 0; i < 11; i++ {
		rows = append(rows, fallbackLaunchModel(string(rune('a'+i))+"-model"))
	}
	if err := (&OpenCode{}).Edit(rows); err != nil {
		t.Fatalf("OpenCode.Edit: %v", err)
	}
	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	doc := readJSONMapForTest(t, statePath)
	recent, _ := doc["recent"].([]any)
	if len(recent) != 10 {
		t.Fatalf("opencode's state holds %d entries after an 11-model write: this test's premise about the cap no longer holds", len(recent))
	}
	if _, err := os.Stat(statePath); err != nil {
		t.Fatal(err)
	}
}
