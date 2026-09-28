package main

// round79_run_bridge_and_indexed_complete_args_integrity_test.go — leg 3, the
// two places where this bridge's three arms still disagreed about one upstream
// body (2026-09-28 audit, round 79).
//
//  1. F79-L3-A. Round 78 taught this leg's splitFromCurrent that an EMPTY
//     accumulated argument list is a COMPLETE one, so a fragment naming the
//     same call again WITH arguments begins the next call. It is asked by
//     toolKey's id arm and id-less arm; the INDEX arm (#n) never asked it, so
//     the fix could not reach the spelling vendors write:
//     `[{index 0,id call_1,name Read},{index 0,id call_1,name Read,arguments
//     {"a":1}}]` reached the client as ONE call holding `{"a":1}`, where both
//     document arms of this bridge answer the argument-less call AND the
//     completed one.
//
//  2. F79-L3-C. Round 77 settled that a nameless entry's bytes are relayed as
//     prose, ONE block per contiguous run, a run ending where a CALL is NAMED
//     and nowhere else. unnamedRunsOf gave an entry with no bytes its own
//     effect: it set prevNameless, so an empty entry arriving AFTER a named
//     call re-opened the run that call had closed and the next nameless
//     entry's bytes were appended to the run BEFORE it —
//     `[{"arguments":"A"},{name:"Read",arguments:"{\"r\":1}"},{"arguments":""},
//     {"arguments":"B"}]` reached the client as ONE text block holding "AB",
//     the model's prose from both sides of the call in one block, where the
//     frame arm writes two (measured on all three arms before the fix: the
//     frame arm answers `<tool_use …><text "A"><text "B">`).

import "testing"

// TestACompleteArgumentListEndsTheCallOnTheIndexedArm is F79-L3-A.
func TestACompleteArgumentListEndsTheCallOnTheIndexedArm(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
	}{
		{
			"the index and the id are stated",
			[]string{
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Read"}}`,
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
			},
		},
		{
			"the index alone is stated",
			[]string{
				`{"index":0,"type":"function","function":{"name":"Read"}}`,
				`{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
			},
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			doc, plain, frame := r76ThreeArms(t, tc.entries)
			gotDoc, gotFrame := r76BlockOrder(t, doc), r76BlockOrder(t, frame)
			if gotDoc != gotFrame {
				t.Errorf("one body, two arms: the frame arm answered %s where the adopted-document arm answered %s — an empty argument list is a complete one, so a call restated with arguments is the next call (2026-09-28 audit, round 79, F79-L3-A)", gotFrame, gotDoc)
			}
			if gotPlain := r76BlockOrder(t, plain); gotPlain != gotDoc {
				t.Errorf("one body, two document arms: the plain arm answered %s where the adopted arm answered %s (2026-09-28 audit, round 79, F79-L3-A)", gotPlain, gotDoc)
			}
		})
	}
}

// TestANamelessRunDoesNotBridgeAcrossANamedCall is F79-L3-C: a run ends where a
// CALL is NAMED, and an entry carrying no bytes — which names nothing — neither
// starts a run nor ends one.
func TestANamelessRunDoesNotBridgeAcrossANamedCall(t *testing.T) {
	entries := []string{
		`{"type":"function","function":{"arguments":"A"}}`,
		`{"type":"function","function":{"name":"Read","arguments":"{\"r\":1}"}}`,
		`{"type":"function","function":{"arguments":""}}`,
		`{"type":"function","function":{"arguments":"B"}}`,
	}
	want := `<tool_use call_ab204a28><text "A"><text "B">`
	doc, plain, frame := r76ThreeArms(t, entries)
	for _, arm := range []struct {
		name, body string
	}{
		{"adopted-document", doc},
		{"plain-document", plain},
		{"frame", frame},
	} {
		if got := r76BlockOrder(t, arm.body); got != want {
			t.Errorf("the %s arm answered %s, want %s — the model's prose from either side of a named call is two runs, not one block (2026-09-28 audit, round 79, F79-L3-C)", arm.name, got, want)
		}
	}
}
