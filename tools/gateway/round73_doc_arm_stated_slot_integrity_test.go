package main

// round73_doc_arm_stated_slot_integrity_test.go — leg 3, the slot a document
// entry STATES and the twin a restatement must have (2026-09-28 audit, round
// 73, F73-L3-1).
//
// Two halves, both measured against the client leg on the same bodies:
//
//  1. A document entry that states an index names the same slot the frame arm
//     would number it by. Read as its list position alone, an entry restating a
//     call the list already stated under that call's own index opened a second
//     block here while this bridge's own frame arm answered one.
//  2. A restatement is folded only onto a twin the UPSTREAM named an id for.
//     Two listings that name themselves nowhere are two calls — round 69's rule
//     on the client leg, and the answer its non-stream arm and the local
//     converter both give. This arm folded them into one, so an upstream that
//     asked for the same tool twice lost a call here alone (F73-L3-2).
//
// Every row below is the answer the client leg's fragment arm gives for the
// same body, measured on 2026-09-28: 1, 2, 2, 2, 2.

import (
	"testing"
)

const (
	r73L3Named    = `{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	r73L3Idless0  = `{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	r73L3Idless1  = `{"index":1,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	r73L3IdlessNo = `{"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
)

// TestADocumentEntrysStatedIndexIsTheSlotItOccupies is the F73-L3-1/F73-L3-2
// pin: both of this leg's arms must answer the client leg's count for each of
// these bodies, whichever spelling carries them.
func TestADocumentEntrysStatedIndexIsTheSlotItOccupies(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries string
		want    int
	}{
		{"a restatement naming the slot its stated twin occupies", r73L3Named + "," + r73L3Idless0, 1},
		{"a restatement naming a fresh slot", r73L3Named + "," + r73L3Idless1, 2},
		{"a restatement naming no slot", r73L3Named + "," + r73L3IdlessNo, 2},
		{"two listings that state one slot and no id", r73L3Idless0 + "," + r73L3Idless0, 2},
		{"two listings that state no slot and no id", r73L3IdlessNo + "," + r73L3IdlessNo, 2},
	} {
		t.Run(tc.name+"/document arm", func(t *testing.T) {
			doc := r72DocTurn(t, r71Leg3Doc(tc.entries))
			if got := len(r72Calls(t, doc)); got != tc.want {
				t.Errorf("the document arm answered %d call(s), want %d — the client leg's fragment arm answers %d for the same bytes (2026-09-28 audit, round 73, F73-L3-1)\n%s",
					got, tc.want, tc.want, doc)
			}
		})
		t.Run(tc.name+"/frame arm", func(t *testing.T) {
			body := r72FrameTurn(t, r71Leg3Frame(tc.entries))
			if got := len(r72Calls(t, body)); got != tc.want {
				t.Errorf("the frame arm answered %d call(s), want %d — the two arms of one leg may not answer one body differently, and the client leg answers %d (2026-09-28 audit, round 73, F73-L3-2)\n%s",
					got, tc.want, tc.want, body)
			}
		})
	}
}
