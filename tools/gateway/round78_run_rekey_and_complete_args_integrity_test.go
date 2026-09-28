package main

// round78_run_rekey_and_complete_args_integrity_test.go — leg 3, two bodies on
// which the frame arm answered what neither document arm answered
// (2026-09-28 audit, round 78, F78-L3-1 and F78-L3-2).
//
//  1. F78-L3-1, a REGRESSION of round 77's own fix (55ca99ade). Round 77 routes
//     a nameless fragment to the block of the run currently taking bytes
//     (`namelessRunKey`). An entry of that run which STATES AN ID moves the
//     run's block to the id key — the B45-1 re-key `rekeyToolBlock` exists for —
//     and the re-key rewrote `indexKeys` and `indexChain` but not the run key,
//     which went on naming the key the block had just left. The next entry of
//     the same run then found no block there, minted one of its own, and one
//     contiguous run of nameless entries reached a streaming client as TWO text
//     blocks where both document arms write one.
//  2. F78-L3-2, pre-existing. `splitFromCurrent` ended an accumulated call when
//     its arguments were a complete JSON object, and treated an EMPTY argument
//     list as complete only for the bare repeat (round 39's B-F9: the fragment
//     names the call again and states no arguments). A fragment that named the
//     same call again WITH arguments over a call that had accumulated none was
//     read as its continuation, so `[{name:"Read"},{name:"Read",arguments:
//     "{\"a\":1}"}]` reached a streaming client as ONE tool_use — the first
//     call, which the client runs, did not exist on this arm — where both
//     document arms deliver two. An empty argument list is a COMPLETE argument
//     list for a call that takes none, whatever the next fragment carries.
//
// The conservative direction F78-L3-2's fix must not break is the other one: an
// upstream that RESTATES the name on every argument fragment of one call is not
// stating many calls, so a partial object stays a continuation. That reading is
// pinned here too.

import (
	"testing"
)

// TestARunsBlockSurvivesBeingRekeyedMidRun is F78-L3-1: the key the run is
// taking bytes under is a key like any other the re-key invalidates.
func TestARunsBlockSurvivesBeingRekeyedMidRun(t *testing.T) {
	entries := []string{
		`{"type":"function","function":{"arguments":"A"}}`,
		`{"id":"call_z","type":"function","function":{"arguments":"B"}}`,
	}
	doc, plain, frame := r76ThreeArms(t, entries)
	want := `<text "AB">`
	gotDoc, gotPlain, gotFrame := r76BlockOrder(t, doc), r76BlockOrder(t, plain), r76BlockOrder(t, frame)
	if gotDoc != want {
		t.Errorf("the adopted arm answered %s, want %s (2026-09-28 audit, round 78, F78-L3-1)", gotDoc, want)
	}
	if gotPlain != want {
		t.Errorf("the non-stream arm answered %s, want %s (2026-09-28 audit, round 78, F78-L3-1)", gotPlain, want)
	}
	if gotFrame != want {
		t.Errorf("the fragment arm answered %s, want %s — an entry of a nameless run that states an id moves the run's BLOCK, not the run, so the bytes after it are still the same run's (2026-09-28 audit, round 78, F78-L3-1)\n%s",
			gotFrame, want, frame)
	}
}

// TestACompleteArgumentListEndsTheCall is F78-L3-2, with the reading it must not
// disturb: the second row's accumulator holds a partial object, which is NOT a
// complete argument list, so the restated name continues it.
func TestACompleteArgumentListEndsTheCall(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
		want    string
	}{
		{
			"a call that takes no arguments, then the same name with arguments",
			[]string{
				`{"type":"function","function":{"name":"Read"}}`,
				`{"type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
			},
			"<tool_use call_eb2776e1><tool_use call_8cb43a45>",
		},
		{
			"two calls that take no arguments",
			[]string{
				`{"type":"function","function":{"name":"Read"}}`,
				`{"type":"function","function":{"name":"Read"}}`,
			},
			"<tool_use call_eb2776e1><tool_use call_ec4c11f5>",
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			doc, plain, frame := r76ThreeArms(t, tc.entries)
			gotDoc, gotPlain, gotFrame := r76BlockOrder(t, doc), r76BlockOrder(t, plain), r76BlockOrder(t, frame)
			if gotDoc != tc.want {
				t.Errorf("the adopted arm answered %s, want %s (2026-09-28 audit, round 78, F78-L3-2)", gotDoc, tc.want)
			}
			if gotPlain != tc.want {
				t.Errorf("the non-stream arm answered %s, want %s (2026-09-28 audit, round 78, F78-L3-2)", gotPlain, tc.want)
			}
			if gotFrame != tc.want {
				t.Errorf("the fragment arm answered %s, want %s — the call that takes no arguments has all of them the moment the wire names it, so the next entry naming the same call begins another (2026-09-28 audit, round 78, F78-L3-2)\n%s",
					gotFrame, tc.want, frame)
			}
		})
	}
}

// TestARestatedNameOverAPartialObjectIsNotANewCall is the reading F78-L3-2's fix
// must leave alone: one call whose arguments arrive in fragments that each
// RESTATE the name. The accumulated bytes are not yet valid JSON, so they are
// not a complete argument list and the call has not ended.
func TestARestatedNameOverAPartialObjectIsNotANewCall(t *testing.T) {
	entries := []string{
		`{"type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}`,
		`{"type":"function","function":{"name":"Bash","arguments":"\"ls\"}"}}`,
	}
	_, _, frame := r76ThreeArms(t, entries)
	if got := r76BlockOrder(t, frame); got != "<tool_use call_83cf9330>" {
		t.Errorf("the fragment arm answered %s, want one call — a partial object is not a complete argument list, so a name restated over it continues that call (2026-09-28 audit, round 78, F78-L3-2)\n%s", got, frame)
	}
}
