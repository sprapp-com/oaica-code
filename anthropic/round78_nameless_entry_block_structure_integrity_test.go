package anthropic

// round78_nameless_entry_block_structure_integrity_test.go — leg 1, that a
// nameless entry's relayed text does not corrupt the stream's block structure
// (2026-09-28 audit, round 78, F78-L1-1).
//
// Round 77 (commit 175f45acb) started relaying an entry the upstream never named
// as text, into the open text block where one was open and a new one where it
// was not. It was the only one of this converter's four content_block_start
// sites that did not close the block already open at the index it took: the
// thinking site closes the text block, the content site closes thinking, and the
// named-call site closes both. A reasoning model that reached a nameless entry
// straight after its thinking — the shape a capability-thinking model served
// with the model/parsers laguna parser produces, which builds a ToolCall with an
// empty name and non-empty arguments from a bare <arg_key>/<arg_value> pair —
// therefore handed a streaming client a content_block_start for a text block at
// an index whose thinking block was still open. The thinking block was never
// closed, the client (which keys blocks by index) lost the model's reasoning,
// and the event sequence was not legal: the text block that followed went to an
// index whose block was never started. The document arm answered the same body
// [thinking, text(args)].
//
// This file holds the STRUCTURE, which is what the arms can be compared on
// regardless of how each of them spells the same turn: every block this
// converter opens is closed, no index is taken twice without the first block
// being closed, no delta lands in a block that was never started, and the
// stream arm's block list is the document arm's.

import (
	"testing"

	"github.com/ollama/ollama/api"
)

// r78Structure reports every illegal move in a stream arm's events: a start at
// an index whose block is still open, a delta in a block never started, a stop
// with nothing to stop, and a block left open at the end of the turn.
func r78Structure(events []StreamEvent) (note string) {
	open := map[int]string{}
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			s, ok := e.Data.(ContentBlockStartEvent)
			if !ok {
				continue
			}
			if prev, held := open[s.Index]; held {
				note += "start at index " + itoa(s.Index) + " while a " + prev + " block is still open; "
			}
			open[s.Index] = s.ContentBlock.Type
		case "content_block_stop":
			s, ok := e.Data.(ContentBlockStopEvent)
			if !ok {
				continue
			}
			if _, held := open[s.Index]; !held {
				note += "stop at index " + itoa(s.Index) + " with no block open; "
			}
			delete(open, s.Index)
		case "content_block_delta":
			d, ok := e.Data.(ContentBlockDeltaEvent)
			if !ok {
				continue
			}
			if _, held := open[d.Index]; !held {
				note += "delta at index " + itoa(d.Index) + " in a block that was never started; "
			}
		}
	}
	for i, typ := range open {
		note += "the " + typ + " block at index " + itoa(i) + " is never closed; "
	}
	return note
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	if neg {
		out = "-" + out
	}
	return out
}

// TestANamelessEntryDoesNotCorruptTheBlocksAroundIt is F78-L1-1. Each row asks
// one upstream turn of the stream arm and checks both that every event is legal
// and that the arm answers the same block list as the document arm for the same
// turn.
func TestANamelessEntryDoesNotCorruptTheBlocksAroundIt(t *testing.T) {
	for _, tc := range []struct {
		note     string
		chunks   []api.ChatResponse
		document api.ChatResponse
		want     string
	}{
		{
			note: "thinking, then a nameless entry carrying arguments",
			chunks: []api.ChatResponse{
				{Message: api.Message{Thinking: "the model reasons first"}},
				{DoneReason: "tool_calls", Message: api.Message{ToolCalls: []api.ToolCall{{
					ID:       "call_srv1",
					Function: api.ToolCallFunction{Name: "", Arguments: r78Args("cmd", "ls")},
				}}}},
				{Done: true, DoneReason: "tool_calls"},
			},
			document: api.ChatResponse{
				DoneReason: "tool_calls",
				Message: api.Message{
					Thinking:    "the model reasons first",
					ContentRuns: []string{"", ""},
					ToolCalls: []api.ToolCall{{
						ID:       "call_srv1",
						Function: api.ToolCallFunction{Name: "", Arguments: r78Args("cmd", "ls")},
					}},
				},
			},
			want: `<thinking><text "{\"cmd\":\"ls\"}">`,
		},
		{
			note: "thinking, a nameless entry, then prose the entry must not swallow",
			chunks: []api.ChatResponse{
				{Message: api.Message{Thinking: "hmm"}},
				{Message: api.Message{ToolCalls: []api.ToolCall{{
					Function: api.ToolCallFunction{Name: "", Arguments: r78Args("cmd", "ls")},
				}}}},
				{Message: api.Message{Content: "prose"}},
				{Done: true, DoneReason: "stop"},
			},
			document: api.ChatResponse{
				DoneReason: "stop",
				Message: api.Message{
					Thinking: "hmm",
					Content:  "prose",
					// The runs the buffered lane fills in: Content is the runs
					// joined, split at every tool call including the one no block
					// is written for (api.Message.ContentRuns). The model wrote
					// the prose AFTER the entry, so it is the LAST run.
					ContentRuns: []string{"", "prose"},
					ToolCalls: []api.ToolCall{{
						Function: api.ToolCallFunction{Name: "", Arguments: r78Args("cmd", "ls")},
					}},
				},
			},
			want: `<thinking><text "{\"cmd\":\"ls\"}prose">`,
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			events := r77Chunks(tc.chunks...)
			if note := r78Structure(events); note != "" {
				t.Errorf("the stream arm's events are not a legal sequence: %s (2026-09-28 audit, round 78, F78-L1-1)", note)
			}
			got := r77StreamOrder(t, events)
			if got != tc.want {
				t.Errorf("the stream arm answered %s, want %s (2026-09-28 audit, round 78, F78-L1-1)\n%v", got, tc.want, events)
			}
			if doc := r77TurnOrder(t, ToMessagesResponse("msg_r78", tc.document)); doc != tc.want {
				t.Errorf("the document arm answered %s, want %s — one body, two arms, one block list (2026-09-28 audit, round 78, F78-L1-1)", doc, tc.want)
			}
		})
	}
}

func r78Args(k, v string) api.ToolCallFunctionArguments {
	a := api.NewToolCallFunctionArguments()
	a.Set(k, v)
	return a
}
