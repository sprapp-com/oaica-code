package launch

// store_document_embedded_integrity_test.go — a store struct that EMBEDS
// another struct panicked the merge, or lost members nested under it
// (2026-09-26 audit).
//
// The merge decides what may be re-attached by walking the caller's TYPE with
// reflection. A field is declared as one member named by its json tag, or by
// the field name. That is how encoding/json reads an ordinary field, and NOT
// how it reads an embedded one: an embedded struct has no key of its own — its
// members are PROMOTED into the object being marshalled. So a type like
//
//	type base struct{ Host string `json:"host"` }
//	type remote struct {
//	    base
//	    Name string `json:"name"`
//	}
//
// marshals to {"name":..., "host":...} while the schema declared a single
// member called "base". Every promoted key was therefore absent from the
// schema, and the struct case walked into it with a nil schema:
//
//	panic: runtime error: invalid memory address or nil pointer dereference
//
// A store type that embeds one is a natural thing to write — the shared
// connection fields of a remote, the shared identity fields of a provider —
// and the panic arrives on the first rewrite of a file that has any such key
// on disk. This is the helper's own contract, so it is pinned here on the
// helper, the way the rest of this file's rules are.

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type embeddedConn struct {
	BaseURL string `json:"base_url"`
	APIKey  string `json:"api_key,omitempty"`
}

type embeddedRemote struct {
	embeddedConn
	Name string `json:"name"`
}

// A promoted member is the caller's member: it is what the type marshals, so
// it is declared, and walking it must not panic on the way.
func TestAPromotedMemberIsDeclaredNotPanickedOn(t *testing.T) {
	path := filepath.Join(t.TempDir(), "remotes.json")
	seed := `{
  "remotes": [
    {"name": "box", "base_url": "http://old:8000", "api_key": "sk-old"}
  ]
}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	merged, err := storeDocumentMergeValue(map[string]any{
		"remotes": []embeddedRemote{{embeddedConn: embeddedConn{BaseURL: "http://new:8000"}, Name: "box"}},
	}, path)
	if err != nil {
		t.Fatalf("storeDocumentMergeValue: %v", err)
	}
	got := string(merged)

	if !strings.Contains(got, "http://new:8000") {
		t.Errorf("the value the caller wrote is not in the document:\n%s", got)
	}
	if strings.Contains(got, "http://old:8000") {
		t.Errorf("the caller's own value did not win over the one on disk:\n%s", got)
	}
	// api_key was cleared (omitempty) and the type declares it: it stays cleared.
	if strings.Contains(got, "sk-old") {
		t.Errorf("a promoted member the caller cleared came back:\n%s", got)
	}
}

// The rule under an embedded struct is the same rule as everywhere else: a
// member the type does not declare is carried, at every depth. Promotion has to
// be followed recursively for that to hold — the schema has to describe the
// promoted member's own shape, not just its name.
func TestAMemberUnderAPromotedStructIsStillCarried(t *testing.T) {
	type inner struct {
		Token string `json:"token"`
	}
	type base struct {
		Inner inner `json:"inner"`
	}
	type outer struct {
		base
		Name string `json:"name"`
	}

	path := filepath.Join(t.TempDir(), "outer.json")
	seed := `{"name":"a","inner":{"token":"t","refresh_token":"rt-keep"}}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	merged, err := storeDocumentMergeValue(outer{base: base{Inner: inner{Token: "t"}}, Name: "a"}, path)
	if err != nil {
		t.Fatalf("storeDocumentMergeValue: %v", err)
	}
	got := string(merged)
	if !strings.Contains(got, "rt-keep") {
		t.Errorf("a member nested under a promoted struct was dropped — the schema declared the embedded struct, but not what is inside it:\n%s", got)
	}
	if !strings.Contains(got, `"token"`) {
		t.Errorf("the promoted member itself was dropped:\n%s", got)
	}
}

// And an outer field of the same name shadows the promoted one, the way
// encoding/json resolves it: there is one "name" key in the marshalled object
// and it is the outer field's, so it is the outer field that decides.
func TestAnOuterFieldShadowsThePromotedOne(t *testing.T) {
	type base struct {
		Name string `json:"name"`
	}
	type outer struct {
		base
		Name string `json:"name"`
	}

	path := filepath.Join(t.TempDir(), "shadow.json")
	seed := `{"name":"on-disk"}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	merged, err := storeDocumentMergeValue(outer{Name: "written"}, path)
	if err != nil {
		t.Fatalf("storeDocumentMergeValue: %v", err)
	}
	got := string(merged)
	if !strings.Contains(got, "written") || strings.Contains(got, "on-disk") {
		t.Errorf("the outer field did not shadow the promoted one:\n%s", got)
	}
}

// Two embedded structs promoting the same name is the one case encoding/json
// resolves by emitting NO such key. Neither is the caller's member then, so the
// name has to stay a stranger's and be carried — declaring it would delete
// whatever the user hand-wrote under it.
//
// The pair is composed at run time rather than written as a struct literal
// because vet's structtag check flags exactly this shape in source, which is the
// right warning for a hand-written type and only noise for a test of it.
func TestANameTwoEmbeddedStructsBothPromoteIsCarried(t *testing.T) {
	type embLeft struct {
		Name string `json:"name"`
	}
	type embRight struct {
		Name string `json:"name"`
	}

	outer := reflect.StructOf([]reflect.StructField{
		{Name: "EmbLeft", Type: reflect.TypeOf(embLeft{}), Anonymous: true},
		{Name: "EmbRight", Type: reflect.TypeOf(embRight{}), Anonymous: true},
	})

	path := filepath.Join(t.TempDir(), "ambiguous.json")
	seed := `{"name":"hand-written"}`
	if err := os.WriteFile(path, []byte(seed), 0o600); err != nil {
		t.Fatal(err)
	}

	merged, err := storeDocumentMergeValue(reflect.New(outer).Elem().Interface(), path)
	if err != nil {
		t.Fatalf("storeDocumentMergeValue: %v", err)
	}
	got := string(merged)
	if !strings.Contains(got, "hand-written") {
		t.Errorf("a key encoding/json deliberately omits (two embedded structs promote it) was treated as the caller's and deleted:\n%s", got)
	}
}

// A nil schema is what an undeclared key looks like to the walk. It has to be
// terminal rather than fatal: the helper's doc promises a preservation step,
// and no document a user can write should be able to panic it.
func TestTheMergeNeverPanicsOnASchemaItDoesNotHave(t *testing.T) {
	for _, sch := range []*jsonSchema{nil, {shape: shapeStruct}, {shape: shapeMap}, {shape: shapeArray}} {
		had := []byte(`{"a":{"b":1},"l":[{"x":1}]}`)
		now := []byte(`{"a":{"b":2},"l":[{"x":2}]}`)
		mergeStoreDocument(had, now, sch) // must return, not panic
	}
}
