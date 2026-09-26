package launch

// json_null_document_integrity_test.go — a store whose content is the literal
// JSON `null` panicked the launch (2026-09-27 audit, round 19).
//
// `null` is a valid JSON document, and decoding it into a map yields a NIL map:
// reads on it are fine, and every WRITE to it panics. Seven readers in this
// package decoded a document into a map they then wrote — pi's config, cline's
// providers, droid's settings, opencode's picker state, qwen's settings,
// OpenClaw's config and opencode's auth.json — so a `null` left behind by a
// hand-edit, an editor or an interrupted writer took the launch down with a Go
// panic instead of being read as the empty object it is. pi's is the worst of
// them: it writes models.json before the panic, so the launch dies having
// changed half its configuration.
//
// Each case below drives the real entry point with the document already on
// disk and asserts the write went through — a panic fails the test on its own,
// so what is pinned here is that the launch SURVIVES `null` and produces a
// usable document.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func writeNullDocument(t *testing.T, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("null"), 0o600); err != nil {
		t.Fatal(err)
	}
}

func readJSONObject(t *testing.T, path string) map[string]any {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	var doc map[string]any
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("%s is not a JSON object any more: %v\n%s", path, err, data)
	}
	if doc == nil {
		t.Fatalf("%s was left as `null` — the write never happened", path)
	}
	return doc
}

func TestQwenSurvivesANullSettingsDocument(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)
	stubDaemon(t)
	settings := filepath.Join(home, ".qwen", "settings.json")
	writeNullDocument(t, settings)

	if err := (&Qwen{}).Configure("gemma4"); err != nil {
		t.Fatalf("Configure: %v", err)
	}
	cfg := readJSONObject(t, settings)
	if _, ok := cfg["modelProviders"]; !ok {
		t.Errorf("the qwen provider entry was not written:\n%v", cfg)
	}
}

func TestClineSurvivesANullProvidersDocument(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	stubDaemon(t)
	c := &Cline{}
	// Both stores cline writes; Paths() itself only reports the ones that
	// already exist, so the paths are named here and the write asserted
	// directly.
	paths := []string{clineProvidersPath(home), clineLegacyGlobalStatePath(home)}
	for _, path := range paths {
		writeNullDocument(t, path)
	}

	if err := c.Edit(testLaunchModels("gemma4")); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	for _, path := range paths {
		readJSONObject(t, path)
	}
}

func TestDroidSurvivesANullSettingsDocument(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	stubDaemon(t)
	d := &Droid{}
	path := droidSettingsPath(t, home)
	writeNullDocument(t, path)

	if err := d.Edit(testLaunchModels("gemma4")); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	doc := readJSONObject(t, path)
	if _, ok := doc["customModels"]; !ok {
		t.Errorf("the model entries were not written:\n%v", doc)
	}
}

func TestPiSurvivesANullConfigDocument(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	stubDaemon(t)
	p := &Pi{}
	// pi's own settings.json is read through the same reader, so both files are
	// null here: the panic the reader guards against happens in either.
	for _, name := range []string{"models.json", "settings.json"} {
		writeNullDocument(t, filepath.Join(home, ".pi", "agent", name))
	}

	if err := p.Edit(testLaunchModels("gemma4")); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	doc := readJSONObject(t, filepath.Join(home, ".pi", "agent", "models.json"))
	if _, ok := doc["providers"]; !ok {
		t.Errorf("the pi provider was not written:\n%v", doc)
	}
}

func TestOpenCodeSurvivesANullModelStateDocument(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubDaemon(t)
	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	writeNullDocument(t, statePath)

	if err := (&OpenCode{}).Edit(testLaunchModels("gemma4")); err != nil {
		t.Fatalf("Edit: %v", err)
	}
	readJSONObject(t, statePath)
}

func TestOpenCodeAuthSurvivesANullDocument(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	path, err := opencodeStorePath()
	if err != nil {
		t.Fatal(err)
	}
	writeNullDocument(t, path)

	if err := SaveOpencodeAPIKey("openai", "sk-test-key"); err != nil {
		t.Fatalf("SaveOpencodeAPIKey: %v", err)
	}
	doc := readJSONObject(t, path)
	entry, _ := doc["openai"].(map[string]any)
	if entry["key"] != "sk-test-key" {
		t.Errorf("the key was not written:\n%v", doc)
	}
}

func TestOpenClawSurvivesANullConfigDocument(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	path := filepath.Join(home, ".openclaw", "openclaw.json")
	writeNullDocument(t, path)

	configureOllamaWebSearch()

	doc := readJSONObject(t, path)
	if _, ok := doc["plugins"]; !ok {
		t.Errorf("the web-search provider entry was not written:\n%v", doc)
	}
}
