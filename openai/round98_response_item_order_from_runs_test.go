package openai

// round98_response_item_order_from_runs_test.go — leg 1, F98-L1-3 (2026-09-29
// audit, round 98).
//
// The BUFFERED arm of the Responses wire used to state a fixed item order —
// reasoning, then the message, then the calls — whatever order the turn's own
// chunks had arrived in, while its streamed sibling announces and documents the
// items in the arrival order its events claimed (round 95, F95-L1-2). One
// upstream body therefore reached a streaming client and a non-streaming one
// with the message and the reasoning item swapped, or with the call in front of
// the prose that followed it. Measured on the same turn:
//
//	chunks [text A][thinking T]          streamed [message reasoning]  buffered [reasoning message]
//	chunks [text A][thinking T][text B]  streamed [message reasoning]  buffered [reasoning message]
//	chunks [thinking T][call][text A]    streamed [reasoning call message]  buffered [reasoning message call]
//
// The buffered arm reads the turn's ordered run list (api.Message.OutputRuns,
// the list the merge lane in server/routes.go builds and the Anthropic surface
// has read since round 79) and states each item where its FIRST run of that kind
// stands, with one function_call item per call run. The text and the reasoning of
// a turn are one item each on this wire — that is what the streamed arm's own
// terminal document states too (`buildFinalOutput` joins the runs of one kind),
// so the runs decide ORDER here, not the boundaries inside an item.
//
// A turn whose runs do not account for it (api.Message.OutputRunsAccountFor) is
// written as before — round 92's fixed order for the item set this arm can
// express, which is the deliberate exception round 95 recorded.

import (
	"testing"

	"github.com/ollama/ollama/api"
)

// r98Text, r98Thinking and r98Call are one-chunk pieces of a turn.
func r98Text(s string) api.ChatResponse {
	return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: s}}
}

func r98Thinking(s string) api.ChatResponse {
	return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Thinking: s}}
}

func r98Call() api.ChatResponse {
	return api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", ToolCalls: toolCallResponse("Read").Message.ToolCalls}}
}

// r98MergeTurn merges the chunks the way the buffered lane does and hands the
// turn the run list that same lane builds for it.
func r98MergeTurn(chunks []api.ChatResponse, runs []api.OutputRun) api.ChatResponse {
	merged := api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant"}}
	for _, ch := range chunks {
		merged.Message.Content += ch.Message.Content
		merged.Message.Thinking += ch.Message.Thinking
		merged.Message.ToolCalls = append(merged.Message.ToolCalls, ch.Message.ToolCalls...)
	}
	merged.Message.OutputRuns = runs
	if !merged.Message.OutputRunsAccountFor() {
		merged.Message.OutputRuns = nil
	}
	return merged
}

// A buffered turn states its items in the order its own runs state, which is the
// order the streaming arm announces them in and documents them in.
func TestTheBufferedResponsesArmStatesTheOrderItsRunsState(t *testing.T) {
	for _, tc := range []struct {
		name   string
		chunks []api.ChatResponse
		runs   []api.OutputRun
		want   []string
	}{
		{
			name:   "the prose, then the reasoning that follows it",
			chunks: []api.ChatResponse{r98Text("A"), r98Thinking("T")},
			runs:   []api.OutputRun{{Kind: "text", Text: "A"}, {Kind: "thinking", Text: "T"}},
			want:   []string{"message", "reasoning"},
		},
		{
			name:   "the reasoning, then the prose",
			chunks: []api.ChatResponse{r98Thinking("T"), r98Text("A")},
			runs:   []api.OutputRun{{Kind: "thinking", Text: "T"}, {Kind: "text", Text: "A"}},
			want:   []string{"reasoning", "message"},
		},
		{
			name:   "prose either side of the reasoning",
			chunks: []api.ChatResponse{r98Text("A"), r98Thinking("T"), r98Text("B")},
			runs:   []api.OutputRun{{Kind: "text", Text: "A"}, {Kind: "thinking", Text: "T"}, {Kind: "text", Text: "B"}},
			want:   []string{"message", "reasoning"},
		},
		{
			name:   "reasoning either side of the prose",
			chunks: []api.ChatResponse{r98Thinking("T1"), r98Text("A"), r98Thinking("T2")},
			runs:   []api.OutputRun{{Kind: "thinking", Text: "T1"}, {Kind: "text", Text: "A"}, {Kind: "thinking", Text: "T2"}},
			want:   []string{"reasoning", "message"},
		},
		{
			name:   "a call between two stretches of prose",
			chunks: []api.ChatResponse{r98Text("A"), r98Call(), r98Text("B")},
			runs:   []api.OutputRun{{Kind: "text", Text: "A"}, {Kind: "call"}, {Kind: "text", Text: "B"}},
			want:   []string{"message", "function_call"},
		},
		{
			name:   "reasoning, the call, then the prose that follows it",
			chunks: []api.ChatResponse{r98Thinking("T"), r98Call(), r98Text("A")},
			runs:   []api.OutputRun{{Kind: "thinking", Text: "T"}, {Kind: "call"}, {Kind: "text", Text: "A"}},
			want:   []string{"reasoning", "function_call", "message"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buffered := r95BufferedItemTypes(t, r98MergeTurn(tc.chunks, tc.runs))
			_, streamed := r95StreamedTurn(t, tc.chunks...)
			if !r95EqualOrder(buffered, streamed) {
				t.Errorf("one turn, two orders — the buffered arm states %v and the streamed arm's own terminal document states %v (2026-09-29 audit, round 98, F98-L1-3)",
					buffered, streamed)
			}
			if !r95EqualOrder(buffered, tc.want) {
				t.Errorf("the buffered arm states %v, want %v — the order the turn's runs state (2026-09-29 audit, round 98, F98-L1-3)",
					buffered, tc.want)
			}
		})
	}
}

// A turn with no runs to read — or a run list that does not account for it — is
// stated as round 92 pinned, which is the exception round 95 recorded.
func TestATurnWithNoRunsKeepsTheFixedOrder(t *testing.T) {
	chat := r98MergeTurn([]api.ChatResponse{r98Call(), r98Text("A")}, nil)
	if got, want := r95BufferedItemTypes(t, chat), []string{"message", "function_call"}; !r95EqualOrder(got, want) {
		t.Errorf("a turn carrying no runs stated %v, want the fixed %v (round 92, F92-L1-3)", got, want)
	}

	// A run list that does not add up to the turn (here: a call run short) is not
	// one this arm may trust.
	chat = r98MergeTurn([]api.ChatResponse{r98Text("A"), r98Thinking("T")},
		[]api.OutputRun{{Kind: "thinking", Text: "T"}, {Kind: "text", Text: "A"}, {Kind: "call"}})
	if chat.Message.OutputRuns != nil {
		t.Fatalf("premise: the run list accounts for the turn: %v", chat.Message.OutputRuns)
	}
	if got, want := r95BufferedItemTypes(t, chat), []string{"reasoning", "message"}; !r95EqualOrder(got, want) {
		t.Errorf("a turn whose runs do not account for it stated %v, want the fixed %v (2026-09-29 audit, round 98, F98-L1-3)", got, want)
	}
}
