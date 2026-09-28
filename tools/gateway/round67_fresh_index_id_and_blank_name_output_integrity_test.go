package main

// round67_fresh_index_id_and_blank_name_output_integrity_test.go — round 67's
// two findings on the metered gateway, both of them a predicate that was right
// on one arm of this leg and stale on the other.
//
// F67-L3-1. A call that stated its id and name and NO arguments has an argument
// list that is already complete — an empty one. The id route inside toolKey
// asked whether that list was FINISHED and folded a whole object into it when
// the answer was no, which an empty list always gives; its sibling, the
// fresh-index fallback, had been moved to argsAreMidObject in round 66
// (F66-L3-1) with exactly this reasoning and this route kept the old predicate.
// So a nameless fragment stating the call's own id at a fresh index had its
// object run by the client as that call's arguments, where the document arm of
// the same body relays the bytes as prose and leaves the call argument-less.
//
// F67-L3-2. Round 66 ruled that every arm decides whether an entry introduces a
// call with namesItself, and moved the block-open guard and finishStream's
// truncated-turn opener onto it — but callsCarryingOutput, the predicate behind
// nothingRelayed() and so behind the 502 an empty turn gets, still asked
// `tb.name != ""`. A name of whitespace therefore counted as output, and a
// stream whose only content was a blank-named fragment with no arguments was
// relayed as a COMPLETED empty assistant turn where the document arm refuses the
// same body.

import (
	"strings"
	"testing"
)

// r67DocStatus is r60DocArm's document arm keeping the status it reported.
func r67DocStatus(t *testing.T, doc string) (int, []string, []string, string) {
	t.Helper()
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	return status, round46ToolUseIDs(t, body), r58Partials(t, body), r53BlockText(t, body)
}

// r67FrameBody is r58FrameCalls with the raw body kept, for the shapes whose
// answer is a verdict rather than a call list.
func r67FrameBody(t *testing.T, frames ...string) (int, string) {
	t.Helper()
	up := round45Frames(t, append(frames, `data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`, `data: [DONE]`)...)
	srv, _ := round39Gateway(t, up, nil)
	return round45Ask(t, srv, round45AskStream)
}

// TestAnIdStatedAtAFreshIndexIsNotTheArgumentsOfAnArgumentLessCall is F67-L3-1.
// One upstream answer, two spellings: a call stating its id and name and no
// arguments, then a nameless fragment at a fresh index stating the same id and
// a whole object.
func TestAnIdStatedAtAFreshIndexIsNotTheArgumentsOfAnArgumentLessCall(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"arguments":"{\"x\":1}"}}]}}]}`,
	}
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":""}},`+
			`{"index":1,"id":"a","type":"function","function":{"arguments":"{\"x\":1}"}}`)

	if len(wantIDs) != 1 || len(wantParts) != 1 || wantParts[0] != "" {
		t.Fatalf("PREMISE: the document arm answers ids=%v parts=%v, want one call with an empty argument list", wantIDs, wantParts)
	}
	if wantText != `{"x":1}` {
		t.Fatalf("PREMISE: the document arm relays %q as prose, want the fragment's bytes — a call that has written nothing has a complete (empty) argument list, so a whole object at an index it never introduced is not its first arguments", wantText)
	}

	ids, parts, text := r60FrameArm(t, frames...)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a call stated its id and name with no arguments, and a nameless fragment restated that id at a fresh index with a whole object, so the fragment's bytes are that call's first arguments only if a complete argument list can still grow — it cannot, which is why the same bytes are prose on every other arm of this body (2026-09-28 audit, round 67, F67-L3-1)",
		"")
}

// TestABlankNameIsNoOutputOnTheStreamingArmEither is F67-L3-2: the same body,
// spelled as fragments and as one list, whose only content is a blank-named
// call with no arguments.
func TestABlankNameIsNoOutputOnTheStreamingArmEither(t *testing.T) {
	const blankFrame = `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":" ","arguments":""}}]}}]}`

	status, ids, parts, text := r67DocStatus(t, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,`+
		`"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":" ","arguments":""}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":0}}`)
	if status != 502 {
		t.Fatalf("PREMISE: the document arm reports %d, want 502 (a name of whitespace introduces no call and states no arguments, so the turn says nothing)", status)
	}
	if len(ids) != 0 || len(parts) != 0 || text != "" {
		t.Fatalf("PREMISE: the document arm relays ids=%v parts=%v text=%q, want nothing", ids, parts, text)
	}

	_, body := r67FrameBody(t, blankFrame)
	if strings.Contains(body, `"message_stop"`) {
		t.Errorf("the same body streamed is reported as a COMPLETED turn while the document arm answers 502: a blank name is no name (namesItself), so the fragment relays no call and no arguments, and the turn has said nothing (2026-09-28 audit, round 67, F67-L3-2)\n%s", body)
	}
	if !strings.Contains(body, `"type":"error"`) {
		t.Errorf("the stream reports neither a completed turn nor an error for a body the document arm refuses: the client is left with a turn that never ends (2026-09-28 audit, round 67, F67-L3-2)\n%s", body)
	}
	fids, fparts, ftext := round46ToolUseIDs(t, body), r58Partials(t, body), r53BlockText(t, body)
	if len(fids) != 0 || len(fparts) != 0 || ftext != "" {
		t.Errorf("the stream arm relays ids=%v parts=%v text=%q where the document arm relays nothing", fids, fparts, ftext)
	}
}

// TestABlankNamedCallWithArgumentsIsStillOutput is the control for F67-L3-2:
// namesItself must not be read as "a call with no arguments relays nothing" —
// round 39's B-F8 requires the bytes of a call the upstream never named to reach
// the client, so such a turn is NOT empty.
func TestABlankNamedCallWithArgumentsIsStillOutput(t *testing.T) {
	_, body := r67FrameBody(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":" ","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`)
	if !strings.Contains(body, `"message_stop"`) {
		t.Errorf("a blank-named call that STATES arguments is held and its bytes are owed to the client, so the turn is not empty and must complete:\n%s", body)
	}
	if got := r53BlockText(t, body); got != `{"cmd":"ls"}` {
		t.Errorf("the arguments a call never named are relayed as %q, want the raw bytes (round 39, B-F8)", got)
	}
}
