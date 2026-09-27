package main

// round49_bridge_integrity_test.go — round 49's findings on the gateway leg.
//
// Every finding here was reproduced on the unmodified tree before it was fixed.
//
// A-F3: the merge round 48 introduced for a restated call keyed on the id
// alone, so a call the upstream numbered ITSELF (an id-less entry whose minted
// id a later entry then stated) was folded into the earlier call and the client
// was handed one block where both other legs answer two. A restatement is the
// same call only when the id it repeats is one the UPSTREAM stated (A49-3).
//
// A-F1: a block that merged under round 48's rule never opened, so the
// arguments its own fragments had accumulated were left in the block's buffer —
// and finishStream's nameless-call flush then read that buffer as text the
// model had written, handing the client a text block carrying the call's
// arguments. A merged block is not a call and leaves nothing behind (A49-1).
//
// C-F5 (corrected): round 48 folded the literal arguments "null" into the empty
// object for BOTH the input and the mint, on the reading that both other legs
// fold null. They do not: legs 1 and 2 keep a null input as {} but mint the id
// from the text "null", so the gateway numbered a call the other two number
// differently — call_8dd75358, not call_ab56f21. canonicalCallArgs now returns
// the literal, and the input stays the empty object the contract requires.
//
// B-F2: temperature, top_p, top_k, stream and stop_sequences were copied onto
// the upstream body after a bare type switch, so a string temperature, a
// fractional top_k, a "true" stream or a stop list holding a number reached the
// backend as-is — a shape no other leg forwards (legs 1 and 2 decode into typed
// fields and refuse the body). Each is now checked against the shape the wire
// accepts.
//
// B-F3: a built-in web_search carries its schema in its TYPE, not in an
// input_schema, so the tool went upstream with no parameters at all — legs 1
// and 2 map the type to the same fixed schema. The same pre-scan those legs do
// drops a client tool that claims the name the built-in owns.
//
// B-F4: an assistant turn's text and its tool_use blocks are ONE OpenAI
// message — content and tool_calls are two fields of one turn — and the gateway
// opened a second assistant message for the tools, which the backends' chat
// templates answer differently and which no other leg sends.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// round49BlockTypes lists the type of every content block a stream opened, in
// order. A client walking the stream sees exactly these.
func round49BlockTypes(t *testing.T, stream string) []string {
	t.Helper()
	var types []string
	for _, line := range strings.Split(stream, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			ContentBlock *struct {
				Type string `json:"type"`
			} `json:"content_block"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_start" && ev.ContentBlock != nil {
			types = append(types, ev.ContentBlock.Type)
		}
	}
	return types
}

// TestAMintedIDIsNotARestatement is A-F3. The upstream names its second call
// with the id the id-less first call mints for itself. The non-stream list and
// the client leg both answer that wire with TWO blocks — a bridge's own mint is
// reproducible, so an upstream that states it is naming a second call — and the
// stream path used to fold the second into the first, handing the client one
// block for two calls.
func TestAMintedIDIsNotARestatement(t *testing.T) {
	minted := "call_7fdcafb6" // round47ToolCallIDFor("a", "{}")
	if got := round47ToolCallIDFor("a", "{}"); got != minted {
		t.Fatalf("this test's own FNV copy disagrees with the recorded literal: %s != %s", got, minted)
	}
	// The second call is the same call text a second time, so the repeat rule
	// numbers it from "{}#1".
	repeated := "call_495b501a" // round47ToolCallIDFor("a", "{}#1")
	if got := round47ToolCallIDFor("a", "{}#1"); got != repeated {
		t.Fatalf("this test's own FNV copy disagrees with the recorded literal: %s != %s", got, repeated)
	}

	t.Run("stream", func(t *testing.T) {
		up := round45Frames(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"`+minted+`","type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		)
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskStream)
		if status != http.StatusOK {
			t.Fatalf("status %d\n%s", status, body)
		}
		blocks := round47Blocks(t, body)
		if len(blocks) != 2 {
			t.Fatalf("the stream opened %d tool_use block(s), want 2 — the second entry states an id this bridge would MINT for a call of that text, so it names a second call:\n%s", len(blocks), body)
		}
		if blocks[0].id != minted {
			t.Errorf("the id-less call's id is %s, want the minted %s\n%s", blocks[0].id, minted, body)
		}
		if blocks[1].id != repeated {
			t.Errorf("the second call's id is %s, want %s — the two calls were left sharing one id:\n%s", blocks[1].id, repeated, body)
		}
	})

	t.Run("document", func(t *testing.T) {
		bumped := "call_20544379" // round47ToolCallIDFor("a", "{}\x00#0")
		if got := round47ToolCallIDFor("a", "{}\x00#0"); got != bumped {
			t.Fatalf("this test's own FNV copy disagrees with the recorded literal: %s != %s", got, bumped)
		}
		doc := round48Doc(
			`{"type":"function","function":{"name":"a","arguments":"{}"}},` +
				`{"id":"` + minted + `","type":"function","function":{"name":"a","arguments":"{}"}}`)
		up := round45Upstream(t, "application/json", doc)
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, round45AskPlain)
		if status != http.StatusOK {
			t.Fatalf("status %d\n%s", status, body)
		}
		blocks := round48DocBlocks(t, body)
		if len(blocks) != 2 {
			t.Fatalf("the list carries %d tool_use block(s), want 2:\n%s", len(blocks), body)
		}
		if blocks[0].id != bumped {
			t.Errorf("the id-less call's id is %s, want %s — it took a mint the later call's own id had already reserved\n%s", blocks[0].id, bumped, body)
		}
		if blocks[1].id != minted {
			t.Errorf("the id the upstream STATED is %s, want the stated %s — the one id the wire names was renumbered:\n%s", blocks[1].id, minted, body)
		}
		for i, b := range blocks {
			if b.input != `{}` {
				t.Errorf("block %d's input is %s, want {}\n%s", i, b.input, body)
			}
		}
	})
}

// TestARestatedCallLeavesNoTextBehind is A-F1. The upstream restates the SAME
// call under the id it stated, which every leg answers with one block — but the
// restated entry's own fragments had already been read into a block that never
// opened, and the end-of-stream flush read that buffer as the model's text, so
// the client was handed the call's arguments as prose beside the call.
func TestARestatedCallLeavesNoTextBehind(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_X","type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_X","type":"function","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%s", status, body)
	}
	types := round49BlockTypes(t, body)
	if len(types) != 1 || types[0] != "tool_use" {
		t.Fatalf("the stream opened block(s) %v, want exactly [tool_use] — a restated call is not a call, and its arguments are not the model's text:\n%s", types, body)
	}
	if blocks := round47Blocks(t, body); len(blocks) != 1 || blocks[0].id != "call_X" {
		t.Errorf("the one block is not the stated call_X:\n%s", body)
	}
}

// TestSamplingFieldShapesAreChecked is B-F2. A field whose type the wire does
// not accept is refused rather than copied onto the upstream body, the way both
// other legs answer it, and the correct shape is forwarded unchanged.
func TestSamplingFieldShapesAreChecked(t *testing.T) {
	const model = `"model":"oaica-35b-a3b-vision","max_tokens":16`
	for _, c := range []struct{ name, body string }{
		{"temperature-string", `{` + model + `,"temperature":"0.5","messages":[{"role":"user","content":"hi"}]}`},
		{"temperature-bool", `{` + model + `,"temperature":true,"messages":[{"role":"user","content":"hi"}]}`},
		{"top_p-string", `{` + model + `,"top_p":"0.5","messages":[{"role":"user","content":"hi"}]}`},
		{"top_k-float", `{` + model + `,"top_k":40.5,"messages":[{"role":"user","content":"hi"}]}`},
		{"top_k-string", `{` + model + `,"top_k":"40","messages":[{"role":"user","content":"hi"}]}`},
		{"stream-string", `{` + model + `,"stream":"true","messages":[{"role":"user","content":"hi"}]}`},
		{"stop_sequences-string", `{` + model + `,"stop_sequences":"END","messages":[{"role":"user","content":"hi"}]}`},
		{"stop_sequences-number", `{` + model + `,"stop_sequences":[7],"messages":[{"role":"user","content":"hi"}]}`},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got map[string]any
			g, _ := testGatewayForMessages(t, &got)
			var m map[string]any
			if err := json.Unmarshal([]byte(c.body), &m); err != nil {
				t.Fatalf("probe body: %v", err)
			}
			w := postMessages(t, g, "sk-test", m)
			if w.Code != http.StatusBadRequest {
				t.Fatalf("status %d, want 400 — the body was forwarded upstream as %v\n%s", w.Code, got, w.Body.String())
			}
			if got != nil {
				t.Errorf("the refused body still reached the backend: %v", got)
			}
		})
	}

	t.Run("accepted_shapes", func(t *testing.T) {
		var got map[string]any
		g, _ := testGatewayForMessages(t, &got)
		var m map[string]any
		body := `{` + model + `,"temperature":0.5,"top_p":0.9,"top_k":40,"stream":false,"stop_sequences":["END"],"messages":[{"role":"user","content":"hi"}]}`
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("probe body: %v", err)
		}
		w := postMessages(t, g, "sk-test", m)
		if w.Code != http.StatusOK {
			t.Fatalf("status %d, want 200\n%s", w.Code, w.Body.String())
		}
		if got["temperature"] != 0.5 || got["top_p"] != 0.9 {
			t.Errorf("the sampling fields were dropped or rewritten: %v", got)
		}
		stop, _ := got["stop"].([]any)
		if len(stop) != 1 || stop[0] != "END" {
			t.Errorf("stop_sequences reached the backend as %v, want [END]", got["stop"])
		}
	})
}

// TestABuiltinWebSearchCarriesItsSchema is B-F3. The built-in's schema lives in
// its TYPE, and both other legs map that type onto one fixed function — the
// gateway sent the tool upstream with no parameters at all.
func TestABuiltinWebSearchCarriesItsSchema(t *testing.T) {
	ask := func(t *testing.T, tools string) (int, map[string]any) {
		t.Helper()
		var got map[string]any
		g, _ := testGatewayForMessages(t, &got)
		var m map[string]any
		body := `{"model":"oaica-35b-a3b-vision","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":` + tools + `}`
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("probe body: %v", err)
		}
		w := postMessages(t, g, "sk-test", m)
		return w.Code, got
	}
	round49Tools := func(t *testing.T, got map[string]any) []map[string]any {
		t.Helper()
		list, _ := got["tools"].([]any)
		out := make([]map[string]any, 0, len(list))
		for _, e := range list {
			asMap, _ := e.(map[string]any)
			fn, _ := asMap["function"].(map[string]any)
			out = append(out, fn)
		}
		return out
	}

	t.Run("builtin_alone", func(t *testing.T) {
		status, got := ask(t, `[{"type":"web_search_20250305","name":"web_search"}]`)
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		names := round49Tools(t, got)
		if len(names) != 1 {
			t.Fatalf("the backend was handed %d tool(s), want 1: %v", len(names), names)
		}
		fn := names[0]
		if fn["name"] != "web_search" {
			t.Errorf("the tool's name is %v", fn["name"])
		}
		if s, _ := fn["description"].(string); s == "" {
			t.Errorf("the built-in went upstream without its description: %v", fn)
		}
		params, _ := fn["parameters"].(map[string]any)
		if params["type"] != "object" {
			t.Fatalf("the built-in's parameters are %v, want the object both other legs send", fn["parameters"])
		}
		req, _ := params["required"].([]any)
		if len(req) != 1 || req[0] != "query" {
			t.Errorf("required is %v, want [query]", params["required"])
		}
		props, _ := params["properties"].(map[string]any)
		query, _ := props["query"].(map[string]any)
		if query["type"] != "string" {
			t.Errorf("the query property is %v, want a string", props["query"])
		}
	})

	t.Run("client_tool_of_the_same_name", func(t *testing.T) {
		// The name the built-in owns cannot be redefined; legs 1 and 2 drop the
		// client's tool rather than hand the backend two tools with one name.
		status, got := ask(t, `[{"type":"web_search_20250305","name":"web_search"},{"name":"web_search","input_schema":{"type":"object"}}]`)
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		if names := round49Tools(t, got); len(names) != 1 {
			t.Fatalf("the backend was handed %d tools with the name web_search, want 1: %v", len(names), names)
		}
	})

	t.Run("client_tool_alone_keeps_its_own_schema", func(t *testing.T) {
		status, got := ask(t, `[{"name":"web_search","input_schema":{"type":"object"}}]`)
		if status != http.StatusOK {
			t.Fatalf("status %d", status)
		}
		names := round49Tools(t, got)
		if len(names) != 1 {
			t.Fatalf("the backend was handed %d tool(s), want 1", len(names))
		}
		if s, _ := names[0]["description"].(string); s != "" {
			t.Errorf("a client tool with no type was rewritten into the built-in: %v", names[0])
		}
	})
}

// TestAToolWithoutASchemaStillCarriesAParametersObject is B-F3's sibling: the
// wire's tool needs a parameters object, and both other legs send the zero
// ToolFunctionParameters for a tool that states no schema — while a schema of
// the wrong KIND is refused by both, which the gateway used to forward.
func TestAToolWithoutASchemaStillCarriesAParametersObject(t *testing.T) {
	ask := func(t *testing.T, tools string) (int, map[string]any) {
		t.Helper()
		var got map[string]any
		g, _ := testGatewayForMessages(t, &got)
		var m map[string]any
		body := `{"model":"oaica-35b-a3b-vision","max_tokens":16,"messages":[{"role":"user","content":"hi"}],"tools":` + tools + `}`
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("probe body: %v", err)
		}
		w := postMessages(t, g, "sk-test", m)
		return w.Code, got
	}

	status, got := ask(t, `[{"name":"Read"}]`)
	if status != http.StatusOK {
		t.Fatalf("status %d\n%v", status, got)
	}
	list, _ := got["tools"].([]any)
	if len(list) != 1 {
		t.Fatalf("the backend was handed %d tool(s), want 1", len(list))
	}
	entry, _ := list[0].(map[string]any)
	fn, _ := entry["function"].(map[string]any)
	params, ok := fn["parameters"].(map[string]any)
	if !ok {
		t.Fatalf("the tool went upstream with parameters %v, want an object", fn["parameters"])
	}
	if params["type"] != "" || params["properties"] != nil {
		t.Errorf("parameters are %v, want the zero object {\"type\":\"\",\"properties\":null} both other legs send", params)
	}

	for _, bad := range []string{`"nope"`, `[1,2]`} {
		status, got := ask(t, `[{"name":"Read","input_schema":`+bad+`}]`)
		if status != http.StatusBadRequest {
			t.Errorf("input_schema %s: status %d, want 400 — legs 1 and 2 refuse the body:\n%v", bad, status, got)
		}
	}
}

// TestAssistantTextAndToolUseAreOneTurn is B-F4. An assistant turn's text and
// its tool calls are two fields of ONE OpenAI message; a second assistant
// message invents a turn the client never wrote.
func TestAssistantTextAndToolUseAreOneTurn(t *testing.T) {
	var got map[string]any
	g, _ := testGatewayForMessages(t, &got)
	var m map[string]any
	body := `{"model":"oaica-35b-a3b-vision","max_tokens":16,"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"text","text":"reading now"},{"type":"tool_use","id":"toolu_1","name":"Read","input":{"path":"a.txt"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"data"}]}]}`
	if err := json.Unmarshal([]byte(body), &m); err != nil {
		t.Fatalf("probe body: %v", err)
	}
	w := postMessages(t, g, "sk-test", m)
	if w.Code != http.StatusOK {
		t.Fatalf("status %d\n%s", w.Code, w.Body.String())
	}
	list, _ := got["messages"].([]any)
	if len(list) != 3 {
		t.Fatalf("the assistant turn became %d upstream messages, want 3 (user, assistant, tool):\n%v", len(list), got["messages"])
	}
	turn, _ := list[1].(map[string]any)
	if turn["role"] != "assistant" {
		t.Fatalf("the second message is %v, want the assistant turn", turn)
	}
	if turn["content"] != "reading now" {
		t.Errorf("the assistant's text is %v, want the text it wrote beside the call", turn["content"])
	}
	calls, _ := turn["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("the assistant turn carries %d tool call(s), want 1", len(calls))
	}
	call, _ := calls[0].(map[string]any)
	if call["id"] != "toolu_1" {
		t.Errorf("the call's id is %v, want the stated toolu_1", call["id"])
	}
	result, _ := list[2].(map[string]any)
	if result["role"] != "tool" || result["tool_call_id"] != "toolu_1" {
		t.Errorf("the tool result is %v, want the tool message answering toolu_1", result)
	}
}
