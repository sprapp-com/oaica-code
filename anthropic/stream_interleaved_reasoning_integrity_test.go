package anthropic

// stream_interleaved_reasoning_integrity_test.go — reasoning that arrives after
// the thinking block was already closed produced NO event at all (2026-09-26
// audit, fifteenth round).
//
// The converter's `thinkingDone` guard exists to stop it re-opening a CLOSED
// block when a model emits thinking → content → more thinking; it closed the
// block on the first content delta and then discarded every later reasoning
// delta silently — no error event, nothing in the client's stream, nothing to
// diagnose. That is the shape agentic reasoning models actually produce (they
// re-emit reasoning between tool calls), and the loss is invisible.
//
// An assistant turn's content is an ARRAY of blocks and the client renders
// them in arrival order, so the second reasoning run is a new thinking block.

import (
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/ollama/ollama/api"
)

func TestStreamConverter_ReasoningAfterTextIsNotDropped(t *testing.T) {
	conv := NewStreamConverter("msg_123", "test-model", 0)

	responses := []api.ChatResponse{
		{Message: api.Message{Role: "assistant", Thinking: "THINK-A"}},
		{Message: api.Message{Role: "assistant", Content: "the answer"}},
		{Message: api.Message{Role: "assistant", Thinking: "THINK-B"}},
		{
			Message:    api.Message{Role: "assistant"},
			Done:       true,
			DoneReason: "stop",
			Metrics:    api.Metrics{PromptEvalCount: 10, EvalCount: 5},
		},
	}

	var got []string
	for _, response := range responses {
		for _, event := range conv.Process(response) {
			switch data := event.Data.(type) {
			case ContentBlockStartEvent:
				got = append(got, fmt.Sprintf("%s:%s:%d", event.Event, data.ContentBlock.Type, data.Index))
			case ContentBlockDeltaEvent:
				got = append(got, fmt.Sprintf("%s:%s:%d", event.Event, data.Delta.Type, data.Index))
			case ContentBlockStopEvent:
				got = append(got, fmt.Sprintf("%s:%d", event.Event, data.Index))
			default:
				got = append(got, event.Event)
			}
		}
	}

	want := []string{
		"message_start",
		"content_block_start:thinking:0",
		"content_block_delta:thinking_delta:0",
		"content_block_stop:0",
		"content_block_start:text:1",
		"content_block_delta:text_delta:1",
		"content_block_stop:1",
		// The second reasoning run — dropped entirely before the fix.
		"content_block_start:thinking:2",
		"content_block_delta:thinking_delta:2",
		"content_block_stop:2",
		"message_delta",
		"message_stop",
	}

	if diff := cmp.Diff(want, got); diff != "" {
		t.Fatalf("unexpected stream events (-want +got):\n%s", diff)
	}
}
