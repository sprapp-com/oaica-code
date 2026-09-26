package launch

// round27_store_declaration_integrity_test.go — round 26 asked the drift
// question in a flat vocabulary, and round 27's audit found what a flat list
// cannot say (2026-09-27).
//
// The term asked whether the store's ids CONTAIN the ids the launch would
// write. Two things fell out of that:
//
//   - A store holding MORE than the selection read as current, so an editor
//     whose writer owns a second field kept it stale: OpenClaw's
//     agents.defaults.model.primary still named a model the launch had not
//     chosen, Edit was skipped, and the app ran that model (F1).
//   - A store holding the same id beside a different endpoint or under a
//     different provider block read as current, because an id is not an
//     identity: opencode's state keys a model by the block that declares it
//     (F3) and Cline records the endpoint beside the id in both of its
//     documents (F4). The launch then left the config dialling an endpoint the
//     selection did not choose — the daemon, for a model only a remote serves.
//
// The same flat reading could also not rescue a FALSE match: a store holding
// the LOCAL model "gpt-oss" while the launch picked the cloud row of that name
// read as equal in the picker's vocabulary and as equal in nothing else (F2).
//
// The question is now asked of the editor, about its own stores
// (storeDeclarationEditor), so each integration answers with what its writers
// publish, what half of a model's identity its store records, and whether its
// writer keeps rows the selection does not name.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAStoreHoldingMoreThanTheSelectionIsStillDrift is F1: OpenClaw's provider
// list holding the selection is not the whole store — the same write owns
// agents.defaults.model.primary, and a primary naming another model is the app
// running a model this launch did not choose.
func TestAStoreHoldingMoreThanTheSelectionIsStillDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	if err := (&Openclaw{}).Edit([]LaunchModel{{Name: "llama3.2"}}); err != nil {
		t.Fatalf("Openclaw.Edit: %v", err)
	}

	// The auditor's store: the selection plus a row of the user's own (no
	// "cost" object, which is what marks an entry oaica wrote), and a primary
	// left pointing at that row.
	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	doc := readJSONMapForTest(t, configPath)
	modelsSection, _ := doc["models"].(map[string]any)
	providers, _ := modelsSection["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	list, _ := ollama["models"].([]any)
	ollama["models"] = append(list, map[string]any{"id": "my-own-model", "name": "my-own-model"})
	agents, _ := doc["agents"].(map[string]any)
	defaults, _ := agents["defaults"].(map[string]any)
	modelConfig, _ := defaults["model"].(map[string]any)
	modelConfig["primary"] = "ollama/my-own-model"
	writeJSONMapForTest(t, configPath, doc)

	c := &launcherClient{}
	if c.liveEditorDeclaration(t.Context(), &Openclaw{}, []string{"llama3.2"}) {
		t.Error("liveEditorDeclaration said the store is current while its primary names another model: the launch skips Edit, and OpenClaw keeps running a model this launch did not choose")
	}

	// The same store with the primary the launch would write IS current: the
	// user's own row after the selection is not drift.
	modelConfig["primary"] = "ollama/llama3.2"
	writeJSONMapForTest(t, configPath, doc)
	if !c.liveEditorDeclaration(t.Context(), &Openclaw{}, []string{"llama3.2"}) {
		t.Error("liveEditorDeclaration said a store holding the selection, with the primary this launch would write, is not current: every launch would rewrite a config that did not change")
	}
}

