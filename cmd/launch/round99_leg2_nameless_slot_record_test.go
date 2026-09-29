package launch

// round99_leg2_nameless_slot_record_test.go — leg 2, F99-L2-2 (2026-09-29
// audit, round 99). RECORDED, not changed.
//
// The adoption numbers a whole completion's entries the way every other arm
// numbers this wire: an entry that STATES an index occupies it, one that states
// none takes the next free slot, and the counter is kept above every index the
// list has stated (round 98's F98-L2-1). The walk skips an entry the upstream
// never named before it books a slot, so a NAMELESS entry's stated index does
// not advance that counter — and the call beside it is booked lower than the
// fragment arm and the document arm book it.
//
// Measured on this tree, a whole frame carrying `[nameless @index 5, c1]`
// followed by the delta that restates c1 at index 0 reads as ONE call on the
// adopted arm and TWO on both other spellings of the same entries:
//
//	adopted   [c1]                  the delta at 0 lands on the slot c1 occupies
//	fragment  [c1] [<minted>]       the delta at 0 is a call of its own
//	document  [c1] [<minted>]
//
// The entries carry one call the wire named and one restatement of it, so this
// is the doc-versus-fragment split rounds 82, 97 and 98 record — every arm
// answers each spelling of the wire consistently, and the two spellings need
// not read alike. No producer of the shape exists in this tree: it needs a
// whole completion AND further tool-call deltas after it, and no upstream this
// leg has a fixture for emits both.
//
// What a later round should know before "fixing" it: consuming the slot for a
// nameless entry does align the adopted arm with the other two here, but it
// also claims a slot NO BLOCK was written for — and a fragment that arrives at
// a claimed slot with no block is dropped, not read as a new call (round 68's
// rule; round 81's L2-3 is the same trap found on a truncated call, where the
// fix was to leave the slot UNclaimed). With the nameless entry at index 0 the
// same turn then reads ONE call on the adopted arm against two on the others,
// which is the skew this pin measures, from the other side. The two readings
// are stated here so that trade-off is met as a decision.

import "testing"

// TestTheAdoptionsSlotWalkSkipsANamelessEntrysIndex states F99-L2-2's readings
// as they are.
func TestTheAdoptionsSlotWalkSkipsANamelessEntrysIndex(t *testing.T) {
	nameless5 := `{"index":5,"type":"function","function":{"name":"","arguments":"{\"z\":9}"}}`
	c1 := `{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	restate0 := `{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	entries := nameless5 + "," + c1 + "," + restate0

	frag, whole := r81Both(t,
		[]string{r81WholeFrame(nameless5+","+c1, "tool_calls"), r81Frame(restate0), r81FinToolCalls, r81Done},
		r81Doc(entries, "tool_calls"), "c1")
	if frag != `stop=tool_use [c1 Bash {"a":1}]` {
		t.Errorf("the adopted arm reads %s — the walk books c1 at the slot the nameless entry's stated index did not take, so the restatement at index 0 folds into it (2026-09-29 audit, round 99, F99-L2-2)", frag)
	}
	if whole != `stop=tool_use [c1 Bash {"a":1}] [<minted> Bash {"a":1}]` {
		t.Errorf("the document arm reads %s — the same entries are two list entries and two calls there", whole)
	}
	t.Logf("adopted %s | document %s", frag, whole)

	// The fragment spelling with no adoption at all reads the way the document
	// arm does: the split is between the adoption's slot walk and the other two
	// spellings, not between this leg and another.
	delta, _ := r81Both(t,
		[]string{r81Frame(nameless5), r81Frame(c1), r81Frame(restate0), r81FinToolCalls, r81Done},
		r81Doc(entries, "tool_calls"), "c1")
	if delta != whole {
		t.Errorf("the delta spelling reads %s and the document arm reads %s — the same entries must read alike on those two spellings (2026-09-29 audit, round 99, F99-L2-2)", delta, whole)
	}

	// The same turn with the nameless entry at index 0 is the mirror image and
	// is stated the same way: the adopted arm still folds what the others keep
	// apart.
	nameless0 := `{"index":0,"type":"function","function":{"name":"","arguments":"{\"z\":9}"}}`
	frag0, whole0 := r81Both(t,
		[]string{r81WholeFrame(nameless0+","+c1, "tool_calls"), r81Frame(restate0), r81FinToolCalls, r81Done},
		r81Doc(nameless0+","+c1+","+restate0, "tool_calls"), "c1")
	if frag0 != `stop=tool_use [c1 Bash {"a":1}]` || whole0 != `stop=tool_use [c1 Bash {"a":1}] [<minted> Bash {"a":1}]` {
		t.Errorf("with the nameless entry at index 0 the arms read %s | %s — the readings this pin states have moved (2026-09-29 audit, round 99, F99-L2-2)", frag0, whole0)
	}
	t.Logf("nameless at 0: adopted %s | document %s", frag0, whole0)
}
