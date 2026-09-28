package launch

// round77_nameless_entry_bytes_integrity_test.go — leg 2, where an entry the
// upstream never named keeps its bytes and the order it wrote them in
// (2026-09-28 audit, round 77, F77-L2-1 and F77-L2-2).
//
// An entry whose function.name is empty is not a call on this leg on any arm:
// content_block_start is the only event that carries a name, so a nameless
// entry's arguments are the model's own prose, relayed to the client as text.
// Two places on the fragment arm disagreed with the whole-list arms about that.
//
//  1. The fold that drops a call's restatement fired on any entry carrying the
//     bytes of the call at its slot, whether or not the entry NAMED that call.
//     A nameless entry that happened to repeat the call's arguments was
//     `continue`d — its bytes reached the client on neither arm of this leg,
//     where both document arms answer the call and the prose beside it
//     (F77-L2-1).
//  2. The prose was assembled by walking the accumulators in slot-arrival
//     order, so a body whose nameless fragments interleave across two slots
//     read back in the order the SLOTS were first seen rather than the order
//     the wire wrote them — the same bytes the two document arms write in
//     list order (F77-L2-2).
//
// Recorded, not fixed (same round):
//
//   - F77-L2-3, the frame arm cannot tell a nameless entry that CONTINUES a
//     free-form argument line (`{"name":"Bash","arguments":"echo "}` then
//     `{"arguments":"hi"}`) from one that merely follows it, so the following
//     entry's bytes are folded into the call where both document arms relay
//     them beside it. Byte-identical before and after this round, and the same
//     trade-off leg 3 records as F77-L3-3: relaying that half as prose would
//     hand the client a tool_use whose command is truncated. The reading that
//     keeps the model's command runnable wins.
//   - A cross-leg placement difference this round measured but did not settle:
//     both legs group the nameless prose into blocks of its own, but this leg's
//     document arms write it BEFORE the calls (msg.Content += text, then the
//     calls) while leg 3's document and frame arms write it AFTER them. The
//     bytes and the order WITHIN the prose agree on every arm of both legs; the
//     position of the prose block relative to the calls does not. Leg 1 relays
//     it at the position the entry stood, which is a third answer. Settling it
//     means choosing one placement and applying it to the document arms of all
//     three legs, and it is carried to round 78 rather than pinned here.

import (
	"strings"
	"testing"
)

// TestANamelessEntrysBytesAreNotSwallowedByTheFold is F77-L2-1: the fold that
// drops a call's restatement must only fire for an entry that NAMES the call.
// An entry that names nothing is not a call, so its bytes are never another
// entry's restatement however exactly they match.
func TestANamelessEntrysBytesAreNotSwallowedByTheFold(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
	}{
		{
			"a nameless entry repeating a call's own bytes",
			[]string{
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"1}"}}`,
				`{"index":0,"type":"function","function":{"name":"","arguments":"1}"}}`,
			},
		},
		{
			"a nameless entry first, then the call, then the nameless entry again",
			[]string{
				`{"index":0,"type":"function","function":{"name":"","arguments":"{\"z\":9}"}}`,
				`{"index":1,"id":"c1","type":"function","function":{"name":"Bash","arguments":"1}"}}`,
				`{"index":0,"type":"function","function":{"name":"","arguments":"1}"}}`,
			},
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			frag, whole, fragText, wholeText, _, _ := r75Arms(t, tc.entries)
			// What the turn's output CARRIES, which is the property the fold
			// could only break: every byte the wire stated is somewhere in the
			// answer. The document arms relay the nameless entry's bytes as
			// text; the frame arm folds them into the call at their slot only
			// when they can extend its argument line (F77-L2-3), so the two
			// arms need not place them the same way — but neither may drop
			// them, and before this round the frame arm did.
			for _, b := range []struct {
				arm, blocks, text string
			}{
				{"fragment", r76ArmBlocks(t, frag), fragText},
				{"whole-list", r76ArmBlocks(t, whole), wholeText},
			} {
				// The wire stated the bytes `1}` twice — once for the call and
				// once for the nameless entry — so the answer carries them
				// twice. Where each copy lands is the arms' own business (the
				// frame arm folds the second into the call's argument line,
				// F77-L2-3; the document arms relay it as text), but a fold that
				// fires on an entry which names nothing leaves ONE copy, and the
				// text the model wrote is gone.
				if n := strings.Count(b.blocks, "1}") + strings.Count(b.text, "1}"); n != 2 {
					t.Errorf("the %s arm's answer carries the bytes the wire stated for the nameless entry %d time(s), want 2: blocks=%s text=%q (2026-09-28 audit, round 77, F77-L2-1)",
						b.arm, n, b.blocks, b.text)
				}
			}
		})
	}
}

// TestANamelessRunsBytesKeepTheWiresOrder is F77-L2-2: the bytes of the entries
// the upstream never named reach the client in the order the wire wrote them,
// which is the order both document arms concatenate the same entries in.
func TestANamelessRunsBytesKeepTheWiresOrder(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
		want    string
	}{
		{
			"fragments interleaved across two slots",
			[]string{
				`{"index":0,"type":"function","function":{"arguments":"{\"a\":"}}`,
				`{"index":1,"type":"function","function":{"arguments":"{\"z\":9}"}}`,
				`{"index":0,"type":"function","function":{"arguments":"{\"a\":1}"}}`,
			},
			`{"a":{"z":9}{"a":1}`,
		},
		{
			"one free-form line then a whole object",
			[]string{
				`{"type":"function","function":{"arguments":"zzz"}}`,
				`{"type":"function","function":{"arguments":"{\"c\":3}"}}`,
			},
			`zzz{"c":3}`,
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			_, _, fragText, wholeText, _, _ := r75Arms(t, tc.entries)
			if fragText != tc.want {
				t.Errorf("the fragment arm relayed %q, want %q — a nameless entry's bytes are the model's prose, and prose is relayed in the order it was written, not in the order its slot was first seen (2026-09-28 audit, round 77, F77-L2-2)",
					fragText, tc.want)
			}
			if wholeText != tc.want {
				t.Errorf("the whole-list arm relayed %q, want %q — the two arms of this leg answer one body alike (2026-09-28 audit, round 77, F77-L2-2)",
					wholeText, tc.want)
			}
		})
	}
}
