package launch

// hermes_subtree_comments_integrity_test.go — the hermes writer promised to
// leave everything oaica does not manage exactly as the user wrote it, and then
// re-encoded two whole subtrees (2026-09-27 audit, round 18).
//
// writeHermesConfig edits the document as a yaml.Node so comments and scalar
// spellings survive (round 17), but `custom_providers` and `toolsets` were
// decoded with yamlNodeAsAny and written back with yamlSetValue: every comment
// inside those two subtrees, and every formatting choice in them, was deleted
// on every launch — in a file whose other providers' entries are deliberately
// carried through untouched. The work those subtrees need is a filter and an
// append, both of which a node tree supports directly.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

func hermesSubtreeCommentEnv(t *testing.T) string {
	t.Helper()
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	withHermesPlatform(t, "darwin")

	configPath := filepath.Join(tmpDir, ".hermes", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	existing := "" +
		"memory:\n" +
		"  provider: local\n" +
		"custom_providers:\n" +
		"  # keep-custom-comment\n" +
		"  - name: other\n" +
		"    base_url: https://other.invalid/v1 # keep-custom-line\n" +
		"  - name: Ollama\n" +
		"    base_url: https://stale.invalid/v1\n" +
		"toolsets:\n" +
		"  # keep-toolset-comment\n" +
		"  - terminal\n"
	if err := os.WriteFile(configPath, []byte(existing), 0o644); err != nil {
		t.Fatal(err)
	}
	return configPath
}

func TestHermesConfigureKeepsCommentsInsideTheSubTreesItFilters(t *testing.T) {
	configPath := hermesSubtreeCommentEnv(t)

	if err := writeHermesConfig(configPath, "gemma4", []string{"gemma4"}); err != nil {
		t.Fatalf("writeHermesConfig returned error: %v", err)
	}

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, comment := range []string{"# keep-custom-comment", "# keep-custom-line", "# keep-toolset-comment"} {
		if !strings.Contains(text, comment) {
			t.Errorf("the user's comment %q was deleted from a subtree oaica only filters:\n%s", comment, text)
		}
	}

	var cfg map[string]any
	if err := yaml.Unmarshal(data, &cfg); err != nil {
		t.Fatalf("rewritten yaml does not parse: %v", err)
	}

	// The entry oaica does not own is still there, and the managed one is gone.
	customProviders, _ := cfg["custom_providers"].([]any)
	var names []string
	for _, raw := range customProviders {
		entry, _ := raw.(map[string]any)
		name, _ := entry["name"].(string)
		names = append(names, name)
		if name == "other" && entry["base_url"] != "https://other.invalid/v1" {
			t.Errorf("the preserved entry lost its base_url: %#v", entry)
		}
	}
	if len(names) != 1 || names[0] != "other" {
		t.Errorf("custom_providers = %v, want just the user's other provider", names)
	}

	toolsets, _ := cfg["toolsets"].([]any)
	var got []string
	for _, item := range toolsets {
		if s, _ := item.(string); s != "" {
			got = append(got, s)
		}
	}
	if !strings.Contains(strings.Join(got, ","), "terminal") || !strings.Contains(strings.Join(got, ","), "web") {
		t.Errorf("toolsets = %v, want the user's terminal plus web", got)
	}
}
