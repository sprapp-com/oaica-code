package main

// round74_document_arm_agreement_integrity_test.go — leg 3, the two ways one
// bridge relays a whole completion (2026-09-28 audit, round 74, F74-L3-1).
//
// This leg answers a whole upstream completion down two paths. A request that
// streams and is answered with a document is ADOPTED (adoptNonSSECompletion);
// a request that does not stream is answered from the same document. They are
// one arm reached twice, and round 73 gave the slot a document entry states to
// only one of them: the adopted copy folded a restatement naming the slot its
// stated twin occupies (one call) while the plain copy kept two, so one upstream
// body reached the client as one tool_use under `stream:true` and two under
// `stream:false`, the second under a different id — the split round 73 set out
// to remove, moved rather than removed. Measured on 2026-09-28 with round 73's
// read in place: adopted=[call_1] nonstream=[call_1 call_7ff51383] for the
// first row below, and adopted=[call_1 call_7ff51383] nonstream=[call_1
// call_7ff51383] with it removed (round 74's fix).
//
// A tool_calls LIST has no slot identity to fold on. Each entry of it is a call
// the upstream named, in the list's own order — the rule this arm has always
// stated — and the frame arm keeps the index-as-slot rule because it numbers
// FRAGMENTS, where an index is a slot.

import (
	"encoding/json"
	"testing"
)

// r74PlainCalls counts the tool_use blocks of the non-stream answer, which this
// leg writes as a message rather than as a stream.
func r74PlainCalls(t *testing.T, body string) int {
	t.Helper()
	var out struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the non-stream answer is not a message: %v\n%s", err, body)
	}
	n := 0
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			n++
		}
	}
	return n
}

// r74PlainTurn relays the whole completion to a NON-streaming request.
func r74PlainTurn(t *testing.T, doc string) string {
	t.Helper()
	_, body := round44Run(t, "application/json", doc, round44AskPlain)
	return body
}

// TestTheDocumentArmsOfOneBridgeAnswerOneBodyAlike is the F74-L3-1 pin: for each
// body, the adopted document arm and the plain document arm must answer the same
// number of calls — and for these bodies, the number the list's own reading
// gives, one call per entry except where two entries are one call (the last row:
// the same id, name and complete arguments twice).
func TestTheDocumentArmsOfOneBridgeAnswerOneBodyAlike(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries string
		want    int
	}{
		{"a restatement naming the slot its stated twin occupies", r73L3Named + "," + r73L3Idless0, 2},
		{"a restatement naming a fresh slot", r73L3Named + "," + r73L3Idless1, 2},
		{"a restatement naming no slot", r73L3Named + "," + r73L3IdlessNo, 2},
		{"two listings that state one slot and no id", r73L3Idless0 + "," + r73L3Idless0, 2},
		{"two listings that state no slot and no id", r73L3IdlessNo + "," + r73L3IdlessNo, 2},
		{
			"an argument-less holder and the same id carrying the object",
			`{"id":"call_c","type":"function","function":{"name":"Bash","arguments":""}},` +
				`{"id":"call_c","type":"function","function":{"name":"Bash","arguments":"{\"a\": 1}"}}`,
			2,
		},
		{
			"the same call stated twice under its own id",
			`{"index":0,"id":"call_c","type":"function","function":{"name":"Bash","arguments":"{\"a\": 1}"}},` +
				`{"index":1,"id":"call_c","type":"function","function":{"name":"Bash","arguments":"{\"a\": 1}"}}`,
			1,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := r71Leg3Doc(tc.entries)
			adopted := len(r72Calls(t, r72DocTurn(t, doc)))
			plain := r74PlainCalls(t, r74PlainTurn(t, doc))
			if adopted != plain {
				t.Errorf("one bridge answered one upstream body with %d call(s) when the request streamed and %d when it did not; a whole completion has one answer (2026-09-28 audit, round 74, F74-L3-1)\nadopted: %d\nplain:   %d",
					adopted, plain, adopted, plain)
			}
			if adopted != tc.want {
				t.Errorf("the document arms answer %d call(s), want %d — one call per entry of the list, in the list's own order (2026-09-28 audit, round 74, F74-L3-1)\n%s",
					adopted, tc.want, doc)
			}
		})
	}
}
