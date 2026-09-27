package anthropic

// round51_null_schema_and_block_order_test.go — round 51's two findings on the
// local leg.
//
// L1/D2: `"input_schema":null` and a tool with no schema key at all mean the
// same thing, but only one of them put the empty schema on the wire. The decode
// kept the bytes `null`, the tool marshalled them back, and the backend was
// handed `"parameters":null` where the metered gateway leg serves
// `{"type":"","properties":null}` for the same body — one request, two upstream
// signatures, one of them a shape no function-calling validator accepts.
//
// L3: the blocks of one turn were written back with EVERY tool result hoisted
// ahead of the turn's own text. A turn that states its instruction before its
// result — `[{text}, {tool_result}]` — was reordered to `[{tool_result},
// {text}]`, so the model was asked with its instruction after the data it was
// about, and the same client body was two prompts depending on which leg served
// it (the gateway writes a turn's parts in the order the blocks arrived).
//
// L2: `tools:[null]` — a null element in the tool list — decoded without error
// here and was refused in words by the gateway. The typed list now refuses it,
// which is the same verdict both legs give (2026-09-27 audit, round 51).

import (
	"encoding/json"
	"testing"
)

// round51Parameters marshals the parameters the leg hands the backend for a
// tools array written as given.
func round51Parameters(t *testing.T, tools string) string {
	t.Helper()
	var req MessagesRequest
	body := `{"model":"m","messages":[{"role":"user","content":"go"}],"tools":` + tools + `}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion refused a tool the other legs serve: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("the backend was handed %d tool(s), want 1", len(out.Tools))
	}
	encoded, err := json.Marshal(out.Tools[0].Function.Parameters)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	return string(encoded)
}

// TestANullToolSchemaIsTheSameToolAsNoSchema is L1/D2. The pinned bytes are the
// ones the gateway leg puts on the wire and the api package's zero value
// marshals to.
func TestANullToolSchemaIsTheSameToolAsNoSchema(t *testing.T) {
	const pinned = `{"type":"","properties":null}`

	stated := round51Parameters(t, `[{"name":"Read","input_schema":null}]`)
	if stated != pinned {
		t.Errorf("a tool stating input_schema:null reached the backend as %s, want %s\nthe gateway leg serves this body with the empty schema, so one client body was two tool signatures", stated, pinned)
	}

	absent := round51Parameters(t, `[{"name":"Read"}]`)
	if absent != stated {
		t.Errorf("the two spellings of no schema disagree: absent -> %s, null -> %s", absent, stated)
	}

	// A schema that IS stated still travels as the client wrote it, byte for
	// byte (round 50) — a null property inside it included, which is what the
	// metered gateway leg forwards for the same schema. A null PROPERTY is not
	// the null SCHEMA above: the whole-schema case was a schema not stated,
	// while this one is a schema stated as `null` inside a stated object, and
	// rewriting it would be a signature the client did not write.
	statedProps := round51Parameters(t, `[{"name":"Read","input_schema":{"type":"object","properties":{"cursor":null},"required":["cursor"]}}]`)
	want := `{"type":"object","properties":{"cursor":null},"required":["cursor"]}`
	if statedProps != want {
		t.Errorf("a stated schema reached the backend as %s, want %s", statedProps, want)
	}
}

// TestANullToolElementIsRefused is L2: `tools:[null]` is not a tool, and this
// leg's typed list says so instead of serving a function the client never
// defined (the gateway refuses the same body in words).
func TestANullToolElementIsRefused(t *testing.T) {
	var req MessagesRequest
	body := `{"model":"m","messages":[{"role":"user","content":"go"}],"tools":[null]}`
	if err := json.Unmarshal([]byte(body), &req); err == nil {
		t.Fatal("a null tool element decoded without error; both legs refuse this body, and this one served a tool the client never defined")
	}
}

// TestATurnsTextStaysBeforeItsToolResult is L3: a turn that states its
// instruction first and its result second is written back in that order.
func TestATurnsTextStaysBeforeItsToolResult(t *testing.T) {
	var req MessagesRequest
	body := `{"model":"m","messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{"path":"a"}}]},` +
		`{"role":"user","content":[{"type":"text","text":"now read b"},{"type":"tool_result","tool_use_id":"call_1","content":"a"}]}` +
		`]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("the turn became %d messages, want 3", len(out.Messages))
	}
	if out.Messages[1].Role != "user" || out.Messages[1].Content != "now read b" {
		t.Fatalf("the client wrote its instruction before its result and the backend was asked with %q first", out.Messages[1].Role)
	}
	if out.Messages[2].Role != "tool" || out.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("the tool result followed the text and must stay there: got %q", out.Messages[2].Role)
	}

	// The other order is the client's too: a result stated before the text
	// keeps that order.
	body = `{"model":"m","messages":[` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Read","input":{"path":"a"}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1","content":"a"},{"type":"text","text":"now read b"}]}` +
		`]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err = FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion: %v", err)
	}
	if len(out.Messages) != 3 || out.Messages[1].Role != "tool" || out.Messages[2].Role != "user" {
		t.Fatalf("a result stated before the text must stay before it: %+v", out.Messages)
	}
}
