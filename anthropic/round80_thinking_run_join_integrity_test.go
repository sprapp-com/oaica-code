package anthropic

// round80_thinking_run_join_integrity_test.go — leg 1, the document arm joins a
// thinking run to the reasoning block before it (2026-09-28 audit, round 80,
// F80-L1-1 / F80-L1-3).
//
// Round 79 taught this arm to read the lane's ordered run list, and it opened a
// fresh thinking block at every thinking run. But a run boundary is where a
// BLOCK opens on the streaming arm, and the streaming arm opens no block for
// reasoning that is merely resumed after an entry it could not write — a
// fragment the upstream never named, a restatement of a call already written.
// There, the thinking block stays open and the reasoning arrives as one block.
// The document arm split what the streaming arm kept whole, which is a
// regression round 79 introduced: reading the merged fields, this arm produced
// the streaming arm's own answer.
//
// The join is the reasoning twin of the text join round 76 pinned in addBlock,
// and it is also what settles the trust question a hostile list raises: a list
// whose two adjacent thinking runs no chunk sequence can produce now reads as
// the one block every producer makes of it, rather than as two.

import (
	"testing"

	"github.com/ollama/ollama/api"
)

// TestReasoningJoinsTheBlockBeforeIt: a thinking run after a thinking block
// extends it, because nothing between them wrote a block.
func TestReasoningJoinsTheBlockBeforeIt(t *testing.T) {
	resp := ToMessagesResponse("msg_r80", api.ChatResponse{
		DoneReason: "stop",
		Message: api.Message{
			Thinking: "TT2",
			OutputRuns: []api.OutputRun{
				{Kind: "thinking", Text: "T"},
				// A run boundary the lane records but no arm writes a block for:
				// its entry named no call, so nothing opens here.
				{Kind: "call"},
				{Kind: "thinking", Text: "T2"},
			},
		},
	})
	if got := r77TurnOrder(t, resp); got != `<thinking>` {
		t.Fatalf("reasoning resumed across a blockless entry wrote %s, want one thinking block (2026-09-28 audit, round 80, F80-L1-1)", got)
	}
	if len(resp.Content) != 1 || resp.Content[0].Thinking == nil || *resp.Content[0].Thinking != "TT2" {
		t.Fatalf("the joined block holds %v, want one block of T+T2 (2026-09-28 audit, round 80, F80-L1-1)", resp.Content)
	}
}

// TestAListNoProducerCanWriteReadsAsOneBlock is F80-L1-3: the unproducible
// order is not refused, it is joined — the same rule, because the live lane
// reaches the same state through a call run that writes no block.
func TestAListNoProducerCanWriteReadsAsOneBlock(t *testing.T) {
	resp := ToMessagesResponse("msg_r80", api.ChatResponse{
		DoneReason: "stop",
		Message: api.Message{
			Thinking: "AB",
			OutputRuns: []api.OutputRun{
				{Kind: "thinking", Text: "A"},
				{Kind: "thinking", Text: "B"},
			},
		},
	})
	if got := r77TurnOrder(t, resp); got != `<thinking>` {
		t.Fatalf("two adjacent thinking runs wrote %s, want one thinking block — no chunk sequence produces two (2026-09-28 audit, round 80, F80-L1-3)", got)
	}
	if len(resp.Content) != 1 || resp.Content[0].Thinking == nil || *resp.Content[0].Thinking != "AB" {
		t.Fatalf("the joined block holds %v, want one block of AB (2026-09-28 audit, round 80, F80-L1-3)", resp.Content)
	}
}

// TestReasoningResumesAfterAWrittenBlock: the join is not a merge of everything
// — reasoning that follows a block the arm DID write opens a new thinking block,
// which is what the streaming arm does where it closes one.
func TestReasoningOpensAgainAfterANamedCall(t *testing.T) {
	resp := ToMessagesResponse("msg_r80", api.ChatResponse{
		DoneReason: "tool_calls",
		Message: api.Message{
			Thinking:  "TT2",
			ToolCalls: []api.ToolCall{r79Call()},
			OutputRuns: []api.OutputRun{
				{Kind: "thinking", Text: "T"},
				{Kind: "call"},
				{Kind: "thinking", Text: "T2"},
			},
		},
	})
	got := r77TurnOrder(t, resp)
	want := `<thinking><tool_use call_1><thinking>`
	if got != want {
		t.Fatalf("reasoning either side of a named call wrote %s, want %s (2026-09-28 audit, round 80, F80-L1-1)", got, want)
	}
}
