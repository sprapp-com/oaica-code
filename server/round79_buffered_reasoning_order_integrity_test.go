package server

// round79_buffered_reasoning_order_integrity_test.go — leg 1, the buffered lane
// merges a turn's REASONING as well as its text, and the order of the merged
// pieces is recoverable (2026-09-28 audit, round 79, F79-L1-1).
//
// The streaming arm of this leg keeps the model's reasoning in the runs it
// arrived in: a thinking block closes where prose or a call arrives and a new
// one opens where reasoning resumes, because an agentic reasoning model
// re-emits reasoning between tool calls. The document arm has only one merged
// Thinking string to read, so `thinking / call / thinking / text` reached a
// client that asked for no stream as ONE thinking block holding both runs, and
// `thinking / text / thinking` — no call at all — as one thinking block where
// the streaming twin writes two. Nothing is lost and nothing is reordered
// within a run, but one upstream body reached the client in two different
// block structures depending on `stream`, which is what this audit holds every
// arm of every leg to.
//
// F68-L1-1 reported this and was rejected on the grounds that no live producer
// had been exhibited and the fix meant carrying a per-block order through
// api.ChatResponse. Round 79 exhibited the producer: the buffered lane itself
// sees every chunk the streaming arm sees, so it already has the order — it was
// merging it away. The lane now records the turn's output as an ordered run
// list (api.Message.OutputRuns) alongside the text-only boundaries it already
// kept for the calls (ContentRuns), and the document arm writes the blocks in
// that order.

