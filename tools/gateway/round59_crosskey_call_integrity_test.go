package main

// round59_crosskey_call_integrity_test.go — round 59's findings on the bridge's
// frame arm (F59-L3-1 … F59-L3-5, F59-C-1, F59-C-2).
//
// One root runs through all of them: a block's KEY is not the only name the wire
// has for the call it carries. The bridge keys a call by the index that opened
// it, by the id the upstream stated for it, and by a synthetic key when neither
// is usable — and each of the three arms that resolves a fragment asked only
// about the key that fragment computes, so a call the wire named a second way
// became a second block.
//
//   - F59-L3-1: the index that opened a block stops naming it the moment a later
//     fragment states the block's id (the B45-1 re-key). Round 58 taught
//     rekeyToolBlock to move every index that named the block with it; the split
//     below it was not told, so the fragments of a call split off a repeated
//     index — the very fragments that follow the split and state the index the
//     vendor writes for every call — were routed back into the call they were
//     split from.
//   - F59-L3-2: that same shape with the whole list beside it: the two calls the
//     document arm answers with two blocks must BE two blocks here, with each
//     call's own arguments and the upstream's own ids.
//   - F59-L3-3: a fragment that restates a call under the id its key names — the
//     B45-1 re-keyed block whose delivered id is the MINTED one — was not read
//     as a restatement, so its arguments were appended to the finished ones
//     already accumulated: `{"a":1}{"a":1}`, JSON no tool can parse, under a
//     stop_reason of tool_use.
//   - F59-L3-4: the split block refused the id the upstream then stated for it,
//     because the refusal asked whether the BLOCK was numbered rather than
//     whether any other block answers to that id — so one call reached the
//     client under a hash on this arm and under the stated id on every other.
//   - F59-L3-5: the restatement of that split call, whose id the upstream states
//     under a key of its own, opened a THIRD block repeating the second call
//     whole: the model's two calls reached the client as three.
//   - F59-C-1 / F59-C-2: a call introduced with no index at all is the call the
//     client leg parks at slot 0, so the index-0 fragments that name it reach it
//     there. This bridge keyed it under a synthetic key that neither the index
//     arm nor the id arm could reach, and the wire's own index 0 — whether it
//     restates the call or carries the tail of its arguments — opened a second
//     block beside it: the model's one call delivered twice under two ids (and
//     run twice), or a truncated tool_use with the tail of its arguments relayed
//     to the agent as prose.
//
// Each case is fail-first: RED against the tree before this round's fix, with
// the same body's document arm (and the client leg) as the answer it must match.

import (
	"strings"
	"testing"
)

// r59FrameBlocks drives one frame wire and returns the calls the client was
// handed, in the order the blocks opened.
func r59FrameBlocks(t *testing.T, frames ...string) ([]string, []string, string) {
	t.Helper()
	_, body := r58FrameCalls(t, frames...)
	return round46ToolUseIDs(t, body), r58Partials(t, body), body
}

// r59DocBlocks is the same body as ONE list, the target every frame case is
// judged by: the calls the non-stream arm delivers, ids and inputs.
func r59DocBlocks(t *testing.T, doc string) ([]string, []string) {
	t.Helper()
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	_, body := round45Ask(t, srv, round45AskStream)
	return round46ToolUseIDs(t, body), r58Partials(t, body)
}

// r59Same requires the frame arm to deliver the same calls — same count, same
// ids, same inputs, same order — the document arm delivers for the same body.
func r59Same(t *testing.T, ids, parts, wantIDs, wantParts []string, note, body string) {
	t.Helper()
	if len(ids) != len(wantIDs) {
		t.Errorf("the same body is %d call(s) as fragments and %d as one list:\nfragments: %v %v\none list:  %v %v\n%s\n%s",
			len(ids), len(wantIDs), ids, parts, wantIDs, wantParts, note, body)
		return
	}
	for i := range wantIDs {
		if ids[i] != wantIDs[i] || parts[i] != wantParts[i] {
			t.Errorf("call %d is %s %s as fragments and %s %s as one list\n%s\n%s",
				i, ids[i], parts[i], wantIDs[i], wantParts[i], note, body)
		}
	}
}

