package main

// round71_stated_id_owner_and_order_integrity_test.go — round 71's two findings
// on the gateway bridge (F71-L3-1, F71-L3-2).
//
// Both are the same shape: the fragment arm and the document arms disagree about
// the SAME upstream body. In F71-L3-1 they disagree about WHICH call wears a
// reused stated id; in F71-L3-2 they disagree about the ORDER two held calls are
// opened in. Neither is reachable from the corpus: every id-reuse pin in it
// asserts only that the two ids differ (or that two blocks exist), and the
// order pins carry a single held call.
//
// Measured on the frozen tree before this round's fixes: F71-L3-1's frame arm
// keeps `call_1` for the SECOND call where both document arms keep it for the
// first; F71-L3-2's frame arm opens the two held calls in the order the
// restating fragment's index implies, which neither document arm can see.

import (
	"strings"
	"testing"
)

// r71Leg3Doc is the whole completion the same upstream writes for a two-entry
// turn, with the entries given verbatim (no index field: the document arms hand
// every entry its list position).
func r71Leg3Doc(entries string) string {
	return `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[` + entries + `]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
}

// r71Leg3Frame spells one fragment of the same turn.
func r71Leg3Frame(entry string) string {
	return `data: {"choices":[{"delta":{"tool_calls":[` + entry + `]}}]}`
}

// TestAReusedStatedIDBelongsToTheCallThatStatedItFirst is F71-L3-1: two
// index-less fragments, the SAME id stated on both, for two DIFFERENT calls —
// the first argument-less (an ordinary no-argument tool), the second carrying a
// complete object under another name. The id is the turn's first statement of
// it, so the call that stated it first is the one that wears it; both document
// arms read it that way, and the fragment arm minted the FIRST call and left the
// reused id on the second.
func TestAReusedStatedIDBelongsToTheCallThatStatedItFirst(t *testing.T) {
	first := `{"id":"call_1","type":"function","function":{"name":"Read"}}`
	second := `{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"b\":2}"}}`

	frameIDs, frameParts, frameText := r60FrameArm(t,
		r71Leg3Frame(`{"id":"call_1","function":{"name":"Read"}}`),
		r71Leg3Frame(`{"id":"call_1","function":{"name":"Bash","arguments":"{\"b\":2}"}}`),
	)
	wantIDs, wantParts, wantText := r60DocArm(t, r71Leg3Doc(first+`,`+second))

	// Premise: both arms really answer two calls, so the id comparison below is
	// about who owns `call_1` and not about a lost call.
	if len(wantIDs) != 2 || len(frameIDs) != 2 {
		t.Fatalf("PREMISE: want two calls per arm, got one-list=%v fragments=%v", wantIDs, frameIDs)
	}
	if wantIDs[0] != "call_1" || strings.Contains(wantParts[0], "b") {
		t.Fatalf("PREMISE: the one-list arm answers %v %v for the first call, want the stated call_1 carrying the argument-less call's own input", wantIDs, wantParts)
	}
	if frameIDs[0] != wantIDs[0] || frameIDs[1] != wantIDs[1] ||
		frameParts[0] != wantParts[0] || frameParts[1] != wantParts[1] {
		t.Errorf("the same body is %v %v as fragments and %v %v as one list: the id the wire stated twice belongs to the call that stated it first, and the fragment arm handed it to the other one — a reused stated id landed on a different call depending on whether the upstream streamed it or wrote it as one document (2026-09-28 audit, round 71, F71-L3-1)", frameIDs, frameParts, wantIDs, wantParts)
	}
	if frameText != wantText {
		t.Errorf("the same body relays %q as prose on the fragment arm and %q as one list", frameText, wantText)
	}
}

// TestTwoHeldCallsAreOpenedInTheSameOrderOnBothArms is F71-L3-2: two
// argument-less calls are held (nothing of them can be opened yet), then the
// first is restated with its arguments. The document arms hand every entry its
// list position and open them in that order; the fragment arm's order depends on
// the index the restating fragment states, which no document arm reads.
//
// The order both arms must answer is the one the client leg answers this very
// body with — [call_1 Read, call_781541ff Bash] under BOTH of its spellings,
// measured on the frozen tree — which is also the order the model wrote the
// calls in: the call it introduced first is the one the client's list opens
// first (round 63's F63-L3-1).
func TestTwoHeldCallsAreOpenedInTheSameOrderOnBothArms(t *testing.T) {
	for _, restated := range []struct {
		name, entry string
	}{
		{"the restatement stating the first call's own index", `{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{}"}}`},
		{"the restatement stating a later index", `{"index":2,"id":"call_1","function":{"name":"Read","arguments":"{}"}}`},
	} {
		t.Run(restated.name, func(t *testing.T) {
			frameIDs, frameParts, _ := r60FrameArm(t,
				r71Leg3Frame(`{"index":0,"id":"call_1","function":{"name":"Read"}}`),
				r71Leg3Frame(`{"id":"call_1","function":{"name":"Bash"}}`),
				r71Leg3Frame(restated.entry),
			)
			wantIDs, wantParts, _ := r60DocArm(t, r71Leg3Doc(
				`{"index":0,"type":"function","id":"call_1","function":{"name":"Read"}},`+
					`{"type":"function","id":"call_1","function":{"name":"Bash"}},`+
					`{"index":0,"type":"function","id":"call_1","function":{"name":"Read","arguments":"{}"}}`,
			))
			if len(frameIDs) != 2 || len(wantIDs) != 2 {
				t.Fatalf("PREMISE: want two calls per arm, got one-list=%v parts=%v fragments=%v", wantIDs, wantParts, frameIDs)
			}
			// Premise: both arms keep the call the model introduced first in the
			// place it was introduced, and its restatement's arguments on it.
			for _, arm := range []struct {
				name     string
				ids      []string
				parts    []string
				mintName string
			}{{"one list", wantIDs, wantParts, "Bash"}, {"fragments", frameIDs, frameParts, "Bash"}} {
				if arm.ids[0] != "call_1" || !strings.Contains(arm.parts[0], "{}") {
					t.Errorf("PREMISE: the %s arm answers %v %v for the first call, want the call the upstream introduced first, under its stated call_1 and with the restatement's arguments", arm.name, arm.ids, arm.parts)
				}
				if arm.ids[1] == "call_1" || strings.Contains(arm.parts[1], "{}") {
					t.Errorf("PREMISE: the %s arm answers %v %v for the second call, want the argument-less call the upstream never named under an id of its own", arm.name, arm.ids, arm.parts)
				}
			}
			if frameIDs[0] != wantIDs[0] || frameIDs[1] != wantIDs[1] ||
				frameParts[0] != wantParts[0] || frameParts[1] != wantParts[1] {
				t.Errorf("the same body opens %v %v as fragments and %v %v as one list: two calls the upstream held cannot be ordered two ways, and the fragment arm's answer moved with the index a RESTATEMENT stated (2026-09-28 audit, round 71, F71-L3-2)", frameIDs, frameParts, wantIDs, wantParts)
			}
		})
	}
}