import (
	"fmt"
	"testing"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// r79DocOrder renders a document the way the client reads it.
func r79DocOrder(resp anthropic.MessagesResponse) string {
	out := ""
	for _, b := range resp.Content {
		switch b.Type {
		case "text":
			text := ""
			if b.Text != nil {
				text = *b.Text
			}
			out += fmt.Sprintf("<text %q>", text)
		case "thinking":
			text := ""
			if b.Thinking != nil {
				text = *b.Thinking
			}
			out += fmt.Sprintf("<thinking %q>", text)
		case "tool_use":
			out += fmt.Sprintf("<tool_use %s>", b.ID)
		default:
			out += "<" + b.Type + ">"
		}
	}
	return out
}

// r79StreamOrder renders the same turn out of the streaming arm's events.
func r79StreamOrder(events []anthropic.StreamEvent) string {
	type blk struct{ typ, id, text string }
	blocks := map[int]*blk{}
	var order []int
	for _, e := range events {
		switch e.Event {
		case "content_block_start":
			start, ok := e.Data.(anthropic.ContentBlockStartEvent)
			if !ok {
				continue
			}
			b := &blk{typ: start.ContentBlock.Type, id: start.ContentBlock.ID}
			if start.ContentBlock.Text != nil {
				b.text = *start.ContentBlock.Text
			}
			if start.ContentBlock.Thinking != nil {
				b.text = *start.ContentBlock.Thinking
			}
			blocks[start.Index] = b
			order = append(order, start.Index)
		case "content_block_delta":
			d, ok := e.Data.(anthropic.ContentBlockDeltaEvent)
			if !ok {
				continue
			}
			if b := blocks[d.Index]; b != nil {
				switch d.Delta.Type {
				case "text_delta":
					b.text += d.Delta.Text
				case "thinking_delta":
					b.text += d.Delta.Thinking
				}
			}
		}
	}
	out := ""
	for _, i := range order {
		b := blocks[i]
		switch b.typ {
		case "text":
			out += fmt.Sprintf("<text %q>", b.text)
		case "thinking":
			out += fmt.Sprintf("<thinking %q>", b.text)
		case "tool_use":
			out += fmt.Sprintf("<tool_use %s>", b.id)
		default:
			out += "<" + b.typ + ">"
		}
	}
	return out
}

// r79StreamTurn answers the same chunks as one streaming turn.
func r79StreamTurn(chunks []api.ChatResponse) []anthropic.StreamEvent {
	conv := anthropic.NewStreamConverter("msg_r79", "m", 10)
	var events []anthropic.StreamEvent
	for _, c := range chunks {
		events = append(events, conv.Process(c)...)
	}
	return events
}

// r79BothArms drives one chunk list down both arms of this leg and compares the
// blocks the client is handed.
func r79BothArms(t *testing.T, chunks []api.ChatResponse) (doc, stream string) {
	t.Helper()
	resp := r75BufferedTurn(t, true, chunks...)
	doc = r79DocOrder(anthropic.ToMessagesResponse("msg_r79", resp))
	stream = r79StreamOrder(r79StreamTurn(chunks))
	if doc != stream {
		t.Errorf("one upstream body, two arms: the buffered arm answered %s where the streaming arm answered %s — a run of reasoning is a block of its own, and the buffered lane sees the same chunks the streaming arm does (2026-09-28 audit, round 79, F79-L1-1)",
			doc, stream)
	}
	return doc, stream
}

// TestTheBufferedArmKeepsWhereTheReasoningArrived is F79-L1-1: the model's
// reasoning either side of a call is two runs, not one merged block.
func TestTheBufferedArmKeepsWhereTheReasoningArrived(t *testing.T) {
	doc, stream := r79BothArms(t, []api.ChatResponse{
		{Message: api.Message{Thinking: "First I look."}},
		{Message: api.Message{ToolCalls: []api.ToolCall{r75OneCall()}}},
		{Message: api.Message{Thinking: "Now I wait."}},
		{Message: api.Message{Content: "Done."}},
	})
	want := `<thinking "First I look."><tool_use call_1><thinking "Now I wait."><text "Done.">`
	if doc != want {
		t.Errorf("the buffered arm answered %s, want %s (2026-09-28 audit, round 79, F79-L1-1)", doc, want)
	}
	_ = stream
}

// TestTheBufferedArmKeepsReasoningAroundProse: the same question with no call
// in the turn at all — the lane has to record the order for these too.
func TestTheBufferedArmKeepsReasoningAroundProse(t *testing.T) {
	doc, _ := r79BothArms(t, []api.ChatResponse{
		{Message: api.Message{Thinking: "A"}},
		{Message: api.Message{Content: "prose"}},
		{Message: api.Message{Thinking: "B"}},
	})
	want := `<thinking "A"><text "prose"><thinking "B">`
	if doc != want {
		t.Errorf("the buffered arm answered %s, want %s (2026-09-28 audit, round 79, F79-L1-1)", doc, want)
	}
}

// TestTheBufferedArmKeepsProseEitherSideOfACall: the property round 75 pinned,
// still held by the wider run list.
func TestTheBufferedArmKeepsProseEitherSideOfACall(t *testing.T) {
	doc, _ := r79BothArms(t, []api.ChatResponse{
		{Message: api.Message{Content: "before"}},
		{Message: api.Message{ToolCalls: []api.ToolCall{r75OneCall()}}},
		{Message: api.Message{Content: "after"}},
	})
	want := `<text "before"><tool_use call_1><text "after">`
	if doc != want {
		t.Errorf("the buffered arm answered %s, want %s (2026-09-28 audit, round 75, F75-L1-1, re-pinned by round 79)", doc, want)
	}
}

// TestTheBufferedArmKeepsOneRunPerKind: a run is a contiguous stretch, so
// consecutive chunks of one kind are one block and the merged text is
// untouched.
func TestTheBufferedArmKeepsOneRunPerKind(t *testing.T) {
	doc, _ := r79BothArms(t, []api.ChatResponse{
		{Message: api.Message{Thinking: "one "}},
		{Message: api.Message{Thinking: "two"}},
		{Message: api.Message{Content: "three "}},
		{Message: api.Message{Content: "four"}},
	})
	want := `<thinking "one two"><text "three four">`
	if doc != want {
		t.Errorf("the buffered arm answered %s, want %s (2026-09-28 audit, round 79, F79-L1-1)", doc, want)
	}
}

// TestTheNativeWireKeepsItsShapeForReasoning: the run list is read by the
// Anthropic surface alone.
func TestTheNativeWireKeepsItsShapeForReasoning(t *testing.T) {
	resp := r75BufferedTurn(t, false,
		api.ChatResponse{Message: api.Message{Thinking: "A"}},
		api.ChatResponse{Message: api.Message{Content: "prose"}},
		api.ChatResponse{Message: api.Message{Thinking: "B"}},
	)
	if len(resp.Message.OutputRuns) != 0 {
		t.Errorf("the native wire carries runs %q; the OpenAI wire has one text field and one merged reasoning field by definition (2026-09-28 audit, round 79, F79-L1-1)", resp.Message.OutputRuns)
	}
	if resp.Message.Thinking != "AB" || resp.Message.Content != "prose" {
		t.Errorf("the native wire's merge is thinking %q content %q, want AB / prose", resp.Message.Thinking, resp.Message.Content)
	}
}

// TestTheRunListAccountsForTheTurnExactly: a reader may not trust a shape that
// does not add up, and the lane hands over only one that does.
func TestTheRunListAccountsForTheTurnExactly(t *testing.T) {
	resp := r75BufferedTurn(t, true,
		api.ChatResponse{Message: api.Message{Thinking: "A"}},
		api.ChatResponse{Message: api.Message{Content: "prose"}},
		api.ChatResponse{Message: api.Message{Thinking: "B"}},
	)
	var text, think string
	calls := 0
	for _, run := range resp.Message.OutputRuns {
		switch run.Kind {
		case "text":
			text += run.Text
		case "thinking":
			think += run.Text
		case "call":
			calls++
		default:
			t.Fatalf("the lane wrote a run of kind %q", run.Kind)
		}
	}
	if text != resp.Message.Content || think != resp.Message.Thinking || calls != len(resp.Message.ToolCalls) {
		t.Errorf("the run list %q accounts for text %q / thinking %q / %d call(s), want %q / %q / %d (2026-09-28 audit, round 79, F79-L1-1)",
			resp.Message.OutputRuns, text, think, calls, resp.Message.Content, resp.Message.Thinking, len(resp.Message.ToolCalls))
	}
}
