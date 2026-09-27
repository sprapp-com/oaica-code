package launch

// round32_declaration_fields_integrity_test.go — the fields a writer sets that
// its declaration still did not read (2026-09-27 audit, round 32, A-F1/F2/F3).
//
// Each of these is the same shape the previous rounds closed elsewhere, in a
// store whose declaration was rewritten in round 31 while one of the writer's
// fields stayed behind:
//
//   - Droid stores the endpoint's credential beside the id and the address
//     (entry.APIKey), says plainly that a user-remote entry "holds the remote's
//     token, which Droid sends as the bearer" — and no key mentioned it, so a
//     rotated-away token read as current (round 29's Cline finding). It also
//     stores which model the session STARTS on, and Droid.Run passes the model
//     to nothing but the capability gate.
//   - Pi writes TWO documents and its declaration read one: models.json's ids
//     and slot, never settings.json's defaultProvider/defaultModel — which is
//     what Pi actually opens on, because Pi.Run ignores its model argument
//     entirely.
//   - Cline's legacy state has four fields naming a model or an endpoint and
//     the declaration read one of the four: the act-mode pair, never the plan
//     mode the user can set separately in Cline's own UI, and never the root
//     ollamaBaseUrl.

import (
	"path/filepath"
	"testing"
)

// TestADroidStoreHoldingAReplacedRemoteTokenIsDrift is A-F1: the credential the
// declaration did not read.
func TestADroidStoreHoldingAReplacedRemoteTokenIsDrift(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://127.0.0.1:30099/v1","api_key":"sk-OLD"}]}`)

	models := []LaunchModel{{Name: "box/llama3"}}
	if err := (&Droid{}).Edit(models); err != nil {
		t.Fatalf("Droid.Edit: %v", err)
	}
	c := &launcherClient{}
	if !c.liveEditorDeclaration(t.Context(), &Droid{}, []string{"box/llama3"}) {
		t.Fatal("control: the store a write just left reads as drift, so this test cannot tell whether the credential is being read")
	}

	// The documented path: the remote's key is rotated (oaica remote add ... --key).
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://127.0.0.1:30099/v1","api_key":"sk-NEW"}]}`)
	if c.liveEditorDeclaration(t.Context(), &Droid{}, []string{"box/llama3"}) {
		t.Error("Droid's store reads as current while it holds the credential the remote has replaced: Droid keeps presenting the old bearer and every request fails auth, with no later launch correcting it")
	}

	// It is not a rubber stamp on the other side either: a rotated key that the
	// write would have stored is exactly what the store must hold.
	if err := (&Droid{}).Edit(models); err != nil {
		t.Fatalf("Droid.Edit: %v", err)
	}
	if !c.liveEditorDeclaration(t.Context(), &Droid{}, []string{"box/llama3"}) {
		t.Error("the store the write just left reads as drift: the credential is read, but not the one the writer stores")
	}
}

// TestADroidSessionDefaultTheWriterWouldResetIsDrift is the second half of A-F1:
// the model the file says the session starts on.
func TestADroidSessionDefaultTheWriterWouldResetIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")

	models := []LaunchModel{fallbackLaunchModel("qwen3")}
	if err := (&Droid{}).Edit(models); err != nil {
		t.Fatalf("Droid.Edit: %v", err)
	}
	c := &launcherClient{}
	if !c.liveEditorDeclaration(t.Context(), &Droid{}, []string{"qwen3"}) {
		t.Fatal("control: the store a write just left reads as drift")
	}

	// What Droid's own UI leaves behind when the user switches model in it:
	// sessionDefaultSettings.model points at another entry, and Droid.Run hands
	// the launch's model to nothing but the capability gate.
	path := filepath.Join(home, ".factory", "settings.json")
	doc := readJSONMapForTest(t, path)
	session, _ := doc["sessionDefaultSettings"].(map[string]any)
	session["model"] = "custom:user-picked-3"
	writeJSONMapForTest(t, path, doc)

	if c.liveEditorDeclaration(t.Context(), &Droid{}, []string{"qwen3"}) {
		t.Error("Droid's store reads as current while its session default names another model: the file is what decides what Droid opens on, so the launch reports success and starts something else")
	}
}

// TestAPiSettingsNamingAnotherDefaultIsDrift is A-F2: the second document Pi's
// writer publishes, which Pi.Run does not override.
func TestAPiSettingsNamingAnotherDefaultIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")

	models := []LaunchModel{fallbackLaunchModel("qwen3")}
	if err := (&Pi{}).Edit(models); err != nil {
		t.Fatalf("Pi.Edit: %v", err)
	}
	if !(&Pi{}).DeclaresSelection(models) {
		t.Fatal("control: the store a write just left reads as drift")
	}

	// Pi's own TUI writes this when the user switches provider or model inside
	// it; Pi.Run ignores the model it is handed, so the file is the whole effect
	// of a launch.
	path := filepath.Join(home, ".pi", "agent", "settings.json")
	doc := readJSONMapForTest(t, path)
	doc["defaultProvider"] = "anthropic"
	doc["defaultModel"] = "claude-3"
	writeJSONMapForTest(t, path, doc)

	if (&Pi{}).DeclaresSelection(models) {
		t.Error("Pi's store reads as current while its settings name another provider and model: the launch reports success and Pi opens on the model the user last picked inside it")
	}
}

// TestAClineLegacyHalfNamingAnotherPlanModelIsDrift is A-F3: the two fields of
// globalState.json the writer sets beside the act-mode pair.
func TestAClineLegacyHalfNamingAnotherPlanModelIsDrift(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")

	models := []LaunchModel{fallbackLaunchModel("qwen3")}
	if err := (&Cline{}).Edit(models); err != nil {
		t.Fatalf("Cline.Edit: %v", err)
	}
	c := &launcherClient{}
	if !c.liveEditorDeclaration(t.Context(), &Cline{}, []string{"qwen3"}) {
		t.Fatal("control: the store a write just left reads as drift, so this test cannot tell whether the plan half is being read")
	}

	// Cline's UI lets plan and act run different models; that is what writes this.
	path := clineLegacyGlobalStatePath(home)
	doc := readJSONMapForTest(t, path)
	doc["planModeOllamaModelId"] = "user-plan-model"
	writeJSONMapForTest(t, path, doc)
	if c.liveEditorDeclaration(t.Context(), &Cline{}, []string{"qwen3"}) {
		t.Error("Cline's legacy state reads as current while its plan half names the user's other model: the write that would publish this launch's model is skipped and Cline's plan mode keeps running something else")
	}

	// The root endpoint is the third copy of the address in this document, and
	// the writer sets it from the same value (clineRefuseForeignLegacyEndpoint
	// treats all three as one).
	doc["planModeOllamaModelId"] = "qwen3"
	doc["ollamaBaseUrl"] = "http://127.0.0.1:1"
	writeJSONMapForTest(t, path, doc)
	if c.liveEditorDeclaration(t.Context(), &Cline{}, []string{"qwen3"}) {
		t.Error("Cline's legacy state reads as current while its root endpoint names another daemon")
	}
}
