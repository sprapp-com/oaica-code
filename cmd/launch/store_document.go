package launch

// store_document.go — the two properties every store writer in this package
// needs, kept in one place because getting them right per-writer is how three
// of them got them wrong (2026-09-26 audit).
//
// The stores (remotes.json, auth.json, the aliases file, plans.json,
// models.json, config.json) are documents a user also edits: they can carry a
// note, a label, or a field written by a newer version of the client. The Go
// structs that model them are PARTIAL views on purpose — the client only needs
// the members it acts on — so a writer that serialises the struct straight over
// the file deletes everything it does not model.
//
//   - storeDocumentMergeValue re-attaches those members, so a rewrite preserves
//     what it does not understand;
//   - storeDocumentChanged decides whether there is anything to write at all,
//     which is what keeps a command that changed nothing from touching the
//     file (and from dropping unmodelled members in the first place).
//
// What "does not understand" means is decided by the caller's TYPE, walked with
// reflection — not by the shape of the JSON. The two are not the same thing and
// guessing from shape got this wrong twice (2026-09-26 audit):
//
//   - A member absent from the marshalled struct is either somebody else's or
//     one the caller cleared. `oauth`'s refresh_token is a member no field
//     declares, and deleting it is data loss; a `sonnet_model` set to "" is a
//     member the type declares that the caller removed, and putting it back
//     makes `oaica config set sonnet ""` report success and do nothing. Only
//     the type can tell them apart.
//   - The same test one level down: an entry of the providers map is a struct
//     with declared members, while the map itself is keyed by names the caller
//     chooses. A key missing from a map or a slice-length change is the caller
//     deleting an entry, never a stranger's member.

import (
	"bytes"
	"encoding/json"
	"os"
	"reflect"
	"sync"
)

// jsonShape is how a Go type appears in JSON, as far as preservation is
// concerned.
type jsonShape uint8

const (
	shapeOpaque jsonShape = iota // scalars, and anything with its own marshaller
	shapeStruct                  // an object with declared members
	shapeMap                     // an object keyed by names the caller chooses
	shapeArray                   // a list whose entries the caller chooses
)

// jsonSchema is one type's JSON shape, resolved once per reflect.Type.
type jsonSchema struct {
	shape  jsonShape
	fields map[string]*jsonSchema // shapeStruct: JSON name → member
	elem   *jsonSchema            // shapeMap / shapeArray: value type
}

var jsonSchemaCache sync.Map // reflect.Type → *jsonSchema

// schemaFor describes t. A type that marshals itself (time.Time, and every
// type with a MarshalJSON) is opaque: oaica has no idea what members it
// produces or accepts, so nothing of it is re-attached.
func schemaFor(t reflect.Type) *jsonSchema {
	for t != nil && t.Kind() == reflect.Pointer {
		t = t.Elem()
	}
	if t == nil {
		return &jsonSchema{shape: shapeOpaque}
	}
	if cached, ok := jsonSchemaCache.Load(t); ok {
		return cached.(*jsonSchema)
	}
	// Built and cached before recursing so a self-referential type terminates.
	s := &jsonSchema{shape: shapeOpaque}
	jsonSchemaCache.Store(t, s)

	jsonMarshaler := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	if t.Implements(jsonMarshaler) || reflect.PointerTo(t).Implements(jsonMarshaler) {
		return s
	}

	switch t.Kind() {
	case reflect.Struct:
		s.shape = shapeStruct
		s.fields = map[string]*jsonSchema{}
		declareStructFields(s.fields, t, map[reflect.Type]bool{})
	case reflect.Map:
		s.shape = shapeMap
		s.elem = schemaFor(t.Elem())
	case reflect.Slice, reflect.Array:
		if t.Elem().Kind() == reflect.Uint8 { // a JSON string, not a list
			return s
		}
		s.shape = shapeArray
		s.elem = schemaFor(t.Elem())
	}
	return s
}

