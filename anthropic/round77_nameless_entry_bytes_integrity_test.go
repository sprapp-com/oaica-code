package anthropic

// round77_nameless_entry_bytes_integrity_test.go — leg 1, the bytes of a tool
// entry the upstream never NAMED (2026-09-28 audit, round 77, F77-L1-1).
//
// An entry whose `function.name` is empty is not a call: content_block_start is
// the only event that carries a name and a start without one can never be
// corrected, so there is nothing the client could ever dispatch. It is not
// nothing either — the arguments the upstream stated for it are the model's own
// output, and the other two legs have relayed them to the client as TEXT on
// every arm since round 75 (the client proxy) and round 76 (the gateway). Both
// arms of THIS leg dropped them: blockFor returned no block at all and the
// stream converter `continue`d, so a turn the model wrote reached the client
// with bytes missing, and a turn whose only entry was nameless reported
// stop_reason end_turn with nothing to show for the model's output.
//
// The rule both arms now keep, stated once: those bytes are relayed as TEXT at
// the position the entry stood, inside the open text block — a whole document
// carries ONE merged text string, so the prose either side of the entry and the
// entry's own arguments are ONE text block, exactly as the streaming arm's open
// block takes all three deltas. An entry with no arguments has nothing to relay
// and writes no block. Each row below asks for the same turn both ways and
// asserts the two arms agree, block for block.

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/ollama/ollama/api"
)

// r77TurnOrder renders a relayed turn the way the CLIENT reads it: the block
// types in wire order, with each text block's own text and each tool_use
// block's id. A streaming turn is read from its events, a message from its
// content — the two spellings of the same answer.
func r77TurnOrder(t *testing.T, resp MessagesResponse) string {
	t.Helper()
	out := ""
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text := ""
			if b.Text != nil {
				text = *b.Text
			}
			out += fmt.Sprintf("<text %q>", text)
		case "tool_use":
			out += fmt.Sprintf("<tool_use %s>", b.ID)
		default:
			out += "<" + b.Type + ">"
		}
	}
	return out
}

// r77StreamOrder reads the same blocks out of the stream arm's events.
func r77StreamOrder(t *testing.T, events []StreamEvent) string {
	t.Helper()
	type blk struct{ typ, id, text string }
	blocks := map[int]*blk{}
	var order []int
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			start, ok := e.Data.(ContentBlockStartEvent)
			if !ok {
				continue
			}
			b := &blk{typ: start.ContentBlock.Type, id: start.ContentBlock.ID}
			if start.ContentBlock.Text != nil {
				b.text = *start.ContentBlock.Text
			}
			blocks[start.Index] = b
			order = append(order, start.Index)
		case "content_block_delta":
			d, ok := e.Data.(ContentBlockDeltaEvent)
			if !ok || d.Delta.Type != "text_delta" {
				continue
			}
			if b := blocks[d.Index]; b != nil {
				b.text += d.Delta.Text
			}
		}
	}
	out := ""
	for _, i := range order {
		b := blocks[i]
		switch b.typ {
		case "text":
			out += fmt.Sprintf("<text %q>", b.text)
		case "tool_use":
			out += fmt.Sprintf("<tool_use %s>", b.id)
		default:
			out += "<" + b.typ + ">"
		}
	}
	return out
}

// r77Chunks answers the chunks the upstream streams as one turn.
func r77Chunks(chunks ...api.ChatResponse) []StreamEvent {
	conv := NewStreamConverter("msg_r77", "m", 10)
	var events []StreamEvent
	for _, c := range chunks {
		events = append(events, conv.Process(c)...)
	}
	return events
}

