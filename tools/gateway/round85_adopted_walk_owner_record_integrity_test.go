package main

// round85_adopted_walk_owner_record_integrity_test.go — leg 3, R85-L3-1
// (2026-09-28 audit, round 85).
//
// A listed entry that names an id the list already stated, under a DIFFERENT
// call, is re-minted: the id is the first call's, and a second call wearing it
// would be a call the client cannot tell apart from the first. That reading is
// round 45's A45-3 and all three legs share it — the non-stream arm of this
// bridge writes it as `id = ""` with the id's owner record left alone (the
// assignment to statedIDOwner sits in the ELSE of "already held by a different
// call"), and the adopted walk re-mints the same way.
//
// The adopted walk used to write the owner record unconditionally, so the
// re-minted entry took the stated id's record with it. A later entry repeating
// the FIRST call byte-for-byte then no longer matched that record, and the
// round-84 byte clause could not answer it either (the first block is
// mid-object, so blockCarrying refuses it) — the repeat opened a third block
// under a minted id. One upstream body reached the client as three tool_use
// blocks when the request streamed and two when it did not: a call the model
// never listed, which the client then runs.
//
// Measured on 2026-09-28 before the fix: adopted [call_1 call_eb2776e1
// call_19d972f0] against the non-stream arm's [call_1 call_eb2776e1]. A sweep of
// 4096 triples over a 16-entry pool gave 8 divergences, every one of this shape;
// the two-entry case never diverges, being answered by the round-84 byte clause.

import (
	"encoding/json"
	"testing"
)

// r85PlainCalls reads the non-stream answer of this bridge as the client reads
// it: one entry per tool_use block, the id the block wears.
func r85PlainCalls(t *testing.T, body string) []r72Call {
	t.Helper()
	var out struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("the non-stream answer is not a message: %v\n%s", err, body)
	}
	var calls []r72Call
	for _, b := range out.Content {
		if b.Type == "tool_use" {
			calls = append(calls, r72Call{id: b.ID})
		}
	}
	return calls
}

// TestTheAdoptedWalkKeepsAStatedIDsRecordAgainstARemint is the R85-L3-1 pin: a
// three-entry list whose middle entry re-mints under the first entry's id, and
// whose third entry repeats the first byte-for-byte. The two document arms of
// one bridge answer it with the same calls.
func TestTheAdoptedWalkKeepsAStatedIDsRecordAgainstARemint(t *testing.T) {
	for _, tc := range []struct {
		name    string
		entries string
	}{
		{
			"an argument-less re-mint between a call and its repeat",
			`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"a\":"}},` +
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Read"}},` +
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"a\":"}}`,
		},
		{
			"a re-mint carrying an empty object between a call and its repeat",
			`{"index":0,"id":"c","type":"function","function":{"name":"Read","arguments":"{\"a\":"}},` +
				`{"index":1,"id":"c","type":"function","function":{"name":"Read","arguments":"{}"}},` +
				`{"index":2,"id":"c","type":"function","function":{"name":"Read","arguments":"{\"a\":"}}`,
		},
		{
			"a re-mint under another name between a call and its repeat",
			`{"index":0,"id":"c","type":"function","function":{"name":"Read","arguments":"{\"a\":"}},` +
				`{"index":1,"id":"c","type":"function","function":{"name":"Bash","arguments":"{\"b\":1}"}},` +
				`{"index":2,"id":"c","type":"function","function":{"name":"Read","arguments":"{\"a\":"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			doc := r71Leg3Doc(tc.entries)
			adopted := r72Calls(t, r72DocTurn(t, doc))
			plain := r85PlainCalls(t, r74PlainTurn(t, doc))
			if len(adopted) != len(plain) {
				t.Fatalf("one bridge answered one upstream body with %d tool_use block(s) when the request streamed and %d when it did not — the re-minted entry must not take the stated id's owner record with it, or a repeat of the call that stated it opens a block the list never listed (2026-09-28 audit, round 85, R85-L3-1)\nadopted: %d\nplain:   %d\n%s",
					len(adopted), len(plain), len(adopted), len(plain), doc)
			}
			// And no block of the turn wears an id the list never stated: the
			// repeat is the first call, not a third one.
			for i, c := range adopted {
				if i == 0 {
					if c.id != "call_1" && c.id != "c" {
						t.Errorf("the first block wears %q, want the id the list stated first (2026-09-28 audit, round 85, R85-L3-1)\n%s", c.id, doc)
					}
					continue
				}
				if plain[i].id != c.id {
					t.Errorf("block %d wears %q when the request streamed and %q when it did not — one body, one answer (2026-09-28 audit, round 85, R85-L3-1)\n%s",
						i, c.id, plain[i].id, doc)
				}
			}
		})
	}
}
