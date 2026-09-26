package openai

// responses_message_with_tool_calls_integrity_test.go — a turn can carry both
// text and a tool call, and on the Responses paths the text was dropped when it
// did (2026-09-27 audit, round 18).
//
// The stream converter announced the message item, sent its deltas, and then
// skipped every event that closes it — because `toolCallsSent` was true — so
// the client was left holding an item it was told about and never told was
// finished, and `response.completed`'s output array held the function_call
// alone. When the text and the call arrived in the SAME chunk the text was not
// even announced: `Process` refuses content whenever the chunk has tool calls.
// The non-streaming sibling (ToResponse) had the same shape as an if/else, so
// the text was dropped there too, and the two paths disagreed with each other
// about the same upstream body.
//
// "Let me look at the file." followed by the call is an ordinary turn for a
// model that narrates before acting, and both paths serve it: middleware/
// openai.go:487 streams through the converter, :498 answers through ToResponse.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// completedOutput returns the output array of the response.completed event, as
// the item maps the client receives.
func completedOutput(t *testing.T, events []ResponsesStreamEvent) []map[string]any {
	t.Helper()
	for _, ev := range events {
		if ev.Event != "response.completed" {
			continue
		}
		data, _ := ev.Data.(map[string]any)
		response, _ := data["response"].(map[string]any)
		raw, _ := response["output"].([]any)
		out := make([]map[string]any, 0, len(raw))
		for _, item := range raw {
			if m, ok := item.(map[string]any); ok {
				out = append(out, m)
			}
		}
		return out
	}
	t.Fatalf("no response.completed event")
	return nil
}

func itemTypes(items []map[string]any) []string {
	out := make([]string, 0, len(items))
	for _, item := range items {
		kind, _ := item["type"].(string)
		out = append(out, kind)
	}
	return out
}

func hasType(items []map[string]any, want string) bool {
	for _, item := range items {
		if item["type"] == want {
			return true
		}
	}
	return false
}

// streamTextThenToolCall drives the converter through the ordinary shape: the
// model narrates, then calls a tool, then the turn ends.
func streamTextThenToolCall(t *testing.T) []ResponsesStreamEvent {
	t.Helper()
	c := NewResponsesStreamConverter("resp_1", "msg_1", "m", ResponsesRequest{})

	var events []ResponsesStreamEvent
	events = append(events, c.Process(api.ChatResponse{
		Message: api.Message{Role: "assistant", Content: "Let me look at the file."},
	})...)
	events = append(events, c.Process(toolCallResponse("Read"))...)
	events = append(events, c.Process(api.ChatResponse{
		Model: "m", Done: true,
		Message: api.Message{Role: "assistant"},
	})...)
	return events
}

func TestStreamedTextIsClosedAndKeptWhenAToolCallFollows(t *testing.T) {
	events := streamTextThenToolCall(t)

	// Every announced item must be closed: a client that keys item bookkeeping
	// on output_item.added waits forever for an item that never finishes.
	added := 0
	closed := map[string]bool{}
	for _, ev := range events {
		data, _ := ev.Data.(map[string]any)
		item, _ := data["item"].(map[string]any)
		id, _ := item["id"].(string)
		switch ev.Event {
		case "response.output_item.added":
			added++
		case "response.output_item.done":
			closed[id] = true
			if kind, _ := item["type"].(string); kind == "message" {
				if _, ok := item["content"]; !ok {
					t.Errorf("the message item was closed without its content:\n%+v", item)
				}
			}
		}
	}
	if added != 2 {
		t.Fatalf("announced %d items, want the message and the function call", added)
	}
	if len(closed) != added {
		t.Errorf("%d items were announced but only %d were closed (%v) — the message item the client was told about is never finished", added, len(closed), closed)
	}

	items := completedOutput(t, events)
	if !hasType(items, "function_call") {
		t.Errorf("the function_call is missing from response.completed: %v", itemTypes(items))
	}
	if !hasType(items, "message") {
		t.Errorf("the text the model wrote before it called the tool is missing from response.completed (%v) — the client never sees it: %+v", itemTypes(items), items)
	}
}

// The same shape in ONE chunk: the text must be emitted, not silently dropped.
func TestTextAndAToolCallInOneChunkBothReachTheClient(t *testing.T) {
	c := NewResponsesStreamConverter("resp_1", "msg_1", "m", ResponsesRequest{})

	withCall := toolCallResponse("Read")
	withCall.Message.Content = "Let me look at the file."

	var events []ResponsesStreamEvent
	events = append(events, c.Process(withCall)...)
	events = append(events, c.Process(api.ChatResponse{Model: "m", Done: true, Message: api.Message{Role: "assistant"}})...)

	sawText := false
	for _, ev := range events {
		if ev.Event == "response.output_text.delta" {
			data, _ := ev.Data.(map[string]any)
			if data["delta"] == "Let me look at the file." {
				sawText = true
			}
		}
	}
	if !sawText {
		t.Errorf("the text of a chunk that also carries a tool call was never emitted — the model's answer is lost with no error:\n%v", sseEventNames(events))
	}
	if items := completedOutput(t, events); !hasType(items, "message") || !hasType(items, "function_call") {
		t.Errorf("response.completed holds %v, want both the message and the function_call", itemTypes(items))
	}
}

// The non-streaming sibling: ToResponse must keep the text as well.
func TestToResponseKeepsTextBesideToolCalls(t *testing.T) {
	chat := toolCallResponse("Read")
	chat.Message.Content = "Let me look at the file."

	resp := ToResponse("m", "resp_1", "msg_1", chat, ResponsesRequest{Model: "m"})

	var items []map[string]any
	raw, err := json.Marshal(resp.Output)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	if !hasType(items, "function_call") {
		t.Errorf("the function_call is missing: %v", itemTypes(items))
	}
	if !hasType(items, "message") {
		t.Errorf("ToResponse dropped the text that accompanied a tool call (%v) — the streaming path serves the same body and must not disagree with it: %+v", itemTypes(items), items)
	}
}

func sseEventNames(events []ResponsesStreamEvent) []string {
	out := make([]string, 0, len(events))
	for _, ev := range events {
		out = append(out, ev.Event)
	}
	return out
}