func TestANamelessEntrysArgumentsReachTheClientAsText(t *testing.T) {
	for _, tc := range []struct {
		note    string
		turn    api.ChatResponse
		want    string
		chunked bool
	}{
		{
			note: "the only entry is nameless, after the model's prose",
			turn: api.ChatResponse{
				DoneReason: "tool_calls",
				Message: api.Message{
					Content: "Let me look.",
					ToolCalls: []api.ToolCall{{
						ID:       "call_srv1",
						Function: api.ToolCallFunction{Name: "", Arguments: r77Args("cmd", "ls")},
					}},
				},
			},
			want: `<text "Let me look.{\"cmd\":\"ls\"}">`,
		},
		{
			note: "a nameless entry beside a call the model DID name",
			turn: api.ChatResponse{
				DoneReason: "tool_calls",
				Message: api.Message{
					Content: "Let me look.",
					ToolCalls: []api.ToolCall{
						{ID: "call_srv1", Function: api.ToolCallFunction{Name: "", Arguments: r77Args("cmd", "ls")}},
						{ID: "call_srv2", Function: api.ToolCallFunction{Name: "Read", Arguments: r77Args("file_path", "/tmp/x")}},
					},
				},
			},
			want: `<text "Let me look.{\"cmd\":\"ls\"}"><tool_use call_srv2>`,
		},
		{
			note: "a nameless entry with no arguments writes nothing",
			turn: api.ChatResponse{
				DoneReason: "tool_calls",
				Message: api.Message{
					Content: "Let me look.",
					ToolCalls: []api.ToolCall{{
						ID:       "call_srv1",
						Function: api.ToolCallFunction{Name: ""},
					}},
				},
			},
			want: `<text "Let me look.">`,
		},
		{
			note: "the buffered turn's own run boundaries hold the entry's place",
			turn: api.ChatResponse{
				DoneReason: "stop",
				Message: api.Message{
					Content: "Let me check the tree.\n\nNow I wait for the result.",
					ToolCalls: []api.ToolCall{{
						ID:       "call_srv1",
						Function: api.ToolCallFunction{Name: "", Arguments: r77Args("cmd", "ls")},
					}},
					ContentRuns: []string{"Let me check the tree.\n", "\nNow I wait for the result."},
				},
			},
			want:    `<text "Let me check the tree.\n{\"cmd\":\"ls\"}\nNow I wait for the result.">`,
			chunked: true,
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			doc := r77TurnOrder(t, ToMessagesResponse("msg_r77", tc.turn))

			var chunks []api.ChatResponse
			if tc.chunked {
				runs := tc.turn.Message.ContentRuns
				chunks = append(chunks, api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: runs[0]}})
			} else {
				chunks = append(chunks, api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: tc.turn.Message.Content}})
			}
			chunks = append(chunks, api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", ToolCalls: tc.turn.Message.ToolCalls}})
			if tc.chunked {
				chunks = append(chunks, api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", Content: tc.turn.Message.ContentRuns[1]}})
			}
			chunks = append(chunks, api.ChatResponse{Model: "m", Done: true, DoneReason: tc.turn.DoneReason, Message: api.Message{Role: "assistant"}})
			stream := r77StreamOrder(t, r77Chunks(chunks...))

			if doc != tc.want {
				t.Errorf("the document arm answered %s, want %s — an entry the upstream never named is not a call, and the arguments it stated are the model's own output relayed to the client as text (2026-09-28 audit, round 77, F77-L1-1)", doc, tc.want)
			}
			if stream != tc.want {
				t.Errorf("the stream arm answered %s, want %s — one turn, two arms, one answer (2026-09-28 audit, round 77, F77-L1-1)", stream, tc.want)
			}
		})
	}
}

// TestANamelessEntryAloneDoesNotClaimToolUse is the other half of F77-L1-1: the
// bytes now reach the client, but they reach it as TEXT, so the turn still has
// no block for the client to run and its stop_reason must not tell it to wait
// for one (round 39's rule, which this arm has kept since it stopped counting
// nameless entries as tool blocks).
func TestANamelessEntryAloneDoesNotClaimToolUse(t *testing.T) {
	turn := api.ChatResponse{
		DoneReason: "tool_calls",
		Message: api.Message{
			ToolCalls: []api.ToolCall{{
				ID:       "call_srv1",
				Function: api.ToolCallFunction{Name: "", Arguments: r77Args("cmd", "ls")},
			}},
		},
	}
	doc := ToMessagesResponse("msg_r77", turn)
	if got, want := r77TurnOrder(t, doc), `<text "{\"cmd\":\"ls\"}">`; got != want {
		t.Errorf("the document arm answered %s, want %s (2026-09-28 audit, round 77, F77-L1-1)", got, want)
	}
	if doc.StopReason != "end_turn" {
		t.Errorf("stop_reason is %q, want end_turn — the turn carries no block the client can run, and the entry's bytes are text (2026-09-28 audit, round 77, F77-L1-1)", doc.StopReason)
	}
	events := r77Chunks(
		api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant", ToolCalls: turn.Message.ToolCalls}},
		api.ChatResponse{Model: "m", Done: true, DoneReason: "tool_calls", Message: api.Message{Role: "assistant"}},
	)
	var stop string
	for _, e := range events {
		if e.Event == "message_delta" {
			if d, ok := e.Data.(MessageDeltaEvent); ok {
				stop = d.Delta.StopReason
			}
		}
	}
	if stop != "end_turn" {
		b, _ := json.Marshal(events)
		t.Errorf("the stream arm reported stop_reason %q, want end_turn — one turn, two arms, one answer (%s)", stop, b)
	}
	if got, want := r77StreamOrder(t, events), `<text "{\"cmd\":\"ls\"}">`; got != want {
		t.Errorf("the stream arm answered %s, want %s (2026-09-28 audit, round 77, F77-L1-1)", got, want)
	}
}

func r77Args(kv ...string) api.ToolCallFunctionArguments {
	args := api.NewToolCallFunctionArguments()
	for i := 0; i+1 < len(kv); i += 2 {
		args.Set(kv[i], kv[i+1])
	}
	return args
}
