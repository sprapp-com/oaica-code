package openai

// responses_output_index_integrity_test.go — a stream's items must each claim
// their own output_index (2026-09-27 audit, round 17).
//
// round-16's TestStreamedToolCallsGetDistinctOutputIndexes pinned the tool-call
// case: two calls arriving in two chunks both claimed index 0. The fix added a
// running toolCallCount to the index — but left outputIndex as a SECOND
// counter that the text and reasoning items use directly and that the tool-call
// items do not advance. The two schemes therefore disagree the moment any item
// other than a tool call has been emitted:
//
//   - text first, tool call second: the message item claims outputIndex 0 and
//     never advances it, so the function_call that follows claims 0 as well.
//     Text followed by a tool call is an ordinary shape ("Let me look at the
//     file." then the call).
//   - a tool call first, thinking second: the call takes outputIndex +
//     toolCallCount, outputIndex stays put, and the reasoning item that follows
//     claims the index the call already owns.
//
// A Responses client (Codex, OMP) keys its item bookkeeping on output_index, so
// a duplicate merges two different items.

import (
	"testing"

	"github.com/ollama/ollama/api"
)

// claimedItem is one item the converter started, with the index it claimed.
type claimedItem struct {
	id    string
	kind  string
	index int
}

// claimedItems returns every item the converter starts, in order.
func claimedItems(events []ResponsesStreamEvent) []claimedItem {
	var claimed []claimedItem
	for _, ev := range events {
		if ev.Event != "response.output_item.added" {
			continue
		}
		m, ok := ev.Data.(map[string]any)
		if !ok {
			continue
		}
		item, _ := m["item"].(map[string]any)
		id, _ := item["id"].(string)
		kind, _ := item["type"].(string)
		idx, _ := m["output_index"].(int)
		claimed = append(claimed, claimedItem{id: id, kind: kind, index: idx})
	}
	return claimed
}

func assertDistinctOutputIndexes(t *testing.T, claimed []claimedItem, shape string) {
	t.Helper()
	byIndex := map[int]claimedItem{}
	for _, item := range claimed {
		if other, taken := byIndex[item.index]; taken {
			t.Errorf("output_index %d is claimed by two items (%s %s and %s %s) in %s — a Responses client keys its item bookkeeping on this index, so the two items collide", item.index, other.kind, other.id, item.kind, item.id, shape)
			continue
		}
		byIndex[item.index] = item
	}
}

func toolCallResponse(name string) api.ChatResponse {
	args := api.NewToolCallFunctionArguments()
	args.Set("file_path", "/tmp/x")
	return api.ChatResponse{
		Model:   "m",
		Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{Name: name, Arguments: args}}}},
	}
}

// Text, then a tool call. Both the message item and the function_call are
// items of the response, so they cannot share an index.
func TestTextBeforeAToolCallDoesNotShareItsOutputIndex(t *testing.T) {
	c := NewResponsesStreamConverter("r", "i", "m", ResponsesRequest{})

	claimed := claimedItems(c.Process(api.ChatResponse{
		Model:   "m",
		Message: api.Message{Role: "assistant", Content: "Let me look at the file."},
	}))
	claimed = append(claimed, claimedItems(c.Process(toolCallResponse("Read")))...)

	if len(claimed) != 2 {
		t.Fatalf("saw %d items, want the message and the function call: %v", len(claimed), claimed)
	}
	assertDistinctOutputIndexes(t, claimed, "text then a tool call")
}

// A tool call, then more thinking. The reasoning item is a third item and must
// take an index of its own.
func TestThinkingAfterAToolCallDoesNotShareItsOutputIndex(t *testing.T) {
	c := NewResponsesStreamConverter("r", "i", "m", ResponsesRequest{})

	claimed := claimedItems(c.Process(toolCallResponse("Read")))
	claimed = append(claimed, claimedItems(c.Process(api.ChatResponse{
		Model:   "m",
		Message: api.Message{Role: "assistant", Thinking: "the file says so"},
	}))...)

	if len(claimed) != 2 {
		t.Fatalf("saw %d items, want the function call and the reasoning summary: %v", len(claimed), claimed)
	}
	assertDistinctOutputIndexes(t, claimed, "a tool call then thinking")
}

// Thinking, then a tool call, then another tool call: the shape the index
// arithmetic was written for. It must keep working — and the reasoning item
// must not be indexed as if it were absent.
func TestToolCallsAfterThinkingKeepTheirOwnIndexes(t *testing.T) {
	c := NewResponsesStreamConverter("r", "i", "m", ResponsesRequest{})

	var claimed []claimedItem
	chunks := []api.ChatResponse{
		{Model: "m", Message: api.Message{Role: "assistant", Thinking: "let me think"}},
		toolCallResponse("Read"),
		toolCallResponse("Write"),
	}
	for _, chunk := range chunks {
		claimed = append(claimed, claimedItems(c.Process(chunk))...)
	}

	if len(claimed) != 3 {
		t.Fatalf("saw %d items, want the reasoning summary and two function calls: %v", len(claimed), claimed)
	}
	assertDistinctOutputIndexes(t, claimed, "thinking then two tool calls")
	if claimed[0].kind != "reasoning" || claimed[0].index != 0 {
		t.Errorf("the first item is %s at index %d, want the reasoning summary at index 0 — it is the first item of the response", claimed[0].kind, claimed[0].index)
	}
	for i, item := range claimed {
		if i > 0 && item.index == 0 {
			t.Errorf("item %s %s reuses index 0 after the reasoning summary", item.kind, item.id)
		}
	}
}