// TestAStaleSessionIsDrift is F1's other half: the primary can be right while a
// session record still names another model, and the same write clears it.
func TestAStaleSessionIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	if err := (&Openclaw{}).Edit([]LaunchModel{{Name: "llama3.2"}}); err != nil {
		t.Fatalf("Openclaw.Edit: %v", err)
	}
	sessionsDir := filepath.Join(home, ".openclaw", "agents", "main", "sessions")
	if err := os.MkdirAll(sessionsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	sessionsPath := filepath.Join(sessionsDir, "sessions.json")
	if err := os.WriteFile(sessionsPath, []byte(`{"sess-1":{"model":"ollama/old-model"}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	c := &launcherClient{}
	if c.liveEditorDeclaration(t.Context(), &Openclaw{}, []string{"llama3.2"}) {
		t.Error("liveEditorDeclaration said the store is current while a session still names another model: the session shadows the primary on the next TUI launch, so the launch skips the very Edit that clears it")
	}
	if err := os.WriteFile(sessionsPath, []byte(`{"sess-1":{"model":"ollama/llama3.2"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !c.liveEditorDeclaration(t.Context(), &Openclaw{}, []string{"llama3.2"}) {
		t.Error("liveEditorDeclaration said a session naming the primary the launch would write is drift: every launch would rewrite the session state")
	}
}

// TestALocalModelOfTheCloudRowsNameIsDrift is F2: Cline's stores holding the
// model id the LAUNCH would use for a local model is not the cloud row the
// selection picked, even though the picker name of the two is one string.
func TestALocalModelOfTheCloudRowsNameIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	// The store as a previous launch of the LOCAL model "gpt-oss" left it.
	local := LaunchModel{Name: "gpt-oss"}
	if err := writeClineProvidersConfig(clineProvidersPath(home), map[string]any{}, local); err != nil {
		t.Fatalf("writeClineProvidersConfig: %v", err)
	}
	if err := writeClineLegacyGlobalState(clineLegacyGlobalStatePath(home), map[string]any{}, local); err != nil {
		t.Fatalf("writeClineLegacyGlobalState: %v", err)
	}

	c := &launcherClient{}
	c.inventory = &modelInventory{loaded: true, models: []LaunchModel{cloudCatalogueRow()}}
	if c.liveEditorDeclaration(t.Context(), &Cline{}, []string{"gpt-oss"}) {
		t.Error("liveEditorDeclaration said Cline's store is current while it holds the local model \"gpt-oss\" and the selection is the cloud row served as \"gpt-oss:cloud\": the launch leaves Cline asking the daemon for a model it did not choose")
	}

	// Written for the cloud row, the same store IS current — the id the writer
	// publishes is the one the term has to compare against.
	if err := (&Cline{}).Edit([]LaunchModel{cloudCatalogueRow()}); err != nil {
		t.Fatalf("Cline.Edit: %v", err)
	}
	if !c.liveEditorDeclaration(t.Context(), &Cline{}, []string{"gpt-oss"}) {
		t.Error("liveEditorDeclaration said a store holding the id this launch writes is not current: every launch would rewrite both of Cline's documents")
	}
}

// TestARemoteModelBesideTheDaemonsEndpointIsDrift is F4: Cline's documents
// record an endpoint beside the id, and the daemon's endpoint for a model only
// a remote serves is a different model.
func TestARemoteModelBesideTheDaemonsEndpointIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"ds","base_url":"https://api.deepseek.invalid/v1","api_key":"KEY","tool_format":"tool_calls"}]}`)

	// A store written for the daemon model of the same id.
	daemonRow := LaunchModel{Name: "deepseek-chat"}
	if err := writeClineProvidersConfig(clineProvidersPath(home), map[string]any{}, daemonRow); err != nil {
		t.Fatalf("writeClineProvidersConfig: %v", err)
	}
	if err := writeClineLegacyGlobalState(clineLegacyGlobalStatePath(home), map[string]any{}, daemonRow); err != nil {
		t.Fatalf("writeClineLegacyGlobalState: %v", err)
	}

	remoteRow := LaunchModel{Name: "ds/deepseek-chat"}
	if (&Cline{}).DeclaresSelection([]LaunchModel{remoteRow}) {
		t.Error("Cline.DeclaresSelection said the store is current while both documents dial the DAEMON for a model the selection serves from a remote: the launch leaves Cline asking the wrong endpoint for it")
	}

	if err := (&Cline{}).Edit([]LaunchModel{remoteRow}); err != nil {
		t.Fatalf("Cline.Edit: %v", err)
	}
	if !(&Cline{}).DeclaresSelection([]LaunchModel{remoteRow}) {
		t.Error("Cline.DeclaresSelection said the pair it just wrote does not hold the selection: every launch would rewrite both documents")
	}
}

// TestTheStateUnderAnotherBlockIsDrift is F3: opencode's state keys a model by
// the provider block that declares it, and the daemon's block is not a
// remote's.
func TestTheStateUnderAnotherBlockIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"ds","base_url":"https://api.deepseek.invalid/v1","api_key":"KEY","tool_format":"tool_calls"}]}`)

	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	writeJSONMapForTest(t, statePath, map[string]any{
		"recent": []any{map[string]any{"providerID": "ds", "modelID": "llama3.2"}},
	})

	c := &launcherClient{}
	if c.liveEditorDeclaration(t.Context(), &OpenCode{}, []string{"llama3.2"}) {
		t.Error("liveEditorDeclaration said opencode's state is current while the entry names a REMOTE's block and the launch would write the daemon's: the launch leaves opencode dialling the remote for a model it did not attribute there")
	}

	// Written under the daemon's block, the same id IS current.
	if err := (&OpenCode{}).Edit([]LaunchModel{{Name: "llama3.2"}}); err != nil {
		t.Fatalf("OpenCode.Edit: %v", err)
	}
	if !c.liveEditorDeclaration(t.Context(), &OpenCode{}, []string{"llama3.2"}) {
		t.Error("liveEditorDeclaration said the state Edit just wrote is not current: every launch would rewrite opencode's state file")
	}
}

