package main

// round81_call_boundary_and_closed_block_integrity_test.go — leg 3, the places
// a call the upstream left half-written, or a block it had already closed, was
// answered differently by the arms of one body (2026-09-28 audit, round 81).
//
// One body reaches this bridge three ways: as one whole completion read as a
// document (the document arm), as that same completion read by the non-stream
// path (the non-stream arm), and as a run of chunks (the frame arm). All three
// read the same calls in the same order and have to hand the client the same
// thing.
//
//  1. F81-L3-1. A NAMED call whose arguments are half an object on a turn the
//     upstream truncated. The arguments are a fact about the call, not about
//     who numbers it: the block's start is the input the client RUNS, and both
//     document arms of this body answer no tool block at all and report
//     max_tokens, while this arm opened one and handed the client `{"a":`.
//
//  2. F81-L3-2. The same half-object on a turn the upstream did NOT truncate:
//     the whole-object wrap the document arms keep (F80-L3-4) was reached only
//     for a call this bridge had to number, so an upstream that had already
//     stated the call's id handed the client `{"r":1` under a stop_reason of
//     tool_use — the bare text of an input no client can parse.
//
//  3. F81-L3-3. A nameless fragment standing after a call that has CLOSED. Its
//     bytes cannot be more of that call — the client has been told the call is
//     finished and the wire has no way to reopen its block — and the router
//     sent them there anyway, where the write refused them and they were lost.
//     Both document arms relay them as prose.
//
// Round 80 ruled the wrap and the drop for the call this bridge NUMBERS (the
// held one); these are the same shapes for a call the upstream named itself,
// which the hold did not reach. Two landed pins read the named half-object the
// other way — round 52's TestATruncatedCallIsNotACall and the third case of
// round 65's TestTheFreshIndexThatClosesTheCallFinishesIt — and both are
// reversed here rather than kept, because what forced the reading they pin is
// gone: the block used to be out before the turn's verdict was known, so the
// bytes could not be taken back, and the hold means it never goes out at all
// (2026-09-28 audit, round 81).

import (
	"strings"
	"testing"
)

// r81ThreeArms reads one entry list as a document, as the non-stream path, and
// as a run of chunks — the verdict spelled out by hand, because the frame
// helper states one of its own and the last reason the wire states is the
// turn's.
func r81ThreeArms(t *testing.T, entries []string, fin string) (doc, plain, frame string) {
	t.Helper()
	whole := r80Leg3Doc(strings.Join(entries, ","), fin)
	frames := make([]string, 0, len(entries)+2)
	for _, e := range entries {
		frames = append(frames, r71Leg3Frame(e))
	}
	frames = append(frames,
		`data: {"choices":[{"delta":{},"finish_reason":"`+fin+`"}]}`,
		`data: [DONE]`)
	up := round45Frames(t, frames...)
	srv, _ := round39Gateway(t, up, nil)
	_, frame = round45Ask(t, srv, round45AskStream)
	return r72DocTurn(t, whole), r74PlainTurn(t, whole), frame
}

// TestATruncatedNamedCallIsNotACall is F81-L3-1. The turn is cut at the token
// limit with a call the upstream NAMED and left mid-object: no arm of the body
// hands the client a tool block, and every arm reports the truncation.
func TestATruncatedNamedCallIsNotACall(t *testing.T) {
	entries := []string{
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"a\":"}}`,
	}
	doc, plain, frame := r81ThreeArms(t, entries, "length")
	for _, arm := range []struct {
		name string
		body string
	}{{"document", doc}, {"non-stream", plain}, {"frame", frame}} {
		if got := r76BlockOrder(t, arm.body); strings.Contains(got, "tool_use") {
			t.Errorf("the %s arm opened %s for a call the upstream cut off inside its arguments — the block's input is what the client would RUN, and a half object is not an input any client can parse (2026-09-28 audit, round 81, F81-L3-1)\n%s",
				arm.name, got, arm.body)
		}
		if !strings.Contains(arm.body, `"stop_reason":"max_tokens"`) {
			t.Errorf("the %s arm does not report the upstream's truncation (2026-09-28 audit, round 81, F81-L3-1)\n%s", arm.name, arm.body)
		}
	}
}

// TestAMidObjectNamedCallIsWrappedOnEveryArm is F81-L3-2. The same half-object
// on a turn that is not truncated: the arguments never became an object, so
// every arm keeps them as the single-key _raw object (the shape F80-L3-4 pinned
// for the call this bridge numbers).
func TestAMidObjectNamedCallIsWrappedOnEveryArm(t *testing.T) {
	entries := []string{
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"r\":1"}}`,
	}
	doc, plain, frame := r81ThreeArms(t, entries, "tool_calls")
	want := `{"_raw":"{\"r\":1"}`
	for _, arm := range []struct {
		name string
		body string
	}{{"document", doc}, {"non-stream", plain}, {"frame", frame}} {
		got := r80Leg3ToolInputs(t, arm.body)
		if len(got) != 1 || got[0] != want {
			t.Errorf("the %s arm handed the client %v, want [%s] — the arguments a call never closed into an object are kept as the _raw object on every arm, whoever stated the call's id (2026-09-28 audit, round 81, F81-L3-2)\n%s",
				arm.name, got, want, arm.body)
		}
	}
}

// TestANamelessFragmentAfterAClosedCallIsProse is F81-L3-3. A nameless fragment
// stands after a call that has closed: its bytes are the model's own output, the
// client is told the call is finished, and both document arms relay it as prose.
func TestANamelessFragmentAfterAClosedCallIsProse(t *testing.T) {
	entries := []string{
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hi"}}`,
		`{"index":0,"id":"call_2","type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`,
		`{"index":0,"type":"function","function":{"arguments":"A"}}`,
	}
	doc, plain, frame := r81ThreeArms(t, entries, "tool_calls")
	want := `<tool_use call_1><tool_use call_2><text "A">`
	for _, arm := range []struct {
		name string
		body string
	}{{"document", doc}, {"non-stream", plain}, {"frame", frame}} {
		if got := r76BlockOrder(t, arm.body); got != want {
			t.Errorf("the %s arm answered %s, want %s — a nameless fragment is the model's prose, and a block that has CLOSED cannot take it: the write refuses the bytes and the model's output reaches no client at all (2026-09-28 audit, round 81, F81-L3-3)\n%s",
				arm.name, got, want, arm.body)
		}
	}
}
