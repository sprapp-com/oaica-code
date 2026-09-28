package main

// round70_nameless_holder_duplicate_id_integrity_test.go — leg 3, the holder
// that cannot hold.
//
// Two tool_use blocks under one id get one tool_result for two calls, so the
// fragment arm's startToolBlock asks, at the one point an id reaches the
// client, whether another block already carries it — and answers a restatement
// of the same call with ONE block. A block the upstream never named opens no
// tool_use at all (round 39's B-F8), so it is not a holder a client can see;
// but the sweep stopped at the FIRST block carrying the id and then required
// that block to name itself before doing anything, so a nameless first holder
// made the whole guard unreachable. The call restated at a fresh index then
// opened a second block under the same id, where both whole-document arms of
// the same body answer one (2026-09-28 audit, round 70, R70-L3-1).
//
// The holder has to be nameless AND the restatement has to arrive at an index
// the earlier fragments never used: at the call's own index the fragment is
// folded by toolKey's index arm, which is round 68's fix, and the sweep is
// never reached. Every case below is one upstream answer spelled twice — as
// delta fragments and as one tool_calls list — and the two arms must agree.

import "testing"

// TestANamelessHolderDoesNotHideTheCarryingBlock is R70-L3-1's shape: a chunk
// that states the id and no name introduces the call, the call names itself at
// that same index, and the whole thing is restated at a fresh index. The named
// block carries the id; the nameless one never will.
func TestANamelessHolderDoesNotHideTheCarryingBlock(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
	}
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"index":0,"id":"call_1","type":"function","function":{}},`+
			`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"index":1,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`)

	if len(wantIDs) != 1 || wantIDs[0] != "call_1" || wantText != "" {
		t.Fatalf("PREMISE: the document arm must answer one call under the stated id, got ids=%v parts=%v text=%q", wantIDs, wantParts, wantText)
	}

	ids, parts, text := r60FrameArm(t, frames...)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a nameless block hid the block that carries the id, so the call restated at a fresh index opened a SECOND tool_use under one id: one tool_result cannot answer two calls (2026-09-28 audit, round 70, R70-L3-1)",
		`{"index":0,"id":"call_1","function":{}} ++ {"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}} ++ {"index":1,...}`)
}

// TestABlankNameHolderDoesNotHideTheCarryingBlock is the same defect through
// the other spelling of "no name": a holder whose name is whitespace.
func TestABlankNameHolderDoesNotHideTheCarryingBlock(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":" ","arguments":"{\"f\":6}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
	}
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"index":0,"id":"call_1","type":"function","function":{"name":" ","arguments":"{\"f\":6}"}},`+
			`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"index":1,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`)

	if len(wantIDs) != 1 || wantIDs[0] != "call_1" {
		t.Fatalf("PREMISE: the document arm must answer one call under the stated id, got ids=%v parts=%v text=%q", wantIDs, wantParts, wantText)
	}

	ids, parts, text := r60FrameArm(t, frames...)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a blank-named block hid the block that carries the id, and the restatement opened a second tool_use under one id (2026-09-28 audit, round 70, R70-L3-1)",
		"blank-name holder ++ named call ++ restatement at a fresh index")
}

// TestARestatementBehindANamedHolderStaysOneCall is the control: with a holder
// that names itself the sweep always reached the guard, so this shape was
// already right and the fix must not change it.
func TestARestatementBehindANamedHolderStaysOneCall(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
	}
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"index":1,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`)

	if len(wantIDs) != 1 || wantIDs[0] != "call_1" {
		t.Fatalf("PREMISE: the document arm must answer one call, got ids=%v parts=%v text=%q", wantIDs, wantParts, wantText)
	}

	ids, parts, text := r60FrameArm(t, frames...)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a restatement at a fresh index behind a NAMED holder must stay one call (control for round 70's fix)",
		"named call ++ restatement at a fresh index")
}
