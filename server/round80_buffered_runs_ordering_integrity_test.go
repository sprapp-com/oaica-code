package server

// round80_buffered_runs_ordering_integrity_test.go — leg 1, the buffered lane's
// run list has to describe BLOCKS, not entries (2026-09-28 audit, round 80,
// F80-L1-1 / F80-L1-2).
//
// Round 79 gave this lane an ordered run list so the document arm could write
// the streaming arm's block structure. Two shapes it recorded were not that
// structure:
//
//   - every entry of ToolCalls became a "call" run, including one the upstream
//     never named and which therefore writes NO block. A call run that writes no
//     block is a boundary the streaming arm does not have, so reasoning either
//     side of it — or either side of a restatement of a call already written —
//     was split into two thinking blocks on the document arm and left in one on
//     the streaming arm. Pre-79 the merged fields gave the streaming arm's own
//     answer, so this was a regression.
//   - consecutive "call" runs carry no text and so always merged into one. The
//     list then claimed one call where the turn had two, failed the lane's own
//     accounting check, and was discarded — leaving the parallel-call shape (the
//     one agentic clients hit most) on the pre-round-79 reading, which is the
//     very divergence round 79 existed to close.
//
// The lane now keeps one call run per entry, and the document arm joins a
// thinking run to the thinking block before it — the reasoning twin of the text
// join round 76 pinned — so a run boundary that writes no block leaves the
// reasoning in one block, exactly as the streaming arm's open block does.

import (
	"testing"

	"github.com/ollama/ollama/api"
)

// TestTwoCallsInOneChunkKeepTwoRunBoundaries is F80-L1-2: a chunk carrying two
// parallel calls is two call runs, and the list accounts for the turn.
func TestTwoCallsInOneChunkKeepTwoRunBoundaries(t *testing.T) {
	second := r75OneCall()
	second.ID = "call_2"
	second.Function.Name = "Read"

	doc, stream := r79BothArms(t, []api.ChatResponse{
		{Message: api.Message{Thinking: "T", ToolCalls: []api.ToolCall{r75OneCall(), second}}},
		{Message: api.Message{Thinking: "T2"}},
	})
	want := `<thinking "T"><tool_use call_1><tool_use call_2><thinking "T2">`
	if doc != want {
		t.Errorf("two parallel calls in one chunk answered %s, want %s (2026-09-28 audit, round 80, F80-L1-2)", doc, want)
	}
	_ = stream
}

// TestTwoCallsInAdjacentChunksKeepTwoRunBoundaries: the same shape spread over
// two chunks, where the merge happened across chunks rather than inside one.
func TestTwoCallsInAdjacentChunksKeepTwoRunBoundaries(t *testing.T) {
	second := r75OneCall()
	second.ID = "call_2"
	second.Function.Name = "Read"

	doc, stream := r79BothArms(t, []api.ChatResponse{
		{Message: api.Message{ToolCalls: []api.ToolCall{r75OneCall()}}},
		{Message: api.Message{ToolCalls: []api.ToolCall{second}}},
		{Message: api.Message{Thinking: "T"}},
	})
	want := `<tool_use call_1><tool_use call_2><thinking "T">`
	if doc != want {
		t.Errorf("two calls in adjacent chunks answered %s, want %s (2026-09-28 audit, round 80, F80-L1-2)", doc, want)
	}
	_ = stream
}

// TestAnEntryThatWritesNoBlockDoesNotSplitTheReasoning is F80-L1-1: a nameless
// entry with no arguments writes no block on either arm, so the reasoning either
// side of it is one block.
func TestAnEntryThatWritesNoBlockDoesNotSplitTheReasoning(t *testing.T) {
	doc, stream := r79BothArms(t, []api.ChatResponse{
		{Message: api.Message{Thinking: "T"}},
		{Message: api.Message{ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{}}}}},
		{Message: api.Message{Thinking: "T2"}},
	})
	want := `<thinking "TT2">`
	if doc != want {
		t.Errorf("reasoning across an entry that writes no block answered %s, want %s (2026-09-28 audit, round 80, F80-L1-1)", doc, want)
	}
	_ = stream
}

// TestARestatementDoesNotSplitTheReasoning: the same boundary with no nameless
// entry at all — a call restated between two runs of reasoning writes no second
// block, so it closes nothing.
func TestARestatementDoesNotSplitTheReasoning(t *testing.T) {
	doc, stream := r79BothArms(t, []api.ChatResponse{
		{Message: api.Message{ToolCalls: []api.ToolCall{r75OneCall()}}},
		{Message: api.Message{Thinking: "T", ToolCalls: []api.ToolCall{r75OneCall()}}},
		{Message: api.Message{Thinking: "T2"}},
	})
	want := `<tool_use call_1><thinking "TT2">`
	if doc != want {
		t.Errorf("reasoning across a restatement answered %s, want %s (2026-09-28 audit, round 80, F80-L1-1)", doc, want)
	}
	_ = stream
}
