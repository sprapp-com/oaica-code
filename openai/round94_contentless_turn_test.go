package openai

// round94_contentless_turn_test.go — leg 1, F94-L1-2 (2026-09-29 audit, round
// 94).
//
// The Responses surface has two arms that must state one turn the same way: the
// buffered arm builds `output` from the whole response with ToResponse, the
// streamed arm builds it from the events it relayed with buildFinalOutput. On a
// turn that stated no text they disagreed about whether the turn has a message
// item at all. Measured on this leg before the fix, one upstream body:
//
//	buffered  output = [{"type":"message","content":[{"type":"output_text","text":""}]}]
//	streamed  output = null
//
// and for the same turn carrying thinking, buffered `[reasoning, message]` where
// streamed stated only `[reasoning]`. The streamed arm is not the odd one out:
// the Anthropic surface, whose `content` is the same kind of array this `output`
// is, answers that turn `"content": []` — no part at all.
//
// The second half is the same defect in the array's own rendering: a turn with
// no items at all was written as `null`, where the streamed arm's own
// `response.created` and `response.in_progress` events for that very response
// state `"output": []`. An array on this wire is an array on every event of it.

import (
	"encoding/json"
	"reflect"
	"testing"

	"github.com/ollama/ollama/api"
)

// r94Args is the argument object this pin's calls carry.
func r94Args() api.ToolCallFunctionArguments {
	a := api.NewToolCallFunctionArguments()
	a.Set("m", 1)
	return a
}

// r94OutputItems marshals one arm's output array into plain values so the two
// arms can be compared without their Go types getting in the way.
func r94OutputItems(t *testing.T, v any) any {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshalling an arm's output: %v", err)
	}
	var out any
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatalf("reading back an arm's output: %v", err)
	}
	// The minted IDs are NOT compared here: the two arms mint them in different
	// namespaces and the buffered arm's call ID is minted per call rather than
	// per index (2026-09-29 audit, round 94, F94-L1-3, recorded). What this pin
	// holds is the shape and order of the items and what each one carries.
	items, _ := out.([]any)
	for _, it := range items {
		if item, ok := it.(map[string]any); ok {
			delete(item, "id")
			delete(item, "call_id")
		}
	}
	return out
}

// r94StreamedOutput is the output array of the streamed arm's terminal
// response.completed event.
func r94StreamedOutput(t *testing.T, resp api.ChatResponse) any {
	t.Helper()
	c := NewResponsesStreamConverter("resp_1", "msg_1", "m", ResponsesRequest{})
	var events []ResponsesStreamEvent
	events = append(events, c.Process(resp)...)
	for _, e := range events {
		if e.Event != "response.completed" {
			continue
		}
		data, ok := e.Data.(map[string]any)
		if !ok {
			t.Fatalf("response.completed data is %T", e.Data)
		}
		response, ok := data["response"].(map[string]any)
		if !ok {
			t.Fatalf("response.completed response is %T", data["response"])
		}
		return r94OutputItems(t, response["output"])
	}
	t.Fatal("the streamed arm stated no response.completed event")
	return nil
}

// One upstream body, one output array: the buffered arm and the streamed arm
// state the same items, carrying the same things, in the same order, for every
// shape a turn can take. (The minted IDs are pinned separately — see
// r94OutputItems.)
func TestTheTwoResponsesArmsStateTheSameOutputItems(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp api.ChatResponse
	}{
		{"text", api.ChatResponse{Message: api.Message{Content: "hi"}, Done: true}},
		{"calls only", api.ChatResponse{Message: api.Message{ToolCalls: []api.ToolCall{{
			Function: api.ToolCallFunction{Name: "Read", Arguments: r94Args()},
		}}}, Done: true}},
		{"text and calls", api.ChatResponse{Message: api.Message{
			Content: "let me look",
			ToolCalls: []api.ToolCall{{
				Function: api.ToolCallFunction{Name: "Read", Arguments: r94Args()},
			}},
		}, Done: true}},
		{"no content at all", api.ChatResponse{Message: api.Message{}, Done: true}},
		{"thinking only", api.ChatResponse{Message: api.Message{Thinking: "why"}, Done: true}},
		{"thinking and text", api.ChatResponse{Message: api.Message{Thinking: "why", Content: "hi"}, Done: true}},
		{"thinking and calls", api.ChatResponse{Message: api.Message{Thinking: "why", ToolCalls: []api.ToolCall{{
			Function: api.ToolCallFunction{Name: "Read", Arguments: r94Args()},
		}}}, Done: true}},
	} {
		buffered := r94OutputItems(t, ToResponse("m", "resp_1", "msg_1", tc.resp, ResponsesRequest{}).Output)
		streamed := r94StreamedOutput(t, tc.resp)
		if !reflect.DeepEqual(buffered, streamed) {
			bf, _ := json.Marshal(buffered)
			sf, _ := json.Marshal(streamed)
			t.Errorf("%s: the buffered arm states %s where the streamed arm states %s — one turn, one output array, whichever way the client asked for it (2026-09-29 audit, round 94, F94-L1-2)", tc.name, bf, sf)
		}
	}
}

// A turn that stated no text states no message item: the item exists to hold the
// text, and the Anthropic surface answers the same turn `"content": []`.
func TestATurnThatStatedNoTextStatesNoMessageItem(t *testing.T) {
	for _, tc := range []struct {
		name string
		resp api.ChatResponse
	}{
		{"no content at all", api.ChatResponse{Message: api.Message{}, Done: true}},
		{"thinking only", api.ChatResponse{Message: api.Message{Thinking: "why"}, Done: true}},
		{"calls only", api.ChatResponse{Message: api.Message{ToolCalls: []api.ToolCall{{
			Function: api.ToolCallFunction{Name: "Read", Arguments: r94Args()},
		}}}, Done: true}},
	} {
		items, ok := r94OutputItems(t, ToResponse("m", "resp_1", "msg_1", tc.resp, ResponsesRequest{}).Output).([]any)
		if !ok {
			t.Fatalf("%s: the buffered arm's output is not an array", tc.name)
		}
		for _, it := range items {
			item, ok := it.(map[string]any)
			if !ok {
				continue
			}
			if item["type"] == "message" {
				t.Errorf("%s: the buffered arm states a message item for a turn that stated no text: %v — an item holding an empty string is one no other arm states (2026-09-29 audit, round 94, F94-L1-2)", tc.name, item)
			}
		}
	}
}

// A turn with no items at all is `[]`, not `null`: `output` is an array on this
// wire, and the streamed arm's own response.created event for the same response
// states it as one.
func TestATurnWithNoItemsStatesAnEmptyArray(t *testing.T) {
	resp := api.ChatResponse{Message: api.Message{}, Done: true}
	buffered, ok := r94OutputItems(t, ToResponse("m", "resp_1", "msg_1", resp, ResponsesRequest{}).Output).([]any)
	if !ok {
		t.Fatal("the buffered arm's output is not an array — a nil slice renders null where this wire states `[]` (2026-09-29 audit, round 94, F94-L1-2)")
	}
	if len(buffered) != 0 {
		t.Errorf("buffered output = %v, want no items", buffered)
	}
	streamed, ok := r94StreamedOutput(t, resp).([]any)
	if !ok {
		t.Fatal("the streamed arm's terminal output is not an array — the same response's own response.created event states `[]` (2026-09-29 audit, round 94, F94-L1-2)")
	}
	if len(streamed) != 0 {
		t.Errorf("streamed output = %v, want no items", streamed)
	}
}