// declareStructFields puts t's members into dst: its own fields first, then the
// members the structs it EMBEDS promote.
//
// That order is not cosmetic. An embedded struct has no key of its own — its
// members are promoted into the object being marshalled, so the schema has to
// describe them as members of the outer struct or every promoted key looks
// undeclared to the merge (which walked into it with a nil schema and panicked,
// 2026-09-26 audit). And an outer field of the same name shadows the promoted
// one, because that is what encoding/json does: there is one such key in the
// marshalled object, and the outer field is what produced it.
//
// dst answers exactly one question about a key: does the type being written
// declare it, or does it belong to whoever else put it there.
func declareStructFields(dst map[string]*jsonSchema, t reflect.Type, seen map[reflect.Type]bool) {
	if seen[t] {
		return
	}
	seen[t] = true

	// Embedded structs are resolved after the direct fields, and only into the
	// names still free. Two of them promoting the SAME name at the same depth is
	// the one case encoding/json resolves by dropping both from the document —
	// so neither is the caller's, and the name goes back to being a stranger's,
	// which is the direction that keeps a hand-written member (see `dead`).
	var promoted []reflect.Type
	dead := map[string]bool{}

	for i := 0; i < t.NumField(); i++ {
		f := t.Field(i)
		name, opts := parseJSONTag(f)

		if f.Anonymous {
			// Only a struct reaches through: an embedded field that is not a
			// struct is an ordinary member named by its type, and a json tag on
			// an embedded field names the field itself rather than promoting it.
			if f.Type.Kind() == reflect.Struct && name == "" && !marshalsItself(f.Type) {
				promoted = append(promoted, f.Type)
				continue
			}
			// An embedded POINTER to a struct is deliberately not followed: a
			// nil one marshals to nothing, so its members are absent from the
			// document by the caller's own choice, and declaring them would make
			// the merge delete whatever is on disk under those names.
			if f.PkgPath != "" && f.Type.Kind() != reflect.Struct {
				continue
			}
		} else if f.PkgPath != "" { // unexported
			continue
		}

		if name == "-" {
			continue
		}
		if name == "" {
			name = f.Name
		}
		if _, ok := opts["string"]; ok {
			dst[name] = &jsonSchema{shape: shapeOpaque}
			continue
		}
		dst[name] = schemaFor(f.Type)
	}

	from := map[string]reflect.Type{}
	for _, et := range promoted {
		inner := schemaFor(et)
		if inner.shape != shapeStruct {
			continue
		}
		for name, fields := range inner.fields {
			if dead[name] {
				continue
			}
			src, wasPromoted := from[name]
			if wasPromoted && src != et {
				// Same depth, two sources: encoding/json emits no such key at
				// all, so it is nobody's member and gets carried.
				delete(dst, name)
				dead[name] = true
				continue
			}
			if _, taken := dst[name]; !taken {
				dst[name] = fields
				from[name] = et
			}
		}
	}
}

// marshalsItself reports whether t produces its own JSON — time.Time, and every
// type with a MarshalJSON. Nothing of such a type is described or re-attached.
func marshalsItself(t reflect.Type) bool {
	jm := reflect.TypeOf((*json.Marshaler)(nil)).Elem()
	return t.Implements(jm) || reflect.PointerTo(t).Implements(jm)
}

func parseJSONTag(f reflect.StructField) (string, map[string]bool) {
	tag := f.Tag.Get("json")
	if tag == "" {
		return "", nil
	}
	parts := bytes.Split([]byte(tag), []byte(","))
	opts := map[string]bool{}
	for _, o := range parts[1:] {
		opts[string(bytes.TrimSpace(o))] = true
	}
	return string(parts[0]), opts
}

// storeDocumentMergeValue marshals v and re-attaches, from the document at
// path, every member v's own type does not declare. A member the type does
// declare is always the marshalled one: the caller's value wins over the one on
// disk, which is what it is writing for — including when it cleared the member,
// which is why the type has to be known rather than guessed.
//
// Every preservation failure returns the plain marshalled value: this is a
// preservation step, not a validation one, and refusing to write because the
// existing file is unreadable would turn a hand-broken store into a store the
// client can no longer operate on.
func storeDocumentMergeValue(v any, path string) ([]byte, error) {
	body, err := json.MarshalIndent(v, "", "  ")
	if err != nil {
		return nil, err
	}
	original, err := os.ReadFile(path)
	if err != nil || len(bytes.TrimSpace(original)) == 0 {
		return body, nil
	}
	if !json.Valid(original) {
		return body, nil
	}
	merged, carried := mergeStoreDocument(original, body, schemaFor(reflect.TypeOf(v)))
	if !carried || !json.Valid(merged) {
		return body, nil
	}
	var out bytes.Buffer
	if err := json.Indent(&out, merged, "", "  "); err != nil {
		return body, nil
	}
	return out.Bytes(), nil
}

