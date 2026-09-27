package api

// round50_schema_view_test.go — a schema value carries the bytes it arrived as
// (raw) so the model is shown the schema the caller wrote, and go-cmp cannot
// see an unexported field. A type that defines Equal(T) bool is compared
// through it instead of panicking, so comparing two of these values — anywhere,
// in any container — is the typed view this struct names, which is what a
// value built in code and the same value read off a wire have in common
// (2026-09-27 audit, round 50).

import (
	"encoding/json"
	"testing"

	"github.com/google/go-cmp/cmp"
)

// TestASchemaValueComparesThroughItsTypedView pins the comparison these types
// offer. Before round 50 go-cmp compared the typed fields directly; the bytes a
// schema now carries must not change that, and must not panic.
func TestASchemaValueComparesThroughItsTypedView(t *testing.T) {
	const schema = `{"type":"object","title":"Read","properties":{"path":{"type":"string","format":"uri"}},"required":["path"]}`

	var decoded ToolFunctionParameters
	if err := json.Unmarshal([]byte(schema), &decoded); err != nil {
		t.Fatalf("decode schema: %v", err)
	}
	if !json.Valid(decoded.SchemaJSON()) {
		t.Fatalf("the decoded schema carries no bytes: %q", decoded.SchemaJSON())
	}

	props := NewToolPropertiesMap()
	props.Set("path", ToolProperty{Type: PropertyType{"string"}})
	built := ToolFunctionParameters{Type: "object", Required: []string{"path"}, Properties: props}

	// The bytes are not part of the comparison: what the struct READS is equal.
	if diff := cmp.Diff(built, decoded); diff != "" {
		t.Errorf("a schema read off a wire and the same schema built in code compare as the fields this struct names; diff:\n%s", diff)
	}

	// The typed view is still the comparison: a field that differs is reported.
	other := built
	other.Type = "array"
	if diff := cmp.Diff(built, other); diff == "" {
		t.Error("two schemas stating different types compare equal")
	}

	// The same holds wherever the value sits — inside a plain map, which is
	// where the comparison used to panic on the bytes this type carries.
	var got map[string]ToolProperty
	if err := json.Unmarshal([]byte(`{"path":{"type":"string","format":"uri"}}`), &got); err != nil {
		t.Fatalf("decode properties: %v", err)
	}
	want := map[string]ToolProperty{"path": {Type: PropertyType{"string"}}}
	if diff := cmp.Diff(want, got); diff != "" {
		t.Errorf("a property read off a wire and the same property built in code compare as the fields this struct names; diff:\n%s", diff)
	}

	// And the bytes are what is marshalled when they exist.
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatalf("marshal schema: %v", err)
	}
	if string(encoded) != schema {
		t.Errorf("the schema marshalled as\n%s\nwant\n%s\n— the model is asked about the schema the caller wrote", encoded, schema)
	}
}

// TestAPropertyMarshalsBackAsItArrived is the same finding one level down: a
// template that renders a decoded tool's property map (templateProperties)
// marshals each property on its own, and a property with `format`, `pattern`,
// `default` or `title` on it states those keys. Rebuilding it from the five
// fields this struct names drops them, which is a tool signature the model is
// asked about and cannot see (2026-09-27 audit, round 50).
func TestAPropertyMarshalsBackAsItArrived(t *testing.T) {
	const props = `{"path":{"type":"string","format":"uri","pattern":"^/","default":"/tmp"},"ref":{"$ref":"#/$defs/loc"}}`

	var decoded ToolPropertiesMap
	if err := json.Unmarshal([]byte(props), &decoded); err != nil {
		t.Fatalf("decode properties: %v", err)
	}
	encoded, err := json.Marshal(&decoded)
	if err != nil {
		t.Fatalf("marshal properties: %v", err)
	}
	if string(encoded) != props {
		t.Errorf("the property map marshalled as\n%s\nwant\n%s\n— each property is stated as the bytes it arrived as", encoded, props)
	}

	// A property built in code has no bytes and states its typed fields, which
	// is what a renderer reads while building one.
	built, err := json.Marshal(ToolProperty{Type: PropertyType{"string"}})
	if err != nil {
		t.Fatalf("marshal property: %v", err)
	}
	if string(built) != `{"type":"string"}` {
		t.Errorf("a property built in code marshalled as %s, want {\"type\":\"string\"}", built)
	}
}
