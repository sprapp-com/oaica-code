package main

// round68_nameless_slot_occupant_integrity_test.go — leg 3's slot occupant,
// pinned: a block that never named itself is not a call.
//
// Round 39's B-F8 settled what a call the upstream never named is worth: its
// content_block_start could only go out with an empty name (the event that
// carries the name has no second chance), so the block is never opened and its
// arguments reach the client as TEXT. A block that is never opened fills no
// slot either — but this bridge recorded it as the occupant of its index, and
// asked only that it EXIST. So the call that then named itself at that index
// was read as more of a block that is not a call: written into it (the prose's
// bytes folded into the call's input) or split off beside it once per
// restatement (two tool_use blocks under one id), where the document arm of the
// same body answers the call whole and the nameless bytes as prose.
//
// Every case below is ONE upstream answer spelled twice: as delta fragments and
// as one tool_calls list. The two arms must agree.

import "testing"

// TestANamelessFragmentDoesNotHoldTheSlotFromTheCallThatNamesItself is
// F68-L3-1's first shape: a name-less fragment opens the index with arguments,
// and the call that names itself arrives at that same index twice.
func TestANamelessFragmentDoesNotHoldTheSlotFromTheCallThatNamesItself(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"x\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
	}
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"index":0,"type":"function","function":{"arguments":"{\"x\":"}},`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`)

	if len(wantIDs) != 1 || wantText == "" {
		t.Fatalf("PREMISE: the document arm answers ids=%v parts=%v text=%q, want one call and the nameless bytes as prose", wantIDs, wantParts, wantText)
	}

	ids, parts, text := r60FrameArm(t, frames...)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a fragment that named nothing opened the index, and the call that named itself at that index was folded into its bytes: the client ran Bash with prose in its input where the document arm answers the call whole and relays those bytes as text (2026-09-28 audit, round 68, F68-L3-1)",
		"")
}

// TestARestatementAtANamelesslyOpenedIndexStaysOneCall is the same shape with a
// whole (finished) nameless object first and a blank name, which reaches the
// slot by the index record rather than by the fragment's own bytes.
func TestARestatementAtANamelesslyOpenedIndexStaysOneCall(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":" ","arguments":"{\"f\":6}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
	}
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"index":0,"id":"a","type":"function","function":{"name":" ","arguments":"{\"f\":6}"}},`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`)

	if len(wantIDs) != 1 || wantText == "" {
		t.Fatalf("PREMISE: the document arm answers ids=%v parts=%v text=%q, want one call and the blank-named bytes as prose", wantIDs, wantParts, wantText)
	}

	ids, parts, text := r60FrameArm(t, frames...)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a call whose name is whitespace is no call (namesItself), so the index it was recorded for is free: the call that named itself there reached the client twice, under one id, while the document arm answers it once (2026-09-28 audit, round 68, F68-L3-1)",
		"")
}

// TestANamelessContinuationFragmentDoesNotHoldTheSlotEither is the id-less
// spelling of the same rule: the nameless entry states the id of the call that
// follows it, and that call is stated twice.
func TestANamelessContinuationFragmentDoesNotHoldTheSlotEither(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"arguments":"}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
	}
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"id":"a","type":"function","function":{"arguments":"}"}},`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`)

	if len(wantIDs) != 1 || wantText != "}" {
		t.Fatalf("PREMISE: the document arm answers ids=%v parts=%v text=%q, want one call and the nameless bytes as prose", wantIDs, wantParts, wantText)
	}

	ids, parts, text := r60FrameArm(t, frames...)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"the block an id-less nameless fragment opened held the index, so the call that named itself there was split off once per restatement: the client was handed two tool_use blocks under one id where the document arm answers one call (2026-09-28 audit, round 68, F68-L3-1)",
		"")
}
