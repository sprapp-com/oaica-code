package launch

// openclaw_user_switches_integrity_test.go — a launch turned OpenClaw's web
// search back on against the user's setting (2026-09-27 audit, round 21, F12).
//
// The tool-config writer set plugins.entries.ollama.enabled and
// tools.web.search.enabled to true unconditionally, on every launch. Both are
// switches the user owns: the plugin entry is one OpenClaw itself ships, and
// "search" is whether web search runs at all — not which provider serves it,
// which is the part oaica configures. A user who turned search off had it
// turned back on by the next launch, with no message.
//
// Both keys are now written only where they are missing, or where an earlier
// launch left the stale plugin shape that this writer exists to repair.

import (
	"os"
	"path/filepath"
	"testing"
)

// openclawSwitchesConfig is an OpenClaw config in which the user has turned web
// search off and disabled the ollama plugin entry.
const openclawSwitchesConfig = `{
  "plugins": {
    "entries": {
      "ollama": {"enabled": false, "theirOption": "keep"}
    }
  },
  "tools": {
    "web": {
      "search": {"enabled": false, "provider": "ollama"}
    }
  },
  "userKey": true
}
`

func TestOpenclawLeavesTheUsersSearchSwitchesAlone(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(openclawSwitchesConfig), 0o644); err != nil {
		t.Fatal(err)
	}

	configureOllamaWebSearchLocked(configPath)

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := decodeJSONObject(data)
	if err != nil {
		t.Fatalf("rewritten config does not parse: %v\n%s", err, data)
	}

	search := openclawNestedObject(t, config, "tools", "web", "search")
	if got, ok := search["enabled"].(bool); !ok || got {
		t.Errorf("tools.web.search.enabled = %v, want the user's false: the launch owns the provider behind search, not whether search runs", search["enabled"])
	}
	if got := search["provider"]; got != "ollama" {
		t.Errorf("tools.web.search.provider = %v, want ollama left in place", got)
	}

	entries := openclawNestedObject(t, config, "plugins", "entries")
	entry, _ := entries["ollama"].(map[string]any)
	if entry == nil {
		t.Fatalf("plugins.entries.ollama was dropped: %v", entries)
	}
	if got, ok := entry["enabled"].(bool); !ok || got {
		t.Errorf("plugins.entries.ollama.enabled = %v, want the user's false", entry["enabled"])
	}
	if entry["theirOption"] != "keep" {
		t.Errorf("the user's own key in the plugin entry was dropped: %v", entry)
	}
}

// TestOpenclawStillEnablesSearchWhenTheKeyIsMissing is the control: a config
// that never expressed a preference still gets the switch set, which is what
// makes the integration work out of the box.
func TestOpenclawStillEnablesSearchWhenTheKeyIsMissing(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)

	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(configPath, []byte(`{"tools":{"web":{}}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	configureOllamaWebSearchLocked(configPath)

	data, err := os.ReadFile(configPath)
	if err != nil {
		t.Fatal(err)
	}
	config, err := decodeJSONObject(data)
	if err != nil {
		t.Fatalf("rewritten config does not parse: %v\n%s", err, data)
	}
	search := openclawNestedObject(t, config, "tools", "web", "search")
	if got, ok := search["enabled"].(bool); !ok || !got {
		t.Errorf("tools.web.search.enabled = %v, want true when the config never set it", search["enabled"])
	}
	if got := search["provider"]; got != "ollama" {
		t.Errorf("tools.web.search.provider = %v, want ollama", got)
	}
	entries := openclawNestedObject(t, config, "plugins", "entries")
	entry, _ := entries["ollama"].(map[string]any)
	if entry == nil {
		t.Fatalf("plugins.entries.ollama was not created: %v", entries)
	}
	if got, ok := entry["enabled"].(bool); !ok || !got {
		t.Errorf("plugins.entries.ollama.enabled = %v, want true when absent", entry["enabled"])
	}
}

// openclawNestedObject walks a chain of object keys, failing the test when any
// level is absent or not an object.
func openclawNestedObject(t *testing.T, config map[string]any, keys ...string) map[string]any {
	t.Helper()
	current := config
	for _, key := range keys {
		next, ok := current[key].(map[string]any)
		if !ok {
			t.Fatalf("%v is missing or not an object: %v", keys, config)
		}
		current = next
	}
	return current
}
