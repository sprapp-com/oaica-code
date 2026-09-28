package openai

// round92_response_item_order_test.go — leg 1, F92-L1-3 (2026-09-29 audit,
// round 92).
//
// One upstream body — a chunk carrying the prose "…" and then a call — reached a
// client as `[message, function_call]` on every arm of this leg except one: the
// BUFFERED Responses arm built its output array as reasoning, then the calls,
// then the message, while the streamed arm's own `buildFinalOutput` states
// reasoning, then the message, then the calls, whatever order the events arrived
// in. So the same model that narrated before acting streamed the prose in front
// of the call it introduces and, asked for no stream, was handed the call first.
// Measured with the tools declared, across all four surfaces and both arms of
// each: `api chat buffered/streamed`, `openai chat buffered/streamed`,
// `messages buffered/streamed` and `responses streamed` all say text-then-call;
// `responses buffered` said call-then-text.
//
// The pin is the agreement itself, not a fixed literal: the two arms of this
// package, fed the same api.ChatResponse, must state the same items in the same
// order.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// r92ItemTypesOf is the item order the buffered arm answers with.
func r92ItemTypesOf(t *testing.T, chat api.ChatResponse) []string {
	t.Helper()
	resp := ToResponse("m", "resp_1", "msg_1", chat, ResponsesRequest{Model: "m"})
	raw, err := json.Marshal(resp.Output)
	if err != nil {
		t.Fatalf("marshal output: %v", err)
	}
	var items []map[string]any
	if err := json.Unmarshal(raw, &items); err != nil {
		t.Fatalf("unmarshal output: %v", err)
	}
	return itemTypes(items)
}

// r92StreamedItemTypes is the item order the streamed arm answers with, for the
// same body: the array its own response.completed carries.
func r92StreamedItemTypes(t *testing.T, chat api.ChatResponse) []string {
	t.Helper()
	c := NewResponsesStreamConverter("resp_1", "msg_1", "m", ResponsesRequest{})
	events := c.Process(chat)
	events = append(events, c.Process(api.ChatResponse{Model: "m", Done: true, Message: api.Message{Role: "assistant"}})...)
	return itemTypes(completedOutput(t, events))
}

func r92EqualOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestTheBufferedResponsesArmOrdersItemsLikeItsStreamingArm(t *testing.T) {
	prose := "Let me look at the file."
	for _, tc := range []struct {
		name string
		chat api.ChatResponse
	}{
		{
			name: "prose then a call",
			chat: api.ChatResponse{
				Model:   "m",
				Message: api.Message{Role: "assistant", Content: prose, ToolCalls: toolCallResponse("Read").Message.ToolCalls},
			},
		},
		{
			name: "reasoning, prose, and a call",
			chat: api.ChatResponse{
				Model:   "m",
				Message: api.Message{Role: "assistant", Thinking: "because", Content: prose, ToolCalls: toolCallResponse("Read").Message.ToolCalls},
			},
		},
		{
			name: "prose alone",
			chat: api.ChatResponse{
				Model:   "m",
				Message: api.Message{Role: "assistant", Content: prose},
			},
		},
	} {
		buffered := r92ItemTypesOf(t, tc.chat)
		streamed := r92StreamedItemTypes(t, tc.chat)
		if !r92EqualOrder(buffered, streamed) {
			t.Errorf("%s: one upstream body, two orders (2026-09-29 audit, round 92, F92-L1-3):\n  buffered %v\n  streamed %v",
				tc.name, buffered, streamed)
		}
	}

	// And the order they agree on is the one every other arm of this leg states:
	// the prose before the call it introduces.
	chat := api.ChatResponse{
		Model:   "m",
		Message: api.Message{Role: "assistant", Content: prose, ToolCalls: toolCallResponse("Read").Message.ToolCalls},
	}
	got := r92ItemTypesOf(t, chat)
	want := []string{"message", "function_call"}
	if !r92EqualOrder(got, want) {
		t.Errorf("a turn that narrated before it called reached a non-streaming client as %v, want %v — the call in front of the prose that introduced it", got, want)
	}
}
