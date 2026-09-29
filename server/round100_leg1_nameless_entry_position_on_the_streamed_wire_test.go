package server

// round100_leg1_nameless_entry_position_on_the_streamed_arm_test.go — leg 1,
// round 100 (2026-09-29 audit), F100-L1-1.
//
// One chunk that carries a NAMED entry and then a nameless one is two items on
// every arm of the Responses surface: the call, and the bytes of the entry the
// upstream never named, relayed as the model's own text at the position the
// entry stood (round 98's F98-L1-1, round 99's F99-L1-1). The buffered arm states
// them in that order — it reads the ordered run list the merge lane built — and
// so does the Anthropic arm of the same handler, whose blocks are
// [tool_use text].
//
// The STREAMED arm did not. A streamed chunk carries no run list (server/routes.go
// states one on the buffered lane only), and the translated-surface pass that
// folds the nameless entry into the turn's prose appended the bytes to the END of
// the chunk's text instead of to the place the entry held — so the chunk's message
// item was announced before every call of that chunk:
//
//	one chunk [Bash call_a {"cmd":"ls"}][nameless {"cmd":"rm -rf /"}]
//	  anthropic   [tool_use text]
//	  responses buffered [function_call message]   streamed [message function_call]
//
// A client that reads the item order — the order the wire exists to state — sees
// the model's call after the text that came from a later entry, and on a chunk
// whose calls are all preceded by a nameless entry sees it after text that
// belonged to none of them. This pin states the order on both arms and the
// reference the Anthropic arm writes.
//
// `model/parsers/cohere.go` produces the shape: a `parser_actions` array whose
// second member states parameters but no tool_name reaches the runner as
// [Bash, nameless] and both entries are stated in ONE Done chunk (server/routes.go
// appends every call of one upstream Add to one response).

import (
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/llm"
)

// zz100Nameless is an entry the upstream never named, carrying the model's own
// bytes — the second member of the producer's `parser_actions` array.
func zz100Nameless(id, args string) api.ToolCall {
	return api.ToolCall{ID: id, Function: api.ToolCallFunction{Name: "", Arguments: zzArgs("cmd", args)}}
}

// zz100Call is a named entry, the shape every other arm reads as a call.
func zz100Call(id, name, args string) api.ToolCall {
	return api.ToolCall{ID: id, Function: api.ToolCallFunction{Name: name, Arguments: zzArgs("cmd", args)}}
}

// TestMine100TheStreamedResponsesArmStatesANamelessEntryWhereItStood is
// F100-L1-1: the streamed arm of the Responses surface states the same items in
// the same order as the buffered arm of the same handler, and the order is the
// order the entries stood in.
func TestMine100TheStreamedResponsesArmStatesANamelessEntryWhereItStood(t *testing.T) {
	done := llm.ChatResponse{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop, PromptEvalCount: 5, EvalCount: 3}
	for i, turn := range []struct {
		name   string
		chunks []llm.ChatResponse
		want   []string
	}{
		{
			"a nameless entry after the call of the same chunk",
			[]llm.ChatResponse{{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{
				zz100Call("call_a", "Bash", "ls"), zz100Nameless("1", "rm -rf /"),
			}}}},
			[]string{"function_call", `message:{"cmd":"rm -rf /"}`},
		},
		{
			"a nameless entry between two calls of the same chunk",
			[]llm.ChatResponse{{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{
				zz100Call("call_a", "Bash", "ls"), zz100Nameless("1", "rm -rf /"), zz100Call("call_b", "Read", "x"),
			}}}},
			[]string{"function_call", `message:{"cmd":"rm -rf /"}`, "function_call"},
		},
		{
			"a call after the nameless entry of the same chunk",
			[]llm.ChatResponse{{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{
				zz100Nameless("1", "rm -rf /"), zz100Call("call_a", "Bash", "ls"),
			}}}},
			[]string{`message:{"cmd":"rm -rf /"}`, "function_call"},
		},
	} {
		chunks := append(append([]llm.ChatResponse{}, turn.chunks...), done)
		model := "zz-100n-" + string(rune('a'+i))
		_, doc, _ := zzRunChunks(t, chunks, "responses", model, false)
		_, str, _ := zzRunChunks(t, chunks, "responses", model, true)
		buffered, streamed := zzOrderDump(t, doc, false), zzOrderDump(t, str, true)
		if len(buffered) == 0 || len(streamed) == 0 {
			t.Fatalf("premise: %s answered %d/%d items", turn.name, len(buffered), len(streamed))
		}
		want := strings.Join(turn.want, "|")
		if strings.Join(buffered, "|") != want {
			t.Errorf("%s: the buffered arm states %v, want %v", turn.name, buffered, turn.want)
		}
		if strings.Join(streamed, "|") != want {
			t.Errorf("%s: the streamed arm of the Responses surface states %v, want %v — a nameless entry's bytes are the model's own output and stand where the entry stood, before the text of anything that came after it, which is the order the buffered arm of this same handler states and the order the Anthropic arm writes ([tool_use text]) (2026-09-29 audit, round 100, F100-L1-1)",
				turn.name, streamed, turn.want)
		}
	}
}

// TestMine100TheNamelessEntrysPlaceIsTheReferenceEveryArmWrites is the premise
// the pin above is measured against, end to end: the Anthropic arm of the same
// handler writes the call first and the entry's bytes second, on both arms, for
// the same chunk.
func TestMine100TheNamelessEntrysPlaceIsTheReferenceEveryArmWrites(t *testing.T) {
	chunks := []llm.ChatResponse{
		{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{
			zz100Call("call_a", "Bash", "ls"), zz100Nameless("1", "rm -rf /"),
		}}},
		{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop, PromptEvalCount: 5, EvalCount: 3},
	}
	for _, stream := range []bool{false, true} {
		_, anthropic, _ := zzRunChunks(t, chunks, "anthropic", "zz-100r-a", stream)
		call := strings.Index(anthropic, "tool_use")
		if call < 0 {
			t.Fatalf("premise: the Anthropic arm answered no tool_use on stream=%v: %s", stream, anthropic)
		}
		var text int
		for _, m := range zz99TextRe.FindAllStringSubmatch(anthropic, -1) {
			if strings.Contains(m[1], "rm -rf /") {
				text = strings.Index(anthropic, m[0])
				break
			}
		}
		if text < 0 || text < call {
			t.Fatalf("premise: the Anthropic arm states the entry's bytes at %d and the call at %d on stream=%v", text, call, stream)
		}
	}
}
