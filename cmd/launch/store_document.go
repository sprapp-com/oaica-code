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
		for i := 0; i < t.NumField(); i++ {
			f := t.Field(i)
			if f.PkgPath != "" { // unexported
				continue
			}
			name, opts := parseJSONTag(f)
			if name == "-" {
				continue
			}
			if name == "" {
				name = f.Name
			}
			if _, ok := opts["string"]; ok {
				s.fields[name] = &jsonSchema{shape: shapeOpaque}
				continue
			}
			s.fields[name] = schemaFor(f.Type)
		}
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
func mergeStoreDocument(had, now json.RawMessage, sch *jsonSchema) (json.RawMessage, bool) {
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
		if !ok || len(ha) != len(na) {
			return now, false
		}
		carried := false
		for i := range ha {
			if sch.elem.shape == shapeStruct && !sameEntry(ha[i], na[i]) {
				// Same length, but these two are not the same entry: a
				// reordered list must not hand one entry's members to another.
				continue
			}
			if m, c := mergeStoreDocument(ha[i], na[i], sch.elem); c {
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
