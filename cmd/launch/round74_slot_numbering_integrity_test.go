package launch

// round74_slot_numbering_integrity_test.go — leg 2, the NUMBERING behind the
// slot rule (2026-09-28 audit, round 74, F74-1 and F74-3).
//
// Round 73 made this leg's non-stream list fold a restatement only when it names
// the slot its stated twin occupies, and it numbered those slots two ways the
// fragment arm does not: an index-less entry took its LIST POSITION, and the
// slots a call was stated at were kept one per call — the last one seen. Each
// cost a call the upstream had asked for, and both cost it on the same wire:
//
//  1. List position. An entry that states no index is numbered, on the fragment
//     arm, by a counter kept above every index the stream has stated. Read as
//     its position instead, an index-less entry following a stated index took a
//     slot the fragment arm had given to another call, so the same body answered
//     one call on one arm and two on the other — eight bodies that agreed before
//     round 73 diverged the moment the fold became slot-aware (F74-1).
//  2. The last slot only. A call listed twice under its own id was stated at two
//     slots; a restatement naming the EARLIER of them was read as a fresh slot
//     and minted a second call (F74-3).
//
// The bodies below are the ones the finding is made of: for each, both of this
// leg's arms must answer the same count, and that count is what the gateway
// leg's two arms and leg 1 answer for the same bytes.
//
// F74-2, the argument-less holder, is NOT one of them and is deliberately left
// as it is. `[{"id":"call_c","name":"Bash","arguments":""},{... same id and name,
// "arguments":"{\"a\": 1}"}]` is read as ONE call by both streaming arms of legs
// 2 and 3 (the accumulator takes the second entry as the first call's arguments
// arriving) and as TWO by leg 1's two arms, this leg's whole-list arm and the
// gateway's non-stream document arm, which read the two entries as two calls
// whose identities differ and re-mint the reused id (A45-3). Measured 2026-09-28
// (round 74): 5 arms answer 2, 2 answer 1, and the divergence is older than
// round 73 (it is red on a pre-round-73 control tree). Both readings are
// deliberate — an empty argument list then the object IS the ordinary wire of a
// vendor whose tool-call header carries no arguments, which the accumulator
// exists to fold; and a stated id reused for different arguments IS a second
// call, which A45-3 exists to re-mint — so the fix belongs to whichever side a
// later round decides against, and it is recorded here rather than guessed. The
// one spelling all six arms already agree on is the holder that carries the
// COMPLETE object in both entries: one call, and the control below pins it, so a
// change on either side of the disagreement shows up as a diff against it.

import (
	"testing"
)

// r74Arms spells the same entries as a whole document and as one frame each, and
// returns both arms' blocks.
func r74Arms(t *testing.T, entries []string) (fragments, wholeBlocks []map[string]any) {
	t.Helper()
	list := ""
	for i, e := range entries {
		if i > 0 {
			list += ","
		}
		list += e
	}
	frames := make([]string, 0, len(entries)+2)
	for _, e := range entries {
		frames = append(frames, r59L2Frame(e))
	}
	frames = append(frames, r59L2Fin, `data: [DONE]`)
	return r59L2Arms(t, frames, r68Doc(list))
}

// TestTheListArmNumbersASlotsAsTheFragmentArmDoes is the F74-1 pin: the
// numbering has to be the fragment arm's, or the two arms of this leg answer one
// body differently.
func TestTheListArmNumbersASlotsAsTheFragmentArmDoes(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries []string
		want    int
	}{
		{
			// The twin stated no index, so its slot is the one the numbering
			// gave it; the restatement names that slot, and is the same call.
			"a restatement naming the slot an index-less stated twin holds",
			[]string{
				`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
				`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
				`{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
			},
			1,
		},
		{
			// The Bash was split onto slot 1, so index 1 names a slot, but not
			// the slot the wire stated this call at (index 0). The id-less entry
			// stating a fresh index is a call of its own — one the model asked
			// for, which the list arm's position numbering swallowed whenever an
			// earlier entry had moved the numbering past that index.
			"a call stated at index 0, then the arguments of a fresh index 1",
			[]string{
				`{"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}`,
				`{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
				`{"index":1,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
			},
			3,
		},
		{
			// F74-3: the twin was stated at slot 0 and again at slot 1; a
			// restatement naming the EARLIER of the two is still that call. Kept
			// only one slot per call — the last one seen — that entry read as a
			// fresh slot and was minted as a second call.
			"a restatement naming the earlier of the two slots its twin was stated at",
			[]string{
				`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
				`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
				`{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
			},
			1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frag, whole := r74Arms(t, tc.entries)
			if len(whole) != tc.want {
				t.Errorf("the whole-list arm answers %d call(s), want %d — the upstream asked for %d and the other legs' arms answer %d (2026-09-28 audit, round 74, F74-1, F74-3)\nwhole: %v",
					len(whole), tc.want, tc.want, tc.want, whole)
			}
			if len(frag) != tc.want {
				t.Errorf("the fragment arm answers %d call(s), want %d — the two arms of one leg may not answer one body differently (2026-09-28 audit, round 74, F74-1, F74-3)\nfragments: %v",
					len(frag), tc.want, frag)
			}
			if len(frag) == tc.want && len(whole) == tc.want {
				r59L2SameAsWhole(t, frag, whole)
			}
		})
	}
}

// TestAnArgumentLessHolderCarryingItsObjectIsOneCall is the control that all six
// arms agree on, and the one the F74-2 disagreement is measured against: the
// same call stated twice — the same id, the same name, the same COMPLETE
// arguments — is one call, on both arms of this leg. It is also what an
// id-bearing entry that repeats a call the list already stated keeps doing
// (round 68's F68-L2-3), where the holder whose arguments are still EMPTY is the
// spelling that reaches two blocks here and one on this leg's fragment arm (see
// the file header — recorded, not fixed).
func TestAnArgumentLessHolderCarryingItsObjectIsOneCall(t *testing.T) {
	entries := []string{
		`{"index":0,"id":"call_c","type":"function","function":{"name":"Bash","arguments":"{\"a\": 1}"}}`,
		`{"index":1,"id":"call_c","type":"function","function":{"name":"Bash","arguments":"{\"a\": 1}"}}`,
	}
	frag, whole := r74Arms(t, entries)
	if len(whole) != 1 || len(frag) != 1 {
		t.Errorf("a call stated twice with the same id, name and complete arguments is %d call(s) whole and %d as fragments, want 1 each — every arm that reads these bytes answers one (2026-09-28 audit, round 74)\nwhole: %v\nfragments: %v",
			len(whole), len(frag), whole, frag)
	}
}
