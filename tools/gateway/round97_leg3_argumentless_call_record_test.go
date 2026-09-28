package main

// round97_leg3_argumentless_call_record_test.go — leg 3, round 97,
// R97-L3-1 and R97-L3-2, RECORDED (2026-09-29 audit).
//
// The auditor's repro: one upstream turn whose call states its name and NO
// arguments, followed by a nameless fragment carrying bytes. The two spellings
// read differently, and have since the fragment arm was written:
//
//	plain/adopt (whole document)   call Bash input {}   +  text "{\"cmd\":\"ls\"}"
//	framed   (one entry per delta) call Bash input {"cmd":"ls"}   (or _raw)
//
// On the whole-document spellings each `tool_calls` entry is an entry of a
// LIST, and an entry that names nothing is not a call (round 39's B-F8; round
// 74's F74-L3-1 gives each entry the list's own order): its bytes are prose. On
// the fragment spelling a delta that states the call's index (or nothing, after
// a nameless prefix) is a CONTINUATION of the call already open, and
// `callArgsExtend` — the write's own test — accepts it.
//
// It is recorded, not fixed, and the fix is not the code's to make blind:
//
//   - The fragment arm's reading is PINNED, and by more than one round. Round
//     45's B45-1 ("an id that arrives after the fragment naming the call is
//     that call's own id, and the client leg merges the two fragments for
//     exactly this reason"), round 48's B48 (`TestASplitIDLessCallMintsIts…
//     Arguments`: the split wire must mint the SAME id the unsplit wire mints,
//     which holds only if the fragment folded), rounds 58/59/62's indexed-slot
//     and call-feed wires, round 67's F67-L3-1 (which made the sibling route
//     ask `argsAreMidObject` and left THIS route on the write's test), and
//     round 81's F81-L3-3 (which closed only the closed-block case). Rounding
//     `callArgsExtend` off this branch was attempted in this round and measured:
//     the whole gateway suite goes RED on ten pinned tests of those rounds. A
//     deliberate, already-pinned decision is not reverted by a later round.
//   - The two readings are the two LEGS' own split, not one leg's mistake: the
//     client leg (cmd/launch) merges fragments the same way, and its document
//     arm files list entries the same way (round 45's B45-1 says so in the
//     code). Every leg answers the same spelling the same way; the divergence
//     is between the two spellings of the wire.
//   - R97-L3-2 is the same branch with the accumulator non-empty (a freform
//     call still open): the fold there is the freeform continuation round 60's
//     F60-L3-2 family records. `callArgsExtend` is what the freeform line
//     needs ("more of a line is more of the call", round 81's F81-L3-3), so the
//     empty-accumulator carve-out cannot be spelled without it either.
//
// Whether a whole document and a fragment stream of it must read alike is a
// decision about what the two spellings ARE — round 82's F82-L3-1 already ruled
// once that they need not (the adopted walk keeps list entries as calls where
// the fragment walk folds a mid-object continuation) — and a later round takes
// it deliberately, with both rounds' pins in front of it.
//
// This pin states the reading as it stands, and its failure is how a later
// round learns it changed the decision.

import (
	"strings"
	"testing"
)

// nonEmptyArgs drops the empty `{}` a tool_use block carries when the call has
// no arguments.
func nonEmptyArgs(in []string) []string {
	out := make([]string, 0, len(in))
	for _, a := range in {
		if strings.TrimSpace(a) != "" && strings.TrimSpace(a) != "{}" {
			out = append(out, a)
		}
	}
	return out
}

// TestAnArgumentLessCallKeepsTheFragmentSpellingsReading pins R97-L3-1's
// measured divergence, in both of the shapes the auditor filed.
func TestAnArgumentLessCallKeepsTheFragmentSpellingsReading(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		frames  []string
	}{
		{
			"the fragment states no index",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
				`{"function":{"arguments":"rm -rf /"}}`,
			},
			[]string{
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"Bash"}}]}}]}`,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"rm -rf /"}}]}}]}`,
			},
		},
		{
			"the fragment states the call's index",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
				`{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}`,
			},
			[]string{
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"Bash"}}]}}]}`,
				`data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			texts, args := round97Spellings(t, tc.entries, tc.frames)
			documentProse := strings.Join(texts[0], "")
			fragmentProse := strings.Join(texts[2], "")
			if documentProse == "" || !strings.Contains(documentProse, "cmd") && !strings.Contains(documentProse, "rm -rf") {
				t.Errorf("the document spelling no longer relays the nameless entry as prose: %q", documentProse)
			}
			if fragmentProse != "" {
				t.Errorf("the fragment spelling now relays the nameless fragment as prose (%q): the fold was pinned off this branch deliberately (round 48's B48 and nine more)", fragmentProse)
			}
			if len(args[2]) == 0 || !strings.Contains(strings.Join(args[2], ""), "cmd") && !strings.Contains(strings.Join(args[2], ""), "rm -rf") {
				t.Errorf("the fragment spelling no longer folds the fragment's bytes into the call's arguments: %v", args[2])
			}
			// `{}` is the argument-less call's own (empty) input on the document
			// spelling, not bytes folded into it.
			documentArgs := nonEmptyArgs(args[0])
			if len(documentArgs) != 0 {
				t.Errorf("the document spelling now folds the nameless entry into the call's arguments (%v): round 39's B-F8 keeps it prose", documentArgs)
			}
			t.Logf("document prose %q, call %v | fragment prose %q, call %v",
				documentProse, documentArgs, fragmentProse, nonEmptyArgs(args[2]))
		})
	}
}
