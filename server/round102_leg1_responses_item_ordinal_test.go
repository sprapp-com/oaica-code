package server

// round102_leg1_responses_item_ordinal_test.go — leg 1, round 102 (2026-09-29
// audit), F102-L1-1.
//
// A function_call item's id is the only per-item identity the Responses wire
// carries, and both of its halves state where the call stands in the TURN: the
// buffered arm numbers the turn's calls from zero (`callItems`, over
// `chatResponse.Message.ToolCalls`), so a turn that made two calls states
// `fc_…_0` and `fc_…_1`.
//
// The streamed arm numbered each CHUNK's calls from zero instead, so a stream
// that delivers one call per chunk — the ordinary agent shape, and the shape
// this tree's own round-16 pin states as "two calls arriving in two chunks" —
// announced the second call as `fc_…_0` while the same event stated
// `output_index:1`:
//
//	chunk [Bash call_a]  chunk [Read call_b]
//	  buffered document   fc_…_0/call_a  fc_…_1/call_b
//	  streamed announced  fc_…_0/call_a  fc_…_0/call_b
//
// Nothing breaks today — the random base keeps the ids distinct, and the
// `call_id` the client answers with is stated by the upstream, not minted here —
// but the id contradicts the index beside it, and the two arms of one surface
// mint the same item's identity from different counters. Round 17 fixed exactly
// this class for `output_index` (`toolCallCount`); the id's ordinal was left
// per-chunk. It is now the turn's call count, read once before the loop, which
// is what makes a call in the SECOND chunk number from the calls already
// written.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/llm"
)

// zz102Ordinal is the trailing counter of an item id (`fc_123456_7` → "7").
func zz102Ordinal(id string) string {
	if i := strings.LastIndex(id, "_"); i >= 0 {
		return id[i+1:]
	}
	return id
}

// zz102DocumentIDs reads the function_call item ids from a buffered Responses
// body, in the order the document states them.
func zz102DocumentIDs(t *testing.T, body string) []string {
	t.Helper()
	var resp struct {
		Output []map[string]any `json:"output"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("the buffered body did not parse: %v\n%s", err, body)
	}
	var out []string
	for _, item := range resp.Output {
		if item["type"] != "function_call" {
			continue
		}
		out = append(out, fmt.Sprint(item["id"]))
	}
	return out
}

// zz102StreamedIDs reads the announced function_call item ids from a streamed
// Responses body, in the order the stream announces them.
func zz102StreamedIDs(t *testing.T, body string) []string {
	t.Helper()
	var out []string
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(strings.TrimSpace(line), "data:")
		if !ok {
			continue
		}
		var ev struct {
			Type string `json:"type"`
			Item struct {
				ID   string `json:"id"`
				Type string `json:"type"`
			} `json:"item"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(raw)), &ev) != nil {
			continue
		}
		if ev.Type == "response.output_item.added" && ev.Item.Type == "function_call" {
			out = append(out, ev.Item.ID)
		}
	}
	return out
}

// TestMine102ACallsItemOrdinalIsItsPlaceInTheTurn is F102-L1-1.
func TestMine102ACallsItemOrdinalIsItsPlaceInTheTurn(t *testing.T) {
	for i, turn := range []struct {
		name   string
		chunks []llm.ChatResponse
		want   []string
	}{
		{
			"one call per chunk, in two chunks",
			[]llm.ChatResponse{
				{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{zz100Call("call_a", "Bash", "ls")}}},
				{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{zz100Call("call_b", "Read", "x")}}},
			},
			[]string{"0", "1"},
		},
		{
			"three calls across two chunks",
			[]llm.ChatResponse{
				{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{zz100Call("call_a", "Bash", "ls")}}},
				{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{
					zz100Call("call_b", "Read", "x"), zz100Call("call_c", "Grep", "y"),
				}}},
			},
			[]string{"0", "1", "2"},
		},
	} {
		done := llm.ChatResponse{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonStop, PromptEvalCount: 5, EvalCount: 3}
		chunks := append(append([]llm.ChatResponse{}, turn.chunks...), done)
		model := "zz-102o-" + string(rune('a'+i))
		_, doc, _ := zzRunChunks(t, chunks, "responses", model, false)
		_, str, _ := zzRunChunks(t, chunks, "responses", model, true)

		docIDs, strIDs := zz102DocumentIDs(t, doc), zz102StreamedIDs(t, str)
		if len(docIDs) != len(turn.want) || len(strIDs) != len(turn.want) {
			t.Fatalf("premise: %s stated %d/%d calls, want %d (doc %v, stream %v)", turn.name, len(docIDs), len(strIDs), len(turn.want), docIDs, strIDs)
		}
		want := strings.Join(turn.want, "|")
		got := make([]string, 0, len(docIDs))
		for _, id := range docIDs {
			got = append(got, zz102Ordinal(id))
		}
		if strings.Join(got, "|") != want {
			t.Fatalf("the buffered arm states ordinals %v, want %v — the document is the reading the streamed arm is held to (%v)", got, turn.want, docIDs)
		}
		got = got[:0]
		for _, id := range strIDs {
			got = append(got, zz102Ordinal(id))
		}
		if strings.Join(got, "|") != want {
			t.Errorf("%s: the streamed arm announces ordinals %v (%v), want %v — a function_call item's id states the call's place in the TURN, which is what the buffered arm of this same surface states and what the output_index beside it already said; numbered per chunk, the second call's item said _0 where the same event said output_index:1 (2026-09-29 audit, round 102, F102-L1-1)",
				turn.name, got, strIDs, turn.want)
		}
	}
}
