package anthropic

// round50_tool_schema_fidelity_test.go — round 50's finding on the local leg:
// a tool's JSON Schema did not survive the trip.
//
// The schema was decoded into api.ToolFunctionParameters — a struct naming five
// keys — and re-marshalled from it, so everything a client actually writes went
// missing: a top-level `$ref` became `{}`, and `format`, `pattern`,
// `additionalProperties`, `title`, `minLength` and `default` were dropped from
// every property. The metered gateway leg forwards the tool definition as
// written, so one body became two different prompts depending on which leg
// served it, and the model was asked to call a tool whose signature had been
// rewritten by the translator. A schema this struct cannot model (a `required`
// holding a number) refused the WHOLE request, which no other leg does.
//
// The two tests below pin both directions: what the client stated is what the
// model gets, and the typed view a renderer reads is still filled.

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ollama/ollama/api"
)

// round50ConvertTool runs one tool definition through the leg-1 conversion and
// returns the parameters as they were marshalled for the backend, beside the
// typed view a renderer reads.
func round50ConvertTool(t *testing.T, inputSchema string) (string, map[string]any) {
	t.Helper()
	var req MessagesRequest
	body := `{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[{"name":"Read","input_schema":` + inputSchema + `}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion refused a schema the other legs forward: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("the backend was handed %d tool(s), want 1", len(out.Tools))
	}
	encoded, err := json.Marshal(out.Tools[0].Function.Parameters)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(encoded, &got); err != nil {
		t.Fatalf("the marshalled parameters are not an object: %s", encoded)
	}
	return string(encoded), got
}

// TestAToolSchemaTravelsAsTheClientStatedIt is the finding. Every key the
// client wrote reaches the backend, and the typed view a renderer reads is
// still filled from the same bytes.
func TestAToolSchemaTravelsAsTheClientStatedIt(t *testing.T) {
	const schema = `{` +
		`"$defs":{"loc":{"type":"object","properties":{"path":{"type":"string"}}}},` +
		`"type":"object",` +
		`"title":"Read",` +
		`"additionalProperties":false,` +
		`"properties":{` +
		`"path":{"type":"string","format":"uri","pattern":"^/","minLength":1,"default":"/tmp"},` +
		`"ref":{"$ref":"#/$defs/loc"},` +
		`"mode":{"type":["string","null"],"enum":["a","b"]}},` +
		`"required":["path"]}`

	encoded, got := round50ConvertTool(t, schema)

	var want map[string]any
	if err := json.Unmarshal([]byte(schema), &want); err != nil {
		t.Fatalf("probe schema: %v", err)
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("the backend was handed\n%s\nwant\n%s\n— the schema was rebuilt from a struct that names five keys, so the caller's own keys were dropped", encoded, schema)
	}

	// The typed view is still there for the renderers that read it.
	var req MessagesRequest
	body := `{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[{"name":"Read","input_schema":` + schema + `}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	params := out.Tools[0].Function.Parameters
	if params.Type != "object" || len(params.Required) != 1 || params.Required[0] != "path" {
		t.Errorf("the typed view is empty: type %q required %v", params.Type, params.Required)
	}
	if params.Properties == nil || params.Properties.Len() != 3 {
		t.Fatalf("the typed view holds %v properties, want 3", params.Properties)
	}
	if p, ok := params.Properties.Get("path"); !ok || len(p.Type) == 0 || p.Type[0] != "string" {
		t.Errorf("properties.path decoded to %+v, want a string property", p)
	}
	if p, ok := params.Properties.Get("ref"); !ok || len(p.Type) != 0 {
		t.Errorf("properties.ref decoded to %+v, want the property it names nothing for", p)
	}
}

// TestASchemaThisStructCannotModelIsForwardedNotRefused is the second half: a
// schema whose inner shapes the typed decode does not accept is a schema the
// CALLER stated, and the model is asked about it as written. Refusing the whole
// request refused the turn over a tool definition both other legs forward.
func TestASchemaThisStructCannotModelIsForwardedNotRefused(t *testing.T) {
	for _, schema := range []string{
		`{"type":"object","required":[1],"properties":{"a":"x"}}`,
		`{"type":"object","required":"path"}`,
		`{"type":"object","properties":{"a":{"type":7}}}`,
	} {
		encoded, got := round50ConvertTool(t, schema)
		var want map[string]any
		if err := json.Unmarshal([]byte(schema), &want); err != nil {
			t.Fatalf("probe schema: %v", err)
		}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("the backend was handed\n%s\nwant\n%s", encoded, schema)
		}
	}

	// A schema that is not an object at all is still refused: that is the
	// verdict both other legs give it.
	for _, bad := range []string{`"nope"`, `[1,2]`, `7`} {
		var badReq MessagesRequest
		if err := json.Unmarshal([]byte(`{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[{"name":"Read","input_schema":`+bad+`}]}`), &badReq); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}
		if _, err := FromMessagesRequest(badReq); err == nil {
			t.Errorf("input_schema %s was accepted; a schema that is not an object is refused on every leg", bad)
		}
	}
}

// TestASchemaBuiltInCodeStillMarshalsItsTypedFields keeps the two directions
// apart: a zero value (a tool that states no schema) still puts round 49's
// `{"type":"","properties":null}` on the wire, and a value built in code — the
// built-in web_search — states the fields it was built with.
func TestASchemaBuiltInCodeStillMarshalsItsTypedFields(t *testing.T) {
	encoded, err := json.Marshal(api.ToolFunctionParameters{})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if string(encoded) != `{"type":"","properties":null}` {
		t.Errorf("the zero parameters are %s, want {\"type\":\"\",\"properties\":null}", encoded)
	}
}
