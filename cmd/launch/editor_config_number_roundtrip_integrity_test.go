package launch

// editor_config_number_roundtrip_integrity_test.go — three editors' config
// files were decoded into map[string]any and written back out, and a JSON
// number decoded that way is a float64 (2026-09-26 audit).
//
// These are the user's documents, not oaica's: VS Code's settings.json, Cline's
// provider config, Claude Desktop's config.json. oaica opens each of them to
// change one thing and rewrites the whole file. Anything the file holds that
// oaica does not model is now on a trip through float64, and float64 cannot
// hold every integer — 2^53+1 comes back as 2^53 — nor every spelling of a
// number, so 1.0 comes back as 1. The user's setting changed value because
// oaica looked at a file it was only supposed to add a key to.

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

// 2^53+1 is the first integer float64 cannot represent: decode it, encode it,
// and you get 2^53 back — a different number, silently.
const unrepresentableInt = "9007199254740993"

// 1.0 and 1 are the same JSON number by value but not by bytes; a decoder that
// goes through float64 cannot tell which one the user wrote.
const integralFloat = "1.0"

func assertNumbersSurviveUntouched(t *testing.T, before, after []byte, what string) {
	t.Helper()
	for _, lit := range []string{unrepresentableInt, integralFloat} {
		if !bytes.Contains(before, []byte(lit)) {
			continue
		}
		if bytes.Contains(after, []byte(lit)) {
			continue
		}
		t.Errorf("%s: %s was in the file before oaica touched it and is not in the file after. oaica rewrites a document it only meant to add one key to, so every number in it has to survive the round-trip exactly — decoding into map[string]any makes it a float64, and a float64 is not the number the user wrote",
			what, lit)
	}
}

func TestAClineConfigKeepsNumbersOaicaDoesNotModel(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cline", "config.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"welcomeViewCompleted": false, "telemetry": {"timestamp": ` + unrepresentableInt + `, "ratio": ` + integralFloat + `}}`)
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := readClineConfig(path)
	if err != nil {
		t.Fatalf("readClineConfig: %v", err)
	}
	if err := writeClineProvidersConfig(path, cfg, LaunchModel{Name: "oaica-launch"}); err != nil {
		t.Fatalf("writeClineProvidersConfig: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertNumbersSurviveUntouched(t, seed, after, "cline config")
}

func TestAClaudeDesktopConfigKeepsNumbersOaicaDoesNotModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude_desktop_config.json")
	seed := []byte(`{"numStartups": ` + unrepresentableInt + `, "windowScale": ` + integralFloat + `}`)
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := readClaudeDesktopJSON(path)
	if err != nil {
		t.Fatalf("readClaudeDesktopJSON: %v", err)
	}
	if err := writeClaudeDesktopJSON(path, cfg); err != nil {
		t.Fatalf("writeClaudeDesktopJSON: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	assertNumbersSurviveUntouched(t, seed, after, "claude desktop config")
}

func TestVSCodeSettingsKeepNumbersOaicaDoesNotModel(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	t.Setenv("XDG_CONFIG_HOME", "")
	path := testVSCodePath(t, tmpDir, "settings.json")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	// The legacy key is what makes updateSettings rewrite the file at all.
	seed := []byte(`{"ollama.launch.configured": true, "customTimeoutMs": ` + unrepresentableInt + `, "customScale": ` + integralFloat + `}`)
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatal(err)
	}

	v := &VSCode{}
	v.updateSettings()

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(seed, after) {
		t.Fatal("the legacy key was not removed, so the rewrite this test is about never happened")
	}
	assertNumbersSurviveUntouched(t, seed, after, "vs code settings")

	var decoded map[string]any
	if err := json.Unmarshal(after, &decoded); err != nil {
		t.Errorf("the rewritten settings.json is not valid JSON: %v", err)
	}
}

// muse, qwen and droid had the same defect: each reads a document the user or
// another program owns, adds its own keys, and writes the whole thing back —
// and each decoded it through float64 (muse via fileutil.ReadJSON, qwen's
// readConfig and droid's Edit via json.Unmarshal). The numbers oaica does not
// model are on that trip too.

// Muse's file is the one that is also a live document: muse persists its own
// settings into the config root launch hands it (muse.go:243), so whatever
// muse recorded there — including the stamps it writes as integers — is what
// the next launch reads back and re-encodes. "Preserve" therefore has to mean
// byte-for-byte for every number, not just for the keys oaica owns.
func TestAMuseSettingsKeepsNumbersOaicaDoesNotModel(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("XDG_CONFIG_HOME", "")

	path, err := museSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"schema_version": 1, "museSessionStamp": ` + unrepresentableInt + `, "windowScale": ` + integralFloat + `}`)
	if err := os.WriteFile(path, seed, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := writeMuseSettings([]LaunchModel{{Name: "gpt-oss:20b"}}); err != nil {
		t.Fatalf("writeMuseSettings: %v", err)
	}

	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(seed, after) {
		t.Fatal("the settings file was not rewritten, so the round-trip this test is about never happened")
	}
	assertNumbersSurviveUntouched(t, seed, after, "muse settings")
}

func TestAQwenConfigKeepsNumbersOaicaDoesNotModel(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	setQwenTestHome(t, home)

	configDir := filepath.Join(home, ".qwen")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"model":{"name":"gemma4"}, "sessionStamp": ` + unrepresentableInt + `, "windowScale": ` + integralFloat + `}`)
	if err := os.WriteFile(filepath.Join(configDir, "settings.json"), seed, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&Qwen{}).Configure("gemma4"); err != nil {
		t.Fatalf("configure: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(configDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertNumbersSurviveUntouched(t, seed, after, "qwen settings")
}

func TestADroidSettingsKeepsNumbersOaicaDoesNotModel(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	settingsDir := filepath.Join(home, ".factory")
	if err := os.MkdirAll(settingsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	seed := []byte(`{"factorySessionStamp": ` + unrepresentableInt + `, "windowScale": ` + integralFloat + `}`)
	if err := os.WriteFile(filepath.Join(settingsDir, "settings.json"), seed, 0o644); err != nil {
		t.Fatal(err)
	}

	if err := (&Droid{}).Edit(testLaunchModels("model-a")); err != nil {
		t.Fatalf("Edit: %v", err)
	}

	after, err := os.ReadFile(filepath.Join(settingsDir, "settings.json"))
	if err != nil {
		t.Fatal(err)
	}
	assertNumbersSurviveUntouched(t, seed, after, "droid settings")
}
