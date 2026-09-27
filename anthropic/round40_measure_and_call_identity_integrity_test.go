package anthropic

// round40_measure_and_call_identity_integrity_test.go — round 40's findings on
// this package:
//
//   - A40-1: the inline-binary clamp was applied to a value the walker could not
//     descend into, so a base64 payload nested inside a tool_result was charged
//     as its transport encoding — the estimate the local leg seeds the stream
//     converter with, off by the blob's size.
//   - A40-6: the per-message tool-call dedup was keyed only by the call's
//     identity, so a list naming the same id-less argument-less call twice —
//     which the caller's accumulator splits into two calls, the rule both legs
//     adopted in round 39 (B-F9) — collapsed back into one block on this wire.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// TestANestedBinaryIsHeldToTheAllowance is A40-1. The clamp walks the
// SERIALIZED value: a typed block (which is what the converter path hands it)
// used to be marshalled and then walked as a struct, where the inline image
// inside it is invisible, so the whole base64 payload was charged as prompt.
func TestANestedBinaryIsHeldToTheAllowance(t *testing.T) {
	request := func(data string) MessagesRequest {
		return MessagesRequest{
			Model: "m",
			Messages: []MessageParam{{
				Role: "user",
				Content: []ContentBlock{{
					Type: "tool_result",
					Content: []any{map[string]any{
						"type": "image",
						"source": map[string]any{
							"type":       "base64",
							"media_type": "image/png",
							"data":       data,
						},
					}},
				}},
			}},
		}
	}

	// The payload's SIZE is the variable, and the allowance is the fixed point:
	// two requests that differ only in the payload's length must differ in
	// their estimate by what a payload of that size costs the prompt — the
	// allowance — not by its transport encoding. This expectation is derived
	// from the two measurements rather than from this package's own arithmetic.
	empty := EstimateInputTokens(request(""))
	big := EstimateInputTokens(request(strings.Repeat("A", 100_000)))
	delta := big - empty
	if want := inlineImageByteAllowance / 4; delta != want {
		t.Errorf("a 100 000-byte base64 payload inside a tool_result raised the estimate by %d tokens, want %d (a %d-byte allowance, %d tokens): the payload is what the upstream's OWN image handling costs — the converter describes the block, it does not send the encoding — and charging the encoding estimated a turn at %s tokens, which is the number the local leg seeds the session's context meter with",
			delta, want, inlineImageByteAllowance, want, "25 000")
	}
}

// TestTwoIdenticalArgumentlessCallsInOneMessageStayTwoCalls is A40-6. A bare
// repeat inside one message is two calls — the accumulator's rule, and the one
// both legs adopted — and this wire has only the call itself as identity, so
// the dedup collapsed the pair into one block: one tool_use where the model
// asked for two, still reported as stop_reason "tool_use".
func TestTwoIdenticalArgumentlessCallsInOneMessageStayTwoCalls(t *testing.T) {
	conv := NewStreamConverter("msg_round40", "m", 10)
	events := conv.Process(api.ChatResponse{
		Model: "m",
		Message: api.Message{
			Role: "assistant",
			ToolCalls: []api.ToolCall{
				{Function: api.ToolCallFunction{Name: "get_time"}},
				{Function: api.ToolCallFunction{Name: "get_time"}},
			},
		},
	})

	ids := []string{}
	for _, e := range events {
		start, ok := e.Data.(ContentBlockStartEvent)
		if !ok || start.ContentBlock.Type != "tool_use" {
			continue
		}
		ids = append(ids, start.ContentBlock.ID)
	}
	if len(ids) != 2 {
		body, _ := json.Marshal(events)
		t.Fatalf("two identical id-less calls in one message produced %d tool_use blocks, want 2 (%s): the upstream asked for two calls, and the client can only make the one it was told about", len(ids), body)
	}
	if ids[0] == ids[1] || ids[0] == "" || ids[1] == "" {
		t.Errorf("the two blocks carry ids %q and %q: two calls with one id cannot be answered separately — a tool_result names its call by id — and an empty id is not an id", ids[0], ids[1])
	}
}

// TestARestatedCallInALaterTurnIsStillDeduped guards the other direction of
// A40-6: the counter is per Process call, so the dedup this map exists for —
// an upstream restating the same id-less call in a later message — must still
// drop it.
func TestARestatedCallInALaterTurnIsStillDeduped(t *testing.T) {
	conv := NewStreamConverter("msg_round40b", "m", 10)
	call := api.ChatResponse{
		Model: "m",
		Message: api.Message{
			Role:      "assistant",
			ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{Name: "get_time"}}},
		},
	}
	conv.Process(call)
	events := conv.Process(call)

	for _, e := range events {
		if start, ok := e.Data.(ContentBlockStartEvent); ok && start.ContentBlock.Type == "tool_use" {
			t.Fatalf("a restatement of an already-sent call in a LATER message emitted a second block (id %q): the per-message counter exists to split a bare repeat within one list, not to make one call two", start.ContentBlock.ID)
		}
	}
}
