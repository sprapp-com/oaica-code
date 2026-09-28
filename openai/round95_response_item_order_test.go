package openai

// round95_response_item_order_test.go — leg 1, F95-L1-2 (2026-09-29 audit,
// round 95).
//
// Round 92 pinned this package's two arms to the same item order for one
// api.ChatResponse carrying prose and a call together. It did not pin the
// STREAMED arm against its own events, and that is where the disagreement was:
// for a turn whose chunks arrive call-first and prose-second — the shape the
// in-tree qwen3-coder parser produces — the converter emits
// `response.output_item.added` naming the function call, then the message, but
// its terminal document states the message first, because `buildFinalOutput`
// collected the items into a fixed reasoning/message/calls order whatever order
// the events had announced them in. Measured with the probe that found it:
//
//	chunks [call][text]  events=[function_call message]  document=[message function_call]
//	chunks [text][call]  events=[message function_call]  document=[message function_call]
//
// So on a call-then-text turn a client that assembled its items from the events
// and a client that read the completed response were handed the same turn with
// the message and the call swapped. The pin is the agreement itself: the order the
// events claimed is the order the document states.
//
// The BUFFERED arm cannot honour it and is recorded, not fixed: an
// api.ChatResponse carries Content as one string and ToolCalls as one slice, with
// nothing that says which came first, so the buffered arm keeps round 92's
// default — the prose before the call it introduces. A turn that narrated before
// it called therefore reaches a non-streaming client as `[message, function_call]`
// on all five arms, and a turn that called first reaches the four streaming arms
// as `[function_call, message]` and the buffered Responses arm as
// `[message, function_call]`. That is a deliberate exception of the F90-L1-2
// class: the input does not carry the information the arm would need.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// r95ClaimedOrder is the item order the streamed arm's own events claimed, in the
// order the events were emitted.
func r95ClaimedOrder(t *testing.T, events []ResponsesStreamEvent) []string {
	t.Helper()
	var out []string
	for _, ev := range events {
		if ev.Event != "response.output_item.added" {
			continue
		}
		data, _ := ev.Data.(map[string]any)
		item, _ := data["item"].(map[string]any)
		kind, _ := item["type"].(string)
		out = append(out, kind)
	}
	if len(out) == 0 {
		t.Fatalf("no response.output_item.added event among %d events", len(events))
	}
	return out
}

// r95StreamedTurn drives one streamed turn and reports the order its events
// claimed and the order its terminal document states.
func r95StreamedTurn(t *testing.T, chunks ...api.ChatResponse) ([]string, []string) {
	t.Helper()
	c := NewResponsesStreamConverter("resp_1", "msg_1", "m", ResponsesRequest{})
	var events []ResponsesStreamEvent
	for _, chunk := range chunks {
		events = append(events, c.Process(chunk)...)
	}
	events = append(events, c.Process(api.ChatResponse{Model: "m", Done: true, Message: api.Message{Role: "assistant"}})...)
	return r95ClaimedOrder(t, events), itemTypes(completedOutput(t, events))
}

// r95EqualOrder reports whether two item orders are the same list.
func r95EqualOrder(a, b []string) bool {
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

// r95BufferedItemTypes is the item order the buffered arm answers with.
func r95BufferedItemTypes(t *testing.T, chat api.ChatResponse) []string {
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

// A streamed turn's terminal document states the items in the order the turn's
// own events announced them — whatever that order is, and whichever chunk came
// first.
func TestAStreamedTurnsDocumentStatesTheOrderItsEventsClaimed(t *testing.T) {
	call := func() api.ChatResponse {
		return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", ToolCalls: toolCallResponse("Read").Message.ToolCalls}}
	}
	text := func() api.ChatResponse {
		return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: "Let me look at the file."}}
	}
	reasoning := func() api.ChatResponse {
		return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Thinking: "because"}}
	}

	for _, tc := range []struct {
		name   string
		chunks []api.ChatResponse
		want   []string
	}{
		{"a call, then the prose that follows it", []api.ChatResponse{call(), text()}, []string{"function_call", "message"}},
		{"the prose, then the call it introduces", []api.ChatResponse{text(), call()}, []string{"message", "function_call"}},
		{"reasoning, a call, then prose", []api.ChatResponse{reasoning(), call(), text()}, []string{"reasoning", "function_call", "message"}},
		{"a call alone", []api.ChatResponse{call()}, []string{"function_call"}},
		{"prose alone", []api.ChatResponse{text()}, []string{"message"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			claimed, documented := r95StreamedTurn(t, tc.chunks...)
			if !r95EqualOrder(claimed, documented) {
				t.Errorf("one turn, one stream, two orders — the events claimed %v and the completed response states %v (2026-09-29 audit, round 95, F95-L1-2):\n  events   %v\n  document %v",
					claimed, documented, claimed, documented)
			}
			if !r95EqualOrder(documented, tc.want) {
				t.Errorf("the completed response states %v, want %v — the order the turn's own chunks arrived in (2026-09-29 audit, round 95, F95-L1-2)",
					documented, tc.want)
			}
		})
	}
}

// The recorded exception, pinned so it is a decision and not an oversight: a turn
// that called before it narrated reaches the BUFFERED arm as prose-then-call,
// because an api.ChatResponse holds no information about which came first. Round
// 92's agreement for a single-response body is unchanged and is re-pinned here.
func TestTheBufferedResponsesArmKeepsItsOrderForATurnWithNoOrderToRead(t *testing.T) {
	const prose = "Let me look at the file."
	chat := api.ChatResponse{
		Model:   "m",
		Message: api.Message{Role: "assistant", Content: prose, ToolCalls: toolCallResponse("Read").Message.ToolCalls},
	}

	// Round 92's property, still standing for the body it was pinned on: this one
	// api.ChatResponse is read the same way by both arms.
	buffered := r95BufferedItemTypes(t, chat)
	_, streamed := r95StreamedTurn(t, chat)
	if !r95EqualOrder(buffered, streamed) {
		t.Errorf("one api.ChatResponse, two orders (2026-09-29 audit, round 92, F92-L1-3):\n  buffered %v\n  streamed %v", buffered, streamed)
	}
	if want := []string{"message", "function_call"}; !r95EqualOrder(buffered, want) {
		t.Errorf("the buffered arm states %v, want %v (2026-09-29 audit, round 92, F92-L1-3)", buffered, want)
	}

	// And the same turn told to the streaming arm as two chunks is documented
	// call-first, which the buffered arm has no way to reproduce from one
	// api.ChatResponse. That gap is the recorded exception, not a defect.
	claimed, documented := r95StreamedTurn(t,
		api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", ToolCalls: toolCallResponse("Read").Message.ToolCalls}},
		api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: prose}},
	)
	if !r95EqualOrder(documented, []string{"function_call", "message"}) || !r95EqualOrder(claimed, documented) {
		t.Errorf("a call-then-prose turn streamed as two chunks claimed %v and stated %v, want function_call first on both (2026-09-29 audit, round 95, F95-L1-2)", claimed, documented)
	}
}
