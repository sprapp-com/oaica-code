package main

// round66_stranded_fragment_and_blank_name_integrity_test.go — round 66's
// three findings on leg 3, one test each. Every one is the same shape: the
// whole-document arm and the client leg answer a body one way and the fragment
// arm answered it another, so one upstream response was two different turns
// depending on whether the client had asked for `stream`.
//
// Each test asserts the VALUE the whole-document arm states before it compares
// the two arms: agreement alone is satisfied by two arms that are wrong
// together, and the document arm here is the one the client leg agrees with.

import "testing"

// r66DocCalls puts a `tool_calls` list on the whole-document arm of one
// completion and returns what the client got.
func r66DocCalls(t *testing.T, calls string) ([]string, []string, string) {
	t.Helper()
	return r60DocArm(t, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,`+
		`"message":{"role":"assistant","content":"","tool_calls":[`+calls+`]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
}

// TestAFreshIndexObjectIsNotTheFirstArgumentsOfAnArgumentLessCall is F66-L3-1:
// a call that stated its name and NO arguments, then a nameless whole object at
// an index it never introduced. An empty argument list is a COMPLETE argument
// list (argsAreFinished), so the call is not mid-object and the object cannot be
// more of it: it is a fragment of a call that never named itself, which every
// arm relays as text the client does not run. Only the fragment arm read it as
// the first arguments, so the client ran a call the model had left empty with
// the arguments of a call that was never introduced.
func TestAFreshIndexObjectIsNotTheFirstArgumentsOfAnArgumentLessCall(t *testing.T) {
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":""}},`+
			`{"index":2,"type":"function","function":{"arguments":"{\"cmd\":\"rm -rf /\"}"}}`)
	// Premise: the whole-document arm is the authority, and it states the object
	// as relayed prose with no call behind it. Its one partial is the empty
	// string: the call it does answer stated no arguments at all.
	if len(wantIDs) != 1 || wantIDs[0] != "a" || len(wantParts) != 1 || wantParts[0] != "" || wantText != `{"cmd":"rm -rf /"}` {
		t.Fatalf("premise: the one-list arm must answer the named call alone with the object as prose, got ids=%v parts=%v text=%q", wantIDs, wantParts, wantText)
	}
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":2,"function":{"arguments":"{\"cmd\":\"rm -rf /\"}"}}]}}]}`,
	)
	note := `{"index":0,"id":"a","function":{"name":"Bash","arguments":""}} ++ {"index":2,"function":{"arguments":"{\"cmd\":\"rm -rf /\"}"}}`
	if len(ids) != 1 || ids[0] != "a" || len(parts) != 1 || parts[0] != "" || text != wantText {
		t.Errorf("a call that stated no arguments is not mid-object, so a whole object at a fresh index is not its first arguments — the client runs a call the model left empty, with arguments of a call it never introduced (2026-09-28 audit, round 66, F66-L3-1)\nfragments: ids=%v parts=%v text=%q\none list:  ids=%v parts=%v text=%q\n%s", ids, parts, text, wantIDs, wantParts, wantText, note)
	}
}

// TestABlankNameDoesNotOpenABlock is F66-L3-2: a name of whitespace is no name.
// Every arm decides whether an entry introduces a call with namesItself —
// a name that is not blank — so both the block-open guard and the truncated-turn
// opener in finishStream must ask the same question. Reading " " as present
// opened a block whose content_block_start carries a name no client can route,
// beside the call that did name itself and stated the SAME id: one answer
// became two tool_use blocks sharing an id.
func TestABlankNameDoesNotOpenABlock(t *testing.T) {
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"index":0,"id":"a","type":"function","function":{"name":" ","arguments":"{}"}},`+
			`{"index":1,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`)
	if len(wantIDs) != 1 || wantIDs[0] != "a" || len(wantParts) != 1 || wantParts[0] != `{"cmd":"ls"}` {
		t.Fatalf("premise: the one-list arm must answer the named call alone, got ids=%v parts=%v text=%q", wantIDs, wantParts, wantText)
	}
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":" ","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
	)
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			t.Errorf("two tool_use blocks share id %q, which no client accumulator can route: ids=%v parts=%v (2026-09-28 audit, round 66, F66-L3-2)", id, ids, parts)
		}
		seen[id] = true
	}
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a blank name is not a name, so it opens no block (2026-09-28 audit, round 66, F66-L3-2)",
		`{"index":0,"id":"a","function":{"name":" "}} ++ {"index":1,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`)
}

// TestAnIdRestatedWithoutAnIndexContinuesTheCall is F66-L3-3: the vendor that
// numbers one call's fragments states the id again on a fragment that carries no
// index. The client leg's startsANewToolCall splits on a fresh id only once the
// accumulator HAS one, so an id equal to the accumulator's is a continuation
// there, and the block that stated its own id must take the same fragment here.
// Read as a new call it opened a second block under an id already spent and split
// the object across the two: the client accumulated `{"cmd` for the call and got
// `:"ls"}` relayed to the agent as prose, under a stop_reason of tool_use.
func TestAnIdRestatedWithoutAnIndexContinuesTheCall(t *testing.T) {
	wantIDs, wantParts, wantText := r66DocCalls(t,
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`)
	if len(wantIDs) != 1 || wantIDs[0] != "a" || len(wantParts) != 1 || wantParts[0] != `{"cmd":"ls"}` {
		t.Fatalf("premise: the one-list arm must answer one complete call, got ids=%v parts=%v text=%q", wantIDs, wantParts, wantText)
	}
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"a","function":{"arguments":":\"ls\"}"}}]}}]}`,
	)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, wantText,
		"a fragment restating the call's own id with nothing to name is a continuation of it (2026-09-28 audit, round 66, F66-L3-3)",
		`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\""}} then an index-less {"id":"a","function":{"arguments":":\"ls\"}"}}`)
}
