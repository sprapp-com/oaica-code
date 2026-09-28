package launch

// round89_adopted_frame_prose_order_test.go — leg 2, R89-L2-2 (2026-09-29
// audit, round 89). RECORDED, not fixed.
//
// One turn — the prose "hi", a call c1, the prose " there" — written as deltas
// and as a whole-completion frame carrying the first prose and the call,
// followed by the trailing prose. Measured at HEAD 4af6ab2ff with my own probe:
//
//	deltas                     blocks=[text tool_use]      text="hi there"
//	frame first, then prose    blocks=[text tool_use text] text="hi there"
//	prose first, then frame    blocks=[text tool_use]      text=" therehi"
//	frame only                 blocks=[text tool_use]      text="hi"
//
// The frame-first spelling relays the same text BYTES, the same one call under
// the same id with the same arguments, and the same stop reason; what differs is
// that the prose arriving after the frame opens a text block of its own, behind
// the call block the frame wrote. The third spelling is not a divergence at all:
// its wire puts " there" before the frame that states "hi", and " therehi" is
// that order.
//
// This is the shape anthropic/anthropic.go's F68-L1-1 note records as
// deliberate, one step wider than round 84's pin of it: an adopted whole
// completion writes its own blocks where it states them (thinking, then text,
// then tool_use), and prose the wire sends AFTER that frame is prose after it —
// the adopted text block is closed by the call the frame itself states, and no
// later delta can reach back into it. Round 84 pinned the boundary case (a frame
// whose prose is EMPTY, so there is nothing to order the call against) and
// recorded the same reason; with prose in the frame there IS something to order
// against, and the order the frame states is the one the client gets.
//
// Recorded rather than fixed because the fix is not a local one: holding an
// adopted frame's calls to the turn's end is exactly what rounds 47, 48, 56, 72
// and 85 build on (the adopted-call slot bookkeeping, the fragment-after-
// adoption rule, the restatement fold), and the divergence it would buy back is
// block layout on one spelling of one wire, with the bytes, ids and call order
// already agreeing. The pin below holds the agreement that does exist and the
// divergence that is deliberately kept, so that a later attempt to close it must
// confront both.

import (
	"strings"
	"testing"
)

// TestAnAdoptedFramesTrailingProseKeepsTheFramesOwnOrder is R89-L2-2's pin.
func TestAnAdoptedFramesTrailingProseKeepsTheFramesOwnOrder(t *testing.T) {
	textHi := `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	textThere := `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":" there"}}]}`
	call := `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`
	frame := `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}]}`
	fin := `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`

	_, deltas, argsA := r83OrderArm(t, []string{textHi, call, textThere, fin, r81Done})
	_, framed, argsB := r83OrderArm(t, []string{frame, textThere, fin, r81Done})

	if !strings.Contains(deltas, `text="hi there"`) || !strings.Contains(framed, `text="hi there"`) {
		t.Errorf("one turn, two spellings, two texts: deltas %s, frame-first %s — the relayed bytes are the half of this that IS held, whatever the block layout is (2026-09-29 audit, round 89, R89-L2-2)", deltas, framed)
	}
	if argsA != argsB || argsA != `{"a":1}` {
		t.Errorf("the call's arguments differ between the spellings (%q and %q) — the call is one call under one id in both (2026-09-29 audit, round 89, R89-L2-2)", argsA, argsB)
	}
	if !strings.Contains(deltas, "blocks=text,tool_use ") {
		t.Errorf("the delta spelling is %s, want blocks=text,tool_use — if this changed, the recorded divergence below has been closed and the note in this file must be rewritten rather than the assertion loosened (2026-09-29 audit, round 89, R89-L2-2)", deltas)
	}
	if !strings.Contains(framed, "blocks=text,tool_use,text ") {
		t.Errorf("the frame-first spelling is %s, want the recorded blocks=text,tool_use,text — a whole completion writes its own blocks where it states them and the prose behind it opens a text block of its own (F68-L1-1, widened by this round; 2026-09-29 audit, round 89, R89-L2-2)", framed)
	}
}
