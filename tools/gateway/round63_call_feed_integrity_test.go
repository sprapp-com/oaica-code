package main

// round63_call_feed_integrity_test.go — round 63's findings on the metered
// gateway's call feed (F63-L3-1, F63-L3-2, F63-L3-3, F63-L3-4).
//
// This bridge holds ONE block open at a time, so a call is opened, written and
// closed in the order the upstream's fragments reach it — and the round 62 hold
// that keeps a block with nothing stated from being opened early gave that
// order two new ways to go wrong, both of them the same shape: a call the
// upstream introduced first was opened after one it introduced later.
//
//   - F63-L3-1: the call introduced with NO arguments is held until bytes
//     arrive, and a call introduced after it was opened in its place — the
//     client's list carried the turn's calls in the reverse of the order the
//     model wrote them.
//   - F63-L3-2: the call in progress has arguments the upstream has not
//     finished writing (an object still open). Opening the next call closed it
//     where it stood and the bytes that would have finished it were refused at
//     the closed block and lost.
//   - F63-L3-3: a fragment that states an id at an index the call never opened
//     was keyed by that index instead of by the id it carried, so the call was
//     opened a second time under a minted id with half its arguments, and the
//     rest of its own arguments were relayed to the client as prose.
//   - F63-L3-4: a fragment that states a call's own id and name with nothing at
//     all, at a slot whose call is already finished, was read as a repeat of
//     that call — a finished argument list takes no bytes — and the call the
//     upstream listed did not exist on this arm, while its own document arm and
//     the client leg both answered it.
//
// Every case asks the same body of both of this bridge's arms — the fragments
// the upstream streamed, and the whole completion the same upstream writes —
// and requires the same calls with the same ids and inputs, in the same order.
// Each is fail-first: RED against the tree before this round's fix.

import (
	"testing"
)

// TestTheCallIntroducedFirstIsOpenedFirst is F63-L3-1: the turn's first call
// states no arguments at all, and the second call's fragments arrive before its
// own do.
func TestTheCallIntroducedFirstIsOpenedFirst(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"c","function":{"name":"Glob","arguments":"{\"g\":\"*\"}"}}]}}]}`,
	)
	// The turn the upstream wrote: the calls in the order its list states them.
	wantIDs, wantParts, wantText := []string{"a", "c"}, []string{"", `{"g":"*"}`}, ""
	docIDs, docParts, docText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":""}},`+
		`{"index":1,"id":"c","type":"function","function":{"name":"Glob","arguments":"{\"g\":\"*\"}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	// Both arms are asked the turn's own order, and the document arm is the
	// premise: a case whose whole-completion arm answered the reverse too would
	// pass a comparison against itself.
	if len(docIDs) != len(wantIDs) {
		t.Fatalf("PREMISE: the whole-completion arm answers %v, want this turn's %v calls", docIDs, wantIDs)
	}
	for i := range wantIDs {
		if docIDs[i] != wantIDs[i] || docParts[i] != wantParts[i] {
			t.Fatalf("PREMISE: the whole-completion arm answers %s %s at call %d, want this turn's %s %s", docIDs[i], docParts[i], i, wantIDs[i], wantParts[i])
		}
	}
	if docText != wantText {
		t.Fatalf("PREMISE: the whole-completion arm relays %q, want %q", docText, wantText)
	}
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the call the model introduced first, with no arguments of its own yet, was opened AFTER the call introduced beside it: the hold that keeps an empty block from opening early let the later call take the open slot, so the client's list carries the turn's calls in the reverse of the order the model wrote them (2026-09-28 audit, round 63, F63-L3-1)", "")
}

// TestACallInProgressIsNotClosedForTheNextOne is F63-L3-2: the open call's
// arguments are an object the model has not finished writing, and the next
// call's fragments arrive across them.
func TestACallInProgressIsNotClosedForTheNextOne(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"b","function":{"name":"Read","arguments":"{\"p\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},`+
		`{"index":1,"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the call in progress had arguments left to write and the next call's start closed it mid-object: the client ran Bash with the input {\"cmd\": and the rest of its own arguments were stranded at a block that could not take them (2026-09-28 audit, round 63, F63-L3-2)", "")
}

// TestAStatedIDIsTheCallWhereverTheFragmentFilesIt is F63-L3-3: the call's id
// is stated again on a fragment the upstream filed at an index the call never
// opened.
func TestAStatedIDIsTheCallWhereverTheFragmentFilesIt(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Bash","arguments":"\"ls\"}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the fragment states the id of the call the model is still writing, at an index that call never opened: keyed by the index, it opened a SECOND call under a minted id with half the arguments, and the rest of the model's own arguments reached the client as prose beside it (2026-09-28 audit, round 63, F63-L3-3)", "")
}

// TestACallListedWithNothingToAddIsTheNextCall is F63-L3-4: the fragment states
// a finished call's own id and name and no arguments at all, at the slot the
// vendor writes for every call of the turn.
func TestACallListedWithNothingToAddIsTheNextCall(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"Read","arguments":"{\"p\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"Read","arguments":""}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}},`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":""}},`+
		`{"index":0,"id":"b","type":"function","function":{"name":"Read","arguments":""}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a call the upstream listed with its own id and name and no arguments at all was read as a repeat of the finished call it names: a finished argument list takes no more bytes, and the call the upstream listed did not exist on this arm (2026-09-28 audit, round 63, F63-L3-4)", "")
}
