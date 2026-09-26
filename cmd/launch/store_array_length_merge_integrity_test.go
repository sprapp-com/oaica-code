package launch

// store_array_length_merge_integrity_test.go — adding or removing one entry
// switched off member preservation for every OTHER entry in the same list
// (2026-09-26 audit).
//
// The array branch decided "the caller deleted an entry" once per ARRAY: any
// length difference and it returned the marshalled value untouched. That is
// right for the entry that came or went, and wrong for its neighbours — the
// two most common writes in this file (`remote add <new>`, `remote rm <gone>`)
// silently dropped every unmodelled member of every entry they kept. The
// struct's own header states the opposite guarantee: a hand-added note on one
// remote must survive the next rewrite of the file.
//
// Alignment has to be per entry and by identity, which is what sameEntry
// already provides: entries that are still there keep their members, the added
// one is new, the removed one stays removed.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const arrayKeptMarker = "kept-entry-note-marker"

func seededRemotes(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", path)
	seed := `{
  "remotes": [
    {"name": "box", "base_url": "http://box:8000", "api_key": "", "note": "` + arrayKeptMarker + `"}
  ]
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

// Adding a second remote must not strip the first one's unknown members.
func TestAddingAnEntryKeepsTheOtherEntriesMembers(t *testing.T) {
	path := seededRemotes(t)

	if _, err := RemoteAdd(RemoteAddOptions{Name: "newbox", BaseURL: "https://new.example.com"}); err != nil {
		t.Fatal(err)
	}

	got := readStoreForAssert(t, path)
	if !strings.Contains(got, "newbox") {
		t.Fatalf("the new remote was not written:\n%s", got)
	}
	if !strings.Contains(got, arrayKeptMarker) {
		t.Errorf("the note on the untouched entry was deleted by adding a DIFFERENT entry:\n%s", got)
	}
}

// Removing one remote must not strip the members of the one that stays.
func TestRemovingAnEntryKeepsTheOtherEntriesMembers(t *testing.T) {
	path := seededRemotes(t)

	if _, err := RemoteAdd(RemoteAddOptions{Name: "gone", BaseURL: "https://gone.example.com"}); err != nil {
		t.Fatal(err)
	}
	if _, err := RemoteRemove("gone"); err != nil {
		t.Fatal(err)
	}

	got := readStoreForAssert(t, path)
	if strings.Contains(got, "gone.example.com") {
		t.Errorf("the removed remote is still in the file:\n%s", got)
	}
	if !strings.Contains(got, arrayKeptMarker) {
		t.Errorf("the note on the surviving entry was deleted by removing a DIFFERENT entry:\n%s", got)
	}
}

// A reordered list whose entries are all present must still align by identity
// rather than by position, and reordering alone must not lose members either.
func TestReorderedEntriesKeepTheirOwnMembers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.json")
	t.Setenv("OAICA_REMOTES_FILE", path)
	seed := `{
  "remotes": [
    {"name": "a", "base_url": "http://a:8000", "api_key": "", "note": "note-for-a"},
    {"name": "b", "base_url": "http://b:8000", "api_key": "", "note": "note-for-b"}
  ]
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := updateUserRemotesFile(func(f *userRemotesFile) error {
		if len(f.Remotes) != 2 {
			t.Fatalf("fixture did not load: %d remotes", len(f.Remotes))
		}
		f.Remotes[0], f.Remotes[1] = f.Remotes[1], f.Remotes[0]
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	got := readStoreForAssert(t, path)
	var doc struct {
		Remotes []map[string]any `json:"remotes"`
	}
	if err := json.Unmarshal([]byte(got), &doc); err != nil {
		t.Fatalf("the rewritten file is not JSON: %v\n%s", err, got)
	}
	if len(doc.Remotes) != 2 {
		t.Fatalf("reorder changed the number of entries: %d\n%s", len(doc.Remotes), got)
	}
	// Each note has to be ON ITS OWN ENTRY. A substring check cannot tell a
	// carried member from a swapped one — an earlier version of this test
	// passed while entry a held b's note, which is the failure alignment by
	// position produces and the reason identity matching exists.
	want := map[string]string{"a": "note-for-a", "b": "note-for-b"}
	for _, r := range doc.Remotes {
		name, _ := r["name"].(string)
		note, _ := r["note"].(string)
		if want[name] == "" {
			t.Errorf("unexpected entry %q in %s", name, got)
			continue
		}
		if note != want[name] {
			t.Errorf("entry %q carries note %q, want %q — one entry's members were handed to another:\n%s", name, note, want[name], got)
		}
	}
}
