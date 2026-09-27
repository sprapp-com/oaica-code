package main

// round51_bridge_and_shape_parity_test.go — round 51's findings on the metered
// gateway leg.
//
// G1: a tool call whose arguments are not a JSON object — the freeform shape a
// model reaches for when the tool takes a line of text — was wrapped one
// fragment at a time. The wrapper this wire gives that text is a prefix AND a
// suffix (`{"_raw":…}`), so the first fragment was delivered wrapped and every
// fragment after it was appended to the wrapper: the client accumulated
// `{"_raw":"echo hel"}lo world`, JSON that cannot be parsed, under a
// stop_reason of tool_use. The turn was billed, the call was unrunnable, and an
// agent that trusted it ran a tool with input it invented. Both other legs
// deliver a call's arguments once, whole; a non-object text is now held until
// the call's arguments are over and delivered as the one value it is.
//
// G2: seventeen spellings of a mis-shaped field were served 200 with the field
// either dropped or handed to the backend as written, while the sibling legs
// decode the same fields into typed structs and refuse the whole request. One
// client body is one verdict on all three legs — shapeMismatch is that verdict
// here. A field that is absent is not stated, and a field that is JSON null is
// not stated either: those stay served.
//
// The two tests at the bottom pin the null spellings against the sibling
// legs: a tool that states no schema and a tool that states `null` put the same
// empty schema on the upstream wire (api.ToolFunctionParameters,
// anthropic/anthropic.go), and a tool list with a null element is refused by
// both (2026-09-27 audit, round 51).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round51StreamArgs drives one streaming turn and returns the argument text the
// client accumulated, index by index: what a client-side accumulator holds
// after the last input_json_delta.
func round51StreamArgs(t *testing.T, stream string) map[int]string {
	t.Helper()
	acc := map[int]string{}
	for _, line := range strings.Split(stream, "\n") {
		body, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(body)), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta.Type == "input_json_delta" {
			acc[ev.Index] += ev.Delta.PartialJSON
		}
	}
	return acc
}

// TestFreeformArgumentsArriveAsOneParseableValue is G1. The model wrote a line
// of shell, not an object; the client must be handed one JSON value it can
// parse, and that value must carry the whole text.
func TestFreeformArgumentsArriveAsOneParseableValue(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hel"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"lo world"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"run it"}]}`)

	acc := round51StreamArgs(t, stream)
	got, ok := acc[0]
	if !ok {
		t.Fatalf("the freeform call reached the client with no arguments at all:\n%s", stream)
	}
	var value map[string]any
	if err := json.Unmarshal([]byte(got), &value); err != nil {
		t.Fatalf("the client accumulated %q, which is not JSON: %v\nthe turn is billed under a stop_reason of tool_use and the agent cannot run the call; the wrapper this wire adds is a prefix and a suffix, so a text delivered in fragments has to be wrapped once, whole", got, err)
	}
	if len(value) != 1 || value["_raw"] != "echo hello world" {
		t.Errorf("the freeform text reached the client as %q, want {\"_raw\":\"echo hello world\"}\nthe model's own words are the call's input", got)
	}
	opened, closed := round50BlockEvents(t, stream)
	if !opened[0] || !closed[0] {
		t.Errorf("the call's block was not both opened and closed (opened=%v closed=%v):\n%s", opened, closed, stream)
	}
}

// TestObjectArgumentsStillArriveAsTheyAreWritten is the other side of G1: a
// call whose arguments ARE an object is delivered as it arrives, and the
// fragments still accumulate into exactly the object the model wrote.
func TestObjectArgumentsStillArriveAsTheyAreWritten(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"a"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":".txt\"}"}}]}}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`+"\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	srv := round36Gateway(t, upstream)
	stream := round36Stream(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"read"}]}`)

	got := round51StreamArgs(t, stream)[0]
	if got != `{"path":"a.txt"}` {
		t.Errorf("a streamed object accumulated as %q, want {\"path\":\"a.txt\"}\nthe client is handed the object's own bytes, in one delta or several", got)
	}
}

