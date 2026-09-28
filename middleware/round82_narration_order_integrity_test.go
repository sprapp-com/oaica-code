package middleware

// round82_narration_order_integrity_test.go — leg 1, the web_search takeover's
// narration read four ways (2026-09-28 audit, round 82).
//
// Round 81 pinned the run structure of the takeover (one block per contiguous
// run of one kind, in the order the run arrived) for the shapes where the
// takeover had already been made to carry the runs. Four shapes of the SAME
// question were still answered two ways by the two arms of one upstream body:
// the STREAMING arm, which relays the chunks that carry no web_search call and
// absorbs the ones it discards, and the BUFFERED arm, which is handed the one
// message the server's document lane merged those chunks into (server/routes.go,
// writeChatResponse) — Content, Thinking, ToolCalls and the ordered OutputRuns
// that lane builds while merging.
//
//  1. F82-L1-1. Reasoning either side of the call the loop superseded. It is
//     one run and so one block — the converter's Process opens a thinking block
//     and keeps writing into it, and the superseded call writes no block at all
//     — but the runs path's thinking case appended unconditionally, so one body
//     reached a buffered client as `<thinking T1><thinking T2>` and a streaming
//     one as `<thinking T1T2>`. Round 81's F81-L1-1 is the same reading applied
//     to PROSE, whose case already joined; this is its reasoning half.
//
//  2. F82-L1-2. The order the absorbed narration arrived in. The takeover
//     recorded every discarded chunk into one bucket per KIND and reassembled
//     them as a fixed [thinking, text] pair, so prose the model wrote before its
//     reasoning reached a streaming client after it — and, on this arm alone,
//     out of the order the buffered arm kept.
//
//  3. F82-L1-3. The index the open block was left at. writeStreamNarration
//     continues an open block of the same kind, but a block of a DIFFERENT kind
//     was written without closing the open one first, so the stale index stayed
//     the continuation target: the model's later prose was appended to a block
//     the client had already been told was something else, and the turn's stops
//     went out of order. Both are the client's own framing, not the model's
//     output — an index-keyed client that never saw a text block's stop reads
//     two blocks merged.
//
//  4. F82-L1-4. An entry's bytes where the upstream NAMED nothing. An entry the
//     upstream never named is not a call: its arguments are the model's own
//     output and reach the client as TEXT, the rule both other legs and this
//     leg's own runs path apply (round 77, F77-L1-1). The takeover released the
//     chunks it had held without their calls — and dropped the nameless entry's
//     bytes with them — while the buffered arm's fallback path, reading a
//     merged message whose runs were not accountable, dropped them too.
//
// Every case below is fail-first: RED against the tree before its round-82 fix.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

func r82L1Call(name string, args map[string]string) api.ToolCall {
	tc := api.ToolCall{Function: api.ToolCallFunction{Name: name, Arguments: api.NewToolCallFunctionArguments()}}
	for k, v := range args {
		tc.Function.Arguments.Set(k, v)
	}
	return tc
}

func r82L1Search(q string) api.ToolCall {
	return r82L1Call("web_search", map[string]string{"query": q})
}

// r82L1Msg is one chunk: the reasoning and prose it carried, then its calls, in
// the order the streaming converter asks for them inside a chunk.
func r82L1Msg(thinking, content string, calls ...api.ToolCall) api.Message {
	return api.Message{Role: "assistant", Thinking: thinking, Content: content, ToolCalls: calls}
}

// r82L1Done is the chunk that ends the turn; the loop's terminal is written
// when it arrives.
func r82L1Done() api.ChatResponse {
	return api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant"}, Done: true, DoneReason: "stop"}
}

// r82L1Both drives ONE chunk list through both arms and renders each.
func r82L1Both(t *testing.T, chunks []api.ChatResponse) (string, string) {
	t.Helper()
	doc := r81Blocks(t, r81Turn(t, false, r81SearchTools, chunks), false)
	str := r81Blocks(t, r81Turn(t, true, r81SearchTools, chunks), true)
	return doc, str
}

