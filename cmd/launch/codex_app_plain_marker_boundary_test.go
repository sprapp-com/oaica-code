package launch

// codex_app_plain_marker_boundary_test.go — pins the boundary audited as F11
// (2026-09-27 audit, round 21): the codex-app restore path must not claim the
// PLAIN codex integration's marker.
//
// `codexAppManagedProfileNames()` lists both "ollama-launch-codex-app" and the
// plain integration's "ollama-launch". The audited claim was that
// codexAppRemoveOwnedRootValues strips root profile/model/model_provider on
// either name, so a second `oaica launch chatgpt --restore` (which takes the
// no-state-file path) could delete the plain codex launch's root keys.
//
// It does not: the destructive paths test codexAppIsOwnedProfileName, which is
// the codex-app name alone, so a root config naming "ollama-launch" is returned
// untouched. This test holds that line: if a future edit widens the destructive
// predicate to the managed set, it fails here.

import (
	"strings"
	"testing"
)

func TestCodexAppRestoreDoesNotStripThePlainCodexRootKeys(t *testing.T) {
	plain := strings.Join([]string{
		`profile = "ollama-launch"`,
		`model = "llama3.2"`,
		`model_provider = "ollama-launch"`,
		``,
		`[model_providers.ollama-launch]`,
		`name = "Ollama"`,
		`base_url = "http://127.0.0.1:11434/v1"`,
		``,
	}, "\n")

	if got := codexAppRemoveOwnedRootValues(plain); got != plain {
		t.Errorf("codexAppRemoveOwnedRootValues rewrote a config naming the PLAIN codex profile:\n got:\n%s\n want:\n%s", got, plain)
	}
	if codexAppRootStillManaged(plain) {
		t.Error("codexAppRootStillManaged claims the plain codex profile name is codex-app's own")
	}
	if codexAppRootReferencesOwnedConfig(plain) {
		t.Error("codexAppRootReferencesOwnedConfig claims the plain codex profile name is codex-app's own")
	}
}

// TestCodexAppRestoreStillStripsItsOwnRootKeys is the control: a root config
// naming the codex-app profile IS cleaned, which is what the restore path is
// for.
func TestCodexAppRestoreStillStripsItsOwnRootKeys(t *testing.T) {
	owned := strings.Join([]string{
		"profile = " + `"` + codexAppProfileName + `"`,
		`model = "llama3.2"`,
		"model_provider = " + `"` + codexAppProfileName + `"`,
		``,
	}, "\n")

	got := codexAppRemoveOwnedRootValues(owned)
	for _, key := range []string{"profile", "model =", "model_provider"} {
		if strings.Contains(got, key) {
			t.Errorf("codex-app's own root key %q survived the restore: %s", key, got)
		}
	}
}
