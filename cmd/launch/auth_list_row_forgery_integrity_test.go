package launch

// auth_list_row_forgery_integrity_test.go — `oaica auth list` printed provider
// names from two untrusted sources raw (2026-09-26 audit, tenth round).
//
// Neither source is a constant in the binary. The provider catalog merges a
// SYNCED cache file fetched from the network (providerCatalogCachePath), and
// the "Stored credentials outside the catalog" section prints the keys of
// ~/.oaica/auth.json. A name carrying a newline from either one forges a whole
// extra row — complete with a status and a masked credential — in the command a
// user runs to find out what is actually configured, and that output is
// documented as pasteable into a bug report. `remote list` and `doctor` already
// quote such names (printableName); this table did not.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A synced catalog row is remote-controlled data, and the table prints it.
func TestAuthListDoesNotPrintARawCatalogName(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	dir := filepath.Join(home, ".oaica", "cache", "providers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// An added row needs no version bump: additive rows merge whatever the
	// document's own version says.
	body := `{"version":1,"providers":[{"name":"synced\nacme      ready     env:FAKE_KEY","base_url":"https://synced.example/v1","api_key_env":"FAKE_KEY"}]}`
	if err := os.WriteFile(filepath.Join(dir, "providers.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	if err := AuthList(&buf); err != nil {
		t.Fatalf("AuthList: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "synced\nacme") {
		t.Errorf("a synced catalog name was printed raw, so the rest of it reads as a row of its own:\n%s", out)
	}
	if !strings.Contains(out, "synced") {
		t.Errorf("the synced row vanished from the listing instead of being quoted:\n%s", out)
	}
}

// The store's own keys are the other source, written by `oaica auth login`.
func TestAuthListDoesNotPrintARawStoredName(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	// A remote added by hand (remotes.json is editable) with a name that
	// add-time validation would refuse — the store is keyed BY that name, and
	// `auth login` stores under the remote's own spelling.
	name := "mine\nacme      ready     stored"
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"`+asJSON(name)+`","base_url":"https://mine.example/v1","tool_format":"tool_calls"}]}`)

	key, err := SplitUserinfoCredential("sk-stored-SECRET-1234567890")
	_ = key
	_ = err
	if err := AuthLogin(&bytes.Buffer{}, name, "sk-stored-SECRET-1234567890"); err != nil {
		t.Fatalf("auth login: %v", err)
	}

	var buf bytes.Buffer
	if err := AuthList(&buf); err != nil {
		t.Fatalf("AuthList: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "mine\nacme") {
		t.Errorf("a stored provider name was printed raw, so the rest of it reads as a row of its own:\n%s", out)
	}
	if !strings.Contains(out, "mine") {
		t.Errorf("the stored provider vanished from the listing instead of being quoted:\n%s", out)
	}
}

// Control: an ordinary provider still prints plainly — this cannot be passed
// by quoting every row.
func TestAuthListStillPrintsOrdinaryNames(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var buf bytes.Buffer
	if err := AuthList(&buf); err != nil {
		t.Fatalf("AuthList: %v", err)
	}
	out := buf.String()
	if !strings.Contains(out, "zai-coding-plan") {
		t.Fatalf("the catalog row is missing from the listing:\n%s", out)
	}
	if strings.Contains(out, `"zai-coding-plan"`) {
		t.Errorf("an ordinary catalog name was quoted:\n%s", out)
	}
}