// r82L1Same requires the two arms of one upstream body to render alike, and
// returns the rendering they agree on.
func r82L1Same(t *testing.T, note string, chunks []api.ChatResponse) string {
	t.Helper()
	doc, str := r82L1Both(t, chunks)
	if doc != str {
		t.Errorf("%s: one upstream body reached the buffered client as\n   %s\nand the streaming one as\n   %s", note, doc, str)
	}
	return str
}

// r82L1Opens counts the blocks of one kind a rendering opens.
func r82L1Opens(rendered, kind string) int {
	return strings.Count(rendered, "<"+kind+" ")
}

// r82L1Event is what the framing check reads off the streaming wire.
type r82L1Event struct {
	kind  string
	index int
}

// r82L1Wire is the streaming arm's own event sequence, in order.
func r82L1Wire(t *testing.T, body string) []r82L1Event {
	t.Helper()
	var events []r82L1Event
	for _, line := range strings.Split(body, "\n") {
		data, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index *int   `json:"index"`
		}
		if json.Unmarshal([]byte(data), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start", "content_block_delta", "content_block_stop":
		default:
			continue
		}
		index := -1
		if ev.Index != nil {
			index = *ev.Index
		}
		events = append(events, r82L1Event{kind: ev.Type, index: index})
	}
	return events
}

// r82L1Framing is the client's own view of the wire: block indices count up
// from zero with no gap, a delta belongs to the block that is open, and a stop
// closes the block that is open and nothing else. Nothing here reads the
// model's output; it is the shape an index-keyed client accumulates by.
func r82L1Framing(t *testing.T, note, body string) {
	t.Helper()
	open, next := -1, 0
	for _, ev := range r82L1Wire(t, body) {
		switch ev.kind {
		case "content_block_start":
			if open != -1 {
				t.Errorf("%s: a block opened at index %d while index %d was still open — a client accumulating by index has nowhere to put the second one\n%s", note, ev.index, open, body)
			}
			if ev.index != next {
				t.Errorf("%s: a block opened at index %d, want %d — the indices a client is handed are its addresses and count up without a gap\n%s", note, ev.index, next, body)
			}
			open, next = ev.index, ev.index+1
		case "content_block_delta":
			if open == -1 || ev.index != open {
				t.Errorf("%s: a delta arrived at index %d while index %d was open — an index-keyed client appends it to the wrong block\n%s", note, ev.index, open, body)
			}
		case "content_block_stop":
			if open == -1 || ev.index != open {
				t.Errorf("%s: index %d was stopped while index %d was open — the stops of one turn go out in the order the blocks opened\n%s", note, ev.index, open, body)
			}
			open = -1
		}
	}
	if open != -1 {
		t.Errorf("%s: the turn ended with index %d still open\n%s", note, open, body)
	}
}

// TestReasoningEitherSideOfASwallowedCallIsOneRun is F82-L1-1.
func TestReasoningEitherSideOfASwallowedCallIsOneRun(t *testing.T) {
	rendered := r82L1Same(t, "reasoning, the web_search call, reasoning", []api.ChatResponse{
		{Model: "test-model", Message: r82L1Msg("T1", "", r82L1Search("news"))},
		{Model: "test-model", Message: r82L1Msg("T2", "")},
		r82L1Done(),
	})
	if n := r82L1Opens(rendered, "thinking"); n != 1 {
		t.Errorf("reasoning written either side of the call the loop superseded reached the client as %d thinking block(s), want the 1 run it is: a run boundary is a block boundary only where the KIND changes, and a call this loop swallowed writes no block of its own (2026-09-28 audit, round 82, F82-L1-1)\n   %s", n, rendered)
	}
	if !strings.Contains(rendered, `{thinking "T1T2"}`) {
		t.Errorf("the model's two stretches of reasoning reached the client as\n   %s\nwant one thinking block carrying both, T1T2", rendered)
	}
}

