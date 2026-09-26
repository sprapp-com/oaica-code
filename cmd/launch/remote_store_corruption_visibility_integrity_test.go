package launch

// remote_store_corruption_visibility_integrity_test.go — one unreadable byte in
// remotes.json dropped every remote AND the fileless built-ins, and the launch
// picker never said so (2026-09-26 audit, tenth round, auditor B).
//
// The parse failure returned before the built-in merge, so a hand-edited or
// third-party-written file (trailing comma, bare array, truncated transfer)
// emptied the picker of rows that need no file at all — z.ai, whose row is
// keyed on an environment variable. `oaica doctor` and `oaica remote list` do
// print the error, but the path the user is actually in showed a smaller menu
// with no reason: the inventory collected the error and never read it.

import (
	"os"
	"strings"
	"testing"
)

// A corrupt store must not hide the built-in providers, and the reason must be
// visible on the launch path.
func TestACorruptRemoteStoreKeepsTheBuiltinsAndWarns(t *testing.T) {
	path := withTempRemotesFile(t)
	// A built-in provider surfaces when its key env var is set — it needs no
	// remotes.json entry at all.
	t.Setenv("Z_AI_API_KEY", "sk-builtin-key")
	if len(builtinRemotes()) == 0 {
		t.Skip("no built-in provider is keyed in this environment, so the premise cannot be built")
	}

	if err := os.WriteFile(path, []byte(`{"remotes":[{"name":"mine","base_url":"https://api.example.com",},]}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// The load itself still reports the failure — that is what doctor and
	// `remote list` print.
	if _, err := loadUserRemotes(); err == nil {
		t.Fatalf("premise: the corrupt store loaded without error")
	}

	var (
		swept []userRemote
		serrs []error
	)
	out := captureStderr(t, func() {
		swept, serrs = launchSweepRemotes()
	})
	if len(serrs) == 0 {
		t.Errorf("the launch sweep reported no error for a store it cannot read, so the user is never told")
	}
	if len(swept) == 0 {
		t.Errorf("a remotes.json that cannot be read as a whole returned no remotes to sweep — every built-in provider needs no file, so the picker silently loses z.ai and the rest for a stray byte")
	}
	if !strings.Contains(out, "Warning") || !strings.Contains(out, "not readable as a remote store") {
		t.Errorf("nothing told the user their remote store is unreadable:\n%s", out)
	}
	if !strings.Contains(out, path) {
		t.Errorf("the warning does not name the file to fix (%s):\n%s", path, out)
	}

	// And a built-in whose key IS set is among them, not just "some rows".
	found := false
	for _, r := range swept {
		if r.Name == builtinRemotes()[0].Name {
			found = true
		}
	}
	if !found {
		t.Errorf("the fallback swept %d remote(s) but not the keyed built-in %q", len(swept), builtinRemotes()[0].Name)
	}
}

// The path that resolves "<remote>/<model>" must not lose the built-ins either:
// with a corrupt store, a model only a built-in serves is still launchable.
func TestABuiltinModelStillResolvesThroughACorruptStore(t *testing.T) {
	path := withTempRemotesFile(t)
	t.Setenv("Z_AI_API_KEY", "sk-builtin-key")
	builtins := builtinRemotes()
	if len(builtins) == 0 {
		t.Skip("no built-in provider is keyed in this environment")
	}
	builtin := builtins[0]

	if err := os.WriteFile(path, []byte(`["not","an","object"]`), 0o600); err != nil {
		t.Fatal(err)
	}

	r, bare, ok := findUserRemoteForModel(builtin.Name + "/some-model")
	if !ok {
		t.Errorf("a corrupt remotes.json hid the built-in provider %q — the row needs no file, so \"%s/some-model\" must still resolve", builtin.Name, builtin.Name)
		return
	}
	if bare != "some-model" {
		t.Errorf("bare model = %q, want some-model", bare)
	}
	if r.Name != builtin.Name {
		t.Errorf("resolved %q, want %q", r.Name, builtin.Name)
	}
}

// Control: an ordinary store still wins over the built-ins and reports
// nothing, so these tests cannot be passed by always warning and always
// falling back.
func TestAReadableStoreStillWinsAndWarnsNothing(t *testing.T) {
	withTempRemotesFile(t)
	t.Setenv("Z_AI_API_KEY", "sk-builtin-key")

	if _, err := RemoteAdd(RemoteAddOptions{Name: "mine", BaseURL: "https://api.example.com"}); err != nil {
		t.Fatalf("add: %v", err)
	}
	out := captureStderr(t, func() {
		r, bare, ok := findUserRemoteForModel("mine/m1")
		if !ok || r.Name != "mine" || bare != "m1" {
			t.Errorf("a readable store did not resolve its own remote: %q %q %t", r.Name, bare, ok)
		}
	})
	if strings.Contains(out, "Warning") {
		t.Errorf("a healthy store warned:\n%s", out)
	}
}
