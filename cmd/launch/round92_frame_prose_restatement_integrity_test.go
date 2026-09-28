package launch

// round92_frame_prose_restatement_integrity_test.go — leg 2, F92-L2-3 (and the
// doubling half of F92-L2-1): a whole-completion frame that arrives after the
// stream has already written is folded into the turn as a delta, and the fold
// asked only whether the frame stated prose — not whether the turn had already
// stated it. Measured on this leg before the fix (2026-09-29 audit, round 92):
// `[delta "hi"][frame "hi"]` reached the client as text="hihi", and
// `[delta "hi"][frame "hi there"]` as "hihi there", while the same turn written
// as deltas, and the frame on its own, both answer "hi" and "hi there".
//
// The frame states the WHOLE answer, so what the turn has already relayed is a
// prefix of it and only the tail beyond that prefix is new — which is the
// question the tool-call half of the same gate has asked since round 86
// (heldCall). The fold relays the tail rather than the whole statement, and the
// turn records the prose it relays, at the two sites a later frame can restate:
// the deltas, and the whole completion an adoption writes.
//
// Three halves, each reverted on its own and each went behaviourally red: the
// fold's strip ("hihi there" and "hihi"), the recording of the deltas' text
// (four cases above), and the recording of the adoption's text ("MM", the
// doubling F92-L2-1 measured). Two things measured NOT load-bearing and were
// dropped rather than shipped: asking the same held-prose question inside
// frameAddsToTheTurn (the fold's strip already answers it — with the strip in
// place the gate's verdict changes no byte, since a frame folded down to
// nothing emits no event and the calls are gated per entry), and recording the
// text flushToolCalls relays at the end of the turn (it is relayed after the
// last frame the stream will read, so no frame can restate it).
//
// F92-L2-1 ("the fold overwrites the frame's own delta") is RECORDED, not
// fixed: a frame carrying `message.content` and `delta.content` together has
// been read by its MESSAGE alone since round 90 (F90-L2-2), where the same
// shape measured text="hihi" and was pinned to text="hi" — the frame's delta is
// not this arm's to relay, so the bytes the auditor saw dropped are the bytes
// that ruling deliberately drops. What was NOT settled there is the doubling a
// later frame caused by restating the adopted text, which this pin covers.
// F92-L2-2 (a call-order reversal when a frame follows a fragment) did not
// reproduce: measured 2026-09-29, `[frag c1][frame c2]` answers
// calls=[Bash/c1, Read/c2], and so does the frame-alone spelling of the same
// turn; the only order that follows the frame's own list is a frame read with
// no fragment before it, which is a different upstream body.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r92ProseFrame is a whole-completion frame stating prose and nothing else.
func r92ProseFrame(content string) string {
	return `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"` + content + `"},"finish_reason":"stop"}]}`
}

// r92ReasoningFrame is a whole-completion frame whose message is reasoning.
func r92ReasoningFrame(reasoning string) string {
	return `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","reasoning_content":"` + reasoning + `"},"finish_reason":"stop"}]}`
}

// r92DeltaAndFrame is one frame carrying a delta AND a whole message — the
// spelling F90-L2-2 ruled on.
func r92DeltaAndFrame(deltaContent, messageContent string) string {
	return `data: {"id":"c","choices":[{"index":0,"delta":{"content":"` + deltaContent +
		`"},"message":{"role":"assistant","content":"` + messageContent + `"}}]}`
}

func r92ProseDelta(content string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"` + content + `"}}]}`
}

var r92ReasoningDelta = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"reasoning_content":"re"}}]}`
var r92StopFrame = `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`

