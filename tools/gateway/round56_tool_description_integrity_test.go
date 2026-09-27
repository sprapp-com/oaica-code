package main

// round56_tool_description_integrity_test.go — round 56, the cross-leg tool
// surface (F56-3): a description the client did not state.
//
// Legs 1 and 2 decode the body into the converter's typed tools, where a
// function's description is `string` with omitempty (api.ToolFunction), so an
// absent, null or empty description never reaches the upstream wire. This leg
// reads tools out of a map and forwarded whatever it found: `"description":null`
// and `"description":""` went upstream for the same body, which is a different
// request — and a different prompt byte count, the number a session's
// auto-compaction and cache keys are sized on — for one conversation. A
// description that is present and not a string is refused here already
// (shapeMismatch), exactly as the sibling's typed decode refuses it.

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// r56ToolBody is one tool, with the description spelled as the case needs it.
func r56ToolBody(desc string) string {
	return `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"go"}],"tools":[{"name":"T","description":` + desc + `,"input_schema":{"type":"object"}}]}`
}

// r56GatewayTools converts the body and returns the tools it puts upstream.
func r56GatewayTools(t *testing.T, body string) []any {
	t.Helper()
	var req map[string]any
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	out, refusal := anthropicToOpenAI(req, false)
	if refusal != "" {
		t.Fatalf("the gateway refused the body: %s", refusal)
	}
	raw, err := json.Marshal(out["tools"])
	if err != nil {
		t.Fatalf("marshal tools: %v", err)
	}
	var tools []any
	if err := json.Unmarshal(raw, &tools); err != nil {
		t.Fatalf("unmarshal tools: %v (%s)", err, raw)
	}
	return tools
}

func TestAToolDescriptionIsWrittenOnlyWhenStated(t *testing.T) {
	for _, tc := range []struct {
		name   string
		desc   string
		wanted map[string]any
	}{
		{
			name: "a stated description is written",
			desc: `"X"`,
			wanted: map[string]any{"type": "function", "function": map[string]any{
				"name": "T", "description": "X", "parameters": map[string]any{"type": "object"},
			}},
		},
		{
			name: "a null description is not a description",
			desc: `null`,
			wanted: map[string]any{"type": "function", "function": map[string]any{
				"name": "T", "parameters": map[string]any{"type": "object"},
			}},
		},
		{
			name: "an empty description is not a description",
			desc: `""`,
			wanted: map[string]any{"type": "function", "function": map[string]any{
				"name": "T", "parameters": map[string]any{"type": "object"},
			}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tools := r56GatewayTools(t, r56ToolBody(tc.desc))
			if len(tools) != 1 {
				t.Fatalf("the body's one tool became %d tools: %v", len(tools), tools)
			}
			got, ok := tools[0].(map[string]any)
			if !ok {
				t.Fatalf("the tool is %T", tools[0])
			}
			if !reflect.DeepEqual(got, tc.wanted) {
				t.Errorf("the gateway put %v upstream, want %v\n"+
					"legs 1 and 2 decode tools into a typed function whose description is a string with omitempty, so a description the client did not state is a key they never write: two legs, two upstream requests, for one body", got, tc.wanted)
			}
			fn, _ := got["function"].(map[string]any)
			if _, present := fn["description"]; present != (tc.desc == `"X"`) {
				t.Errorf("the function carries description=%v for description %s", fn["description"], tc.desc)
			}
		})
	}
}

// A description that is not a string is a body both siblings refuse — this
// leg's shapeMismatch and the other legs' decode — so the refusal must stand.
func TestAToolDescriptionThatIsNotAStringIsRefused(t *testing.T) {
	for _, desc := range []string{`5`, `["a"]`, `{"a":1}`} {
		var req map[string]any
		if err := json.Unmarshal([]byte(r56ToolBody(desc)), &req); err != nil {
			t.Fatalf("decode: %v", err)
		}
		_, refusal := anthropicToOpenAI(req, false)
		if !strings.Contains(refusal, "description") {
			t.Errorf("a tool whose description is %s was answered with %q, want a refusal naming the field: the sibling legs' typed decode refuses the whole body, and forwarding it puts a shape no backend's validator accepts on the wire", desc, refusal)
		}
	}
}
