package main

// round65_nameless_call_id_and_index_scope_integrity_test.go — round 65's two
// findings on the gateway bridge.
//
//	A NAMELESS FRAGMENT'S STATED ID IS NOT A CALL'S ID. content_block_start is
//	the only event that carries a name and a start without one can never be
//	corrected, so a fragment that names nothing opens no block: its arguments
//	reach the client as prose (round 39's B-F8, round 45's B45-1, round 55) and
//	the whole-list arm skips such an entry outright — it reserves the id it
//	states and then walks past it, because a call is introduced by its name.
//	The fragment arm let that fragment CLAIM the id instead, so the call that
//	did name itself, and stated the very same id, looked like a second call
//	under an id already handed out and reached the client minted: the same
//	upstream answer carried `a` as one completion and `call_83cf9330` as the
//	fragments of that same completion, and a tool_result written against one
//	could not be answered by the other (2026-09-28 audit, round 65).
//
//	THE CHUNK THAT CLOSES A CALL'S ARGUMENTS AT A FRESH INDEX. The vendor that
//	states the call's id and name once and then numbers its own fragments closes
//	the argument object at an index it never used to introduce anything. The
//	chain walk that saves such a fragment when the index names a call it has
//	already seen is scoped to indices that named one, so the closing byte opened
//	its own nameless block and was relayed to the client as assistant PROSE
//	while the call it finished arrived unterminated — under a stop_reason of
//	tool_use, so the client ran Bash with `{"cmd":"ls"`. Round 64's F64-L3-3 is
//	this wire with the closer at the index the reroute used; one index over,
//	the five other spellings of the same byte already agreed with each other
//	(2026-09-28 audit, round 65, R65-L3-1).
//
// Both are fail-first: RED against the tree before this round's fix.

import (
	"testing"
)

// TestANamelessFragmentsIDIsNotACallsID is the first finding. The nameless
// fragment states the id the NEXT call states for itself, at another index:
// the call is the id's, and the nameless fragment's own bytes are prose.
func TestANamelessFragmentsIDIsNotACallsID(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"","arguments":"{}"}},`+
		`{"index":1,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a fragment that names nothing is not a call, so the id it states is not one it holds: the call that names itself must still reach the client under the id the upstream wrote for it",
		"")

	// The same wire with NO index stated at all: the id arm rather than the
	// index arm. The fragment that names itself carries a whole object the
	// nameless block's finished `{}` cannot take, so it is the next call — the
	// reading the index arm already applies one clause over. The nameless
	// block's bytes stay the model's own text and the call keeps its arguments.
	ids, parts, text = r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText = r60DocArm(t, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"id":"a","type":"function","function":{"name":"","arguments":"{}"}},`+
		`{"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the nameless fragment's object cannot be the named call's arguments: the call is a call of its own and the nameless fragment's bytes are prose, exactly as the index arm and the whole-list arm read the same body",
		"")
}

// TestTheFreshIndexThatClosesTheCallFinishesIt is R65-L3-1. The vendor states the
// call's id and name on its introducing fragment and numbers its own fragments
// after that; the byte that closes the argument object lands at an index of its
// own, one that introduced nothing.
func TestTheFreshIndexThatClosesTheCallFinishesIt(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Bash","arguments":"\"ls\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":2,"function":{"arguments":"}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the chunk that closes the argument object states neither id nor name, at an index that introduced nothing: it finishes the call still being written, and the byte is not prose",
		"")

	// Round 64's control, kept here because it is the wire one index over: the
	// same chunk at the index the reroute used is already one call.
	ctlIDs, ctlParts, ctlText := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Bash","arguments":"\"ls\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]}}]}`,
	)
	r60ArmSame(t, ctlIDs, ctlParts, ctlText, wantIDs, wantParts, wantText,
		"the same closing chunk at the index the reroute used (round 64's F64-L3-3 wire)", "")

	// And the fragment whose bytes are a whole object of its own does NOT finish
	// the open call: it is a call the upstream never named, and its bytes are
	// relayed as prose, exactly as the whole-list arm reads the same body.
	ownIDs, ownParts, ownText := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Bash","arguments":"\"ls\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":2,"function":{"arguments":"{\"p\":1}"}}]}}]}`,
	)
	if len(ownIDs) != 1 || len(ownParts) != 1 || ownParts[0] != `{"cmd":"ls"` || ownText != `{"p":1}` {
		t.Errorf("a nameless fragment at a fresh index whose bytes do not finish the open call is not a continuation of it: got ids=%v parts=%v text=%q, want one call with the open arguments and the object relayed as prose", ownIDs, ownParts, ownText)
	}
}

// TestTheControlsRound65DeclinesToTradeAway pins the two wires that make the
// remaining frame-vs-document divergence this round found undecidable online.
//
// The fragment that states the id of a call already being written, at another
// index, with a name of its own and a whole object, is BYTE-INDISTINGUISHABLE
// at fold time from the vendor that numbers its own fragments and writes a
// nested object across them:
//
//	the second call   `{"a":` ++ `{"b":2}`  — the document arm reads TWO calls
//	the nested object `{"query":` ++ `{"sql":"select 1"}` ++ `,"limit":10}`
//	                                         — every arm reads ONE call
//
// Neither the name (equal on both) nor the index (fresh on both) separates
// them, and whether the merged text is one object or two only becomes knowable
// once the LAST byte is in — after the first bytes are already deltas on the
// wire. Round 65 declined to add a predicate here: every predicate narrow enough
// to split the second call splits the nested object with it, which is a wire all
// three arms currently answer consistently. Closing it means holding a folded
// fragment's deltas until the object validates or the stream ends (the
// heldToolBlocks machinery), which is a change to the delivery path rather than
// a guard — recorded, not taken (2026-09-28 audit, round 65, R65-L3-2).
func TestTheControlsRound65DeclinesToTradeAway(t *testing.T) {
	// The nested object across fragments of one index-numbering vendor: one
	// call, on the fragment arm and on the whole-list arm alike.
	nested := `{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"query\":{\"sql\":\"select 1\"},\"limit\":10}"}}`
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+nested+`]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"query\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"Bash","arguments":"{\"sql\":\"select 1\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":2,"id":"call_1","function":{"name":"Bash","arguments":",\"limit\":10}"}}]}}]}`,
	)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the vendor that numbers its own fragments and writes a nested object across them is ONE call: any rule that split a whole object at a fresh index would split this too",
		"")

	// The call introduced by id and arguments and NAMED one chunk later: one
	// call, both arms. This is the wire the reroute's nameless-carried-call
	// clause exists for, and the reason a nameless block still has to be a
	// reroute target even though it holds no id.
	late := `{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`
	wantIDs, wantParts, wantText = r60DocArm(t, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+late+`]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	ids, parts, text = r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Bash","arguments":"\"ls\"}"}}]}}]}`,
	)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the call the upstream names one chunk after its id and arguments is one call: the nameless block it was written at is still a call the reroute must reach",
		"")
}
