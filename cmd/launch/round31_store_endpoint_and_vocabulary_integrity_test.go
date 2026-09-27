package launch

// round31_store_endpoint_and_vocabulary_integrity_test.go — two more ways the
// drift term read less than the writer writes (2026-09-27 audit, round 31).
//
// Every writer in this package points its store at the endpoint the launch
// resolved: droid and muse write the daemon address (or the remote's) on every
// write, and pi repoints its single provider slot whenever that slot is one
// this package wrote. The declarations the drift term asks read the model ids
// and never the address, so a launch on another daemon read a store naming the
// old one as current and skipped the write that would have moved it — the
// editor stayed pointed at an endpoint that no longer serves the model the
// launch just resolved, and no later launch corrected it.
//
// And two of those readers answer in a vocabulary the launcher never compares
// against: an ollama-cloud catalogue row is picked as "gpt-oss" and written as
// the daemon-side id "gpt-oss:cloud", which is what droid and muse report back,
// while the saved selection carries the picker name. No comparison between the
// two can ever be true, so those launches re-ran the whole configure path over
// a config they had just read (rounds 25/26 fixed exactly this for openclaw and
// opencode).

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ollama/ollama/cmd/config"
)

// TestAPiStoreNamingAnEndpointTheWriterWouldMoveIsDrift is the endpoint half for
// pi, whose writer repoints baseUrl and apiKey whenever the slot is one it wrote
// (piEndpointWasOurs).
func TestAPiStoreNamingAnEndpointTheWriterWouldMoveIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	// The daemon's documented default address: the one value an earlier oaica
	// wrote that this launch still recognises as its own.
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
	selection := []LaunchModel{fallbackLaunchModel("qwen3")}
	if err := (&Pi{}).Edit(selection); err != nil {
		t.Fatalf("Pi.Edit: %v", err)
	}
	if !(&Pi{}).DeclaresSelection(selection) {
		t.Fatal("control: the store a write just left reads as drift, so this test cannot tell whether the endpoint is being read")
	}

	// The daemon has moved. A write would repoint the slot (the stored address
	// is the 11434 default, which piEndpointWasOurs recognises), so the store
	// does NOT hold what this launch would write.
	t.Setenv("OLLAMA_HOST", "192.168.1.50:11434")
	if (&Pi{}).DeclaresSelection(selection) {
		t.Error("Pi's store reads as current while it names a different endpoint than this launch's daemon: a write would move it, so the launch skips the write that would and pi keeps dialling the old address")
	}

	// The credential is written by the same branch and was unread for the same
	// reason: a slot holding another launch's token reads as current, and the
	// local launch leaves its provider keyed with a credential that does not
	// belong to the endpoint in front of it.
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
	configPath := filepath.Join(home, ".pi", "agent", "models.json")
	doc := readJSONMapForTest(t, configPath)
	providers, _ := doc["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	ollama["apiKey"] = "sk-some-earlier-remote"
	writeJSONMapForTest(t, configPath, doc)
	if (&Pi{}).DeclaresSelection(selection) {
		t.Error("Pi's store reads as current while it holds a credential this launch's write would replace: the declaration reads fewer fields than the writer sets")
	}
}

// TestADroidOrMuseStoreHoldingTheServedIDIsNotDrift is the vocabulary half,
// the same defect round 26 closed for OpenClaw: both stores are written with the
// id the backend serves and report that id back, while the launcher's saved
// selection carries the picker name.
func TestADroidOrMuseStoreHoldingTheServedIDIsNotDrift(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	row := cloudCatalogueRow()
	c := &launcherClient{}
	c.inventory = &modelInventory{loaded: true, models: []LaunchModel{
		{Name: "ollama/gpt-oss", Remote: true, Upstream: row.Upstream},
	}}

	for _, tc := range []struct {
		name   string
		editor Editor
	}{
		{"droid", &Droid{}},
		{"muse", &Muse{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.editor.Edit([]LaunchModel{row}); err != nil {
				t.Fatalf("%s.Edit: %v", tc.name, err)
			}
			if !c.liveEditorDeclaration(t.Context(), tc.editor, []string{"gpt-oss"}) {
				t.Errorf("%s: the store holds exactly the model this selection was written as (the daemon-side id the row's writer stores), so every launch re-resolves the inventory and rewrites an unchanged config", tc.name)
			}

			// Not a rubber stamp: a store this launch did not write is drift.
			if err := tc.editor.Edit([]LaunchModel{fallbackLaunchModel("llama3.2")}); err != nil {
				t.Fatalf("%s.Edit: %v", tc.name, err)
			}
			if c.liveEditorDeclaration(t.Context(), tc.editor, []string{"gpt-oss"}) {
				t.Errorf("%s: the declaration said an unrelated store matches the selection", tc.name)
			}
		})
	}
}

