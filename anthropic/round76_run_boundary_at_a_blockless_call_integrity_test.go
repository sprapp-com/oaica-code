package anthropic

// round76_run_boundary_at_a_blockless_call_integrity_test.go — the run
// boundary belongs to the BLOCK, not to the entry (2026-09-28 audit, round 76,
// F76-L1-1 and F76-L1-2).
//
// Round 75 gave this arm the buffered turn's run boundaries (ContentRuns) so it
// could write the prose around a call in the order the model produced it. The
// producer opens a run at every chunk that carries a call — but not every call
// becomes a block: a fragment the upstream never named is held back, and a
// restatement of a call already written is the same block, not a second one.
// Where the streaming arm writes no tool_use it never closed its text block, so
// the prose stayed in ONE block; this arm split it at the boundary and answered
// two text blocks for a turn its streaming twin answered with one — the arm
// that wrote fewer blocks was the one that split the prose.
//
// A real producer reaches the first shape byte-granularly: the laguna (a.k.a.
// poolside-v1) builtin parser leaves `function.name` empty when the model's
// output carries no `<function=…>` header, and server/routes.go mints an id for
// that call and puts it on the channel like any other, so the buffered lane
// records a boundary at a call this arm writes nothing for.
//
// The rule is now stated once: runs are joined across every call that writes no
// block, and a text block is closed only where a tool_use block is opened. Both
// tests below go RED if the interleave stops joining them.

import (
	"testing"

	"github.com/ollama/ollama/api"
)

// r76TextBlock is one text block's own text.
func r76TextBlock(t *testing.T, resp MessagesResponse) []string {
	t.Helper()
	var out []string
	for _, b := range resp.Content {
		if b.Type == "text" {
			text := ""
			if b.Text != nil {
				text = *b.Text
			}
			out = append(out, text)
		}
	}
	return out
}

// TestACallThatWritesNoBlockDoesNotSplitTheText is the F76-L1-1 pin: the turn
// whose only call is a fragment the upstream never named carries ONE text block,
// which is what the streaming arm writes for the same chunks.
func TestACallThatWritesNoBlockDoesNotSplitTheText(t *testing.T) {
	args := api.NewToolCallFunctionArguments()
	args.Set("cmd", "ls")
	resp := ToMessagesResponse("msg_r76", api.ChatResponse{
		DoneReason: "stop",
		Message: api.Message{
			Content: "Let me check the tree.\n\nNow I wait for the result.",
			ToolCalls: []api.ToolCall{{
				ID:       "call_x",
				Function: api.ToolCallFunction{Name: "", Arguments: args},
			}},
			ContentRuns: []string{"Let me check the tree.\n", "\nNow I wait for the result."},
		},
	})
	got := r76TextBlock(t, resp)
	if len(got) != 1 || got[0] != "Let me check the tree.\n\nNow I wait for the result." {
		t.Fatalf("a call that writes no block split the turn's prose into %q — the streaming arm writes one block for this turn, because it closes a text block only where it opens a tool_use one (2026-09-28 audit, round 76, F76-L1-1)", got)
	}
	if len(resp.Content) != 1 {
		t.Fatalf("the turn wrote %d block(s): %v", len(resp.Content), resp.Content)
	}
}

// TestARestatementThatWritesNoBlockDoesNotSplitTheText is the F76-L1-2 pin: a
// call listed a second time is folded by this arm (it is the block the client
// already has), and the prose around it stays whole.
func TestARestatementThatWritesNoBlockDoesNotSplitTheText(t *testing.T) {
	args := api.NewToolCallFunctionArguments()
	args.Set("cmd", "ls")
	call := api.ToolCall{ID: "call_restated", Function: api.ToolCallFunction{Name: "Bash", Arguments: args}}
	resp := ToMessagesResponse("msg_r76", api.ChatResponse{
		DoneReason: "tool_calls",
		Message: api.Message{
			Content:     "Let me check the tree.Now I wait for the result.",
			ToolCalls:   []api.ToolCall{call, call},
			ContentRuns: []string{"Let me check the tree.", "Now I wait ", "for the result."},
		},
	})
	var types []string
	for _, b := range resp.Content {
		types = append(types, b.Type)
	}
	if len(types) != 3 || types[0] != "text" || types[1] != "tool_use" || types[2] != "text" {
		t.Fatalf("a restatement split the turn into %v — the block it restates is already written, so no boundary belongs there (2026-09-28 audit, round 76, F76-L1-2)", types)
	}
	got := r76TextBlock(t, resp)
	if len(got) != 2 || got[0] != "Let me check the tree." || got[1] != "Now I wait for the result." {
		t.Fatalf("the prose around a restatement came out as %q", got)
	}
}
