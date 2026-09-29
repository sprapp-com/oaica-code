package server

// round99_leg1_nameless_position_runs_and_restatement_test.go — leg 1, round 99
// (2026-09-29 audit). Four findings, all on the two translated OpenAI surfaces,
// all measured against the Anthropic arm of the same handler as the reference.
//
// F99-L1-1. The round-98 fold (F98-L1-1) relays an entry the upstream never
// named as the model's own text, but it did so by appending the bytes to the
// turn's text. On the STREAMED arm that is the right place — the chunk the entry
// arrived in carries its own text, and the bytes are one more text delta at that
// point. On the BUFFERED arm the writer is handed the turn the merge lane
// already merged into one string, so the append put the bytes after prose that
// arrived LATER than the entry:
//
//	chunks [text A][nameless {"cmd":"ls"}][text B]
//	  anthropic  A{"cmd":"ls"}B   (both arms — the reference)
//	  openai     buffered AB{"cmd":"ls"}   streamed A{"cmd":"ls"}B
//
// F99-L1-2. The fold also stopped the turn's ordered run list
// (api.Message.OutputRuns) from accounting for the turn, because the list is
// built over the raw entries by the merge lane and one of them had just been
// dropped — so the Responses arm fell back to the fixed order and round 98's
// F98-L1-3 came back for every turn carrying a nameless entry, a shape round
// 98's own order pins do not carry:
//
//	chunks [text A][nameless][thinking T]
//	  responses  buffered [reasoning message]  streamed [message reasoning]
//
// F99-L1-3. The run list's call gate asks for a declared tool while the
// accumulation below it was widened to the translated surfaces in round 92, so a
// client that declared no tools but whose upstream states a call got the call and
// no run to place it — the same fallback, on a shape round 92 pinned as relayed:
//
//	chunks [call Bash][text A]   buffered [message function_call]  streamed [function_call message]
//
// F99-L1-4. The mint mirror took the Anthropic arm's owner rule (an id reused
// for a DIFFERENT call is re-minted) but not its restatement rule (`sentKey`): a
// call the upstream stated the same id for and restated — the shape round 45's
// A45-3 and round 80's F80-L1-1 were written for, and one `model/parsers/cohere.go`
// produces when the model repeats an action of its own `actions` array — reached
// an OpenAI client as two calls under one id, where the Anthropic arm writes one
// block:
//
//	two identical entries under id "call_x"   anthropic one tool_use, openai/responses [call_x call_x]

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/llm"
)

