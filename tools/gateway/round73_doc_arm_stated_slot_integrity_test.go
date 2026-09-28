package main

// round73_doc_arm_stated_slot_integrity_test.go — leg 3, the slot a document
// entry STATES and the twin a restatement must have (2026-09-28 audit, round
// 73, F73-L3-1 and F73-L3-2; the first half reversed in round 74).
//
//  1. REVERSED in round 74 (F74-L3-1). Round 73 made the adopted document arm
//     read the index a document entry states as its SLOT, so an entry restating
//     a call the list already stated under that call's own index folded. The
//     fold is right for the frame arm and for the client leg's fragment arm, but
//     the document arm is reached twice — adopted inside a streaming request,
//     and as the plain non-stream answer — and only the adopted copy was given
//     the read, so this leg answered one upstream body with one call under
//     `stream:true` and two under `stream:false`. The rows below now assert what
//     the two document arms agree on (two, one call per entry), and the frame
//     arm's own answers — 1, 2, 2, 2, 2 — are asserted separately: they are the
//     streaming arm of this leg and they fold the same-slot restatement.
//  2. KEPT. A restatement is folded only onto a twin the UPSTREAM named an id
//     for. Two listings that name themselves nowhere are two calls — round 69's
//     rule on the client leg, and the answer its non-stream arm and the local
//     converter both give. This arm folded them into one, so an upstream that
//     asked for the same tool twice lost a call here alone (F73-L3-2).

import (
	"testing"
)

const (
	r73L3Named    = `{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	r73L3Idless0  = `{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	r73L3Idless1  = `{"index":1,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	r73L3IdlessNo = `{"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
)

// r73L3Rows is the round-73 body set: the document arms' answer for each row,
// and the frame arm's own — 1, 2, 2, 2, 2, as measured on 2026-09-28.
var r73L3Rows = []struct {
	name               string
	entries            string
	wantDoc, wantFrame int
}{
	{"a restatement naming the slot its stated twin occupies", r73L3Named + "," + r73L3Idless0, 2, 1},
	{"a restatement naming a fresh slot", r73L3Named + "," + r73L3Idless1, 2, 2},
	{"a restatement naming no slot", r73L3Named + "," + r73L3IdlessNo, 2, 2},
	{"two listings that state one slot and no id", r73L3Idless0 + "," + r73L3Idless0, 2, 2},
	{"two listings that state no slot and no id", r73L3IdlessNo + "," + r73L3IdlessNo, 2, 2},
}

// TestADocumentEntrysStatedIndexIsTheSlotItOccupies pins the document arms'
// answer and the frame arm's own, separately, because they differ on exactly one
// row: the frame arm folds a restatement that names the slot its stated twin
// occupies and the document arms keep every entry as a call.
func TestADocumentEntrysStatedIndexIsTheSlotItOccupies(t *testing.T) {
	for _, tc := range r73L3Rows {
		t.Run(tc.name+"/document arm", func(t *testing.T) {
			doc := r72DocTurn(t, r71Leg3Doc(tc.entries))
			if got := len(r72Calls(t, doc)); got != tc.wantDoc {
				t.Errorf("the document arm answered %d call(s), want %d — each entry of a tool_calls list is a call the upstream named, and the same bytes answered down this leg's other document path give %d (2026-09-28 audit, round 74, F74-L3-1)\n%s",
					got, tc.wantDoc, tc.wantDoc, doc)
			}
			if got := r74PlainCalls(t, r74PlainTurn(t, r71Leg3Doc(tc.entries))); got != tc.wantDoc {
				t.Errorf("the non-stream document arm answered %d call(s), want %d, where the adopted one answered %d — one bridge may not answer one body two ways (2026-09-28 audit, round 74, F74-L3-1)",
					got, tc.wantDoc, tc.wantDoc)
			}
		})
		t.Run(tc.name+"/frame arm", func(t *testing.T) {
			body := r72FrameTurn(t, r71Leg3Frame(tc.entries))
			if got := len(r72Calls(t, body)); got != tc.wantFrame {
				t.Errorf("the frame arm answered %d call(s), want %d — it numbers FRAGMENTS, where an index is a slot, and the client leg's fragment arm answers %d for the same body (2026-09-28 audit, round 73, F73-L3-1)\n%s",
					got, tc.wantFrame, tc.wantFrame, body)
			}
		})
	}
}
