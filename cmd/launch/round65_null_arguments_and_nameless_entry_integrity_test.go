package launch

// round65_null_arguments_and_nameless_entry_integrity_test.go — round 65's two
// findings on the client proxy.
//
//	A CALL WITH NO ARGUMENTS IS THE SAME CALL WHICHEVER ARM ANSWERS IT. The
//	OpenAI wire spells "this call takes none" as the literal `null` as often as
//	it spells it `{}`, and every leg's client-facing answer folds it: the
//	gateway's whole-list arm (callInput, round 48's C-F5), this proxy's
//	whole-list arm (ToMessagesResponse, round 45's A45-4) and the local
//	converter all hand the client `{}`. The streaming arm did not fold: the
//	literal `null` went out as the block's input_json_delta, so the client's
//	accumulated input was `null` — not the object the wire defines, and not what
//	the same upstream answer produced when it arrived as one whole completion,
//	so a tool_use the agent should have run with no arguments depended on the
//	framing the upstream chose (2026-09-28 audit, round 65).
//
//	A NAMELESS ENTRY'S STATED ID IS NOT ITS OWN. An entry that names no call is
//	not a call on any arm: the gateway relays its arguments as prose and the
//	local converter does the same. Its id, though, was claimed by this leg's
//	parser, so the call that DID name itself and stated the same id looked like
//	a second call under an id already taken and reached the client minted — the
//	upstream's own id replaced by a hash of the call, which a tool_result
//	written against it cannot answer (2026-09-28 audit, round 65).
//
// Both are fail-first: RED against the tree before this round's fix.

import "testing"

// TestACallWithNullArgumentsIsTheSameCallOnBothArms is the first finding: one
// upstream answer, spelled `null` for the call's arguments.
func TestACallWithNullArgumentsIsTheSameCallOnBothArms(t *testing.T) {
	whole := `{"id":"c","object":"chat.completion","model":"glm-5.3",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[` +
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"null"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`
	frag, wh := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"null"}}`),
		r59L2Fin, `data: [DONE]`,
	}, whole)

	// The whole-list arm is the reference the wire defines, and the premise: an
	// input that is not an object would make the comparison below vacuous.
	if len(wh) != 1 {
		t.Fatalf("PREMISE: the whole-list arm answers %v, want one call", wh)
	}
	if in, ok := wh[0]["input"].(map[string]any); !ok || len(in) != 0 {
		t.Fatalf("PREMISE: the whole-list arm's input is %#v, want the empty object", wh[0]["input"])
	}
	r59L2SameAsWhole(t, frag, wh)
	if len(frag) == 1 {
		if _, ok := frag[0]["input"].(map[string]any); !ok {
			t.Errorf("a call whose arguments the upstream spelled `null` reached the client with input %#v on the streaming arm and with the empty object on the whole-list arm: the literal is the wire's spelling of \"no arguments\", and every other arm of every leg answers it with `{}` (2026-09-28 audit, round 65)", frag[0]["input"])
		}
	}
}

// TestANamelessEntryDoesNotTakeTheIDItStates is the second finding: the entry
// that names nothing states the id the NEXT call states for itself.
func TestANamelessEntryDoesNotTakeTheIDItStates(t *testing.T) {
	whole := `{"id":"c","object":"chat.completion","model":"glm-5.3",` +
		`"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[` +
		`{"index":0,"id":"a","type":"function","function":{"name":"","arguments":"{}"}},` +
		`{"index":1,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`
	frag, wh := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"","arguments":"{}"}}`),
		r59L2Frame(`{"index":1,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, whole)

	if len(wh) != 1 || wh[0]["id"] != "a" {
		t.Fatalf("PREMISE: the whole-list arm answers %v, want the call that names itself under the id the upstream stated (a)", wh)
	}
	r59L2SameAsWhole(t, frag, wh)
	if len(frag) == 1 && frag[0]["id"] != "a" {
		t.Errorf("the call that names itself reached the client as %v: the id belongs to it, not to the entry that named nothing, and the local leg and the gateway both keep the stated id (2026-09-28 audit, round 65)", frag[0]["id"])
	}
}
