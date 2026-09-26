package launch

// opencode_auth_integrity_test.go — `oaica signin opencode:<provider>` and the
// credential-reuse layer it feeds must agree on WHERE opencode's store is, and
// the signin must not destroy a credential it cannot re-mint (2026-09-26
// audit, third round):
//
//   - the writer used OpencodeAuthPath (one fixed location) while every reader
//     used opencodeAuthPaths, which returns ONLY $OPENCODE_AUTH_FILE when it is
//     set — so with that variable set the command printed "Saved API key" for a
//     key no reader would ever find;
//   - it replaced whatever entry was there, including an OAuth entry, whose
//     refresh token and expiry the replacement silently deleted.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func ocStoreAt(t *testing.T, dir string) string {
	t.Helper()
	p := filepath.Join(dir, "opencode-auth.json")
	t.Setenv("OPENCODE_AUTH_FILE", p)
	return p
}

func ocReadStore(t *testing.T, path string) map[string]opencodeAuthEntry {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no store was written at %s: %v", path, err)
	}
	var m map[string]opencodeAuthEntry
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	return m
}

// F5a: the key lands where the readers look.
func TestOpencodeSigninWritesToTheStoreReadersUse(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", "")
	store := ocStoreAt(t, dir)

	if err := SaveOpencodeAPIKey("zai", "sk-opencode-test"); err != nil {
		t.Fatal(err)
	}

	got := ocReadStore(t, store)
	if got["zai"].Key != "sk-opencode-test" {
		t.Errorf("store at $OPENCODE_AUTH_FILE holds %+v — oaica signin wrote somewhere else, so the reuse layer (which reads this path and nothing else while the variable is set) sees no credential at all", got["zai"])
	}
	cred, ok := opencodeCredential("zai")
	if !ok || cred.Key != "sk-opencode-test" {
		t.Errorf("opencodeCredential(\"zai\") = %+v, ok=%v — a signin that reports success must leave a credential the launcher can use", cred, ok)
	}
}

// F5b: an OAuth entry is not replaced by an API key.
func TestOpencodeSigninRefusesToClobberAnOAuthEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", "")
	store := ocStoreAt(t, dir)

	oauth := map[string]opencodeAuthEntry{
		"zai": {Type: "oauth", Access: "at-live", Refresh: "rt-live", Expires: 4102444800000},
	}
	b, _ := json.Marshal(oauth)
	if err := os.WriteFile(store, b, 0o600); err != nil {
		t.Fatal(err)
	}

	err := SaveOpencodeAPIKey("zai", "sk-replacement")
	if err == nil {
		t.Error("signin overwrote a live OAuth entry with an API key and reported success — the refresh token and expiry are gone and only `opencode auth login` can put them back")
	}
	after := ocReadStore(t, store)
	if after["zai"].Type != "oauth" || after["zai"].Refresh != "rt-live" || after["zai"].Expires != 4102444800000 {
		t.Errorf("the OAuth entry did not survive: %+v", after["zai"])
	}

	// An ordinary api entry may still be replaced — that is what re-signin is.
	b, _ = json.Marshal(map[string]opencodeAuthEntry{"zai": {Type: "api", Key: "sk-old"}})
	if err := os.WriteFile(store, b, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveOpencodeAPIKey("zai", "sk-new"); err != nil {
		t.Fatalf("replacing an api-key entry failed: %v", err)
	}
	if got := ocReadStore(t, store)["zai"].Key; got != "sk-new" {
		t.Errorf("api entry = %q, want sk-new", got)
	}
}

// F5c: the signin's "existing providers" list reads the same store.
func TestOpencodeKnownProvidersReadsTheReadersStore(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", "")
	store := ocStoreAt(t, dir)
	b, _ := json.Marshal(map[string]opencodeAuthEntry{"deepseek": {Type: "api", Key: "sk-d"}})
	if err := os.WriteFile(store, b, 0o600); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, p := range OpencodeKnownProviders() {
		if p == "deepseek" {
			found = true
		}
	}
	if !found {
		t.Errorf("OpencodeKnownProviders() = %v — the store the readers use is not the one this lists", OpencodeKnownProviders())
	}
}
