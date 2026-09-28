package launch

// round73_restated_list_slot_integrity_test.go — leg 2's non-stream list, and
// the slot a restatement has to name to be the call it repeats (2026-09-28
// audit, round 73, F73-L2-1).
//
// Round 68 (F68-L2-3) folded an id-less entry that repeats a call the list
// already STATED, and the justification it recorded was that the gateway leg's
// document arm folds them too. Measured, that is false: the gateway's document
// arm answers two calls for the same body. What the gateway's FRAME arm does —
// and what this leg's own fragment arm does — is fold the restatement only when
// it NAMES THE SLOT its stated twin occupies: an id-less entry stating the twin's
// index is that call listed again, and one stating a fresh index, or none at
// all, is a call of its own. The list arm folded on the identity alone, so one
// body reached the client as one call whole and two as fragments.

import (
	"testing"
)

// r73ListSpellings spells the round-68 body (a stated call, then the same call
// listed again with no id) as a whole completion whose second entry names the
// given slot — or none.
func r73ListSpelling(second string) string {
	return r68Doc(r68NamedCall + `,` + second)
}

// TestTheRestatedListEntryFoldsOnlyForTheSlotItNames is the F73-L2-1 pin: the
// list arm must answer what this leg's fragment arm and the gateway's frame arm
// answer for the same body, which is one call only when the restatement names
// the slot its stated twin occupies.
func TestTheRestatedListEntryFoldsOnlyForTheSlotItNames(t *testing.T) {
	for _, tc := range []struct {
		name   string
		second string
		want   int
	}{
		{
			"the restatement names the slot its twin occupies",
			`{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
			1,
		},
		{
			"the restatement names a fresh slot",
			`{"index":1,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
			2,
		},
		{
			"the restatement names no slot",
			`{"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`,
			2,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ids := r68ListBody(t, r73ListSpelling(tc.second))
			if len(ids) != tc.want {
				t.Fatalf("the list arm answers %d call(s) for %s, want %d — this leg's fragment arm and the gateway leg's frame arm, the two arms that already implement the slot rule, answer %d (2026-09-28 audit, round 73, F73-L2-1)\nids: %v",
					len(ids), tc.name, tc.want, tc.want, ids)
			}
		})
	}

	// The control round 68 landed: two identical ID-LESS entries are two calls
	// (A45-2), whatever slot they name, because neither stated one.
	for _, second := range []string{r68IdlessSame, `{"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`} {
		ids := r68ListBody(t, r68Doc(r68IdlessSame+`,`+second))
		if len(ids) != 2 {
			t.Fatalf("two identical id-less calls reached the client as %d block(s) (%v), want 2 — the upstream asked for the tool twice and the client must be able to answer each (A45-2)", len(ids), ids)
		}
	}
}
