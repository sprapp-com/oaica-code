package launch

// round25_written_model_id_integrity_test.go — a catalogue row is written as
// the id its backend knows, not as the label the picker shows (2026-09-27
// audit, round 25).
//
// LaunchModel.Upstream carries the id the backend actually serves when Name is
// a display-only picker id: the ollama-cloud catalogue names a row "ollama/
// gpt-oss" (that prefix is display; findLaunchModel has already stripped it by
// the time a writer sees the row) while the daemon knows the model as
// "gpt-oss:cloud" (ollama_cloud.go). codex_app, deepseek_harness and omp each
// consult Upstream and say so in their own comments; opencode, pi, cline,
// droid, muse and openclaw wrote the row's Name instead.
//
// Two user-visible failures follow, and they compound. The store names a model
// the daemon serves only as a LOCAL model — so the child CLI asks for a local
// "gpt-oss" that is either absent (a multi-GB pull offer) or a different model
// of the same name — and the name the launch saved never equals the name the
// picker selection carries ("gpt-oss:cloud", launchNameForPickerName), so
// liveConfigMatches is false on every run and each launch rewrites the store it
// just read.
//
// Every test below therefore asserts BOTH halves at the writer: the id the
// store holds is the daemon-side one, and the integration's own reader reports
// that same id back (which is what the launcher compares the selection to).

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// cloudCatalogueRow is the shape an ollama-cloud catalogue row reaches a writer
// in: the picker prefix already stripped, the daemon-side id in Upstream.
func cloudCatalogueRow() LaunchModel {
	return LaunchModel{Name: "gpt-oss", Remote: true, Upstream: "gpt-oss:cloud"}
}

// cloudDaemonRow is the other spelling of the same model: a row whose Name IS
// the daemon-side id (the daemon's own catalogues list these, and the picker
// selection arrives as this string). It has no Upstream, and nothing may
// rewrite it.
func cloudDaemonRow() LaunchModel {
	return LaunchModel{Name: "gpt-oss:cloud", Remote: true}
}

// TestACloudRowIsWrittenAsTheIDTheDaemonServes pins the id functions every
// other assertion here goes through, beside the three that already had the
// rule, so the sites cannot drift apart again.
func TestACloudRowIsWrittenAsTheIDTheDaemonServes(t *testing.T) {
	row := cloudCatalogueRow()
	daemon := cloudDaemonRow()

	for _, tc := range []struct {
		name string
		of   func(LaunchModel) string
	}{
		{"launchModelWriteID", launchModelWriteID},
		{"opencodeModelID", opencodeModelID},
		{"piModelIDFor", piModelIDFor},
		{"deepSeekHarnessModelIDFor", deepSeekHarnessModelIDFor},
		{"ompLaunchModelID", ompLaunchModelID},
		{"codexAppRowModelID", codexAppRowModelID},
	} {
		if got := tc.of(row); got != "gpt-oss:cloud" {
			t.Errorf("%s(ollama-cloud row) = %q, want the daemon-side id %q: %q is the LOCAL model of that name", tc.name, got, "gpt-oss:cloud", got)
		}
		if got := tc.of(daemon); got != "gpt-oss:cloud" {
			t.Errorf("%s(row already named by its daemon id) = %q, want it left alone", tc.name, got)
		}
		if got := tc.of(LaunchModel{Name: "llama3.2", LiveSource: liveSourceDaemon}); got != "llama3.2" {
			t.Errorf("%s(daemon row) = %q, want the bare name: a row with no Upstream is not renamed", tc.name, got)
		}
	}
}

