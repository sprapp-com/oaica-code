package anthropic

// round79_document_arm_output_runs_integrity_test.go — leg 1, the document arm
// reads the ordered run list a buffered turn hands it (2026-09-28 audit,
// round 79, F79-L1-1).
//
// The two arms of this leg must answer one upstream body the same way. The
// streaming arm opens a block per run of the model's output — reasoning closes
// where prose or a call arrives, prose closes where reasoning or a call arrives
// — and the document arm has only merged fields to read, so it wrote one merged
// reasoning block first and lost where the reasoning sat. api.Message now
// carries the order as OutputRuns, and this arm prefers it to ContentRuns when
// it accounts for the turn exactly. The buffered lane's own side of this is
// pinned in server/round79_buffered_reasoning_order_integrity_test.go; these
// rows pin what the arm does with the list it is HANDED, including the lists it
// must refuse.

import (
	"testing"

	"github.com/ollama/ollama/api"
)

func r79Call() api.ToolCall {
	a := api.NewToolCallFunctionArguments()
	a.Set("cmd", "ls")
	return api.ToolCall{
		ID:       "call_1",
		Function: api.ToolCallFunction{Name: "Bash", Arguments: a},
	}
}

// TestTheDocumentArmWritesReasoningWhereItArrived is the pin: with the order in
// hand this arm answers the streaming twin's own block list.
func TestTheDocumentArmWritesReasoningWhereItArrived(t *testing.T) {
	resp := ToMessagesResponse("msg_r79", api.ChatResponse{
		DoneReason: "tool_calls",
		Message: api.Message{
			Thinking:  "AB",
			Content:   "prose",
			ToolCalls: []api.ToolCall{r79Call()},
			OutputRuns: []api.OutputRun{
				{Kind: "thinking", Text: "A"},
				{Kind: "call"},
				{Kind: "thinking", Text: "B"},
				{Kind: "text", Text: "prose"},
			},
		},
	})
	got := r77TurnOrder(t, resp)
	want := `<thinking><tool_use call_1><thinking><text "prose">`
	if got != want {
		t.Fatalf("the document arm wrote %s, want %s — reasoning either side of a call is two runs, and the streaming arm of this leg writes two (2026-09-28 audit, round 79, F79-L1-1)", got, want)
	}
}

// TestRunsThatDoNotAccountForTheTurnAreRefused: a reader may not trust a shape
// that does not add up, and falls back to the merged fields.
func TestRunsThatDoNotAccountForTheTurnAreRefused(t *testing.T) {
	for _, tc := range []struct {
		note string
		msg  api.Message
		want string
	}{
		{
			"the text runs do not join to the content",
			api.Message{
				Thinking:   "AB",
				Content:    "prose",
				OutputRuns: []api.OutputRun{{Kind: "thinking", Text: "A"}, {Kind: "text", Text: "not the content"}, {Kind: "thinking", Text: "B"}},
			},
			`<thinking><text "prose">`,
		},
		{
			"the thinking runs do not join to the reasoning",
			api.Message{
				Thinking:   "AB",
				Content:    "prose",
				OutputRuns: []api.OutputRun{{Kind: "thinking", Text: "A"}, {Kind: "text", Text: "prose"}, {Kind: "thinking", Text: "not the reasoning"}},
			},
			`<thinking><text "prose">`,
		},
		{
			"there are not as many call runs as calls",
			api.Message{
				Thinking:   "AB",
				Content:    "prose",
				ToolCalls:  []api.ToolCall{r79Call()},
				OutputRuns: []api.OutputRun{{Kind: "thinking", Text: "A"}, {Kind: "text", Text: "prose"}, {Kind: "thinking", Text: "B"}},
			},
			`<thinking><text "prose"><tool_use call_1>`,
		},
		{
			"a kind this reader does not know",
			api.Message{
				Thinking:   "AB",
				Content:    "prose",
				OutputRuns: []api.OutputRun{{Kind: "thinking", Text: "A"}, {Kind: "signature", Text: ""}, {Kind: "thinking", Text: "B"}, {Kind: "text", Text: "prose"}},
			},
			`<thinking><text "prose">`,
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			resp := ToMessagesResponse("msg_r79", api.ChatResponse{
				DoneReason: "stop",
				Message:    tc.msg,
			})
			if got := r77TurnOrder(t, resp); got != tc.want {
				t.Fatalf("a run list that does not account for the turn produced %s, want the merged reading %s (2026-09-28 audit, round 79, F79-L1-1)", got, tc.want)
			}
		})
	}
}

// TestATurnWithNoCallsStillKeepsItsReasoning: the order is about the output, not
// about the calls, so a call-free turn keeps it too.
func TestATurnWithNoCallsStillKeepsItsReasoning(t *testing.T) {
	resp := ToMessagesResponse("msg_r79", api.ChatResponse{
		DoneReason: "stop",
		Message: api.Message{
			Thinking:   "AB",
			Content:    "prose",
			OutputRuns: []api.OutputRun{{Kind: "thinking", Text: "A"}, {Kind: "text", Text: "prose"}, {Kind: "thinking", Text: "B"}},
		},
	})
	got := r77TurnOrder(t, resp)
	want := `<thinking><text "prose"><thinking>`
	if got != want {
		t.Fatalf("a call-free turn wrote %s, want %s (2026-09-28 audit, round 79, F79-L1-1)", got, want)
	}
}

// TestTheRunsKeepTheCallsOrderAndIdentity: the call runs are read in order and
// go through the same block builder as every other path, so a nameless entry's
// bytes are still the model's prose and a restatement is still one block.
func TestTheRunsKeepTheCallsOrderAndIdentity(t *testing.T) {
	second := r79Call()
	second.ID = "call_2"
	second.Function.Name = "Read"
	resp := ToMessagesResponse("msg_r79", api.ChatResponse{
		DoneReason: "tool_calls",
		Message: api.Message{
			Content:   "beforeafter",
			ToolCalls: []api.ToolCall{r79Call(), second},
			OutputRuns: []api.OutputRun{
				{Kind: "text", Text: "before"},
				{Kind: "call"},
				{Kind: "text", Text: "after"},
				{Kind: "call"},
			},
		},
	})
	got := r77TurnOrder(t, resp)
	want := `<text "before"><tool_use call_1><text "after"><tool_use call_2>`
	if got != want {
		t.Fatalf("two calls with prose between them wrote %s, want %s (2026-09-28 audit, round 79, F79-L1-1)", got, want)
	}
}