// zz99EqualOrder reports whether two stated orders are the same list.
func zz99EqualOrder(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// zz99TextRe finds the text fields of the Anthropic wire, both arms (the
// buffered body and the streamed deltas carry the same `"text"` key).
var zz99TextRe = regexp.MustCompile(`"text":"((?:[^"\\]|\\.)*)"`)

// zz99Text answers the joined prose one OpenAI surface stated, either arm.
func zz99Text(t *testing.T, surface, body string, stream bool) string {
	t.Helper()
	switch surface {
	case "openai":
		text, _, _ := zzCallIDs(t, body, stream)
		return text
	default:
		text, _ := zzResponseNames(t, body, stream)
		return text
	}
}

// zz99ResponsesOrder answers the item types the Responses wire stated, in order.
func zz99ResponsesOrder(body string, stream bool) []string {
	var items []any
	if stream {
		for _, line := range strings.Split(body, "\n") {
			d, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
			if !ok {
				continue
			}
			var ev struct {
				Type     string         `json:"type"`
				Response map[string]any `json:"response"`
			}
			if json.Unmarshal([]byte(d), &ev) != nil || ev.Type != "response.completed" {
				continue
			}
			items, _ = ev.Response["output"].([]any)
		}
	} else {
		var raw struct {
			Output []any `json:"output"`
		}
		if json.Unmarshal([]byte(body), &raw) == nil {
			items = raw.Output
		}
	}
	var types []string
	for _, it := range items {
		m, _ := it.(map[string]any)
		s, _ := m["type"].(string)
		types = append(types, s)
	}
	return types
}

// zz99Prose answers the prose one surface stated on either arm.
func zz99Prose(t *testing.T, surface, body string, stream bool) string {
	t.Helper()
	if surface != "anthropic" {
		return zz99Text(t, surface, body, stream)
	}
	var out strings.Builder
	for _, m := range zz99TextRe.FindAllStringSubmatch(body, -1) {
		var s string
		if json.Unmarshal([]byte(`"`+m[1]+`"`), &s) == nil {
			out.WriteString(s)
		}
	}
	return out.String()
}

// A nameless entry's bytes are stated at the position the entry stood, on every
// translated surface, both arms.
func TestMine99NamelessEntryStandsWhereItStood(t *testing.T) {
	nl := api.ToolCallFunction{Name: "", Arguments: zzArgs("cmd", "ls")}
	for _, tc := range []struct {
		name   string
		chunks []llm.ChatResponse
		want   string
	}{
		{
			name: "prose either side of the entry",
			chunks: []llm.ChatResponse{
				{Message: api.Message{Role: "assistant", Content: "A"}},
				{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{Function: nl}}}},
				{Message: api.Message{Role: "assistant", Content: "B"}},
			},
			want: `A{"cmd":"ls"}B`,
		},
		{
			name: "the entry before any prose",
			chunks: []llm.ChatResponse{
				{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{Function: nl}}}},
				{Message: api.Message{Role: "assistant", Content: "B"}},
			},
			want: `{"cmd":"ls"}B`,
		},
		{
			name: "the entry after all the prose",
			chunks: []llm.ChatResponse{
				{Message: api.Message{Role: "assistant", Content: "A"}},
				{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{Function: nl}}}},
			},
			want: `A{"cmd":"ls"}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := append(tc.chunks, llm.ChatResponse{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop})
			for _, stream := range []bool{false, true} {
				for _, surface := range []string{"anthropic", "openai", "responses"} {
					_, body, _ := zzRunChunks(t, chunks, surface, fmt.Sprintf("zz99nl-%s-%v", surface, stream), stream)
					if got := zz99Prose(t, surface, body, stream); got != tc.want {
						t.Errorf("%s stream=%v stated %q, want %q — an entry the upstream never named is relayed as TEXT where the entry stood (2026-09-29 audit, round 99, F99-L1-1)", surface, stream, got, tc.want)
					}
				}
			}
		})
	}
}

// A turn carrying a nameless entry still states its items in the order its runs
// state: the fold must restate the run list for the turn it rewrote.
func TestMine99TheRunListSurvivesTheFold(t *testing.T) {
	chunks := []llm.ChatResponse{
		{Message: api.Message{Role: "assistant", Content: "A"}},
		{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{Name: "", Arguments: zzArgs("cmd", "ls")}}}}},
		{Message: api.Message{Role: "assistant", Thinking: "T"}},
		{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop},
	}
	_, buffered, _ := zzRunChunks(t, chunks, "responses", "zz99runs-b", false)
	_, streamed, _ := zzRunChunks(t, chunks, "responses", "zz99runs-s", true)
	bo, so := zz99ResponsesOrder(buffered, false), zz99ResponsesOrder(streamed, true)
	if !zz99EqualOrder(bo, so) {
		t.Errorf("one turn, two orders — the buffered arm states %v and the streamed arm %v (2026-09-29 audit, round 99, F99-L1-2)", bo, so)
	}
	if want := []string{"message", "reasoning"}; !zz99EqualOrder(bo, want) {
		t.Errorf("the buffered arm states %v, want %v — the prose arrived before the reasoning (2026-09-29 audit, round 99, F99-L1-2)", bo, want)
	}
	if got := zz99Prose(t, "responses", buffered, false); got != `A{"cmd":"ls"}` {
		t.Errorf("the buffered arm stated %q, want %q (2026-09-29 audit, round 99, F99-L1-1)", got, `A{"cmd":"ls"}`)
	}
}

// A translated client that declared no tools still gets the turn's order: the
// run list's call gate is the gate the accumulation below it uses.
func TestMine99AnUndeclaredCallKeepsTheOrder(t *testing.T) {
	call := llm.ChatResponse{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{Name: "Bash", Arguments: zzArgs("cmd", "ls")}}}}}
	for _, tc := range []struct {
		name   string
		chunks []llm.ChatResponse
		want   []string
	}{
		{"the call, then the prose that followed it",
			[]llm.ChatResponse{call, {Message: api.Message{Role: "assistant", Content: "A"}}}, []string{"function_call", "message"}},
		{"the call, then the reasoning that followed it",
			[]llm.ChatResponse{call, {Message: api.Message{Role: "assistant", Thinking: "T"}}}, []string{"function_call", "reasoning"}},
		{"the reasoning, then the call",
			[]llm.ChatResponse{{Message: api.Message{Role: "assistant", Thinking: "T"}}, call}, []string{"reasoning", "function_call"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := append(tc.chunks, llm.ChatResponse{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop})
			model := "zz99undecl"
			// zzRunChunks declares a tool; the client here declares none, which
			// is the shape round 92 pinned as relayed.
			for _, stream := range []bool{false, true} {
				_, _, s := zzRunChunks(t, chunks, "responses", model, stream)
				body := `{"model":"` + model + `","stream":` + fmt.Sprint(stream) + `,"input":[{"role":"user","content":"hi"}]}`
				_, out := zzPost(t, s, "responses", stream, body)
				if got := zz99ResponsesOrder(out, stream); !zz99EqualOrder(got, tc.want) {
					t.Errorf("responses stream=%v stated %v, want %v — a client that declared no tools is served this call (round 92, F92-L1-2) and the order is the turn's own (2026-09-29 audit, round 99, F99-L1-3)", stream, got, tc.want)
				}
				_, ids := zzResponseNames(t, out, stream)
				if len(ids) != 1 || ids[0] == "" {
					t.Errorf("responses stream=%v stated call ids %v, want the one call this turn carried (2026-09-29 audit, round 99, F99-L1-3)", stream, ids)
				}
			}
		})
	}
}

// A restatement of a call the turn already stated under that id is one call —
// the rule the Anthropic arm's sentKey applies — while an id-less call repeated
// is still two calls, each with its own mint (round 40, A40-6).
func TestMine99ARestatementIsOneCall(t *testing.T) {
	stated := func() api.ToolCall {
		return api.ToolCall{ID: "call_x", Function: api.ToolCallFunction{Name: "Bash", Arguments: zzArgs("cmd", "ls")}}
	}
	for _, tc := range []struct {
		name   string
		chunks []llm.ChatResponse
	}{
		{"restated in one chunk", []llm.ChatResponse{{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{stated(), stated()}}}}},
		{"restated in the next chunk", []llm.ChatResponse{
			{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{stated()}}},
			{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{stated()}}},
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			chunks := append(tc.chunks, llm.ChatResponse{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop})
			for _, stream := range []bool{false, true} {
				_, anth, _ := zzRunChunks(t, chunks, "anthropic", fmt.Sprintf("zz99res-a-%v", stream), stream)
				if n := strings.Count(anth, `"type":"tool_use"`); n != 1 {
					t.Fatalf("premise: the Anthropic arm wrote %d tool_use blocks for a restatement, want 1", n)
				}
				_, oa, _ := zzRunChunks(t, chunks, "openai", fmt.Sprintf("zz99res-o-%v", stream), stream)
				if _, ids, _ := zzCallIDs(t, oa, stream); len(ids) != 1 || ids[0] != "call_x" {
					t.Errorf("openai stream=%v stated calls %v, want the one call the turn carried under call_x (2026-09-29 audit, round 99, F99-L1-4)", stream, ids)
				}
				_, rs, _ := zzRunChunks(t, chunks, "responses", fmt.Sprintf("zz99res-r-%v", stream), stream)
				if _, ids := zzResponseNames(t, rs, stream); len(ids) != 1 || ids[0] != "call_x" {
					t.Errorf("responses stream=%v stated calls %v, want the one call the turn carried under call_x (2026-09-29 audit, round 99, F99-L1-4)", stream, ids)
				}
			}
			// The same identity with NO id is still two calls: a minted id is
			// this leg's own synthesis, not the upstream's correlation key
			// (round 46, A46-4).
			idless := []llm.ChatResponse{{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{
				{Function: api.ToolCallFunction{Name: "Bash", Arguments: zzArgs("cmd", "ls")}},
				{Function: api.ToolCallFunction{Name: "Bash", Arguments: zzArgs("cmd", "ls")}},
			}}}, {Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop}}
			_, oa, _ := zzRunChunks(t, idless, "openai", "zz99res-o-idless", false)
			text, ids, _ := zzCallIDs(t, oa, false)
			if len(ids) != 2 || ids[0] == ids[1] || text != "" {
				t.Errorf("two id-less calls of one identity stated %v, want two distinct ids (round 40, A40-6)", ids)
			}
		})
	}
}
