package cmd

// editor_command_integrity_test.go — a whitespace-only editor variable crashed
// the interactive session (2026-09-26 audit).
//
// editInExternalEditor guards each source with `== ""`, then does
// strings.Fields(editor)[0]. A shell that exports EDITOR=" " (or a config file
// with a trailing space) is not empty, so it passes the guard, Fields returns
// no words, and the index panics: Ctrl+G in the prompt editor takes the whole
// session down instead of falling back to the platform default.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// noEditorInEnv clears the editor variables so the fallback is what is under
// test, not the environment the suite happens to run in.
func noEditorInEnv(t *testing.T) {
	t.Helper()
	t.Setenv("OLLAMA_EDITOR", "")
	t.Setenv("VISUAL", "")
	t.Setenv("EDITOR", "")
}

func TestAWhitespaceEditorVariableFallsBackInsteadOfPanicking(t *testing.T) {
	noEditorInEnv(t)
	t.Setenv("OLLAMA_EDITOR", "   ")

	got := resolveEditorCommand()
	if len(got) == 0 {
		t.Fatalf("a whitespace-only OLLAMA_EDITOR produced no command at all — the caller indexes this slice, so that is a panic in the interactive session")
	}
	if got[0] != defaultEditor {
		t.Errorf("OLLAMA_EDITOR=%q resolved to %q, want the platform default %q", "   ", got[0], defaultEditor)
	}
}

func TestAWhitespaceEditorInTheFallbackChainIsSkipped(t *testing.T) {
	noEditorInEnv(t)
	t.Setenv("VISUAL", " \t ")
	t.Setenv("EDITOR", "\n")

	got := resolveEditorCommand()
	if len(got) == 0 || got[0] != defaultEditor {
		t.Fatalf("a chain of whitespace-only variables resolved to %q, want %q", got, defaultEditor)
	}
}

func TestAnEditorWithArgumentsIsSplitIntoCommandAndArguments(t *testing.T) {
	noEditorInEnv(t)
	t.Setenv("OLLAMA_EDITOR", "  /usr/bin/code --wait  ")

	got := resolveEditorCommand()
	want := []string{"/usr/bin/code", "--wait"}
	if strings.Join(got, " ") != strings.Join(want, " ") {
		t.Errorf("resolveEditorCommand() = %q, want %q — the editor is a command line, and the surrounding space is not part of it", got, want)
	}
	if len(got) > 0 && got[0] == " " {
		t.Errorf("the leading space was kept as the command name")
	}
}

// A real editor on PATH is used as given: the fallback must not replace a
// working editor, only an empty one.
func TestAWorkingEditorIsUsedAsGiven(t *testing.T) {
	noEditorInEnv(t)
	self, err := os.Executable()
	if err != nil {
		t.Skip("no executable path to point the editor at")
	}
	t.Setenv("OLLAMA_EDITOR", filepath.Join(filepath.Dir(self), filepath.Base(self)))

	got := resolveEditorCommand()
	if len(got) == 0 {
		t.Fatal("resolveEditorCommand returned nothing for a real path")
	}
	if got[0] != os.Getenv("OLLAMA_EDITOR") {
		t.Errorf("resolveEditorCommand() = %q, want the configured editor %q", got[0], os.Getenv("OLLAMA_EDITOR"))
	}
}
