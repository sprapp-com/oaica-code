package launch

// round22_robustness_integrity_test.go — four small failures around the
// integrations (2026-09-27 audit, round 22):
//
// 1. An EMPTY store is not a corrupt one. Decode reports io.EOF for a
//    zero-byte or whitespace-only document, so a qwen settings.json or an
//    opencode auth.json that another tool had truncated failed the launch
//    ("it is not valid JSON") although every writer in this package treats
//    emptiness as "nothing to preserve".
// 2. findKimiBinary discarded os.UserHomeDir()'s error, so with HOME unset the
//    home-relative candidates collapsed to CWD-relative paths and a planted
//    file could be executed with KIMI_MODEL_API_KEY in its environment.
// 3. copilot and poolside forwarded a passthrough -m/--model, giving the child
//    two contradictory model flags where kimi refuses the same input.
// 4. openclaw's carry-over comment promised that a row with no id is "kept
//    whole" while the code dropped it.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnEmptyStoreIsNotACorruptOne(t *testing.T) {
	for _, body := range []string{"", "   ", "\n\t\n"} {
		doc, err := decodeJSONObject([]byte(body))
		if err != nil {
			t.Errorf("decodeJSONObject(%q) = %v, want an empty document: an empty store is nothing to preserve, not invalid JSON", body, err)
			continue
		}
		if doc == nil || len(doc) != 0 {
			t.Errorf("decodeJSONObject(%q) = %v, want a non-nil empty map", body, doc)
		}
	}

	// The other decoder, reached by `oaica signin opencode:<provider>`.
	path := filepath.Join(t.TempDir(), "auth.json")
	if err := os.WriteFile(path, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	doc, err := opencodeAuthReadDocument(path)
	if err != nil {
		t.Errorf("opencodeAuthReadDocument of an empty file = %v, want an empty document", err)
	}
	if doc == nil {
		t.Error("opencodeAuthReadDocument returned a nil map for an empty file")
	}
}

// Control: a document that really is malformed is still refused, which is the
// whole point of the refusal.
func TestAMalformedStoreIsStillRefused(t *testing.T) {
	if _, err := decodeJSONObject([]byte("{not json")); err == nil {
		t.Error("decodeJSONObject accepted a malformed document")
	}
}

// TestAnUnsavableKeyPromptNamesTheWorkingPath: a remote with no row in
// remotes.json cannot take a key typed at the prompt, and the error used to be
// a dead end for a key the user had already typed. Not reproducible through the
// prompt itself (a built-in provider is listed only once it HAS a key —
// builtinRemotes), but the message is what a user in that state reads.
func TestAnUnsavableKeyPromptNamesTheWorkingPath(t *testing.T) {
	setTestHome(t, t.TempDir())
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[]}`)

	err := savePromptedRemoteKey("zai-coding-plan", "typed-by-the-user")
	if err == nil {
		t.Fatal("savePromptedRemoteKey accepted a key for a provider with no row in the store")
	}
	if !strings.Contains(err.Error(), "oaica auth login") {
		t.Errorf("error = %v, want it to name `oaica auth login <provider>`, the path that does store that key", err)
	}
	if strings.Contains(err.Error(), "typed-by-the-user") {
		t.Errorf("the error echoes the key: %v", err)
	}
}

func TestKimiRefusesToGuessTheHomeDirectory(t *testing.T) {
	t.Setenv("PATH", t.TempDir()) // no kimi on PATH: the home lookup is next
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "")

	_, err := findKimiBinary()
	if err == nil {
		t.Fatal("findKimiBinary answered with an empty HOME: the home-relative candidates are CWD-relative then, and one of them holds this launch's API key in the child's environment")
	}
	if !strings.Contains(err.Error(), "home") {
		t.Errorf("error = %v, want it to name the missing home directory", err)
	}
}

// TestCopilotAndPoolsideRefuseAPassthroughModelFlag drives the Run paths with
// an empty PATH: the only way they can answer is the refusal, since neither
// binary is reachable — so a launch that answers "not installed" instead is
// forwarding the flag to a child it never gets to build.
func TestCopilotAndPoolsideRefuseAPassthroughModelFlag(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	for _, tc := range []struct {
		name string
		run  func(args []string) error
	}{
		{"copilot", func(args []string) error { return (&Copilot{}).Run("llama3.2", nil, args) }},
		{"pool", func(args []string) error { return (&Poolside{}).Run("llama3.2", nil, args) }},
	} {
		for _, arg := range []string{"--model", "--model=x", "-m", "-m=x"} {
			err := tc.run([]string{arg})
			if err == nil || strings.Contains(err.Error(), "not installed") {
				t.Errorf("%s.Run with %q = %v: the child would get oaica's model flag and this one, so the model that was gated need not be the one that runs", tc.name, arg, err)
			}
		}
		if err := tc.run([]string{"--verbose", "-p", "hello"}); err == nil || !strings.Contains(err.Error(), "not installed") {
			t.Errorf("%s.Run with ordinary args = %v, want the launch to reach the install check (these integrations set the model flag themselves)", tc.name, err)
		}
	}
}
