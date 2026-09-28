package main

// round82_document_entry_and_continuation_integrity_test.go — leg 3, the shape
// round 74 left behind: a whole completion is a LIST, and one of its two arms
// was reading it by the FRAGMENT spelling's rule (2026-09-28 audit, round 82).
//
// A turn reaches this bridge in two spellings, and they are not the same wire:
//
//	the LIST     a whole completion. Every entry of ToolCalls is a call the
//	             upstream named, in the list's own order — there is no slot
//	             identity to fold on, which is why this arm hands each entry its
//	             POSITION as its slot (round 74's F74-L3-1). Two entries that
//	             share an id and a name are two calls, and a second one is
//	             re-minted under the rule both other legs' list arms apply
//	             (round 45's A45-3, leg 1's seenStatedID, the client leg's
//	             dedupKey).
//	the FRAGMENTS a run of chunks. One call may be written across several of
//	             them, so bytes that continue an object the accumulator has not
//	             finished are more of THAT call (callArgsExtend; rounds 60's
//	             F60-L3-1 for the complete objects that are NOT, 61, 63, 64).
//
//  1. F82-L3-1. The two spellings' readings were mixed INSIDE one leg. A list
//     whose second entry continues the first's half-written object reached the
//     adopted arm as ONE call and this bridge's own non-stream arm as TWO (the
//     same body, `stream:true` against `stream:false`), because the adopt walk
//     hands its entries fresh slot positions and the reroute that saves a
//     fragment stranded at a fresh index — written for the FRAGMENT arm,
//     round 63's F63-L3-3 — fired on the list's second entry too.
//
//  2. The freeform continuation, which is the same wire with text instead of an
//     object: the LIST reads it as two calls (both other legs' list arms do;
//     round 69 measured the client leg's) and the FRAGMENTS as one call (round
//     51's G1, round 60's control). Round 60 pinned that on the fragment arm
//     alone and its file header claims both arms were asked for every case; this
//     is the missing half, taken as the deliberate difference between the two
//     spellings rather than a divergence to close — a list cannot tell two calls
//     that share a slot from one call written in two entries, and the two
//     readings are each right for the wire they are given.

import (
	"strings"
	"testing"
)

// r82IDs reads the tool_use ids of a turn in either shape, the way r76BlockOrder
// and r80Leg3ToolInputs do. round46ToolUseIDs decodes EVENTS only, and
// round46BlockIDs a JSON body only; this asks whichever the arm answered in.
func r82IDs(t *testing.T, body string) []string {
	t.Helper()
	if strings.Contains(body, "event: ") {
		return round46ToolUseIDs(t, body)
	}
	return round46BlockIDs(t, body)
}

// TestAListsContinuationIsASecondCall is F82-L3-1. The list's second entry
// continues the first's half-written object under the same id and name: two
// calls on both document arms, the second re-minted, with the same ids and the
// same inputs on each.
func TestAListsContinuationIsASecondCall(t *testing.T) {
	entries := []string{
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}`,
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"1}"}}`,
	}
	doc, plain, _ := r81ThreeArms(t, entries, "tool_calls")
	docIDs, docParts := r82IDs(t, doc), r80Leg3ToolInputs(t, doc)
	plainIDs, plainParts := r82IDs(t, plain), r80Leg3ToolInputs(t, plain)

	if len(docIDs) != 2 || len(plainIDs) != 2 {
		t.Fatalf("the same list is %d call(s) adopted and %d non-stream, want 2 on both — a list states calls, and an entry whose bytes continue an earlier entry's half-written object is the second call the list lists, not more of the first (2026-09-28 audit, round 82, F82-L3-1)\nadopted: %v %v\nnon-stream: %v %v",
			len(docIDs), len(plainIDs), docIDs, docParts, plainIDs, plainParts)
	}
	if docIDs[0] != "call_1" {
		t.Errorf("the adopted arm's first call is %q, want the id the list stated for it (call_1)\n%v", docIDs[0], docIDs)
	}
	if docIDs[1] == "call_1" || plainIDs[1] == "call_1" {
		t.Errorf("the second call wears the FIRST call's stated id (%v / %v): one id over two blocks cannot be answered separately, and the re-mint is what A45-3 asks of a list that states one id twice for different arguments", docIDs, plainIDs)
	}
	if docIDs[0] != plainIDs[0] || docIDs[1] != plainIDs[1] {
		t.Errorf("the two document arms answer one list with different ids: adopted %v, non-stream %v — the same body under `stream:true` and `stream:false` must be the same two calls", docIDs, plainIDs)
	}
	for i := range docParts {
		if docParts[i] != plainParts[i] {
			t.Errorf("call %d is %q adopted and %q non-stream: the two document arms of one bridge must answer one list alike (round 74's F74-L3-1)", i, docParts[i], plainParts[i])
		}
	}
	if docParts[0] != `{"_raw":"{\"a\":"}` || docParts[1] != `{"_raw":"1}"}` {
		t.Errorf("the list's two entries reached the client as %v, want the two halves the list stated — the arguments never became one object here, because a list's entries are whole calls and this arm does not concatenate them", docParts)
	}
}

// TestAContinuationIsReadByItsSpelling is the control for the freeform wire
// above: the LIST is two calls on both document arms and the FRAGMENTS are one,
// which is what each spelling's rule says of it.
func TestAContinuationIsReadByItsSpelling(t *testing.T) {
	entries := []string{
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hel"}}`,
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"lo world"}}`,
	}
	doc, plain, frame := r81ThreeArms(t, entries, "tool_calls")
	for _, arm := range []struct {
		name string
		body string
	}{
		{"adopted", doc},
		{"non-stream", plain},
	} {
		ids, parts := r82IDs(t, arm.body), r80Leg3ToolInputs(t, arm.body)
		if len(ids) != 2 {
			t.Errorf("the %s arm answered the LIST with %d call(s) %v, want the 2 it states: a list's entry is a call, and text that continues an earlier entry's line is that entry's own bytes in the spelling where entries are whole calls\n%s",
				arm.name, len(ids), ids, arm.body)
			continue
		}
		if parts[0] != `{"_raw":"echo hel"}` || parts[1] != `{"_raw":"lo world"}` {
			t.Errorf("the %s arm answered the LIST with %v, want the two entries' own bytes", arm.name, parts)
		}
	}
	// The FRAGMENT spelling of the same bytes is ONE call: the model's whole
	// line — round 51's G1 and round 60's control, which this case must not
	// disturb.
	frameParts := r80Leg3ToolInputs(t, frame)
	if len(frameParts) != 1 || frameParts[0] != `{"_raw":"echo hello world"}` {
		t.Errorf("the fragment arm answered the FRAGMENTS with %v, want one call carrying the model's whole line (`echo hello world`): freeform arrives whole, so more of the same line is more of that call (round 51's G1)\n%s", frameParts, frame)
	}
}