// r92Thinking is every thinking_delta byte the client was handed.
func r92Thinking(t *testing.T, body string) string {
	t.Helper()
	var thinking string
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type     string `json:"type"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		if ev.Type == "content_block_delta" && ev.Delta.Type == "thinking_delta" {
			thinking += ev.Delta.Thinking
		}
	}
	return thinking
}

// A frame that only restates what the deltas already relayed is not said again:
// the turn's text is what its deltas spelling gives, once.
func TestAFrameRestatingTheRelayedProseDoesNotSayItTwice(t *testing.T) {
	deltasOnly := r85Text(t, r85Body(t, []string{r92ProseDelta("hi"), r92StopFrame, r81Done}))
	if deltasOnly != "hi" {
		t.Fatalf("the deltas spelling of the turn answers %q, want %q", deltasOnly, "hi")
	}
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"a frame restating the relayed text", []string{r92ProseDelta("hi"), r92ProseFrame("hi"), r92StopFrame, r81Done}},
		{"two frames restating it", []string{r92ProseFrame("hi"), r92ProseFrame("hi"), r92StopFrame, r81Done}},
	} {
		code, _, _ := r83OrderArm(t, tc.frames)
		if code != 200 {
			t.Errorf("%s: answered %d, want 200", tc.name, code)
			continue
		}
		got := r85Text(t, r85Body(t, tc.frames))
		if got != deltasOnly {
			t.Errorf("%s: the client was handed text=%q, want %q — a frame that only restates the turn's own prose is not new output (2026-09-29 audit, round 92, F92-L2-3)", tc.name, got, deltasOnly)
		}
	}
}

// A frame that states MORE than the client holds relays the part it does not
// hold, and no part it does: the turn's text is the same whichever spelling
// carried it.
func TestAFrameStatesOnlyTheProseTheClientDoesNotHold(t *testing.T) {
	want := r85Text(t, r85Body(t, []string{r92ProseDelta("hi"), r92ProseDelta(" there"), r92StopFrame, r81Done}))
	if want != "hi there" {
		t.Fatalf("the deltas spelling of the turn answers %q, want %q", want, "hi there")
	}
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"the frame states the whole answer", []string{r92ProseDelta("hi"), r92ProseFrame("hi there"), r92StopFrame, r81Done}},
		{"the frame states the tail alone", []string{r92ProseDelta("hi"), r92ProseFrame(" there"), r92StopFrame, r81Done}},
	} {
		got := r85Text(t, r85Body(t, tc.frames))
		if tc.name == "the frame states the tail alone" {
			// The frame states text the turn does not hold and does not
			// continue the prose it holds: relayed whole, as it is on every
			// other arm of this body.
			if got != "hi there" {
				t.Errorf("%s: the client was handed text=%q, want %q", tc.name, got, "hi there")
			}
			continue
		}
		if got != want {
			t.Errorf("%s: the client was handed text=%q, want the deltas spelling's %q — the frame is read for the bytes the client does not already hold (2026-09-29 audit, round 92, F92-L2-3)", tc.name, got, want)
		}
	}
}

// The same rule across the adoption: a frame folded after an adopted whole
// completion does not state that completion's text a second time. Before this
// the turn measured "MM" where every other spelling of it answers "M".
func TestAFrameRestatingProseAnAdoptionWroteDoesNotSayItTwice(t *testing.T) {
	frames := []string{r92DeltaAndFrame("D", "M"), r92ProseFrame("M"), r92StopFrame, r81Done}
	code, _, _ := r83OrderArm(t, frames)
	if code != 200 {
		t.Fatalf("the turn answered %d, want 200", code)
	}
	got := r85Text(t, r85Body(t, frames))
	if got != "M" {
		t.Errorf("the client was handed text=%q, want %q — the frame restates the text the adoption already relayed, and the frame's own delta is not this arm's to relay (F90-L2-2); only the doubling is the defect (2026-09-29 audit, round 92, F92-L2-1/F92-L2-3)", got, "M")
	}
}

// And the round-85 property stands: the frame's prose and the call it states
// both still reach the client, with the prose said once.
func TestTheFrameStillRelaysItsProseAndItsCall(t *testing.T) {
	frames := []string{r85TextDelta, r85WholeFrame("hi"), r85CallsFin, r81Done}
	order, calls := r85Blocks(t, r85Body(t, frames))
	if order != "text,tool_use" {
		t.Errorf("the turn relayed blocks %q, want %q — a whole frame that arrives after the stream has written still states the call it carries (round 85, R85-L2-1)", order, "text,tool_use")
	}
	if strings.Join(calls, " ") != "Read/c2" {
		t.Errorf("the turn relayed calls %v, want [Read/c2]", calls)
	}
	if got := r85Text(t, r85Body(t, frames)); got != "hi" {
		t.Errorf("the client was handed text=%q, want %q — the frame restates the prose the delta before it relayed (2026-09-29 audit, round 92, F92-L2-3)", got, "hi")
	}
}

// Reasoning is prose too: a frame restating the relayed thinking does not say
// it twice, and one that continues it relays only the continuation.
func TestAReasoningRestatementDoesNotSayTheThinkingTwice(t *testing.T) {
	deltasOnly := r92Thinking(t, r85Body(t, []string{r92ReasoningDelta, r92StopFrame, r81Done}))
	if deltasOnly != "re" {
		t.Fatalf("the deltas spelling of the turn answers thinking=%q, want %q", deltasOnly, "re")
	}
	for _, tc := range []struct {
		name   string
		frames []string
		want   string
	}{
		{"the frame restates the thinking", []string{r92ReasoningDelta, r92ReasoningFrame("re"), r92StopFrame, r81Done}, "re"},
		{"the frame continues the thinking", []string{r92ReasoningDelta, r92ReasoningFrame("re there"), r92StopFrame, r81Done}, "re there"},
	} {
		code, _, _ := r83OrderArm(t, tc.frames)
		if code != 200 {
			t.Errorf("%s: answered %d, want 200", tc.name, code)
			continue
		}
		got := r92Thinking(t, r85Body(t, tc.frames))
		if got != tc.want {
			t.Errorf("%s: the client was handed thinking=%q, want %q — reasoning is relayed by the same rule as the answer (2026-09-29 audit, round 92, F92-L2-3)", tc.name, got, tc.want)
		}
	}
}
