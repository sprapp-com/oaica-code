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

// r40LaterCall is one ChatResponse carrying the same id-less argument-less
// call, used by the two tests below to mean "the next Process call".
func r40LaterCall() api.ChatResponse {
	return api.ChatResponse{
		Model: "m",
		Message: api.Message{
			Role:      "assistant",
			ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{Name: "get_time"}}},
		},
	}
}

// r40StreamBlocks runs a sequence of ChatResponses through one converter (one
// per Process call, which is what the runners do — see below) and returns the
// ids of the tool_use blocks it opened.
func r40StreamBlocks(t *testing.T, conv *StreamConverter, seq ...api.ChatResponse) []string {
	t.Helper()
	var ids []string
	for _, r := range seq {
		for _, e := range conv.Process(r) {
			if start, ok := e.Data.(ContentBlockStartEvent); ok && start.ContentBlock.Type == "tool_use" {
				ids = append(ids, start.ContentBlock.ID)
			}
		}
	}
	return ids
}

// TestABareRepeatInALaterChunkIsStillASecondCall REVERSES round 40's
// TestARestatedCallInALaterTurnIsStillDeduped, and records why (2026-09-28
// audit, round 67, F67-L1-1).
//
// The reversed test's premise was that a second Process call means a later
// MESSAGE. It does not, and nothing in the tree makes it so: Process is called
// once per upstream CHUNK of the one response — middleware/anthropic.go:87 loops
// it over every chunk the local server writes, and the client proxy does the
// same at cmd/launch/anthropic_openai_proxy.go:3032. The one place in the tree
// that converts a genuinely different turn — the web_search follow-up — does it
// with anthropic.ToMessagesResponse (middleware/anthropic.go combineServerAndFinalContent),
// whose counter is fresh per call, i.e. per turn; the follow-up's own chunks
// never reach a converter. So there is no wire on which this converter sees two
// turns, and "a bare repeat in a later Process call" is exactly the same event
// as "a bare repeat inside one list": the runner's flush boundary is not
// something the model asked for. Born per chunk, the counter therefore made the
// answer depend on that boundary — the same model output answered two tool_use
// blocks whole (server/routes.go hands the non-stream arm the concatenation of
// those same chunks) and one streamed, and the client never ran the call the
// model asked for twice, on a turn still reported stop_reason "tool_use".
// Measured on this arm alone: 272 random chunkings of one model output, 64
// divergent; 0 after the counter was made converter state. Both other legs and
// this leg's own non-stream twin keep the second call, so this arm was the
// outlier.
//
// The dedup this map exists for still holds, one Process call or many, for a
// call restated under a STATED id — pinned by the test below.
func TestABareRepeatInALaterChunkIsStillASecondCall(t *testing.T) {
	conv := NewStreamConverter("msg_round40b", "m", 10)
	ids := r40StreamBlocks(t, conv, r40LaterCall(), r40LaterCall())

	ns := r45NonStreamBlocks(t, r45Chat(r45Call("get_time", ""), r45Call("get_time", "")))
	if len(ids) != len(ns) {
		t.Errorf("one model output chunked as two Process calls made %d tool_use block(s) on the stream arm (%v) and %d on the non-stream arm: the two runners hand this converter the SAME chunks — one as they arrive, the other concatenated — so a bare repeat cannot depend on where the flush landed (2026-09-28 audit, round 67, F67-L1-1)", len(ids), ids, len(ns))
	}
	if len(ids) == 2 && ids[0] == ids[1] {
		t.Errorf("both blocks carry the id %q: two calls with one id cannot be answered separately", ids[0])
	}
}

// TestARestatedStatedCallIsStillDeduped guards the direction of the dedup that
// survives: an upstream restating the same STATED id is that call restated, not
// a second one — which is what this converter's map is for, and what the
// client's accumulator and the non-stream twin also read it as.
func TestARestatedStatedCallIsStillDeduped(t *testing.T) {
	stated := api.ChatResponse{
		Model: "m",
		Message: api.Message{
			Role: "assistant",
			ToolCalls: []api.ToolCall{{
				ID:       "call_x",
				Function: api.ToolCallFunction{Name: "get_time"},
			}},
		},
	}
	conv := NewStreamConverter("msg_round40c", "m", 10)
	if ids := r40StreamBlocks(t, conv, stated); len(ids) != 1 {
		t.Fatalf("premise: the stated call must open one block, got %v", ids)
	}
	if ids := r40StreamBlocks(t, conv, stated); len(ids) != 0 {
		t.Errorf("a restatement of an already-sent call under the SAME stated id opened %v: one call restated is one call, on every arm", ids)
	}
	// Control: the same id used for a DIFFERENT call is still a second call
	// (round 45's A45-3), so this dedup is not "one id, one block".
	reused := stated
	reused.Message.ToolCalls = []api.ToolCall{
		{ID: "call_y", Function: api.ToolCallFunction{Name: "Bash", Arguments: r66Args(t, `{"cmd":"ls"}`)}},
		{ID: "call_y", Function: api.ToolCallFunction{Name: "Read", Arguments: r66Args(t, `{"p":1}`)}},
	}
	conv2 := NewStreamConverter("msg_round40d", "m", 10)
	if ids := r40StreamBlocks(t, conv2, reused); len(ids) != 2 {
		t.Errorf("one id stated for two different calls opened %d block(s) (%v): the second call is not a repeat of the first", len(ids), ids)
	}
}
