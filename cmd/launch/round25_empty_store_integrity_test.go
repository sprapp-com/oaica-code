package launch

// round25_empty_store_integrity_test.go — an EMPTY store is not a corrupt one,
// for the two readers that hand-rolled their own decode (2026-09-27 audit,
// round 25).
//
// Round 22 adopted the rule for the package's shared reader (json_document.go)
// and for opencode's auth store: Decode reports io.EOF for a zero-byte or
// whitespace-only document, which read as "not valid JSON" and failed the
// launch, while every writer in this package treats emptiness as "nothing to
// preserve". No oaica writer produces an empty store (writes are atomic), so
// the trigger is another tool or an interrupted editor — exactly what the rule
// is for. Two sites never got it: VS Code's chatLanguageModels.json read (its
// own inline decoder inside Edit) and claude_desktop.go's readClaudeDesktopJSON,
// which every Claude Desktop writer and reader goes through.
//
// The consequences differ by site and both are user-visible: `oaica launch
// vscode` aborts with "not valid JSON (EOF)" where an ABSENT file succeeds and
// writes the vendor row, and every Claude Desktop writer fails with "parse
// Claude Desktop config: EOF" — while the error-swallowing readers report "not
// configured", so the launcher retried the same failing configure on every
// launch afterwards.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestVSCodeAcceptsAnEmptyChatLanguageModelsStore drives the real writer: an
// empty store declares no vendors, so there is nothing to preserve and nothing
// to refuse.
func TestVSCodeAcceptsAnEmptyChatLanguageModelsStore(t *testing.T) {
	setTestHome(t, t.TempDir())

	v := &VSCode{}
	clmPath := v.chatLanguageModelsPath()
	if err := os.MkdirAll(filepath.Dir(clmPath), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", "   ", "\n\t\n"} {
		if err := os.WriteFile(clmPath, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		if err := v.Edit([]LaunchModel{{Name: "llama3.2", LiveSource: liveSourceDaemon}}); err != nil {
			t.Errorf("Edit with a %q store = %v, want success: an empty store is nothing to preserve, not invalid JSON", body, err)
			continue
		}
		doc, err := os.ReadFile(clmPath)
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(string(doc), `"ollama"`) {
			t.Errorf("after Edit the store has no ollama vendor entry: %s", strings.TrimSpace(string(doc)))
		}
	}

	// Control: a document that really is malformed is still refused — rewriting
	// a file that cannot be read would delete every other vendor in it.
	if err := os.WriteFile(clmPath, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := v.Edit([]LaunchModel{{Name: "llama3.2"}}); err == nil {
		t.Error("Edit accepted a malformed store: the refusal exists because the rewrite would drop every other vendor")
	} else if !strings.Contains(err.Error(), "not valid JSON") {
		t.Errorf("refusal = %v, want the not-valid-JSON message", err)
	}
}

// TestClaudeDesktopAcceptsAnEmptyConfigStore pins the reader every Claude
// Desktop writer and reader goes through, plus the ConfigureAutodiscovery path
// that failed the launch through it.
func TestClaudeDesktopAcceptsAnEmptyConfigStore(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	withClaudeDesktopPlatform(t, "darwin")

	paths, err := claudeDesktopConfigPaths()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(paths.profile), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{"", " \n"} {
		if err := os.WriteFile(paths.profile, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
		cfg, err := readClaudeDesktopJSON(paths.profile)
		if err != nil {
			t.Errorf("readClaudeDesktopJSON of a %q profile = %v, want an empty document", body, err)
			continue
		}
		if cfg == nil {
			t.Errorf("readClaudeDesktopJSON returned a nil map for a %q profile", body)
		}
		cfg, err = readClaudeDesktopJSONAllowMissing(paths.profile)
		if err != nil {
			t.Errorf("readClaudeDesktopJSONAllowMissing of a %q profile = %v, want an empty document", body, err)
			continue
		}
		if cfg == nil || len(cfg) != 0 {
			t.Errorf("readClaudeDesktopJSONAllowMissing of a %q profile = %v, want a non-nil empty map", body, cfg)
		}
	}

	// Control: genuinely malformed is still an error, and still not swallowed by
	// the AllowMissing wrapper (which is about absence, not corruption).
	if err := os.WriteFile(paths.profile, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := readClaudeDesktopJSON(paths.profile); err == nil {
		t.Error("readClaudeDesktopJSON accepted a malformed document")
	}
	if _, err := readClaudeDesktopJSONAllowMissing(paths.profile); err == nil {
		t.Error("readClaudeDesktopJSONAllowMissing accepted a malformed document: it tolerates absence, not corruption")
	}
}

// TestMuseAcceptsAnEmptySettingsStore is the third site of the same rule: an
// empty launch-owned settings file failed the launch ("read muse settings …
// EOF") where an absent one started muse with defaults.
func TestMuseAcceptsAnEmptySettingsStore(t *testing.T) {
	setTestHome(t, t.TempDir())

	path, err := museSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	settings, err := museReadSettings(path)
	if err != nil {
		t.Fatalf("museReadSettings of an empty store = %v, want an empty document", err)
	}
	if settings == nil {
		t.Error("museReadSettings returned a nil map for an empty store")
	}
	if _, err := museBaseSettings(); err != nil {
		t.Errorf("museBaseSettings = %v, want the base settings to build: an empty store is nothing to preserve", err)
	}

	// Control: malformed is still refused, because falling through would rewrite
	// the file and discard whatever muse persisted in it.
	if err := os.WriteFile(path, []byte("{not json"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := museReadSettings(path); err == nil {
		t.Error("museReadSettings accepted a malformed document")
	}
}
