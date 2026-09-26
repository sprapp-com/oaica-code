package launch

// vscode_opencode_corrupt_config_integrity_test.go — both integrations read a
// file they only partly own, and both swallowed the parse error
// (`_ = json.Unmarshal(...)`), so an unreadable document was treated as an
// empty one and rewritten with only the members oaica writes
// (2026-09-26 audit).
//
// VSCode.Edit is the worse of the two: chatLanguageModels.json lists every
// vendor the user has added to the chat model picker, and Edit's job is to
// remove the ollama entry and re-add it. With the entries parsed as nil, the
// file came back holding the ollama entry alone — every other vendor gone.
//
// The vscode_test.go case "corrupted JSON treated as empty" asserted exactly
// that, and was rewritten with this change.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestVSCodeEditRefusesToRewriteUnreadableLanguageModels(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	t.Setenv("XDG_CONFIG_HOME", "")
	clmPath := testVSCodePath(t, tmpDir, "chatLanguageModels.json")
	if err := os.MkdirAll(filepath.Dir(clmPath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(clmPath, []byte(`[{"vendor":"azure","name":"Azure"`), 0o644); err != nil {
		t.Fatal(err)
	}
	corrupt, err := os.ReadFile(clmPath)
	if err != nil {
		t.Fatal(err)
	}

	v := &VSCode{}
	if err := v.Edit(testLaunchModels("llama3.2")); err == nil {
		t.Errorf("Edit returned nil for a chatLanguageModels.json it could not parse — it then wrote a file holding only the ollama vendor entry, so every other vendor the user configured is gone")
	}
	got, rerr := os.ReadFile(clmPath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != string(corrupt) {
		t.Errorf("the unreadable file was overwritten:\n%s\nwant it left byte-identical at:\n%s", got, corrupt)
	}
}

func TestOpenCodeEditRefusesToRewriteUnreadableModelState(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	statePath, err := openCodeStatePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
		t.Fatal(err)
	}
	// Truncated mid-array: `favorite` and `variant` are the members oaica
	// never writes, and they are what disappears when this parses as nothing.
	corrupt := `{"recent":[{"providerID":"ollama","modelID":"llama3.2"}],"favorite":[`
	if err := os.WriteFile(statePath, []byte(corrupt), 0o644); err != nil {
		t.Fatal(err)
	}

	o := &OpenCode{}
	if err := o.Edit(testLaunchModels("gemma4")); err == nil {
		t.Errorf("Edit returned nil for a model state file it could not parse — it then rewrote it from the defaults map, so the user's favorites are gone")
	}
	got, rerr := os.ReadFile(statePath)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != corrupt {
		t.Errorf("the unreadable state file was overwritten:\n%s\nwant it left byte-identical at:\n%s", got, corrupt)
	}
}

// The controls: readable files are still updated, and members oaica does not
// model survive — including a number past 2^53.
func TestTheStateFilesKeepMembersOaicaDoesNotModel(t *testing.T) {
	t.Run("vscode", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)
		t.Setenv("XDG_CONFIG_HOME", "")
		clmPath := testVSCodePath(t, tmpDir, "chatLanguageModels.json")
		if err := os.MkdirAll(filepath.Dir(clmPath), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(clmPath, []byte(`[{"vendor":"azure","name":"Azure","maxInputTokens":9007199254740993}]`), 0o644); err != nil {
			t.Fatal(err)
		}

		v := &VSCode{}
		if err := v.Edit(testLaunchModels("llama3.2")); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(clmPath)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"azure", "9007199254740993"} {
			if !strings.Contains(string(got), want) {
				t.Errorf("%s did not survive the update:\n%s", want, got)
			}
		}
	})

	t.Run("opencode", func(t *testing.T) {
		tmpDir := t.TempDir()
		setTestHome(t, tmpDir)
		statePath, err := openCodeStatePath()
		if err != nil {
			t.Fatal(err)
		}
		if err := os.MkdirAll(filepath.Dir(statePath), 0o755); err != nil {
			t.Fatal(err)
		}
		fixture := `{"recent":[{"providerID":"other","modelID":"keep-me"}],"favorite":[{"providerID":"other","modelID":"fav"}],"variant":{"m":"high"},"revision":9007199254740993}`
		if err := os.WriteFile(statePath, []byte(fixture), 0o644); err != nil {
			t.Fatal(err)
		}

		o := &OpenCode{}
		if err := o.Edit(testLaunchModels("gemma4")); err != nil {
			t.Fatal(err)
		}
		got, err := os.ReadFile(statePath)
		if err != nil {
			t.Fatal(err)
		}
		for _, want := range []string{"fav", `"high"`, "9007199254740993", "keep-me"} {
			if !strings.Contains(string(got), want) {
				t.Errorf("%s did not survive the update:\n%s", want, got)
			}
		}
	})
}