// TestMuseWritesTheDaemonSideIDForACloudRow drives the real settings writer and
// the reader the launcher compares against.
func TestMuseWritesTheDaemonSideIDForACloudRow(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	if err := writeMuseSettings([]LaunchModel{cloudCatalogueRow()}); err != nil {
		t.Fatalf("writeMuseSettings: %v", err)
	}

	path, err := museSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var settings struct {
		Model        string           `json:"model"`
		ModelCatalog []museCatalogRow `json:"model_catalog"`
	}
	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("settings.json is no longer JSON: %v", err)
	}
	if settings.Model != "gpt-oss:cloud" {
		t.Errorf("settings.model = %q, want %q: the selected model is the one the daemon serves", settings.Model, "gpt-oss:cloud")
	}
	if len(settings.ModelCatalog) != 1 || settings.ModelCatalog[0].ModelID != "gpt-oss:cloud" {
		t.Errorf("model_catalog = %+v, want one row with model_id %q", settings.ModelCatalog, "gpt-oss:cloud")
	}

	// The reader the launcher compares the selection to must agree, or every
	// launch reads its own write back as drift.
	if got := (&Muse{}).Models(); len(got) != 1 || got[0] != "gpt-oss:cloud" {
		t.Errorf("Muse.Models() = %v, want [gpt-oss:cloud]: a store that reports a name the selection never carries reconfigures on every launch", got)
	}

	// A row already named by its daemon id is written unchanged.
	if err := writeMuseSettings([]LaunchModel{cloudDaemonRow()}); err != nil {
		t.Fatalf("writeMuseSettings(daemon row): %v", err)
	}
	if got := (&Muse{}).Models(); len(got) != 1 || got[0] != "gpt-oss:cloud" {
		t.Errorf("Muse.Models() after a daemon-named row = %v, want [gpt-oss:cloud]", got)
	}
}

// TestOpenclawWritesTheDaemonSideIDForACloudRow is the same rule at OpenClaw's
// store, where the entry's "id" is both the merge key and what Models() reads.
func TestOpenclawWritesTheDaemonSideIDForACloudRow(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	dir := filepath.Join(home, ".openclaw")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "openclaw.json")

	if err := openclawEditConfig(configPath, filepath.Join(home, ".clawdbot", "clawdbot.json"), []LaunchModel{cloudCatalogueRow()}); err != nil {
		t.Fatalf("openclawEditConfig: %v", err)
	}

	ids := openclawTestModelIDs(t, configPath)
	if len(ids) != 1 || ids[0] != "gpt-oss:cloud" {
		t.Errorf("openclaw model ids = %v, want [gpt-oss:cloud]: %q is the LOCAL model of that name", ids, "gpt-oss")
	}
	if got := (&Openclaw{}).Models(); len(got) != 1 || got[0] != "gpt-oss:cloud" {
		t.Errorf("Openclaw.Models() = %v, want [gpt-oss:cloud]", got)
	}

	// A second launch of the same row must not append a second copy: the
	// entry's id is what marks the row as this launch's.
	if err := openclawEditConfig(configPath, filepath.Join(home, ".clawdbot", "clawdbot.json"), []LaunchModel{cloudCatalogueRow()}); err != nil {
		t.Fatalf("second openclawEditConfig: %v", err)
	}
	if ids := openclawTestModelIDs(t, configPath); len(ids) != 1 || ids[0] != "gpt-oss:cloud" {
		t.Errorf("after a second launch openclaw model ids = %v, want exactly [gpt-oss:cloud]", ids)
	}
}

func openclawTestModelIDs(t *testing.T, configPath string) []string {
	t.Helper()
	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("openclaw.json is no longer JSON: %v", err)
	}
	modelsSection, _ := doc["models"].(map[string]any)
	providers, _ := modelsSection["providers"].(map[string]any)
	ollama, _ := providers["ollama"].(map[string]any)
	rows, _ := ollama["models"].([]any)
	var ids []string
	for _, raw := range rows {
		if entry, ok := raw.(map[string]any); ok {
			if id, _ := entry["id"].(string); id != "" {
				ids = append(ids, id)
			}
		}
	}
	return ids
}

