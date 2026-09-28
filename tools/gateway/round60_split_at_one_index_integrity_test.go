package main

// round60_split_at_one_index_integrity_test.go — round 60's finding on the
// gateway bridge's index arm (F60-L3-1).
//
// The index arm splits a fragment off as the next call when it states an id the
// slot does not hold, or a name the block it holds does not carry. A vendor that
// reuses ONE index for every call of a turn — the shape round 36's B-F2 is about
// — states nothing new on the second call except its arguments, so neither
// clause reached it: the second call's own complete object was appended to the
// first's, and the client accumulated `{"a":1}{"b":2}` — JSON no tool can parse
// — under a stop_reason of tool_use. The same body's document arm answers two
// calls, and the client and gateway legs' own whole-list arms agree with it.
//
// The first two cases below ask the SAME body of both of this bridge's arms —
// the fragments the upstream streamed, and the whole completion the same
// upstream writes — and require the same calls with the same ids and inputs.
// Each is fail-first: RED against the tree before this round's fix.
//
// The third does NOT, and could not: the two spellings of a CONTINUATION are
// not the same wire. A list states calls, so an entry that continues an earlier
// entry's bytes is a second call (round 69 measured the client leg's list arm
// saying so; round 82 pinned both of this bridge's document arms to it); a run
// of fragments may be writing one call, so more of the same line is more of
// that call. This case pins the fragment arm's half of that difference — the
// list's half is round 82's TestAContinuationIsReadByItsSpelling, and neither
// reading is a divergence from the other (2026-09-28 audit, round 82).

import (
	"testing"
)

// r60DocArm is the document arm's answer: the calls, their inputs, and the text
// the turn relayed besides them.
func r60DocArm(t *testing.T, doc string) ([]string, []string, string) {
	t.Helper()
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	_, body := round45Ask(t, srv, round45AskStream)
	return round46ToolUseIDs(t, body), r58Partials(t, body), r53BlockText(t, body)
}

// r60FrameArm is the fragment arm's answer to the same body.
func r60FrameArm(t *testing.T, frames ...string) ([]string, []string, string) {
	t.Helper()
	_, body := r58FrameCalls(t, frames...)
	return round46ToolUseIDs(t, body), r58Partials(t, body), r53BlockText(t, body)
}

// r60ArmSame requires the fragment arm to answer the same body with the same
// calls, the same inputs, and the same relayed prose as the document arm.
func r60ArmSame(t *testing.T, ids, parts []string, text string, wantIDs, wantParts []string, wantText, note, body string) {
	t.Helper()
	if len(ids) != len(wantIDs) {
		t.Errorf("the same body is %d call(s) as fragments and %d as one list\nfragments: %v\none list:  %v\n%s\n%s", len(ids), len(wantIDs), ids, wantIDs, note, body)
		return
	}
	for i := range wantIDs {
		if ids[i] != wantIDs[i] || parts[i] != wantParts[i] {
			t.Errorf("call %d is %s %s as fragments and %s %s as one list\n%s\n%s", i, ids[i], parts[i], wantIDs[i], wantParts[i], note, body)
		}
	}
	if text != wantText {
		t.Errorf("the same body relays %q as prose on the fragment arm and %q as one list\n%s\n%s", text, wantText, note, body)
	}
}

// TestASecondCompleteObjectAtOneIndexIsTheNextCall is F60-L3-1: two fragments
// at ONE index, the same id and the same name stated on both, each carrying a
// COMPLETE argument object. The vendor that writes index 0 for every call of the
// turn states the slot's own id and name a second time, and the object it brings
// is not more of the object the slot holds.
func TestASecondCompleteObjectAtOneIndexIsTheNextCall(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"b\":2}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"b\":2}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a fragment that restates the slot's own id and name but carries a DIFFERENT, already-complete object is neither a restatement nor a split, so it was appended to the finished one: the client accumulated `{\"a\":1}{\"b\":2}`, JSON no tool can parse, under a stop_reason of tool_use, and the model's second call did not exist on this arm (2026-09-28 audit, round 60, F60-L3-1)", "")
}

// TestASecondCompleteObjectWithoutAnIndexStaysTwoCalls is F60-L3-1's control:
// the same wire with the index omitted is the shape round 38 pinned — a fragment
// that names its call again without a slot continues the call the stream last
// named — and the fix above must not have changed it.
func TestASecondCompleteObjectWithoutAnIndexStaysTwoCalls(t *testing.T) {
	ids, _, _ := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_1","function":{"name":"Bash","arguments":"{\"b\":2}"}}]}}]}`,
	)
	if len(ids) != 2 {
		t.Errorf("CONTROL broke: the index-less twin of the wire above gave %d call(s) %v, want 2", len(ids), ids)
	}
}

// TestOneFreeformObjectAtOneIndexIsStillOneCall is the control the fix's own
// gate needs: argsAreFinished counts freeform text as finished because freeform
// arrives whole, so a fragment restating the slot's own id and name with MORE OF
// THE SAME LINE is not a second call. Splitting it hands the client half of the
// model's command — the freeform wire round 51's G1 pins as one call whose input
// is the whole line.
func TestOneFreeformObjectAtOneIndexIsStillOneCall(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"echo hel"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"lo world"}}]}}]}`,
	)
	if len(ids) != 1 {
		t.Fatalf("the model's one command came out as %d call(s) %v, want one", len(ids), ids)
	}
	if parts[0] != `{"_raw":"echo hello world"}` {
		t.Errorf("the freeform command reached the client as %q, want the model's whole line\n%s", parts[0], text)
	}
}
