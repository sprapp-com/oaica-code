package main

// round62_call_feed_integrity_test.go — round 62's findings on the gateway
// bridge's index arm (F62-L3-1, F62-L3-2, F62-L3-3).
//
// The bridge keeps ONE block open at a time: a text block or a tool block, and
// the next block's start closes the one before it. That is sound only while a
// block has nothing left to receive — and a call whose introducing fragment
// stated no arguments at all has everything left to receive. The vendor that
// writes one index for every call of the turn introduces its calls and streams
// their arguments afterwards, so the slot's FIRST call was opened, closed by the
// second call's start, and left with NO input when its own bytes arrived: the
// client ran a Bash with the empty input, and the bytes themselves were refused
// by the closed-block guard (round 39's B-F7). The document arm of the same body
// answers the call with its arguments.
//
//   - F62-L3-1: an object the model is still writing takes a NESTED object.
//     `{"query":` ++ `{"sql":"select 1"}` ++ `,"limit":10}` is one call's value
//     in progress; refusing the middle piece left the client the pieces either
//     side of it, `{"query":,"limit":10}`, JSON no tool can parse, under a
//     stop_reason of tool_use, while this bridge's own document arm answered the
//     call whole.
//   - F62-L3-2: the same nested piece with the slot's own id and name restated
//     on it — the vendor that restates its identity on every chunk — is still
//     one call.
//   - F62-L3-3: one index for every call of the turn: the calls are introduced
//     in one delta and fed one fragment each, in the order they were
//     introduced, so the FIRST call's arguments arrive while the slot names the
//     second.
//
// Each case asks the SAME body of both of this bridge's arms — the fragments the
// upstream streamed and the whole completion the same upstream writes — and each
// is fail-first: RED against the tree before this round's fix.

import (
	"testing"
)

// TestANestedObjectChunkedOntoAnObjectInProgressIsOneCallOnThisLeg is F62-L3-1.
func TestANestedObjectChunkedOntoAnObjectInProgressIsOneCallOnThisLeg(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"query\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"sql\":\"select 1\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":",\"limit\":10}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"query\":{\"sql\":\"select 1\"},\"limit\":10}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a nested object chunked onto an object the model is still writing is more of that call: refusing the piece left the client `{\"query\":,\"limit\":10}`, JSON no tool can parse, under a stop_reason of tool_use, while the document arm of the same body carried the call whole (2026-09-28 audit, round 62, F62-L3-1)", "")
}

// TestANestedObjectRestatedUnderTheSlotsOwnIdentityIsOneCallOnThisLeg is
// F62-L3-2: the vendor that restates the slot's id and name on every chunk of
// the call's arguments.
func TestANestedObjectRestatedUnderTheSlotsOwnIdentityIsOneCallOnThisLeg(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"sql\":\"select 1\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":{\"sql\":\"select 1\"}}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the restated id and name do not make a nested value a second call: the middle piece was split off into a call the model never wrote and the call left behind held `{\"a\":` (2026-09-28 audit, round 62, F62-L3-2)", "")
}

// TestOneIndexForTheTurnsCallsFeedsEachCallItsOwnArgumentsOnThisLeg is F62-L3-3.
func TestOneIndexForTheTurnsCallsFeedsEachCallItsOwnArgumentsOnThisLeg(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":""}},{"index":0,"id":"b","function":{"name":"Read","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"p\":1}"}}]}}]}`,
	)
	wantIDs, wantParts, wantText := r60DocArm(t, `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},`+
		`{"index":0,"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the slot's first call was opened on the fragment that stated no arguments, closed by the second call's start, and left with an empty input when its own arguments arrived — and those bytes were refused as sent to a closed block (2026-09-28 audit, round 62, F62-L3-3)", "")
}

// TestThePinnedCallFeedWiresStillAnswerOnThisLeg holds the wires this round's
// hold must not disturb: a call whose arguments arrive in pieces after a
// fragment that stated its name alone is ONE call with the whole object, and a
// call that states no arguments at all is still opened, with its empty input, at
// the end of the turn.
func TestThePinnedCallFeedWiresStillAnswerOnThisLeg(t *testing.T) {
	t.Run("a call named before its arguments is one call", func(t *testing.T) {
		ids, parts, _ := r60FrameArm(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":""}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		)
		if len(ids) != 1 || ids[0] != "call_1" {
			t.Fatalf("CONTROL broke: the model's one call came out as %v, want call_1", ids)
		}
		if parts[0] != `{"cmd":"ls"}` {
			t.Errorf("CONTROL broke: the call's arguments reached the client as %q, want the model's object", parts[0])
		}
	})

	t.Run("a call with no arguments is still opened", func(t *testing.T) {
		ids, _, _ := r60FrameArm(t,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash"}}]}}]}`,
		)
		if len(ids) != 1 || ids[0] != "call_1" {
			t.Errorf("CONTROL broke: a call the upstream stated with no arguments came out as %v, want call_1 with its empty input", ids)
		}
	})
}
