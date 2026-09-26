package launch

// store_document_entry_merge_integrity_test.go — storeDocumentMerge put back
// only the TOP-LEVEL members a rewrite did not carry (2026-09-26 audit).
//
// The stores are documents a user edits and a newer client may extend, and the
// structs are partial views of them. The top level was protected; the entries
// inside were not. So a hand-added note on one remote, a `sidecar` key on one
// model, or a `refresh_token` on one provider was deleted the next time oaica
// rewrote that file — the outer container survived, the contents did not.
//
// Every test here drives the real writer (updateAuthStore, updateUserRemotesFile,
// ModelAdd) over a seeded file and then reads the bytes on disk. The two
// deletion tests belong to the same rule: a merge that re-attaches unknown
// members must not re-attach an entry the caller removed on purpose.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// notModelledByAnyStruct is a member no struct in this package has a field for,
// so it can only survive by being carried verbatim.
const entryMarker = "hand written, not modelled"

func readStoreForAssert(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}
	return string(b)
}

func TestASavedAuthEntryKeepsMembersTheStructDoesNotModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("OAICA_AUTH_FILE", path)

	seed := `{
  "version": 1,
  "providers": {
    "zai": {"type": "api_key", "key": "sk-old", "refresh_token": "rt-1", "note": "` + entryMarker + `"}
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := updateAuthStore(func(f *authStoreFile) error {
		c := f.Providers["zai"]
		c.Type = "api_key"
		c.Key = "sk-new"
		f.Providers["zai"] = c
		return nil
	}); err != nil {
		t.Fatalf("updateAuthStore: %v", err)
	}

	got := readStoreForAssert(t, path)
	if !strings.Contains(got, "sk-new") {
		t.Errorf("the credential the caller wrote is not in the file:\n%s", got)
	}
	if strings.Contains(got, "sk-old") {
		t.Errorf("the caller's own value did not win over the one on disk:\n%s", got)
	}
	for _, want := range []string{"refresh_token", "rt-1", entryMarker} {
		if !strings.Contains(got, want) {
			t.Errorf("%q was in the provider entry before oaica rewrote it and is not after — the top level survived the merge but the entry inside it did not:\n%s", want, got)
		}
	}
}

// The same rule's other half: a provider the user logged out of is gone, not
// re-attached as an "unknown member" of the providers map.
func TestLoggingOutOfAProviderDoesNotResurrectIt(t *testing.T) {
	path := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("OAICA_AUTH_FILE", path)

	seed := `{
  "version": 1,
  "providers": {
    "zai": {"type": "api_key", "key": "sk-zai", "refresh_token": "rt-zai"},
    "deepseek": {"type": "api_key", "key": "sk-deep"}
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := updateAuthStore(func(f *authStoreFile) error {
		delete(f.Providers, "zai")
		return nil
	}); err != nil {
		t.Fatalf("updateAuthStore: %v", err)
	}

	got := readStoreForAssert(t, path)
	if strings.Contains(got, "rt-zai") || strings.Contains(got, "sk-zai") {
		t.Errorf("the provider that was logged out is still in the file:\n%s", got)
	}
	if !strings.Contains(got, "sk-deep") {
		t.Errorf("logging one provider out removed another:\n%s", got)
	}
}

func TestASavedRemoteKeepsMembersTheStructDoesNotModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", path)

	seed := `{
  "remotes": [
    {"name": "box", "base_url": "http://box:8000", "api_key": "", "note": "` + entryMarker + `"}
  ]
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := updateUserRemotesFile(func(f *userRemotesFile) error {
		if len(f.Remotes) != 1 {
			t.Fatalf("fixture did not load: %d remotes", len(f.Remotes))
		}
		f.Remotes[0].APIKey = "sk-box"
		return nil
	}); err != nil {
		t.Fatalf("updateUserRemotesFile: %v", err)
	}

	got := readStoreForAssert(t, path)
	if !strings.Contains(got, "sk-box") {
		t.Errorf("the value the caller wrote is not in the file:\n%s", got)
	}
	if !strings.Contains(got, entryMarker) {
		t.Errorf("the note on the remote entry was deleted by a rewrite that only set its api_key:\n%s", got)
	}
}

func TestARemovedRemoteIsNotReAttached(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", path)

	seed := `{
  "remotes": [
    {"name": "keep", "base_url": "http://keep:8000", "api_key": ""},
    {"name": "gone", "base_url": "http://gone:8000", "api_key": "", "note": "gone-marker"}
  ]
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := updateUserRemotesFile(func(f *userRemotesFile) error {
		kept := make([]userRemote, 0, len(f.Remotes))
		for _, r := range f.Remotes {
			if r.Name != "gone" {
				kept = append(kept, r)
			}
		}
		f.Remotes = kept
		return nil
	}); err != nil {
		t.Fatalf("updateUserRemotesFile: %v", err)
	}

	got := readStoreForAssert(t, path)
	if strings.Contains(got, "gone-marker") {
		t.Errorf("a remote the caller removed came back as an unmodelled member:\n%s", got)
	}
	if !strings.Contains(got, "http://keep:8000") {
		t.Errorf("the remote that was kept is gone:\n%s", got)
	}
}

func TestASavedModelKeepsMembersTheStructDoesNotModel(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.json")
	t.Setenv("OAICA_MODELS_FILE", path)

	seed := `{
  "version": 1,
  "models": {
    "existing": {"id": "existing", "engine": "vllm", "model_path": "/m/existing", "hand_note": "` + entryMarker + `"}
  }
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := ModelAdd(ModelAddOptions{ID: "added", Engine: "vllm", ModelPath: "/m/added"}); err != nil {
		t.Fatalf("ModelAdd: %v", err)
	}

	got := readStoreForAssert(t, path)
	if !strings.Contains(got, `"added"`) {
		t.Errorf("the model that was added is not in the file:\n%s", got)
	}
	if !strings.Contains(got, entryMarker) {
		t.Errorf("the hand-written member on the pre-existing entry was deleted by `oaica model add`:\n%s", got)
	}
}

// The rule the recursion is built on, stated directly: a member of the document
// is a collection and its keys belong to the caller — so a key that is gone is a
// deletion, whether its values are entries (providers) or plain strings
// (aliases, which is how `alias rm` broke when the rule was guessed from
// shape). A member of a collection is an entry, and an unknown member of an
// entry is re-attached.
func TestStoreDocumentMergeTellsCollectionsFromEntries(t *testing.T) {
	dir := t.TempDir()

	collection := filepath.Join(dir, "collection.json")
	if err := os.WriteFile(collection, []byte(`{"providers":{"a":{"type":"api_key","extra":1},"b":{"type":"api_key"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got := string(storeDocumentMerge([]byte(`{"providers":{"a":{"type":"api_key"},"version":1}}`), collection))
	if strings.Contains(got, `"b"`) {
		t.Errorf("an entry removed from a collection came back:\n%s", got)
	}
	if !strings.Contains(got, `"extra"`) {
		t.Errorf("an unmodelled member of an entry that is still there was dropped:\n%s", got)
	}
	if !strings.Contains(got, `"version"`) {
		t.Errorf("an unmodelled top-level member was dropped:\n%s", got)
	}

	// A map of names to scalars is a collection too, and a name the caller
	// removed must stay removed.
	aliases := filepath.Join(dir, "aliases.json")
	if err := os.WriteFile(aliases, []byte(`{"aliases":{"kat":"kat-awq","zai":"glm-5.3"},"version":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	got = string(storeDocumentMerge([]byte(`{"aliases":{"kat":"kat-awq"},"version":1}`), aliases))
	if strings.Contains(got, `"glm-5.3"`) {
		t.Errorf("an alias the caller removed came back:\n%s", got)
	}
}
