package api

// round51_null_schema_test.go — round 51's finding on the schema decode, one
// level above round 50's.
//
// A tool that states `"input_schema":null` states no schema, exactly as a tool
// that states no schema key at all. The decode kept the bytes `null`, so the
// struct marshalled them back and the backend was handed `"parameters":null`
// for the very body the metered gateway leg serves as the empty schema
// (`{"type":"","properties":null}`) and the renderer reads as the zero value:
// one body, two upstream requests, and one of them a parameters value no
// function-calling validator accepts. The same held one level down, where a
// property written `null` came back as `null` instead of the empty property the
// sibling legs read it as.
//
// `null` is the ONE non-object a schema decode lets through: a value of another
// kind (`"x"`, `1`, `[]`) is still refused, because the sibling's typed decode
// refuses it too (2026-09-27 audit, round 51).

import (
	"encoding/json"
	"testing"
)

// TestANullSchemaIsASchemaNotStated pins both spellings to the same bytes. The
// pinned value is what the gateway leg puts on the wire for the same body and
// what the zero value marshals to.
func TestANullSchemaIsASchemaNotStated(t *testing.T) {
	const pinned = `{"type":"","properties":null}`

	var statedNull ToolFunctionParameters
	if err := json.Unmarshal([]byte(`null`), &statedNull); err != nil {
		t.Fatalf("`input_schema:null` means no schema stated, not an error: %v", err)
	}
	encoded, err := json.Marshal(statedNull)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != pinned {
		t.Errorf("a null schema marshalled as %s, want %s\nthe gateway leg serves this body with the empty schema; the bytes `null` reach a backend whose validator refuses the tool the client was told it could call", encoded, pinned)
	}

	var absent ToolFunctionParameters
	if encoded, err := json.Marshal(absent); err != nil || string(encoded) != string(mustMarshal(t, statedNull)) {
		t.Errorf("the two spellings of no schema disagree: null -> %s, absent -> %s", mustMarshal(t, statedNull), encoded)
	}

	// A non-object that IS a value is still refused: the sibling's typed decode
	// cannot hold it, and neither leg may serve a schema of another kind.
	for _, bad := range []string{`"x"`, `1`, `[]`, `true`} {
		var p ToolFunctionParameters
		if err := json.Unmarshal([]byte(bad), &p); err == nil {
			t.Errorf("a schema of %s decoded without error; a tool signature of another kind is one the model cannot be given", bad)
		}
	}
}

// TestANullPropertyIsTheEmptyProperty is the same rule where no raw bytes are
// kept: a property written `null` states no schema, and the typed view a
// renderer reads is the empty property `{}` — a schema that accepts anything —
// rather than a `null` it cannot read. A property inside a schema that WAS
// stated is not this case: that schema travels as the client wrote it, `null`
// and all (round 50), and both wire legs forward it verbatim.
func TestANullPropertyIsTheEmptyProperty(t *testing.T) {
	var props ToolPropertiesMap
	if err := json.Unmarshal([]byte(`{"cursor":null}`), &props); err != nil {
		t.Fatalf("decode properties: %v", err)
	}
	encoded, err := json.Marshal(&props)
	if err != nil {
		t.Fatalf("marshal properties: %v", err)
	}
	if string(encoded) != `{"cursor":{}}` {
		t.Errorf("a null property marshalled as %s, want {\"cursor\":{}}\none leg serves this schema with an empty property; `null` is a schema no validator accepts", encoded)
	}

	// A property that states something keeps stating it, byte for byte.
	var stated ToolPropertiesMap
	if err := json.Unmarshal([]byte(`{"path":{"type":"string","format":"uri"}}`), &stated); err != nil {
		t.Fatalf("decode properties: %v", err)
	}
	encoded, err = json.Marshal(&stated)
	if err != nil {
		t.Fatalf("marshal properties: %v", err)
	}
	if string(encoded) != `{"path":{"type":"string","format":"uri"}}` {
		t.Errorf("a stated property lost its bytes: %s", encoded)
	}
}

func mustMarshal(t *testing.T, v any) []byte {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return b
}