// TestADroidOrMuseStoreNamingAnotherEndpointIsDrift is the endpoint half: both
// writers set the address on every write, from the daemon the launch resolved.
func TestADroidOrMuseStoreNamingAnotherEndpointIsDrift(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
	models := []LaunchModel{fallbackLaunchModel("qwen3")}

	for _, tc := range []struct {
		name   string
		editor Editor
	}{
		{"droid", &Droid{}},
		{"muse", &Muse{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.editor.Edit(models); err != nil {
				t.Fatalf("%s.Edit: %v", tc.name, err)
			}
			c := &launcherClient{}
			if !c.liveEditorDeclaration(t.Context(), tc.editor, []string{"qwen3"}) {
				t.Fatal("control: the store a write just left reads as drift, so this test cannot tell whether the endpoint is being read")
			}
			t.Setenv("OLLAMA_HOST", "127.0.0.1:22445")
			if c.liveEditorDeclaration(t.Context(), tc.editor, []string{"qwen3"}) {
				t.Errorf("%s: the store reads as current while it names the daemon this launch is no longer using: the writer sets that address on every write, so the launch skips the write that would move it", tc.name)
			}
			t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
		})
	}
}

// TestAVSCodeVendorEntryNamingAnotherEndpointIsDrift: VSCode's Models() answers
// out of the SAVED integration state, which always equals the selection, so the
// only thing the drift term could ever see is the vendor entry's url — and it
// read nothing from the file the writer writes.
func TestAVSCodeVendorEntryNamingAnotherEndpointIsDrift(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
	if err := (&VSCode{}).Edit([]LaunchModel{fallbackLaunchModel("qwen3")}); err != nil {
		t.Fatalf("VSCode.Edit: %v", err)
	}
	// The saved integration state is what Models() answers from; the launcher
	// writes it beside Edit, and VSCode.Models() returns nil without it (a
	// missing record is not a claim that the vendor entry is current).
	if err := config.SaveIntegration("vscode", []string{"qwen3"}); err != nil {
		t.Fatalf("SaveIntegration: %v", err)
	}
	c := &launcherClient{}
	if !c.liveEditorDeclaration(t.Context(), &VSCode{}, []string{"qwen3"}) {
		t.Fatal("control: the store a write just left reads as drift, so this test cannot tell whether the vendor entry is being read")
	}

	t.Setenv("OLLAMA_HOST", "192.168.1.50:11434")
	if c.liveEditorDeclaration(t.Context(), &VSCode{}, []string{"qwen3"}) {
		t.Error("the vendor entry reads as current while its url names the daemon this launch is no longer using: VSCode.Models() answers from the saved state, so nothing read the file Edit writes")
	}

	// An entry the user removed by hand is drift too — the launch must put it
	// back rather than reason from its own record of what it once wrote.
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
	path := (&VSCode{}).chatLanguageModelsPath()
	if err := os.WriteFile(path, []byte(`[{"vendor":"anthropic","name":"Claude","url":"https://api.anthropic.com"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if c.liveEditorDeclaration(t.Context(), &VSCode{}, []string{"qwen3"}) {
		t.Error("the declaration said the store matches while its ollama vendor entry is gone: the saved state is a record of what a previous launch wrote, not the live configuration")
	}
}
