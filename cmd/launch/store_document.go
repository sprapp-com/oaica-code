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

// storeDocumentMerge re-attaches the members of the document at path that body
// (the freshly marshalled struct) does not carry — at the top level of the
// document and inside its entries. A member the struct does model is always the
// marshalled one: the client's own value wins over the one on disk, which is
// what the caller is writing for. Anything the struct does not know about is
// somebody else's and is put back verbatim.
//
// "Inside its entries" is the part that was missing (2026-09-26 audit): a
// hand-written note on one remote, a key on one model, a refresh token on one
// provider lived one level below the members being preserved, so the container
// survived the rewrite and its contents did not. See mergeStoreDocument for how
// a collection of entries is told apart from a value the caller rewrote.
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
	if !json.Valid(original) || !json.Valid(body) {
		return body
	}
	merged, carried := mergeStoreDocument(original, body, true)
	if !carried || !json.Valid(merged) {
		return body
	}
	var out bytes.Buffer
	if err := json.Indent(&out, merged, "", "  "); err != nil {
		return body
	}
	return out.Bytes()
}

// mergeStoreDocument returns now with the members of had that now does not carry
// re-attached, and reports whether it re-attached anything.
//
// allowMissing says what a member absent from now means at this level:
//
//   - true — somebody else wrote it and the caller rewrote a partial view of
//     the same value, so it is re-attached;
//   - false — the caller enumerates this container's membership, so a missing
//     key is a deletion. The providers map and the remotes array are the
//     caller's; re-attaching a key the caller dropped is how `auth logout`
//     would silently do nothing.
//
// The two are told apart by where the value sits, not by its shape: a direct
// member of the document is a collection (the providers map, the models map,
// the aliases map, the remotes array) and its keys are the caller's — a key it
// dropped was deleted on purpose. A member of a collection is an entry, and an
// entry is a value the caller rewrote from a partial view, so the members it
// does not carry are somebody else's and are re-attached. Shape cannot make
// this distinction: a map of names to strings (aliases) and an entry object
// keyed by schema are the same JSON, and treating that map as an entry made
// `oaica alias rm` report success and leave the alias in the file.
//
// Arrays are the collection case too: element-by-element merge only when the
// two arrays are the same length, so an entry the caller added or removed
// disables the merge instead of pairing up the wrong entries — and each pair
// must agree on some scalar member before it is treated as the same entry, so a
// reordered array cannot hand one entry's members to another.
func mergeStoreDocument(had, now json.RawMessage, allowMissing bool) (json.RawMessage, bool) {
	if ho, no, ok := asJSONObjects(had, now); ok {
		carried := false
		for k, hv := range ho {
			nv, exists := no[k]
			if !exists {
				if !allowMissing {
					continue
				}
				no[k] = hv
				carried = true
				continue
			}
			// This level's kind decides the child's: a member of the document
			// is a collection (its keys are the caller's), a member of a
			// collection is an entry (re-attach what it does not carry).
			if m, c := mergeStoreDocument(hv, nv, !allowMissing); c {
				no[k] = m
				carried = true
			}
		}
		if !carried {
			return now, false
		}
		out, err := json.Marshal(no)
		if err != nil {
			return now, false
		}
		return out, true
	}

	if ha, na, ok := asJSONArrays(had, now); ok {
		if len(ha) != len(na) {
			return now, false
		}
		carried := false
		for i := range ha {
			if !sameEntry(ha[i], na[i]) {
				continue
			}
			// An element is an entry: members it does not carry are unknown to
			// the struct, not deletions (the caller deletes whole entries).
			if m, c := mergeStoreDocument(ha[i], na[i], true); c {
				na[i] = m
				carried = true
			}
		}
		if !carried {
			return now, false
		}
		out, err := json.Marshal(na)
		if err != nil {
			return now, false
		}
		return out, true
	}

	return now, false
}

func asJSONObjects(had, now json.RawMessage) (map[string]json.RawMessage, map[string]json.RawMessage, bool) {
	var ho, no map[string]json.RawMessage
	if json.Unmarshal(had, &ho) != nil || json.Unmarshal(now, &no) != nil {
		return nil, nil, false
	}
	if ho == nil || no == nil { // null or a non-object decodes to nil without error
		return nil, nil, false
	}
	return ho, no, true
}

func asJSONArrays(had, now json.RawMessage) ([]json.RawMessage, []json.RawMessage, bool) {
	var ha, na []json.RawMessage
	if json.Unmarshal(had, &ha) != nil || json.Unmarshal(now, &na) != nil {
		return nil, nil, false
	}
	if ha == nil || na == nil {
		return nil, nil, false
	}
	return ha, na, true
}

// sameEntry reports whether two array elements are the same entry: both objects,
// sharing at least one scalar member with the same value (a name, an id, a URL).
// Without a shared scalar the pair is not evidence of an alignment, and merging
// by index alone would attach one entry's unknown members to a different entry.
func sameEntry(a, b json.RawMessage) bool {
	var ao, bo map[string]json.RawMessage
	if json.Unmarshal(a, &ao) != nil || json.Unmarshal(b, &bo) != nil || ao == nil || bo == nil {
		return false
	}
	for k, av := range ao {
		bv, ok := bo[k]
		if !ok {
			continue
		}
		at, bt := bytes.TrimSpace(av), bytes.TrimSpace(bv)
		if scalarJSON(at) && scalarJSON(bt) && bytes.Equal(at, bt) {
			return true
		}
	}
	return false
}

func scalarJSON(raw []byte) bool {
	if len(raw) == 0 {
		return false
	}
	switch raw[0] {
	case '{', '[', 'n': // object, array, null
		return false
	}
	return true
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
