package main

// round61_fragment_split_integrity_test.go — round 61's findings on the gateway
// bridge's index arm (F61-L3-1, F61-L3-2).
//
// Round 60 split a call off a slot the upstream stated a second time only when
// BOTH argument texts were already complete objects. The wire it was written for
// does not have to spell it that way: the second call's arguments arrive in
// pieces, so under a vendor that reuses one index the pieces `{"b":` and `2}`
// were appended to the first call's finished object, and the client accumulated
// `{"a":1}{"b":2}` — JSON no tool can parse, under a stop_reason of tool_use.
//
//   - F61-L3-1: the second call's object arrives CHUNKED at one index. Neither
//     piece is a complete object, but they cannot be more of the object the slot
//     holds either, so they begin the next call — the same answer the document
//     arm and the other legs' whole-list arms give.
//   - F61-L3-2: a free-form line is the model's whole command (round 51's G1),
//     delivered whole, so a complete OBJECT arriving after one is not more of
//     that line. Appending handed the client `{"_raw":"echo hi{\"c\":3}"}`, a
//     command the model never wrote.
//
// Each case asks the SAME body of both of this bridge's arms — the fragments the
// upstream streamed and the whole completion the same upstream writes — and each
// is fail-first: RED against the tree before this round's fix.

import (
	"testing"
)

// TestTheChunkedSecondObjectAtOneIndexIsTheNextCallOnThisLeg is F61-L3-1: two
// fragments at ONE index, the slot's own id and name restated, carrying a
// CHUNKED second object. The document arm answers two calls.
func TestTheChunkedSecondObjectAtOneIndexIsTheNextCallOnThisLeg(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"b\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"2}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"b\":2}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a fragment that restates the slot's own id and name but carries a CHUNKED second object is neither a restatement nor, under the old both-sides-complete gate, a split: the pieces were appended to the finished object and the client accumulated `{\"a\":1}{\"b\":2}`, JSON no tool can parse, under a stop_reason of tool_use (2026-09-28 audit, round 61, F61-L3-1)", "")
}

// TestAnObjectAfterAFreeformLineIsNotMoreOfItOnThisLeg is F61-L3-2: a complete
// object arriving after a free-form line the slot already holds. It is not more
// of that line — appending handed the client `{"_raw":"echo hi{\"c\":3}"}`, a
// command the model never wrote.
//
// REVISED (2026-09-28 audit, round 76): the body the two arms are asked is now
// the SAME body. This pin used to hand the document arm the first entry alone
// ("the document arm answers the line alone"), and the frame arm's second
// fragment was therefore compared against a document that never carried those
// bytes — which is how a drop on one arm passed as agreement. The fragment
// states no name, so it is not a call on ANY arm of this leg: its bytes are
// relayed to the client as TEXT (round 75, and round 76's F76-L3-1). Measured
// with the faithful body, the document arm has always relayed them as prose,
// and the frame arm — the arm that dropped them — now does too.
func TestAnObjectAfterAFreeformLineIsNotMoreOfItOnThisLeg(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"echo hi"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"c\":3}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hi"}},`+
		`{"index":0,"type":"function","function":{"arguments":"{\"c\":3}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	if text != `{"c":3}` {
		t.Fatalf("CONTROL: with the faithful body the nameless entry's bytes are prose on this leg, got %q", text)
	}
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the model's free-form command is delivered whole (round 51's G1), so an object arriving after it is not more of that line: appended, the client ran `{\"c\":3}` glued to `echo hi` (2026-09-28 audit, round 61, F61-L3-2). The entry states no name, so the bytes are prose — one answer from every arm (round 76)", "")
}

// TestThePinnedFragmentWiresStillAnswerOnThisLeg holds the wires this round's
// change must not disturb: the free-form line delivered in two pieces is ONE
// call whose input is the whole line (round 51's G1), and the index-less twin of
// the chunked wire is the shape round 60 pinned — two calls.
func TestThePinnedFragmentWiresStillAnswerOnThisLeg(t *testing.T) {
	t.Run("a chunked freeform line stays one call", func(t *testing.T) {
		ids, parts, _ := r60FrameArm(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"echo hel"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"lo world"}}]}}]}`,
		)
		if len(ids) != 1 {
			t.Fatalf("the model's one command came out as %d call(s) %v, want one", len(ids), ids)
		}
		if parts[0] != `{"_raw":"echo hello world"}` {
			t.Errorf("the freeform command reached the client as %q, want the model's whole line", parts[0])
		}
	})

	t.Run("the chunked second object without an index stays two calls", func(t *testing.T) {
		ids, _, _ := r60FrameArm(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"name":"Bash","arguments":"{\"b\":"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"2}"}}]}}]}`,
		)
		if len(ids) != 2 {
			t.Errorf("CONTROL broke: the index-less twin of the chunked wire gave %d call(s) %v, want 2", len(ids), ids)
		}
	})
}
