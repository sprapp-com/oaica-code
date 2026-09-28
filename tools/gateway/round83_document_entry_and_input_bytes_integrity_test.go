package main

// round83_document_entry_and_input_bytes_integrity_test.go — leg 3, F83-L3-1
// through F83-L3-4 (2026-09-28 audit, round 83).
//
// One bridge, three arms. A turn written as a whole completion is read twice —
// as a document, entry by entry (the adopted walk), and as one stated list
// (the non-stream arm) — and the same turn written as chunks is read a third
// time (the frame walk). The list is what the upstream STATES, so its order is
// the client's order on every arm, and a call the list states twice under one
// id is one call on all of them. Three ways that stopped being true:
//
//   - The adopted walk had no fold for a repeated entry, so a document whose
//     two entries restate one call reached the client as two calls (F83-L3-1).
//   - A restatement of an argument-less call was not matched to the call it
//     restated, so the held call was swept to the end of the turn and handed
//     over behind the call that had waited for it (F83-L3-2).
//   - The non-stream arm wrote the block's input as an encoding/json-sorted
//     map where the other two arms wrote the model's own bytes (F83-L3-3).
//
// And one that was never a question of order: a fragment whose entry carries a
// name of whitespace relays nothing, as the file's own tests already say
// (F83-L3-4).

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// TestARepeatedEntryOfTheListIsOneCall is F83-L3-1. The list's two entries
// state the same call under the same id, both cut off inside the same object:
// the non-stream arm drops the repeat — statedIDOwner sees one identity under
// one stated id (round 48's C-F1) — and the adopted walk, which is the same
// body read entry by entry, answered TWO calls, the second wearing a minted id.
func TestARepeatedEntryOfTheListIsOneCall(t *testing.T) {
	entries := []string{
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}`,
		`{"index":1,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":"}}`,
	}
	doc, plain, frame := r81ThreeArms(t, entries, "tool_calls")

	if got := r82IDs(t, doc); len(got) != 1 || got[0] != "call_1" {
		t.Errorf("the document arm answered the ids %v for a list that states one call under one id twice — the non-stream arm of the same body keeps one block and drops the repeat (round 48's C-F1), so the entry-by-entry walk must too (2026-09-28 audit, round 83, F83-L3-1)\n%s", got, doc)
	}
	if docIDs, plainIDs := r82IDs(t, doc), r82IDs(t, plain); strings.Join(docIDs, ",") != strings.Join(plainIDs, ",") {
		t.Errorf("one body, two document arms: adopted %v, non-stream %v (2026-09-28 audit, round 83, F83-L3-1)", docIDs, plainIDs)
	}
	if got := r82IDs(t, frame); len(got) != 1 || got[0] != "call_1" {
		t.Errorf("the frame arm answered the ids %v for the same call written as chunks (2026-09-28 audit, round 83, F83-L3-1)\n%s", got, frame)
	}
	if got := r80Leg3ToolInputs(t, doc); len(got) != 1 || got[0] != `{"_raw":"{\"a\":"}` {
		t.Errorf("the document arm's input is %v, want the one half-object the list wrote (2026-09-28 audit, round 83, F83-L3-1)", got)
	}
}

// TestAListsOrderSurvivesAnArglessRestatement is F83-L3-2. The list's FIRST
// call states no arguments and its LAST entry restates it. Nothing about the
// repeat says anything new — the same id, the same name, the same empty
// argument list — so the list still states two calls, and it stated call_2
// first. The frame and adopted arms delivered call_2 LAST, behind the call that
// waited for it, where the non-stream arm of the same body kept the list's
// order. The second case asks the same of a restatement that states no index.
func TestAListsOrderSurvivesAnArglessRestatement(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
	}{
		{
			"the restatement states the index",
			[]string{
				`{"index":0,"id":"call_2","type":"function","function":{"name":"Read"}}`,
				`{"index":1,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
				`{"index":2,"id":"call_2","type":"function","function":{"name":"Read"}}`,
			},
		},
		{
			"the restatement states no index",
			[]string{
				`{"index":0,"id":"call_2","type":"function","function":{"name":"Read"}}`,
				`{"index":1,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
				`{"id":"call_2","type":"function","function":{"name":"Read"}}`,
			},
		},
	} {
		doc, plain, frame := r81ThreeArms(t, tc.entries, "tool_calls")
		want := strings.Join(r82IDs(t, plain), ",")
		if want != "call_2,call_1" {
			t.Fatalf("%s: the non-stream arm answered %v, want the list's own order [call_2 call_1] — this pin reads the other arms against it (2026-09-28 audit, round 83, F83-L3-2)", tc.note, r82IDs(t, plain))
		}
		for _, arm := range []struct {
			name string
			body string
		}{{"document", doc}, {"frame", frame}} {
			if got := strings.Join(r82IDs(t, arm.body), ","); got != want {
				t.Errorf("%s, %s arm: the ids came back %s, want the list's own order %s — a restatement of a call the bridge already holds is that call, and holding it must not move it behind the call that waited for it (2026-09-28 audit, round 83, F83-L3-2)\n%s",
					tc.note, arm.name, got, want, arm.body)
			}
		}
	}
}

// TestThePlainArmsInputKeepsTheModelsKeyOrder is F83-L3-3. The input is what
// the client RUNS, and the model's own argument bytes are what both other arms
// hand it — an ordered map on the sibling leg (api/types.go's
// ToolCallFunctionArguments). This arm re-encoded the block through a
// map[string]any, and encoding/json sorts, so one call reached the client with
// its keys in an order no arm of the wire wrote. A repeated key still takes the
// last value, which is what that ordered map does with one and what round 48's
// C-F5 pinned here.
func TestThePlainArmsInputKeepsTheModelsKeyOrder(t *testing.T) {
	for _, tc := range []struct{ args, want string }{
		{`{"z":1,"a":2}`, `{"z":1,"a":2}`},
		{`{"b":2,"a":1,"c":3}`, `{"b":2,"a":1,"c":3}`},
		{`{"a":1,"a":2}`, `{"a":2}`},
		{`{"a":1}`, `{"a":1}`},
	} {
		entry := `{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":` + r83Quote(tc.args) + `}}`
		_, plain, _ := r81ThreeArms(t, []string{entry}, "tool_calls")
		got := r80Leg3ToolInputs(t, plain)
		if len(got) != 1 || got[0] != tc.want {
			t.Errorf("the arguments %s reached the non-stream arm's client as %v, want %s — the input is the model's own argument object, and the map this arm encoded it through sorts its keys (2026-09-28 audit, round 83, F83-L3-3)\n%s",
				tc.args, got, tc.want, plain)
		}
	}
}

// TestABlankNamedFragmentIsNoTurn is F83-L3-4. The entry's name is a single
// space: no block can be named by it, and the file's own tests say so
// (namesItself trims, and nothingRelayed asks it). The frame arm committed on
// the bare non-empty string, so it began the stream and answered 200 with an
// error event inside it, while both document arms of the same body — and the
// bridge's own ledger row — answer 502.
func TestABlankNamedFragmentIsNoTurn(t *testing.T) {
	entry := `{"index":1,"type":"function","id":"call_1","function":{"name":" "}}`
	up := round45Frames(t, r71Leg3Frame(entry),
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`, `data: [DONE]`)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	if status != http.StatusBadGateway {
		t.Errorf("a turn whose only entry carries a name of whitespace was answered %d, want 502 like both document arms of the same body: nothing was relayed, so no message_start may have been committed (2026-09-28 audit, round 83, F83-L3-4)\n%s", status, body)
	}
	if strings.Contains(body, "message_start") {
		t.Errorf("the blank-named fragment opened a message the bridge then had to close with an error inside it: the upstream answered nothing this bridge can relay, and that verdict belongs in the status line, not in a stream that already began (2026-09-28 audit, round 83, F83-L3-4)\n%s", body)
	}
}

// r83Quote quotes s as a JSON string body: the argument text is escaped twice,
// once into the entry's arguments field and once into the frame.
func r83Quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}