// TestThePickerReadingCannotRescueAStoreIDStore is F2's shape for the reader
// that answers in ids: an editor whose Models() reports stored ids must not be
// judged in the picker's vocabulary, where a store holding "gpt-oss" looks
// equal to a selection picked as "gpt-oss" and written as "gpt-oss:cloud".
func TestThePickerReadingCannotRescueAStoreIDStore(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	c := &launcherClient{}
	c.inventory = &modelInventory{loaded: true, models: []LaunchModel{cloudCatalogueRow()}}
	// idStore stands for an editor that answers in the store's vocabulary
	// without owning a real store: it declares the selection only when it holds
	// the id the launch would write, which is the rule OpenClaw's reader
	// applies to its own file.
	store := &idStore{ids: []string{"gpt-oss"}}
	if c.liveEditorDeclaration(t.Context(), store, []string{"gpt-oss"}) {
		t.Error("liveEditorDeclaration accepted the store on the picker's vocabulary: a store holding the local \"gpt-oss\" is not the cloud row this launch writes as \"gpt-oss:cloud\"")
	}
	if got := launchModelWriteIDs(selectionRows(c.inventory.models, []string{"gpt-oss"})); len(got) != 1 || got[0] != "gpt-oss:cloud" {
		t.Fatalf("launchModelWriteIDs(selectionRows(...)) = %v, want [gpt-oss:cloud]: the test's own premise about the written id", got)
	}
	store.ids = []string{"gpt-oss:cloud"}
	if !c.liveEditorDeclaration(t.Context(), store, []string{"gpt-oss"}) {
		t.Error("liveEditorDeclaration rejected a store holding exactly the id this launch writes")
	}
}

// idStore is an Editor that answers the drift question in the store's
// vocabulary, the way an integration whose store keeps the written id does.
type idStore struct{ ids []string }

func (s *idStore) Paths() []string          { return nil }
func (s *idStore) Edit([]LaunchModel) error { return nil }
func (s *idStore) Models() []string         { return append([]string(nil), s.ids...) }

func (s *idStore) DeclaresSelection(models []LaunchModel) bool {
	return sameStoreStrings(s.ids, launchModelWriteIDs(models))
}

// TestTwoRowsThatShareAnIDAreWrittenOnce is F6: OpenClaw's provider list is
// keyed by id, so a selection naming one backend twice must not publish two
// entries OpenClaw cannot tell apart.
func TestTwoRowsThatShareAnIDAreWrittenOnce(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	// A catalogue row and the daemon row of the id it is served as: one model,
	// two rows, one id.
	rows := []LaunchModel{cloudCatalogueRow(), cloudDaemonRow()}
	if err := (&Openclaw{}).Edit(rows); err != nil {
		t.Fatalf("Openclaw.Edit: %v", err)
	}

	config, err := openclawReadConfig(home)
	if err != nil {
		t.Fatal(err)
	}
	ids := openclawProviderModelIDs(config)
	if len(ids) != 1 || ids[0] != "gpt-oss:cloud" {
		t.Errorf("OpenClaw's provider list = %v for two rows naming one id, want [gpt-oss:cloud] once: a second entry for an id is an entry the provider cannot tell from the first", ids)
	}
	if !(&Openclaw{}).DeclaresSelection(rows) {
		t.Error("DeclaresSelection said the list it just wrote does not hold the selection: every launch would rewrite the config")
	}
}

// TestTheSecondHalfsStagingFailurePublishesNeitherHalf is F5: the pair is
// written together or not at all, and staging is what makes that true. The
// second document's directory is not writable, so the publish has to fail
// before the first document becomes visible.
func TestTheSecondHalfsStagingFailurePublishesNeitherHalf(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	// A previous launch, so there is a pair on disk to preserve.
	if err := (&Cline{}).Edit([]LaunchModel{{Name: "llama3.2"}}); err != nil {
		t.Fatalf("Cline.Edit: %v", err)
	}
	providersPath := clineProvidersPath(home)
	before, err := os.ReadFile(providersPath)
	if err != nil {
		t.Fatal(err)
	}

	// globalState.json's directory is ~/.cline/data, which also holds the
	// settings/ directory the first document lives in: readable and traversable,
	// not writable.
	dataDir := filepath.Join(home, ".cline", "data")
	if err := os.Chmod(dataDir, 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(dataDir, 0o755) })

	err = (&Cline{}).Edit([]LaunchModel{{Name: "qwen3"}})
	if err == nil {
		t.Fatal("Cline.Edit succeeded with an unwritable document directory: want the publish to fail")
	}
	if !strings.Contains(err.Error(), "globalState.json") && !strings.Contains(err.Error(), "create temp failed") {
		t.Errorf("Cline.Edit error = %v, want it to name the document that could not be staged", err)
	}

	after, rerr := os.ReadFile(providersPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(after) != string(before) {
		t.Error("providers.json was published while its sibling could not be staged: the pair now describes two selections, one of which no launch chose")
	}
}