// TestAMisShapedFieldIsRefusedInWords is G2. Every body below is one the
// sibling legs refuse at decode; the gateway must give the same verdict, and
// must not reach the upstream at all.
func TestAMisShapedFieldIsRefusedInWords(t *testing.T) {
	const head = `{"model":"kat-awq","max_tokens":64,"stream":false,"messages":[{"role":"user","content":"go"}]`
	for _, tc := range []struct {
		name string
		body string
		want string
	}{
		{"tools-not-array", `"tools":{"name":"a"}`, "tools must be an array"},
		{"tools-null-element", `"tools":[null]`, "tools element is not an object (index 0)"},
		{"tools-name-number", `"tools":[{"name":7}]`, "tools[0].name must be a string"},
		{"tools-type-bool", `"tools":[{"type":true}]`, "tools[0].type must be a string"},
		{"tools-description-array", `"tools":[{"name":"a","description":[]}]`, "tools[0].description must be a string"},
		{"tools-max-uses-string", `"tools":[{"name":"a","max_uses":"3"}]`, "tools[0].max_uses must be an integer"},
		{"tools-max-uses-fraction", `"tools":[{"name":"a","max_uses":1.5}]`, "tools[0].max_uses must be an integer"},
		{"metadata-not-object", `"metadata":[]`, "metadata must be an object"},
		{"metadata-user-id-number", `"metadata":{"user_id":7}`, "metadata.user_id must be a string"},
		{"messages-role-number", `,"x":0,"messages":[{"role":7,"content":"go"}]`, ""},
		{"block-is-error-string", `"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","is_error":"yes"}]}]`, "messages[0].content[0].is_error must be a boolean"},
		{"block-tool-use-id-number", `"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":7}]}]`, "messages[0].content[0].tool_use_id must be a string"},
		{"block-media-type-number", `"messages":[{"role":"user","content":[{"type":"image","source":{"type":"base64","media_type":7,"data":"aGk="}}]}]`, "messages[0].content[0].source.media_type must be a string"},
		{"thinking-not-object", `"thinking":"on"`, "thinking must be an object"},
		{"thinking-budget-fraction", `"thinking":{"type":"enabled","budget_tokens":1.5}`, "thinking.budget_tokens must be an integer"},
		{"output-config-not-object", `"output_config":"high"`, "output_config must be an object"},
		{"output-config-effort-number", `"output_config":{"effort":1}`, "output_config.effort must be a string"},
		{"tool-choice-not-object", `"tool_choice":"auto"`, "tool_choice must be an object"},
		{"tool-choice-parallel-string", `"tool_choice":{"type":"auto","disable_parallel_tool_use":"yes"}`, "tool_choice.disable_parallel_tool_use must be a boolean"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// The role case is written against the message element itself, so
			// it needs a different assembly: the head already carries messages.
			body := head + "," + tc.body + "}"
			if tc.want == "" {
				body = `{"model":"kat-awq","max_tokens":64,"stream":false` + tc.body + "}"
				tc.want = "messages[0].role must be a string"
			}
			cap := &round44Capture{}
			up := round44Upstream(t, cap, "application/json", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
			srv, _ := round39Gateway(t, up, nil)

			status, out := round44Raw(t, srv, body)
			if status != http.StatusBadRequest {
				t.Fatalf("status %d for %s, body:\n%s\nthe sibling legs refuse this body at decode; one client body is one verdict on all three legs", status, tc.body, out)
			}
			if !strings.Contains(out, tc.want) {
				t.Errorf("the refusal does not name the field (%q):\n%s\na client cannot tell a mis-shaped field from a backend failure", tc.want, out)
			}
			if asked := cap.take(); asked != "" {
				t.Errorf("the mis-shaped body reached the upstream anyway:\n%s", asked)
			}
		})
	}
}

// TestAFieldStatedAsNullIsNotStated is the other half of G2: JSON null is how
// these wires say "not stated", and both siblings decode it into the zero value
// rather than refusing. A body that states null everywhere must be served, and
// the tool schema it puts on the upstream wire must be the same empty schema
// api.ToolFunctionParameters marshals — `{"type":"","properties":null}`.
func TestAFieldStatedAsNullIsNotStated(t *testing.T) {
	cap := &round44Capture{}
	up := round44Upstream(t, cap, "application/json", `{"choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	srv, _ := round39Gateway(t, up, nil)

	body := `{"model":"kat-awq","max_tokens":64,"stream":false,"metadata":{"user_id":null},` +
		`"thinking":null,"tool_choice":null,"tools":[{"name":"Read","input_schema":null},{"name":"Write"}],` +
		`"messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c","is_error":null,"content":null}]}]}`
	status, out := round44Raw(t, srv, body)
	if status != http.StatusOK {
		t.Fatalf("a body stating null where it states nothing was refused with %d:\n%s\nthe sibling legs decode these into the zero value", status, out)
	}

	var sent struct {
		Tools []struct {
			Function struct {
				Name       string          `json:"name"`
				Parameters json.RawMessage `json:"parameters"`
			} `json:"function"`
		} `json:"tools"`
	}
	if err := json.Unmarshal([]byte(cap.take()), &sent); err != nil {
		t.Fatalf("the upstream body is not JSON: %v", err)
	}
	if len(sent.Tools) != 2 {
		t.Fatalf("the upstream was handed %d tool(s), want 2", len(sent.Tools))
	}
	for _, tool := range sent.Tools {
		var params map[string]any
		if err := json.Unmarshal(tool.Function.Parameters, &params); err != nil {
			t.Fatalf("tool %q reached the upstream with parameters %s", tool.Function.Name, tool.Function.Parameters)
		}
		if len(params) != 2 || params["type"] != "" || params["properties"] != nil {
			t.Errorf("tool %q states no schema and reached the upstream as %s, want {\"type\":\"\",\"properties\":null}\nthe sibling leg puts the empty schema on the wire for the same body", tool.Function.Name, tool.Function.Parameters)
		}
	}
}
