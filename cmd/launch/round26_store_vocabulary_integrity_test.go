package launch

// round26_store_vocabulary_integrity_test.go — round 25 moved the writers to the
// id the backend serves and left two readers behind (2026-09-27 audit, round
// 26).
//
// A row whose picker name is a display label (the ollama-cloud catalogue names
// it "ollama/gpt-oss" and the daemon serves it as "gpt-oss:cloud") is now
// written as that daemon-side id. Two things still spoke the old vocabulary:
//
//   - OpenClaw's agents.defaults.model.primary, and the session-override clear
//     that must name the same model, were still built from the row's Name — a
//     model OpenClaw's own provider list no longer declares.
//   - The launcher's drift term compared the SAVED selection (picker names)
//     against what the editor's store reports (stored ids), so it could never be
//     true for such a row: every launch re-resolved the inventory and rewrote a
//     config that had not changed. opencode made it unconditional: its Models()
//     answered nil, which no selection can ever equal.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// TestOpenclawPrimaryNamesTheModelItsProviderDeclares covers both places the
// primary is written: the config OpenClaw reads and the session state that
// would otherwise shadow it.
func TestOpenclawPrimaryNamesTheModelItsProviderDeclares(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	sessionsDir := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// A session left over from an earlier launch: it holds the previous primary
	// in the same spelling the real TUI writes ("ollama/<model>", see the
	// fixtures in openclaw_test.go).
	if err := os.WriteFile(filepath.Join(sessionsDir, "sessions.json"),
		[]byte(`{"sess-1":{"model":"ollama/old-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (&Openclaw{}).Edit([]LaunchModel{cloudCatalogueRow()}); err != nil {
		t.Fatalf("Openclaw.Edit: %v", err)
	}

	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	doc := readJSONMapForTest(t, configPath)
	agents, _ := doc["agents"].(map[string]any)
	defaults, _ := agents["defaults"].(map[string]any)
	modelConfig, _ := defaults["model"].(map[string]any)
	if got := modelConfig["primary"]; got != "ollama/gpt-oss:cloud" {
		t.Errorf("agents.defaults.model.primary = %v, want %q: the entry is declared as %q, so a primary naming %q points at a model OpenClaw's own provider list does not contain", got, "ollama/gpt-oss:cloud", "gpt-oss:cloud", "ollama/"+cloudCatalogueRow().Name)
	}

	session := readJSONMapForTest(t, filepath.Join(sessionsDir, "sessions.json"))
	sess, _ := session["sess-1"].(map[string]any)
	if got := sess["model"]; got != "ollama/gpt-oss:cloud" {
		t.Errorf("session model = %v, want %q: the session state holds the primary in the same spelling, so anything else is not the model this launch configured", got, "ollama/gpt-oss:cloud")
	}
}

// TestTheDriftTermReadsTheStoresOwnVocabulary is the launcher half: a store
// that declares the daemon-side id is not drift for a selection saved under the
// picker name.
//
// It drives the real OpenClaw store, not a stand-in: the editor that answers in
// the store's vocabulary is the one whose writers embed an id no picker name
// equals, and the term reaches it through the editor's own reader
// (storeDeclarationEditor) rather than through any flat list the test could
// fake (2026-09-27 audit, round 27).
func TestTheDriftTermReadsTheStoresOwnVocabulary(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	row := cloudCatalogueRow()
	c := &launcherClient{}
	// The inventory the picker was built from: the catalogue row, under the
	// picker name a save carries (findLaunchModel has not stripped the prefix
	// by the time a saved selection is resolved against it).
	c.inventory = &modelInventory{loaded: true, models: []LaunchModel{
		{Name: "ollama/gpt-oss", Remote: true, Upstream: row.Upstream},
	}}

	if err := (&Openclaw{}).Edit([]LaunchModel{row}); err != nil {
		t.Fatalf("Openclaw.Edit: %v", err)
	}
	if !c.liveEditorDeclaration(t.Context(), &Openclaw{}, []string{"gpt-oss"}) {
		t.Error("liveEditorDeclaration(store declaring gpt-oss:cloud, selection gpt-oss) = false: the store holds exactly the model this selection is written as, so the launch re-resolves the inventory and rewrites an unchanged config on every run")
	}

	// It is not a rubber stamp: a store declaring something else is drift.
	if err := (&Openclaw{}).Edit([]LaunchModel{{Name: "llama3.2"}}); err != nil {
		t.Fatalf("Openclaw.Edit: %v", err)
	}
	if c.liveEditorDeclaration(t.Context(), &Openclaw{}, []string{"gpt-oss"}) {
		t.Error("liveEditorDeclaration said an unrelated store matches the selection: the config would be left pointing at a model the launch did not choose")
	}
}

type fakeEditor struct{ models []string }

func (f fakeEditor) Paths() []string          { return nil }
func (f fakeEditor) Edit([]LaunchModel) error { return nil }
func (f fakeEditor) Models() []string         { return f.models }

// writeJSONMapForTest rewrites a JSON document a test has read with
// readJSONMapForTest, keeping the file's directory in place (opencode's state
// lives several levels below HOME).
func writeJSONMapForTest(t *testing.T, path string, doc map[string]any) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestOpenCodeReportsTheModelsItsStateDeclares: Models() answered nil, so the
// drift term was false for every opencode selection and each launch rewrote the
// state file it had just read.
func TestOpenCodeReportsTheModelsItsStateDeclares(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	if err := (&OpenCode{}).Edit([]LaunchModel{cloudCatalogueRow()}); err != nil {
		t.Fatalf("OpenCode.Edit: %v", err)
	}
	got := (&OpenCode{}).Models()
	if len(got) != 1 || got[0] != "gpt-oss:cloud" {
		t.Errorf("OpenCode.Models() = %v, want [gpt-oss:cloud]: the state Edit writes declares that id", got)
	}

	// An entry naming a provider oaica does not write is the user's own history
	// and must not be reported as this integration's selection.
	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	doc := readJSONMapForTest(t, statePath)
	recent, _ := doc["recent"].([]any)
	doc["recent"] = append(recent, map[string]any{"providerID": "anthropic", "modelID": "claude-sonnet-5"})
	writeJSONMapForTest(t, statePath, doc)

	got = (&OpenCode{}).Models()
	if len(got) != 1 || got[0] != "gpt-oss:cloud" {
		t.Errorf("OpenCode.Models() = %v after an entry for another provider, want [gpt-oss:cloud]: a foreign provider's history is not a model this launch wrote", got)
	}
}

// TestOpenCodeReportsAConfiguredRemotesModel: the state stores the remote's own
// upstream id under the remote's block, and that is the id the launcher's
// selection is written as — so it must be reported, not filtered out.
func TestOpenCodeReportsAConfiguredRemotesModel(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"ds","base_url":"https://api.deepseek.invalid/v1","api_key":"KEY","tool_format":"tool_calls"}]}`)

	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	writeJSONMapForTest(t, statePath, map[string]any{
		"recent": []any{map[string]any{"providerID": "ds", "modelID": "deepseek-chat"}},
	})

	got := (&OpenCode{}).Models()
	if len(got) != 1 || got[0] != "deepseek-chat" {
		t.Errorf("OpenCode.Models() = %v, want [deepseek-chat]: the entry names a configured remote's block, which buildInlineConfig writes for this integration's own selection", got)
	}
}
