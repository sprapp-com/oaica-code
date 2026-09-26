package launch

// store_document.go — the two properties every store writer in this package
// needs, kept in one place because getting them right per-writer is how three
// of them got them wrong (2026-09-26 audit).
//
// The stores (remotes.json, auth.json, the aliases file, plans.json,
// models.json) are documents a user also edits: they can carry a note, a
// label, or a field written by a newer version of the client. The Go structs
// that model them are PARTIAL views on purpose — the client only needs the
// members it acts on — so a writer that serialises the struct straight over
// the file deletes everything it does not model.
//
//   - storeDocumentMerge re-attaches those members, so a rewrite preserves what
//     it does not understand;
//   - storeDocumentChanged decides whether there is anything to write at all,
//     which is what keeps a command that changed nothing from touching the
//     file (and from dropping unmodelled members in the first place).

import (
	"bytes"
	"encoding/json"
	"os"
)

// storeDocumentMerge re-attaches every top-level member of the document at path
// that body (the freshly marshalled struct) does not carry. A member the
// struct does model is always the marshalled one: the client's own value wins
// over the one on disk, which is what the caller is writing for. Anything the
// struct does not know about is somebody else's and is put back verbatim.
//
// Every failure mode returns body unchanged: this is a preservation step, not a
// validation one, and refusing to write because the existing file is
// unreadable would turn a hand-broken store into a store the client can no
// longer operate on.
func storeDocumentMerge(body []byte, path string) []byte {
	original, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(original)) == 0 {
		return body
	}
	var had, now map[string]json.RawMessage
	if json.Unmarshal(original, &had) != nil || len(had) == 0 {
		return body
	}
	if json.Unmarshal(body, &now) != nil {
		return body
	}
	carried := false
	for k, raw := range had {
		if _, ok := now[k]; !ok {
			now[k] = raw
			carried = true
		}
	}
	if !carried {
		return body
	}
	merged, err := json.MarshalIndent(now, "", "  ")
	if err != nil {
		return body
	}
	return merged
}

// storeDocumentSnapshot marshals a store value for a later
// storeDocumentChanged comparison. It has to be the MARSHALLED bytes and not a
// copy of the struct: these types hold maps, so a struct copy shares them and
// a mutation through the copy is invisible to a later comparison — which would
// silently report "nothing changed" for the very writes the caller asked for.
func storeDocumentSnapshot(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	return b
}

// storeDocumentChanged reports whether the document differs from the snapshot.
// It is computed from the document rather than tracked at each mutation site
// because those sites overlap and a branch that writes without setting a flag
// leaves the change in memory, out of the file, and out of the caller's report
// — the same reasoning as ModelSync's "changed" return.
//
// Marshal sorts map keys, so this is a content comparison and not an ordering
// one. A marshal error is reported as "changed": an unreadable document is not
// a reason to skip the write the caller asked for.
func storeDocumentChanged(snapshot []byte, after any) bool {
	if snapshot == nil {
		return true
	}
	a, err := json.Marshal(after)
	if err != nil {
		return true
	}
	return !bytes.Equal(snapshot, a)
}