// TestDroidWritesTheDaemonSideIDForACloudRow: Droid sends the entry's "model"
// to the endpoint, and its id embeds the picker name — the two have to move
// together or the entry stops reading as this integration's own.
func TestDroidWritesTheDaemonSideIDForACloudRow(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	if err := (&Droid{}).Edit([]LaunchModel{cloudCatalogueRow()}); err != nil {
		t.Fatalf("Droid.Edit: %v", err)
	}

	models := droidTestStoredModels(t, home)
	if len(models) != 1 || models[0]["model"] != "gpt-oss:cloud" {
		t.Errorf("droid customModels = %v, want one entry whose model is %q: that field is what Droid sends to the endpoint", models, "gpt-oss:cloud")
	}
	if id, _ := models[0]["id"].(string); id != "custom:gpt-oss:cloud-0" {
		t.Errorf("droid entry id = %q, want %q — the id names the entry, and droidOwnedEntry matches it against the stored model", id, "custom:gpt-oss:cloud-0")
	}
	if got := (&Droid{}).Models(); len(got) != 1 || got[0] != "gpt-oss:cloud" {
		t.Errorf("Droid.Models() = %v, want [gpt-oss:cloud]", got)
	}

	// Re-launching the same row must leave the store alone (and must not append
	// a copy: an entry whose id and model disagree is kept as the user's).
	if err := (&Droid{}).Edit([]LaunchModel{cloudCatalogueRow()}); err != nil {
		t.Fatalf("second Droid.Edit: %v", err)
	}
	if models := droidTestStoredModels(t, home); len(models) != 1 {
		t.Errorf("after a second launch droid customModels has %d entries, want 1: the row was written twice", len(models))
	}
}

func droidTestStoredModels(t *testing.T, home string) []map[string]any {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(home, ".factory", "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("droid settings.json is no longer JSON: %v", err)
	}
	raw, _ := doc["customModels"].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, entry := range raw {
		if m, ok := entry.(map[string]any); ok {
			out = append(out, m)
		}
	}
	return out
}

// TestClineWritesTheDaemonSideIDForACloudRow covers both of Cline's documents:
// the providers entry and the legacy global state, which are one selection's
// two halves.
func TestClineWritesTheDaemonSideIDForACloudRow(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	if err := (&Cline{}).Edit([]LaunchModel{cloudCatalogueRow()}); err != nil {
		t.Fatalf("Cline.Edit: %v", err)
	}

	legacy := readJSONMapForTest(t, clineLegacyGlobalStatePath(home))
	if got := legacy["actModeOllamaModelId"]; got != "gpt-oss:cloud" {
		t.Errorf("globalState actModeOllamaModelId = %v, want %q", got, "gpt-oss:cloud")
	}
	if got := legacy["planModeOllamaModelId"]; got != "gpt-oss:cloud" {
		t.Errorf("globalState planModeOllamaModelId = %v, want %q", got, "gpt-oss:cloud")
	}

	providers := readJSONMapForTest(t, clineProvidersPath(home))
	provider, _ := providers["providers"].(map[string]any)
	entry, _ := provider[clineLaunchProvider].(map[string]any)
	settings, _ := entry["settings"].(map[string]any)
	if got := settings["model"]; got != "gpt-oss:cloud" {
		t.Errorf("providers.json settings.model = %v, want %q", got, "gpt-oss:cloud")
	}

	if got := (&Cline{}).Models(); len(got) != 1 || got[0] != "gpt-oss:cloud" {
		t.Errorf("Cline.Models() = %v, want [gpt-oss:cloud]: a store that reports a name the selection never carries reconfigures on every launch", got)
	}
}

func readJSONMapForTest(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s is no longer JSON: %v", path, err)
	}
	return doc
}

// TestOpenCodeWritesTheDaemonSideIDForACloudRow: opencode's provider blocks and
// its top-level model both name the id, and its state file stores the same pair
// (openCodeStatePath) — the daemon block's id is the one Models-equivalent
// readers report.
func TestOpenCodeWritesTheDaemonSideIDForACloudRow(t *testing.T) {
	row := cloudCatalogueRow()
	daemonID := opencodeDaemonProviderID([]LaunchModel{row})
	providerID, modelID := opencodeProviderFor(row, daemonID)
	if providerID != daemonID || modelID != "gpt-oss:cloud" {
		t.Errorf("opencodeProviderFor(cloud row) = (%q, %q), want (%q, %q): a cloud row is served by the daemon, under the id the daemon knows", providerID, modelID, daemonID, "gpt-oss:cloud")
	}
	if got := opencodeModelID(row); got != "gpt-oss:cloud" {
		t.Errorf("opencodeModelID(cloud row) = %q, want %q", got, "gpt-oss:cloud")
	}
	entries := buildModelEntries([]LaunchModel{row})
	if _, ok := entries["gpt-oss:cloud"]; !ok {
		t.Errorf("buildModelEntries declared %v, want the daemon-side id %q", entries, "gpt-oss:cloud")
	}
}
