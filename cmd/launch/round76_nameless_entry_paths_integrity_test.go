package launch

// round76_nameless_entry_paths_integrity_test.go — leg 2, three more places an
// entry the upstream never NAMED was read differently by this leg's own two
// arms (2026-09-28 audit, round 76, F76-L2-1 to F76-L2-4).
//
// An entry whose `function.name` is empty is not a call on this leg: its
// arguments reach the client as TEXT and no block is built from it. Round 75
// closed the un-indexed spelling of that rule in startsANewToolCall; the three
// findings here are the spellings it left, and they are the ones real vendors
// write — the index IS stated, and the name arrives before the arguments.
//
//  1. The indexed path adopted a nameless accumulator. A fragment that names a
//     call, landing at an index whose accumulator is the parked nameless entry,
//     was written INTO that accumulator: the call took arguments the model
//     never gave it and the model's own arguments for it were dropped —
//     `[{index 0, name "", args {"a":1}},{index 0, id c1, name Bash},{index 0,
//     args {"cmd":"ls"}}]` answered `Bash {"cmd":"ls"}` with no text where this
//     leg's whole-list arm answers `Bash {}` and `{"cmd":"ls"}` as prose.
//  2. The site with nowhere left to put an argument-only fragment DROPPED it,
//     while both document arms relay its arguments as text.
//  3. The index-less path joined an argument-only fragment onto a call whose
//     arguments were already a finished object — no canExtend guard, which the
//     indexed path has had since round 61 — so the client got `Read {"_raw":
//     "{\"p\":1}{\"a\":1}"}`, a call no tool can parse, where the whole-list arm
//     answers the call plus the text.
//
// Every row below is one body spelled both ways, answered by this leg's own two
// arms; the assertion is that they agree, byte for byte, block for block.

// Recorded, not fixed (same round): the same body with the named call's own
// arguments arriving as a further nameless entry at that index —
// `[{index 0, name "", args {"a":1}},{index 0, id c1, name Bash},{index 0, args
// {"cmd":"ls"}}]`. This leg's FRAGMENT arm reads the third entry as the open
// call's arguments and answers `Bash {"cmd":"ls"}` with `{"a":1}` as text; the
// whole-list arm reads every nameless ENTRY as prose and answers `Bash {}` with
// `{"a":1}{"cmd":"ls"}` as text, which is also what the gateway leg's document
// arm answers. Before this round the fragment arm lost `{"cmd":"ls"}` entirely
// (it was written into the parked nameless accumulator), so the fix removes the
// loss; the placement question that remains is the header-late fragment wire
// round 75 recorded as open, and closing it means deciding whether a nameless
// entry at a slot whose call has no arguments yet is that call's arguments on
// all THREE legs' document arms at once.

import (
	"encoding/json"
	"fmt"
	"testing"
)

func r76Str(v any) string { return fmt.Sprintf("%v", v) }

func r76InputJSON(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}

// r76ArmBlocks is the [type, id, name, input] of each block, in order, with the
// text each arm relayed beside it.
func r76ArmBlocks(t *testing.T, blocks []map[string]any) string {
	t.Helper()
	out := ""
	for _, b := range blocks {
		out += "<" + r76Str(b["type"])
		if v, ok := b["id"]; ok {
			out += " id=" + r76Str(v)
		}
		if v, ok := b["name"]; ok {
			out += " name=" + r76Str(v)
		}
		if v, ok := b["input"]; ok {
			out += " input=" + r76InputJSON(v)
		}
		out += ">"
	}
	return out
}

func TestANamelessEntryIsAnsweredAlikeOnBothArms(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
	}{
		{
			"a named fragment WITH an index lands after a nameless entry",
			[]string{
				`{"index":0,"type":"function","function":{"name":"","arguments":"{\"a\":1}"}}`,
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`,
			},
		},
		{
			"a nameless fragment at an index a named call already occupies",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
				`{"index":0,"type":"function","function":{"name":"","arguments":"{\"b\":2}"}}`,
			},
		},
		{
			"an index-less nameless fragment after a finished call",
			[]string{
				`{"index":1,"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}`,
				`{"type":"function","function":{"name":"","arguments":"{\"a\":1}"}}`,
			},
		},
		{
			"index-less nameless fragments, then two named calls",
			[]string{
				`{"type":"function","function":{"name":"","arguments":"{\"a\":1}"}}`,
				`{"type":"function","function":{"name":"","arguments":"{\"a\":1}"}}`,
				`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
				`{"index":1,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
			},
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			frag, whole, fragText, wholeText, fragRaw, wholeRaw := r75Arms(t, tc.entries)
			if len(frag) != len(whole) {
				t.Fatalf("one body, two arms: the fragment arm answered %s and the whole-list arm %s — a call the upstream never named is not a call on either (2026-09-28 audit, round 76)\nstream:\n%s\nwhole:\n%s",
					r76ArmBlocks(t, frag), r76ArmBlocks(t, whole), fragRaw, wholeRaw)
			}
			for i := range frag {
				if r76ArmBlocks(t, frag[i:i+1]) != r76ArmBlocks(t, whole[i:i+1]) {
					t.Errorf("block %d is %s as fragments and %s as one list (2026-09-28 audit, round 76)",
						i, r76ArmBlocks(t, frag[i:i+1]), r76ArmBlocks(t, whole[i:i+1]))
				}
			}
			if fragText != wholeText {
				t.Errorf("the arms relayed different text: %q as fragments, %q as one list — an entry the upstream never named reaches the client as prose on both (2026-09-28 audit, round 76)\nstream:\n%s\nwhole:\n%s",
					fragText, wholeText, fragRaw, wholeRaw)
			}
		})
	}
}
