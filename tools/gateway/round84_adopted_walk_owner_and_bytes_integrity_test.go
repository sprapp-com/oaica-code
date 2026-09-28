package main

// round84_adopted_walk_owner_and_bytes_integrity_test.go — leg 3, R84-L3-2 and
// R84-L3-3 (2026-09-28 audit, round 84).
//
// Round 83 gave the adopted walk the fold the non-stream arm has: an entry that
// restates a call the bridge already holds under the same stated id is that
// call, not a second one. The fold fired on the identity alone, and the
// identity is not the question it needs to ask:
//
//   - The identity folds an empty argument list together with `{}`
//     (canonicalCallArgs("") is "{}"). A held call the upstream never gave
//     arguments, restated with `{}`, was dropped whole and its bytes never
//     delivered (R84-L3-3) — the block reached the client with the arguments it
//     had collected written nowhere, where the frame arm folds those bytes in
//     and delivers them.
//   - The identity is not ownership: a list that states an id on a Read call and
//     then twice on the same Bash call answers three calls on this bridge's
//     other two arms — the id names the Read — and the walk answered two, the
//     third entry matching the recorded identity and being dropped (R84-L3-2).
//
// One clause closes both: the drop requires the block the id names to already
// hold these very bytes. A repeat that adds nothing is still dropped, which is
// what keeps one call listed twice from opening a second block (round 83's
// F83-L3-1). A first draft also recorded the id's owner only once, mirroring the
// non-stream arm; measured against both pins, the byte clause alone answers
// them, so that half was dropped rather than shipped.
//
// R84-L3-1, the same round's third finding, is RECORDED rather than fixed: the
// non-stream arm writes the client's `input` re-encoded through the shared
// canonical writer (float64 numbers, HTML-escaped strings, nested keys sorted)
// while the adopted and frame arms write the model's own bytes. Measured, the
// client leg answers leg 3's non-stream arm exactly (`{"n":9007199254740993}`
// reaches the client as `{"n":9007199254740992}` there too, on both its
// spellings), and the two streaming arms here are the pair that differs — but
// they differ because the bytes go out AS THEY ARRIVE (round 51's rule for a
// text that begins an object), and the whole-body arms differ because a typed
// representation has no key order to keep. Closing it means withholding every
// object's bytes until the turn ends, which is leg 2's model and what rounds
// 51, 72 and 80 each decided against on this arm. It is the byte-level twin of
// anthropic/anthropic.go's F68-L1-1 (whole-body order vs streamed order),
// recorded there for the same reason and not changed.

import (
	"strings"
	"testing"
)

// TestARepeatedCallUnderAnotherCallsIDIsStillACall is R84-L3-2. The list
// states an id on a Read call and then twice on the same Bash call. The id
// belongs to the Read, so each Bash entry is a call of its own — minted in
// turn, exactly as the non-stream and frame arms answer them, and not a
// restatement of anything the id names.
func TestARepeatedCallUnderAnotherCallsIDIsStillACall(t *testing.T) {
	entries := []string{
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
		`{"index":1,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"b\":2}"}}`,
		`{"index":2,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"b\":2}"}}`,
	}
	doc, plain, frame := r81ThreeArms(t, entries, "tool_calls")

	want := r82IDs(t, plain)
	if len(want) != 3 {
		t.Fatalf("PREMISE: the non-stream arm answered %v for a three-entry list — this pin reads the other arms against it", want)
	}
	for _, arm := range []struct {
		name string
		body string
	}{{"document", doc}, {"frame", frame}} {
		got := r82IDs(t, arm.body)
		if strings.Join(got, ",") != strings.Join(want, ",") {
			t.Errorf("the %s arm answers the ids %v for %v, want the list's own %v: the id was stated by the Read call, so neither Bash entry is a restatement of anything the id names — the walk recorded the id's owner again on each entry and dropped the third call outright (2026-09-28 audit, round 84, R84-L3-2)\n%s",
				arm.name, got, entries, want, arm.body)
		}
	}
	if got := r80Leg3ToolInputs(t, doc); len(got) != 3 || got[0] != `{"a":1}` || got[1] != `{"b":2}` || got[2] != `{"b":2}` {
		t.Errorf("the document arm's inputs are %v, want the three the list stated (2026-09-28 audit, round 84, R84-L3-2)", got)
	}
}

// TestARepeatedEntrysBytesAreDelivered is R84-L3-3. The list states a call with
// no arguments and then states it again with `{}`. Both entries are one call by
// identity — canonicalCallArgs folds them together — but the second carries
// bytes the block never received, and the frame arm delivers them. Dropping the
// entry on identity alone handed the client a block whose arguments had been
// collected and never written.
func TestARepeatedEntrysBytesAreDelivered(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
	}{
		{
			"the restatement follows immediately",
			[]string{
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Read"}}`,
				`{"index":1,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{}"}}`,
			},
		},
		{
			"another call stands between them",
			[]string{
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Read"}}`,
				`{"index":1,"id":"call_2","type":"function","function":{"name":"Bash","arguments":"{\"b\":2}"}}`,
				`{"index":2,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{}"}}`,
			},
		},
	} {
		doc, plain, frame := r81ThreeArms(t, tc.entries, "tool_calls")
		want := r80Leg3ToolInputs(t, plain)
		if strings.Join(r82IDs(t, doc), ",") != strings.Join(r82IDs(t, plain), ",") {
			t.Fatalf("%s: the document arm answers %v and the non-stream arm %v — this pin reads the inputs against a body the two already agree on (2026-09-28 audit, round 84, R84-L3-3)",
				tc.note, r82IDs(t, doc), r82IDs(t, plain))
		}
		if got := r80Leg3ToolInputs(t, doc); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: the document arm delivers %v and the non-stream and frame arms %v — a restatement whose arguments the block never received is not a repeat this walk may drop: the bytes are the call's only delta, and the frame arm folds them in (2026-09-28 audit, round 84, R84-L3-3)\n%s",
				tc.note, got, want, doc)
		}
		if got := r80Leg3ToolInputs(t, frame); strings.Join(got, "|") != strings.Join(want, "|") {
			t.Errorf("%s: the frame arm delivers %v, want %v (2026-09-28 audit, round 84, R84-L3-3)\n%s", tc.note, got, want, frame)
		}
	}
}
