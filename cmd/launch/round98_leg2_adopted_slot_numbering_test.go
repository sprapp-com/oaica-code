package launch

// round98_leg2_adopted_slot_numbering_test.go — leg 2, round 98 (2026-09-29
// audit), F98-L2-1.
//
// An upstream answers a `stream:true` request with a WHOLE COMPLETION inside one
// `data:` frame. This leg adopts it (adoptNonSSECompletion) and records the call
// each entry wrote at the slot a later fragment is read against (wroteSlots,
// asked by the caller's slotCall). The entries it numbered by their POSITION in
// the list, on the stated premise that "the document's tool_calls entries have
// no index field" — which the wire does not promise, and round 73's F73-L2-1
// had already settled the other way for the document arm, whose reader was
// folding `"index":0` and `"index":1` into one entry before it was taught to
// read the index.
//
// Measured on the frozen tree, a frame stating `"index":2` followed by the
// id-less delta that restates that call at index 2 — the vendor's own
// continuation — found slot 2 unclaimed (the adoption had spoken for slot 0),
// was read as a NEW call, and reached the client as a second, minted, runnable
// block:
//
//	fragment spelling  [Bash/c1  Bash/call_7ff51383]   two calls, run twice
//	document spelling  [Bash/c1]                       one call
//
// and the same again with the list reversed (`index` 1 then 0: three blocks
// against the document's two) and behind a nameless entry. The adopted entries
// are numbered the way every other arm of every leg numbers the wire: an entry
// that states an index occupies that index, and one that states none takes the
// next free slot, the counter kept above every index the list has stated
// (parseOpenAIToolCalls, and this leg's own fragment arm).
//
// The second pin records the one reading this changes WITHOUT repairing, so a
// later round meets it as a decision rather than a discovery: a frame stating
// `"index":2` whose arguments are still MID-OBJECT, followed by the id-less
// delta that finishes them, now reaches the adopted slot (it did not before) and
// is DROPPED there — the block the adoption wrote is closed and carries the
// partial bytes wrapped under `_raw`, and round 68's adopted-slot rule drops a
// fragment such a slot cannot take rather than splitting it into a second call.
// The document spelling of those same two entries reads two calls. That is the
// two-spellings split rounds 82 and 97 record, not this leg's mistake, and this
// pin states both readings as they are.

import (
	"testing"
)

// TestTheAdoptionsSlotIsTheIndexTheFrameStated is F98-L2-1's pin: a whole
// completion adopted from a frame, and the id-less delta that restates the call
// at the index the frame stated, are ONE call — the same one call the document
// spelling of the same two statements answers.
func TestTheAdoptionsSlotIsTheIndexTheFrameStated(t *testing.T) {
	stated := `{"index":2,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	idless := `{"index":2,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	{
		want := "stop=tool_use [c1 Bash {\"a\":1}]"
		frag, whole := r81Both(t,
			[]string{r81WholeFrame(stated, "tool_calls"), r81Frame(idless), r81FinToolCalls, r81Done},
			r81Doc(stated+","+idless, "tool_calls"), "c1")
		if frag != want {
			t.Errorf("the fragment spelling answered %s, want %s\n  (2026-09-29 audit, round 98, F98-L2-1 — an entry stating `\"index\":2` occupies slot 2, so the id-less restatement at 2 is the call the adoption already wrote; numbered by position it was read as a new call and the model's one call was delivered twice under two ids and run twice)", frag, want)
		}
		if whole != want {
			t.Errorf("the document spelling answered %s, want %s", whole, want)
		}
		t.Logf("fragment %s | document %s", frag, whole)
	}
	{
		// The same list with its indices swapped: entry 1 stated first, entry 0
		// second, restated at 1. Numbered by position the restatement missed
		// again, and the wire's own order is what the client's list must keep.
		a := `{"index":1,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
		b := `{"index":0,"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"f\":2}"}}`
		want := "stop=tool_use [c1 Bash {\"a\":1}] [c2 Read {\"f\":2}]"
		frag, whole := r81Both(t,
			[]string{r81WholeFrame(a+","+b, "tool_calls"), r81Frame(a), r81FinToolCalls, r81Done},
			r81Doc(a+","+b+","+a, "tool_calls"), "c1", "c2")
		if frag != want {
			t.Errorf("the fragment spelling answered %s, want %s (2026-09-29 audit, round 98, F98-L2-1)", frag, want)
		}
		if whole != want {
			t.Errorf("the document spelling answered %s, want %s", whole, want)
		}
		t.Logf("swapped: fragment %s | document %s", frag, whole)
	}
}

// TestAnAdoptedCallsContinuationIsDroppedThereNotSplit is the reading this round
// changed without repairing: the adopted slot is now reached, and the rule that
// governs an adopted slot takes over. Both spellings' readings are stated here,
// so a later round meets the split as a decision.
func TestAnAdoptedCallsContinuationIsDroppedThereNotSplit(t *testing.T) {
	mid := `{"index":2,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}`
	idless := `{"index":2,"type":"function","function":{"name":"Bash","arguments":"1}"}}`
	frag, whole := r81Both(t,
		[]string{r81WholeFrame(mid, "tool_calls"), r81Frame(idless), r81FinToolCalls, r81Done},
		r81Doc(mid+","+idless, "tool_calls"), "c1")
	if frag != "stop=tool_use [c1 Bash {\"_raw\":\"{\\\"a\\\":\"}]" {
		t.Errorf("the fragment spelling answered %s — an adopted block is closed and carries what the adoption held; the fragment at a slot it cannot extend is dropped, not split into a second call (round 68's rule, 2026-09-29 audit round 98)", frag)
	}
	if whole != "stop=tool_use [c1 Bash {\"_raw\":\"{\\\"a\\\":\"}] [<minted> Bash {\"_raw\":\"1}\"}]" {
		t.Errorf("the document spelling answered %s — the two-spellings split rounds 82 and 97 record, where two list entries are two calls; restated here so a later round sees the decision rather than the discovery", whole)
	}
	t.Logf("mid-object adopted call: fragment %s | document %s", frag, whole)
}