// mergeStoreDocument returns now with the members of had that the schema does
// not declare re-attached, and reports whether it re-attached anything.
//
// A map's keys and an array's entries belong to the caller (it enumerates them,
// so one that is gone was deleted on purpose — `auth logout`, `alias rm`,
// `model rm`); a struct's undeclared members belong to whoever wrote them and
// are put back verbatim, at every depth.
// A nil schema means "this position has no shape we know": the caller's value
// stands and nothing under it is carried. It is terminal rather than fatal
// because this is a preservation step — a document a user wrote must never be
// able to panic a rewrite of it, and the walk reaches a nil schema for any key
// the caller's type does not declare.
func mergeStoreDocument(had, now json.RawMessage, sch *jsonSchema) (json.RawMessage, bool) {
	if sch == nil {
		return now, false
	}
	switch sch.shape {
	case shapeStruct:
		ho, no, ok := asJSONObjects(had, now)
		if !ok {
			return now, false
		}
		carried := false
		for k, hv := range ho {
			nv, exists := no[k]
			if !exists {
				if _, declared := sch.fields[k]; declared {
					continue // the caller's member, cleared by the caller
				}
				no[k] = hv
				carried = true
				continue
			}
			if m, c := mergeStoreDocument(hv, nv, sch.fields[k]); c {
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

	case shapeMap:
		ho, no, ok := asJSONObjects(had, now)
		if !ok {
			return now, false
		}
		carried := false
		for k, hv := range ho {
			nv, exists := no[k]
			if !exists {
				continue // a key the caller removed
			}
			if m, c := mergeStoreDocument(hv, nv, sch.elem); c {
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

	case shapeArray:
		ha, na, ok := asJSONArrays(had, now)
		if !ok {
			return now, false
		}
		carried := false
		// A list has no key to delete by, so an entry is matched to the old
		// entry it IS (sameEntry: both objects sharing a scalar member) rather
		// than to whatever sits at the same index. That is what lets a reorder
		// keep each entry's own members, and it is what the length check used
		// to stand in for: the old code bailed out of the whole ARRAY whenever
		// the length changed, so adding or removing one entry switched off
		// preservation for every entry it kept — `remote add <new>` deleted the
		// hand-added note on <old>. The added entry matches nothing (it is new)
		// and the removed one is simply not in the new list (nothing re-attaches
		// it), which is the same contract as before, one entry at a time.
		used := make([]bool, len(ha))
		for i := range na {
			j, ok := matchArrayEntry(ha, i, na[i], used, sch.elem)
			if !ok {
				continue
			}
			used[j] = true
			if m, c := mergeStoreDocument(ha[j], na[i], sch.elem); c {
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

// matchArrayEntry finds the old entry a new entry is: the same index when that
// is a match, else the single unused old entry sharing the most with it.
//
// Position is checked first because it is the cheapest evidence and the common
// case (a list rewritten in place). Otherwise the entry with the highest count
// of shared identifying members wins, and only if that count is unique — two
// candidates with the same score are two entries that cannot be told apart, and
// merging one of them by guess would hand an entry's members to a stranger.
// Declining costs only the carried members, which is the safe direction.
func matchArrayEntry(had []json.RawMessage, i int, now json.RawMessage, used []bool, elem *jsonSchema) (int, bool) {
	if elem.shape != shapeStruct {
		return 0, false // nothing to align by: a list of scalars has no members
	}
	if i < len(had) && !used[i] && sameEntry(had[i], now) {
		return i, true
	}
	best, bestScore, ties := -1, 0, 0
	for j := range had {
		if used[j] {
			continue
		}
		score := sharedIdentityScore(had[j], now)
		switch {
		case score == 0:
		case score > bestScore:
			best, bestScore, ties = j, score, 1
		case score == bestScore:
			ties++
		}
	}
	if best >= 0 && ties == 1 {
		return best, true
	}
	return 0, false
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

// sameEntry reports whether two array elements are the same entry: both
// objects, sharing at least one IDENTIFYING scalar member with the same value
// (a name, an id, a URL). Without one the pair is not evidence of an
// alignment, and merging by index alone would attach one entry's unknown
// members to a different entry.
//
// Identity has to be a value that can identify, which rules out the empty
// string: every remote in remotes.json carries `"api_key": ""` and
// `"version": ""` when unset, so counting those as a shared scalar made every
// entry in the file "the same entry" as every other — which is exactly how a
// list of two remotes, reordered, had one entry's hand-added note re-attached
// to the other (2026-09-26 audit).
func sameEntry(a, b json.RawMessage) bool {
	return sharedIdentityScore(a, b) > 0
}

// sharedIdentityScore counts the scalar members two objects share with equal,
// non-empty values — the strength of the evidence that they are the same
// entry. A higher score is a better match: two entries sharing a name and a
// base URL are unmistakable, two sharing only a vendor field are a guess.
func sharedIdentityScore(a, b json.RawMessage) int {
	var ao, bo map[string]json.RawMessage
	if json.Unmarshal(a, &ao) != nil || json.Unmarshal(b, &bo) != nil || ao == nil || bo == nil {
		return 0
	}
	n := 0
	for k, av := range ao {
		bv, ok := bo[k]
		if !ok {
			continue
		}
		at, bt := bytes.TrimSpace(av), bytes.TrimSpace(bv)
		if identifyingScalar(at) && bytes.Equal(at, bt) {
			n++
		}
	}
	return n
}

// identifyingScalar reports whether a JSON scalar can say WHICH entry this is:
// a non-empty string, or a number/bool. An empty string is what an unset field
// marshals to, so it is shared by every entry that has not set it and cannot
// distinguish anything; null is not a scalar here (scalarJSON rejects it).
func identifyingScalar(raw []byte) bool {
	if !scalarJSON(raw) || len(raw) < 2 {
		return len(raw) == 1 && raw[0] != '"' // a bare digit or letter
	}
	if raw[0] == '"' {
		return len(raw) > 2 // "" is not an identity
	}
	return true
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
