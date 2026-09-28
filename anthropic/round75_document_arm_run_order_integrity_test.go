package anthropic

// round75_document_arm_run_order_integrity_test.go — the document arm writes
// the blocks in the order the model wrote them when the turn still knows that
// order (2026-09-28 audit, round 75, F75-L1-1).
//
// api.Message merges a buffered turn's text into one string, so this arm could
// only ever write the call AFTER all of it: a turn the streaming arm delivers
// as text/call/text reached a client that asked for no stream as one text block
// holding prose the model wrote after the call, then the call. The buffered lane
// hands the boundaries over in ContentRuns, and this arm interleaves when — and
// only when — the runs account for the content exactly.
//
// The producer of an interleaved turn is real and byte-granular: the
// qwen3-coder builtin parser, fed one byte at a time, releases the call in its
// own step between two runs of text. The server's own buffered lane is pinned
// separately (server/round75_buffered_turn_run_order_integrity_test.go).

import (
	"testing"

	"github.com/ollama/ollama/api"
)

func r75RunsCall() api.ToolCall {
	a := api.NewToolCallFunctionArguments()
	a.Set("cmd", "ls")
	return api.ToolCall{
		ID:       "call_a",
		Function: api.ToolCallFunction{Name: "Bash", Arguments: a},
	}
}

// r75BlockTypes is the block list with each text block's own text.
func r75BlockTypes(t *testing.T, resp MessagesResponse) []string {
	t.Helper()
	var out []string
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text := ""
			if b.Text != nil {
				text = *b.Text
			}
			out = append(out, "text:"+text)
		case "thinking":
			out = append(out, "thinking")
		default:
			out = append(out, b.Type)
		}
	}
	return out
}

// TestTheDocumentArmInterleavesTheTextAroundTheCall is the pin: with the run
// boundaries in hand, this arm answers the same block list its streaming twin
// writes for the same turn.
func TestTheDocumentArmInterleavesTheTextAroundTheCall(t *testing.T) {
	resp := ToMessagesResponse("msg_r75", api.ChatResponse{
		DoneReason: "tool_calls",
		Message: api.Message{
			Content:     "Let me check the tree.Now I wait for the result.",
			ToolCalls:   []api.ToolCall{r75RunsCall()},
			ContentRuns: []string{"Let me check the tree.", "Now I wait for the result."},
		},
	})
	got := r75BlockTypes(t, resp)
	want := []string{"text:Let me check the tree.", "tool_use", "text:Now I wait for the result."}
	if len(got) != len(want) {
		t.Fatalf("the document arm wrote %v, want %v — the streaming arm of this leg writes the call between the two runs, and one upstream body may not reach a client in two different shapes (2026-09-28 audit, round 75, F75-L1-1)",
			got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d is %q, want %q (2026-09-28 audit, round 75, F75-L1-1)", i, got[i], want[i])
		}
	}
	if resp.StopReason != "tool_use" {
		t.Errorf("the turn's stop_reason is %q, want tool_use", resp.StopReason)
	}
}

// TestRunsThatDoNotAccountForTheContentAreIgnored: a reader may not trust a
// shape that does not add up, and falls back to the merged content.
func TestRunsThatDoNotAccountForTheContentAreIgnored(t *testing.T) {
	resp := ToMessagesResponse("msg_r75", api.ChatResponse{
		DoneReason: "tool_calls",
		Message: api.Message{
			Content:     "Let me check the tree.Now I wait for the result.",
			ToolCalls:   []api.ToolCall{r75RunsCall()},
			ContentRuns: []string{"not the content", "nor this"},
		},
	})
	got := r75BlockTypes(t, resp)
	want := []string{"text:Let me check the tree.Now I wait for the result.", "tool_use"}
	if len(got) != len(want) {
		t.Fatalf("runs that do not join to the content changed the block list: %v, want the merged reading %v (2026-09-28 audit, round 75, F75-L1-1)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d is %q, want %q (2026-09-28 audit, round 75, F75-L1-1)", i, got[i], want[i])
		}
	}
}

// TestRunsOfTheWrongLengthAreIgnored: the shape is one run per call plus one.
func TestRunsOfTheWrongLengthAreIgnored(t *testing.T) {
	resp := ToMessagesResponse("msg_r75", api.ChatResponse{
		DoneReason: "tool_calls",
		Message: api.Message{
			Content:     "beforeafter",
			ToolCalls:   []api.ToolCall{r75RunsCall()},
			ContentRuns: []string{"beforeafter"},
		},
	})
	got := r75BlockTypes(t, resp)
	if len(got) != 2 {
		t.Fatalf("a run list shorter than the calls changed the block list: %v, want the merged reading (2026-09-28 audit, round 75, F75-L1-1)", got)
	}
}

// TestTwoCallsKeepTheirRuns: the interleaving is per call, not just at the ends.
func TestTwoCallsKeepTheirRuns(t *testing.T) {
	second := r75RunsCall()
	second.ID = "call_b"
	second.Function.Name = "Read"
	resp := ToMessagesResponse("msg_r75", api.ChatResponse{
		DoneReason: "tool_calls",
		Message: api.Message{
			Content:     "one two three",
			ToolCalls:   []api.ToolCall{r75RunsCall(), second},
			ContentRuns: []string{"one ", "two ", "three"},
		},
	})
	got := r75BlockTypes(t, resp)
	want := []string{"text:one ", "tool_use", "text:two ", "tool_use", "text:three"}
	if len(got) != len(want) {
		t.Fatalf("two interleaved calls wrote %v, want %v (2026-09-28 audit, round 75, F75-L1-1)", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Errorf("block %d is %q, want %q (2026-09-28 audit, round 75, F75-L1-1)", i, got[i], want[i])
		}
	}
}

// TestATurnWithNoCallsIsUnchanged: runs with no calls are the merged reading.
func TestATurnWithNoCallsIsUnchanged(t *testing.T) {
	resp := ToMessagesResponse("msg_r75", api.ChatResponse{
		DoneReason: "stop",
		Message: api.Message{
			Content:     "just prose",
			ContentRuns: []string{"just prose"},
		},
	})
	got := r75BlockTypes(t, resp)
	if len(got) != 1 || got[0] != "text:just prose" {
		t.Fatalf("a call-free turn with runs wrote %v, want one text block (2026-09-28 audit, round 75, F75-L1-1)", got)
	}
}
