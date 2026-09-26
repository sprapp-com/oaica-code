package launch

// yaml_user_document_preservation_integrity_test.go — the YAML integrations
// rewrote the user's whole document from a decoded map, so every comment was
// deleted and every scalar was re-rendered in the encoder's own voice
// (2026-09-27 audit, round 17).
//
// The JSON integrations are safe from this: a JSON document decoded into
// map[string]any and re-marshalled has no comments to lose, and the value
// fidelity is pinned by the UseNumber rule this package already follows. YAML
// carries comments, key order, anchors and the author's own scalar style —
// `1.0` is not `1`, a quoted string is not a plain one, and a date-shaped
// scalar written by hand is not the encoder's timestamp — so a whole-document
// rewrite changes a file oaica was only supposed to add a key to.
//
// What these writers own is a handful of keys; the rest of the file is the
// user's. The fix keeps the document as a yaml.Node and sets only the managed
// keys, the pattern deepseek_harness.go already uses.

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

// The pieces of a user's document the writer must leave alone: a file comment,
// an inline comment, an unrelated key, and two scalars whose exact spelling
// distinguishes "read and re-encoded" from "left where it was".
const yamlPreservationProbe = "temperature: 1.0\nreleased: 2026-09-01\n"

func assertYAMLPreserved(t *testing.T, path, what string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s is gone after the write: %v", what, err)
	}
	got := string(data)
	for _, want := range []string{
		"# keep this comment",
		"# and this inline one",
		"unrelated_setting: untouched",
		"temperature: 1.0",
		"released: 2026-09-01",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("%s lost %q — the writer re-encoded the user's document instead of setting its own keys:\n%s", what, want, got)
		}
	}
}

func TestHermesConfigureKeepsTheUsersDocument(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	hermesHome := filepath.Join(home, ".hermes")
	t.Setenv("HERMES_HOME", hermesHome)
	if err := os.MkdirAll(hermesHome, 0o700); err != nil {
		t.Fatal(err)
	}

	path := filepath.Join(hermesHome, "config.yaml")
	seed := "# keep this comment\n" +
		"model:\n" +
		"  provider: ollama # and this inline one\n" +
		"  default: old-model\n" +
		"unrelated_setting: untouched\n" +
		yamlPreservationProbe
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (&Hermes{}).Configure("llama3.2"); err != nil {
		t.Fatalf("Configure: %v", err)
	}

	assertYAMLPreserved(t, path, "hermes config.yaml")
	// And the keys it does own are set.
	data, _ := os.ReadFile(path)
	if !strings.Contains(string(data), "default: llama3.2") {
		t.Errorf("the managed default model was not written:\n%s", data)
	}
}

func TestOMPConfigureKeepsTheUsersDocuments(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	agentDir := filepath.Join(home, ".omp", "agent")
	t.Setenv("PI_CODING_AGENT_DIR", agentDir)
	if err := os.MkdirAll(agentDir, 0o700); err != nil {
		t.Fatal(err)
	}

	modelsPath := filepath.Join(agentDir, "models.yml")
	modelsSeed := "# keep this comment\n" +
		"providers:\n" +
		"  someoneelse:\n" +
		"    baseUrl: http://elsewhere.invalid/v1 # and this inline one\n" +
		"    api: openai-completions\n" +
		"unrelated_setting: untouched\n" +
		yamlPreservationProbe
	if err := os.WriteFile(modelsPath, []byte(modelsSeed), 0o600); err != nil {
		t.Fatal(err)
	}

	configPath := filepath.Join(agentDir, "config.yml")
	configSeed := "# keep this comment\n" +
		"setupVersion: 0 # and this inline one\n" +
		"unrelated_setting: untouched\n" +
		yamlPreservationProbe
	if err := os.WriteFile(configPath, []byte(configSeed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := (&OMP{}).ConfigureWithModels("llama3.2", []LaunchModel{{Name: "llama3.2"}}); err != nil {
		t.Fatalf("ConfigureWithModels: %v", err)
	}

	assertYAMLPreserved(t, modelsPath, "omp models.yml")
	assertYAMLPreserved(t, configPath, "omp config.yml")

	// The other provider is not oaica's to delete — the writer's own comment
	// says a document it cannot preserve must not be rewritten.
	models, _ := os.ReadFile(modelsPath)
	if !strings.Contains(string(models), "someoneelse:") {
		t.Errorf("omp models.yml lost a provider oaica does not own:\n%s", models)
	}
	config, _ := os.ReadFile(configPath)
	if !strings.Contains(string(config), "setupVersion: "+strconv.Itoa(ompSetupVersion)) {
		t.Errorf("omp config.yml did not get its setup version:\n%s", config)
	}
}