// TestTheSplitCallsContinuationStaysTheSplitCall is F59-L3-1: two calls at one
// index, the second's arguments arriving under the index the vendor writes for
// every call.
func TestTheSplitCallsContinuationStaysTheSplitCall(t *testing.T) {
	ids, parts, body := r59FrameBlocks(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"Read"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":1}"}}]}}]}`,
	)
	wantIDs, wantParts := r59DocBlocks(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},`+
		`{"index":0,"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	r59Same(t, ids, parts, wantIDs, wantParts,
		"the index the split call was minted under is the slot that holds it now, so the fragments that state it — and every fragment of the wire that writes one index for all the turn's calls does — are that call's own arguments; routed back into the call they were split from, they were concatenated onto its input and the split call reached the client with an empty one (2026-09-28 audit, round 59, F59-L3-1)", body)
}

// TestTheCallsRestatedUnderTheirOwnIDsAreOneEach is F59-L3-3 and F59-L3-5: a
// call whose id arrives after the fragments that carry it, restated whole.
func TestTheCallsRestatedUnderTheirOwnIDsAreOneEach(t *testing.T) {
	// F59-L3-3: the id arrives on a fragment of its own (the B45-1 re-key), and
	// the call is then restated whole under it.
	ids, parts, body := r59FrameBlocks(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_z"}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_z","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
	)
	if len(ids) != 1 || parts[0] != `{"a":1}` {
		t.Errorf("the call restated under its own stated id reached the client as %d block(s) %v %v, want one {\"a\":1} — appended, it left the client accumulating `{\"a\":1}{\"a\":1}`, JSON no tool can parse, under a stop_reason of tool_use (2026-09-28 audit, round 59, F59-L3-3)\n%s",
			len(ids), ids, parts, body)
	}

	// F59-L3-5: two calls at one index, and the second restated whole with the
	// index omitted — the shape the client leg and the whole-list arm both keep
	// at two calls.
	ids, parts, body = r59FrameBlocks(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"Read","arguments":"{\"p\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"b","function":{"name":"Read","arguments":"{\"p\":1}"}}]}}]}`,
	)
	wantIDs, wantParts := r59DocBlocks(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
		`{"index":0,"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":3,"total_tokens":14}}`)
	r59Same(t, ids, parts, wantIDs, wantParts,
		"the call the split minted a key for, and the upstream then stated the id for, IS the call that id names: a third block repeating it whole handed the client a call the model never wrote, under a second id no tool_result can answer (2026-09-28 audit, round 59, F59-L3-5)", body)
}

// TestTheCallIntroducedWithoutAnIndexIsReachableByTheIndexThatNamesIt is
// F59-C-1 and F59-C-2: the turn's first call opened index-less, named later by
// the index 0 the client leg parks it at.
func TestTheCallIntroducedWithoutAnIndexIsReachableByTheIndexThatNamesIt(t *testing.T) {
	t.Run("the call restated at index 0", func(t *testing.T) {
		ids, parts, body := r59FrameBlocks(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		)
		if len(ids) != 1 || parts[0] != `{"a":1}` {
			t.Errorf("a call introduced with no index and named again at index 0 reached the client as %d block(s) %v %v, want the model's one call — the client leg parks an index-less call at slot 0 and finds it there, and this bridge's synthetic key was unreachable from \"#0\": the one call was delivered TWICE, under two ids, and an agent loop runs it twice (2026-09-28 audit, round 59, F59-C-1)\n%s",
				len(ids), ids, parts, body)
		}
	})

	t.Run("the arguments continued at index 0", func(t *testing.T) {
		ids, parts, body := r59FrameBlocks(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"Bash"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_x","function":{"arguments":"{\"cmd\":"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"ls\"}"}}]}}]}`,
		)
		if len(ids) != 1 || parts[0] != `{"cmd":"ls"}` {
			t.Errorf("the tail of a call's arguments stated at index 0 reached the client as %d block(s) %v %v, want one complete call — the fragment opened a name-less block beside the call and its bytes were relayed as TEXT, so the client got a tool_use whose input never closes under a stop_reason of tool_use, and the model's arguments were delivered as something the model said (2026-09-28 audit, round 59, F59-C-2)\n%s",
				len(ids), ids, parts, body)
		}
	})

	// The control: the same three fragments with no index anywhere are one call,
	// pinned since round 45 (B45-1) — the fix above must not disturb it.
	t.Run("no index anywhere stays one call", func(t *testing.T) {
		ids, parts, _ := r59FrameBlocks(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"Bash"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"id":"call_x","function":{"arguments":"{\"cmd\":"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"arguments":"\"ls\"}"}}]}}]}`,
		)
		if len(ids) != 1 || parts[0] != `{"cmd":"ls"}` {
			t.Errorf("three index-less fragments of one call reached the client as %d block(s) %v %v", len(ids), ids, parts)
		}
	})

	// The other control: a NON-zero index naming a call the stream wrote
	// index-less is left alone — the client leg answers that wire with two
	// blocks as well, and folding them here would diverge the other way.
	t.Run("a non-zero index is not the parked call", func(t *testing.T) {
		ids, _, _ := r59FrameBlocks(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":3,"function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		)
		if len(ids) != 2 {
			t.Errorf("a restatement at index 3 gave %d block(s) %v, want 2 (the client leg's own answer to this wire)", len(ids), ids)
		}
	})
}

// TestTheIndexedWiresKeepTheirCalls holds what must NOT split — the ordinary
// indexed wire and two calls each re-keyed and continued by their own index,
// both already GREEN before this round — beside F59-L3-4: a call opened at a
// slot a re-key has since vacated states its own id there, and the bridge
// refused it, because the refusal asked whether the BLOCK at that key was
// numbered rather than whether any other block answers to the id. One call
// reached the client under a hash on this arm and under the stated id on every
// other (an id no tool_result the client writes can name).
func TestTheIndexedWiresKeepTheirCalls(t *testing.T) {
	// The ordinary restatement wire, index stated on every fragment.
	ids, body := r58FrameCalls(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
	)
	if len(ids) != 1 || ids[0] != "call_1" {
		t.Errorf("the call listed again at its own slot reached the client as %v (round 56's F1):\n%s", ids, body)
	}
	if parts := r58Partials(t, body); len(parts) != 1 || parts[0] != `{"a":1}` {
		t.Errorf("the restated call's input is %v, want one {\"a\":1}\n%s", parts, body)
	}

	// Two calls, each opened by its index and re-keyed to a late id, each then
	// continued by its own index: one block each, under the stated ids.
	ids, parts, body := r59FrameBlocks(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"x","function":{"arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"name":"Read"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"y","function":{"arguments":"{\"b\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"2}"}}]}}]}`,
	)
	if len(ids) != 2 || ids[0] != "x" || ids[1] != "y" ||
		parts[0] != `{"a":1}` || parts[1] != `{"b":2}` {
		t.Errorf("two re-keyed calls continued at their own indexes reached the client as %v %v, want [x y] [{\"a\":1} {\"b\":2}]\n%s", ids, parts, body)
	}

	// F59-L3-4: a second call opened at the slot the re-key vacated, stating its
	// own id. Before this round's fix it reached the client under a minted hash.
	ids, parts, body = r59FrameBlocks(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"x","function":{"arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"z","function":{"name":"Read","arguments":"{\"b\":2}"}}]}}]}`,
	)
	if len(ids) != 2 || ids[0] != "x" || ids[1] != "z" ||
		parts[0] != `{"a":1}` || parts[1] != `{"b":2}` {
		t.Errorf("a second call opened at a re-keyed slot reached the client as %v %v, want [x z] [{\"a\":1} {\"b\":2}] — the id it stated was refused because the refusal asked whether the BLOCK at that key was numbered rather than whether any other block answers to the id, so one call reached the client under a hash on this arm and under the id every other arm delivers (2026-09-28 audit, round 59, F59-L3-4)\n%s", ids, parts, body)
	}

	if !strings.Contains(body, "content_block_stop") {
		t.Errorf("premise: the case above never opened a block:\n%s", body)
	}
}
