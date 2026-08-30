package cmd

// oaica_audit_test.go — regressions for the 0.4.6 fresh-user audit fixes:
// daemon-only commands explaining the thin-client architecture (P0-1), bare
// `oaica` in a pipe (P0-3), unknown-model exit codes and local-model hints
// (P0-4/P1-2), oaica-not-ollama help strings (P1-1), and the auth-failure
// hint (P1-3).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/spf13/cobra"
)

func TestErrNoLocalDaemonExplainsThinClient(t *testing.T) {
	t.Setenv("OLLAMA_HOST", "127.0.0.1:11434")
	msg := errNoLocalDaemon().Error()
	if strings.Contains(msg, "run 'ollama serve'") {
		t.Fatalf("must not tell the user to run a daemon this fork has no notion of: %q", msg)
	}
	for _, want := range []string{
		"no local Ollama daemon at",
		"oaica is a thin client",
		"`oaica run <model>` (hosted)",
		"`oaica pull <model>` + `oaica serve <model>` (self-host)",
		"OLLAMA_HOST",
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("message missing %q, got: %s", want, msg)
		}
	}
}

func TestRootCommandIsNamedOaica(t *testing.T) {
	root := NewCLI()
	if root.Use != "oaica" {
		t.Fatalf("root Use = %q, want oaica", root.Use)
	}
	// The whole help tree must never advertise the upstream binary name.
	var walk func(*cobra.Command)
	walk = func(c *cobra.Command) {
		for _, s := range []string{c.Use, c.Short} {
			if strings.Contains(strings.ToLower(s), "ollama ") {
				t.Errorf("%s: %q still says \"ollama\"", c.CommandPath(), s)
			}
		}
		for _, sub := range c.Commands() {
			walk(sub)
		}
	}
	walk(root)
}

// P2-4: create/push/cp need a daemon this fork doesn't run — hidden from help
// but still invocable. list/ps/rm/show stay visible.
func TestDaemonOnlyCommandsHiddenButPresent(t *testing.T) {
	root := NewCLI()
	byName := map[string]*cobra.Command{}
	for _, c := range root.Commands() {
		byName[c.Name()] = c
	}
	for _, name := range []string{"create", "push", "cp"} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("%s must still exist", name)
		}
		if !c.Hidden {
			t.Errorf("%s should be hidden from top-level help", name)
		}
	}
	for _, name := range []string{"list", "ps", "rm", "show", "run", "pull", "serve", "remote"} {
		c, ok := byName[name]
		if !ok {
			t.Fatalf("%s must exist", name)
		}
		if c.Hidden {
			t.Errorf("%s must stay visible in help", name)
		}
	}
}

// P2-3: `oaica --version` reads "oaica <version>", not "oaica version is ...".
func TestVersionHandlerPhrasing(t *testing.T) {
	out := captureStdout(t, func() { versionHandler(NewCLI(), nil) })
	if !strings.HasPrefix(out, "oaica ") || strings.Contains(out, "version is") {
		t.Fatalf("--version output = %q, want \"oaica <version>\"", out)
	}
}

// P1-3: an auth failure from the router carries the fix on the next line.
func TestOaicaWrapAuthErrorAppendsHint(t *testing.T) {
	err := oaicaWrapAuthError("missing or invalid API key")
	lines := strings.Split(err.Error(), "\n")
	if len(lines) != 2 {
		t.Fatalf("want exactly one message line + one hint line, got %q", err.Error())
	}
	if lines[1] != oaicaAuthHint {
		t.Fatalf("hint = %q, want %q", lines[1], oaicaAuthHint)
	}
	for _, want := range []string{"oaica signin", "OAICA_API_KEY", "https://oaica.com"} {
		if !strings.Contains(lines[1], want) {
			t.Errorf("hint missing %q: %s", want, lines[1])
		}
	}

	// Non-auth errors are passed through untouched.
	if got := oaicaWrapAuthError("context length exceeded").Error(); got != "context length exceeded" {
		t.Fatalf("non-auth error was rewritten: %q", got)
	}
}

// P0-4: a name that isn't a router model but IS a locally pulled GGUF points
// at `oaica serve`, not at a bare "unknown model".
func TestOaicaLocalModelHintFindsPulledGGUF(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("OAICA_MODELS_DIR", dir)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1") // no daemon
	if err := os.WriteFile(filepath.Join(dir, "kat-awq.gguf"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	hint := oaicaLocalModelHint(oaicaRunCmd(t), "kat-awq")
	for _, want := range []string{"'kat-awq' is a local model", "`oaica serve kat-awq` (pulled GGUF)", "OLLAMA_HOST=... ollama run"} {
		if !strings.Contains(hint, want) {
			t.Errorf("hint missing %q, got %q", want, hint)
		}
	}

	if got := oaicaLocalModelHint(oaicaRunCmd(t), "not-pulled"); got != "" {
		t.Fatalf("hint for a name that isn't local = %q, want empty", got)
	}
}