// TestAbsorbedNarrationKeepsItsOrder is F82-L1-2: prose the model wrote before
// its reasoning stays before it on both arms.
func TestAbsorbedNarrationKeepsItsOrder(t *testing.T) {
	rendered := r82L1Same(t, "the web_search call, then prose, then reasoning", []api.ChatResponse{
		{Model: "test-model", Message: r82L1Msg("", "", r82L1Search("news"))},
		{Model: "test-model", Message: r82L1Msg("", "P")},
		{Model: "test-model", Message: r82L1Msg("T", "")},
		r82L1Done(),
	})
	text, think := strings.Index(rendered, "<text "), strings.Index(rendered, "<thinking ")
	if text == -1 || think == -1 {
		t.Fatalf("the turn's prose and reasoning are not both on the wire:\n   %s", rendered)
	}
	if text > think {
		t.Errorf("the model wrote its prose and THEN its reasoning, and the client was handed\n   %s\nthe reasoning first — one kind per block does not reorder them (2026-09-28 audit, round 82, F82-L1-2)", rendered)
	}
	// The turn's leading narration is the prose and the reasoning, one block
	// each, in that order; the text after them is the loop's own answer.
	if !strings.HasPrefix(rendered, `<text >{text "P"}<thinking >{thinking "T"}<`) {
		t.Errorf("the turn's narration reached the client as\n   %s\nwant one text block carrying the prose and then one thinking block carrying the reasoning, before the answer the search loop produced", rendered)
	}
}

// TestAnOpenBlockIsClosedBeforeADifferentKindIsWritten is F82-L1-3: the wire
// the client accumulates by, on the two shapes where a block of another kind
// followed one the passthrough arm had left open.
func TestAnOpenBlockIsClosedBeforeADifferentKindIsWritten(t *testing.T) {
	for _, tc := range []struct {
		note   string
		chunks []api.ChatResponse
	}{
		{"prose, then reasoning and prose in the chunk carrying the call", []api.ChatResponse{
			{Model: "test-model", Message: r82L1Msg("", "P")},
			{Model: "test-model", Message: r82L1Msg("T", "Y", r82L1Search("news"))},
			r82L1Done(),
		}},
		{"prose, the call, then reasoning", []api.ChatResponse{
			{Model: "test-model", Message: r82L1Msg("", "P")},
			{Model: "test-model", Message: r82L1Msg("", "", r82L1Search("news"))},
			{Model: "test-model", Message: r82L1Msg("T", "")},
			r82L1Done(),
		}},
	} {
		r82L1Same(t, tc.note, tc.chunks)
		r82L1Framing(t, tc.note, r81Turn(t, true, r81SearchTools, tc.chunks))
	}
}

// TestANamelessEntrysBytesAreRelayed is F82-L1-4: an entry the upstream never
// named is not a call, so its bytes are the model's output — beside the call
// the loop swallowed, after it, and between two of them.
func TestANamelessEntrysBytesAreRelayed(t *testing.T) {
	nameless := r82L1Call("", map[string]string{"k": "v"})
	for _, tc := range []struct {
		note   string
		chunks []api.ChatResponse
	}{
		{"beside the call", []api.ChatResponse{
			{Model: "test-model", Message: r82L1Msg("", "", nameless, r82L1Search("news"))},
			r82L1Done(),
		}},
		{"after the call", []api.ChatResponse{
			{Model: "test-model", Message: r82L1Msg("", "", r82L1Search("news"))},
			{Model: "test-model", Message: r82L1Msg("", "", nameless)},
			r82L1Done(),
		}},
		{"before the call", []api.ChatResponse{
			{Model: "test-model", Message: r82L1Msg("", "", nameless)},
			{Model: "test-model", Message: r82L1Msg("", "", r82L1Search("news"))},
			r82L1Done(),
		}},
		{"between two swallowed chunks", []api.ChatResponse{
			{Model: "test-model", Message: r82L1Msg("", "", r82L1Call("Bash", map[string]string{"cmd": "ls"}))},
			{Model: "test-model", Message: r82L1Msg("", "", r82L1Search("news"))},
			{Model: "test-model", Message: r82L1Msg("", "", nameless)},
			r82L1Done(),
		}},
	} {
		rendered := r82L1Same(t, "a nameless entry "+tc.note, tc.chunks)
		if !strings.Contains(rendered, `{text "{"k":"v"}"}`) {
			t.Errorf("the model's entry %s reached the client as\n   %s\nits bytes are not on the wire: an entry the upstream never NAMED is not a call, and its arguments are the model's own output (2026-09-28 audit, round 82, F82-L1-4)", tc.note, rendered)
		}
	}
}
