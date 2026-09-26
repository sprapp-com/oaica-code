package launch

// opencode_auth_foreign_members_integrity_test.go — `oaica signin
// opencode:<provider>` rewrote opencode's whole auth.json from a five-field
// view (2026-09-26 audit).
//
// opencodeAuthEntry models type/key/access/refresh/expires. The store is a
// map of PROVIDER to that entry, and signing in for one provider unmarshalled
// every entry into the struct and marshalled the map straight back — so every
// member the struct does not model was deleted, for every provider in the
// file, not just the one being written. opencode's own "well-known" entry
// type stores a `token` that this struct has no field for, so adding a key
// for an unrelated provider signed the user out of that one, silently.
//
// The package's stated discipline for a partially-modelled, user-owned file
// is exactly the opposite: merge the members the Go view does not carry, and
// compare the marshalled bytes (store_document.go). Numbers are the second
// half of it — json.Unmarshal into any turns an integer past 2^53 into a
// float, so the document is decoded with UseNumber.

import (
	"bytes"
	"encoding/json"
	"os"
	"testing"
)

func ocRawDoc(t *testing.T, path string) map[string]map[string]any {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no store was written at %s: %v", path, err)
	}
	doc := map[string]map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		t.Fatalf("the store is not JSON any more: %v\n%s", err, b)
	}
	return doc
}

func TestOpencodeSigninKeepsTheProvidersItDoesNotModel(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", "")
	store := ocStoreAt(t, dir)

	// A credential this struct cannot represent (a "well-known" entry with a
	// token, and an integer past 2^53), plus a member on an ordinary entry
	// that oaica has no field for.
	seed := `{
  "well-known": {"type": "well-known", "token": "wt-live", "expires": 9007199254740993},
  "other": {"type": "api", "key": "sk-other", "note": "keep me"}
}`
	if err := os.WriteFile(store, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SaveOpencodeAPIKey("zai", "sk-new"); err != nil {
		t.Fatal(err)
	}
	doc := ocRawDoc(t, store)

	if got, _ := doc["well-known"]["token"].(string); got != "wt-live" {
		t.Errorf("signing in for zai deleted the well-known provider's token (entry is now %+v) — opencode's own entry type for it has no field in oaica's struct, and signing out a user who never asked is not something a `signin` command may do", doc["well-known"])
	}
	if got := doc["well-known"]["expires"]; got != json.Number("9007199254740993") {
		t.Errorf("the well-known expiry came back as %v — a number the struct cannot hold was re-marshalled as a float, so the value the user's other tool wrote is corrupted, not preserved", got)
	}
	if got, _ := doc["other"]["note"].(string); got != "keep me" {
		t.Errorf("the other provider's extra member was dropped: %+v", doc["other"])
	}
	if got, _ := doc["other"]["key"].(string); got != "sk-other" {
		t.Errorf("another provider's key was rewritten: %+v", doc["other"])
	}
	// The write still lands, on the provider it was asked for.
	if got, _ := doc["zai"]["key"].(string); got != "sk-new" {
		t.Errorf("the signin's own key is missing from the store: %+v", doc["zai"])
	}
	if got, _ := doc["zai"]["type"].(string); got != "api" {
		t.Errorf("the new entry's type is %v, want api", doc["zai"]["type"])
	}
}

// The control: an ordinary api entry is still replaced on re-signin, and the
// refusal for an entry an API key cannot replace still fires.
func TestOpencodeSigninStillReplacesAnAPIEntry(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	t.Setenv("XDG_DATA_HOME", "")
	store := ocStoreAt(t, dir)

	if err := os.WriteFile(store, []byte(`{"zai": {"type": "api", "key": "sk-old", "note": "keep me"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveOpencodeAPIKey("zai", "sk-new"); err != nil {
		t.Fatal(err)
	}
	doc := ocRawDoc(t, store)
	if got, _ := doc["zai"]["key"].(string); got != "sk-new" {
		t.Errorf("re-signin left key %v, want sk-new", doc["zai"]["key"])
	}
	// The members of THAT entry which oaica does not model still survive — it
	// is the same file, the same rule.
	if got, _ := doc["zai"]["note"].(string); got != "keep me" {
		t.Errorf("replacing the entry deleted a member oaica does not model: %+v", doc["zai"])
	}

	// And the oauth refusal is not weakened by any of this.
	if err := os.WriteFile(store, []byte(`{"zai": {"type": "oauth", "access": "at", "refresh": "rt", "expires": 4102444800000}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := SaveOpencodeAPIKey("zai", "sk-replacement"); err == nil {
		t.Error("signin overwrote a live OAuth entry")
	}
}
