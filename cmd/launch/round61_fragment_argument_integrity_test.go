package launch

// round61_fragment_argument_integrity_test.go — round 61's findings on the
// client proxy's fragment arm (F61-L2-1, F61-L2-2, F61-L2-3, F61-L2-5).
//
// Round 60 taught this accumulator to split a call the upstream states a slot
// for a second time, and the gate it used asked whether BOTH argument texts
// were already complete objects. The ordinary wire does not spell it that way:
// a vendor streams a call's arguments in pieces, so the second call's object
// arrived as `{"b":` and `2}` and neither piece was complete — the pieces were
// appended to the first call's finished object and the client accumulated
// `{"a":1}{"b":2}`, JSON no tool can parse, under a stop_reason of tool_use,
// while the model's second call did not exist on this arm at all.
//
// The same reading is what decides the three shapes below:
//
//   - F61-L2-1: a second call at one index whose object arrives CHUNKED is the
//     next call (the bytes cannot be more of the first call's finished object),
//     exactly as this leg's whole-list arm and the gateway leg read it.
//   - F61-L2-2: a vendor that writes ONE index for every call of the turn
//     introduces its calls in a single delta and then streams their arguments,
//     one fragment per call, in the order the calls were introduced. The index
//     names the newest call, so the first call's arguments landed in the second
//     call's block: the client ran a Read whose input was Bash's `{"cmd":"ls"}`
//     — a well-formed call with another call's input, which signals nothing —
//     and the second call's own arguments were dropped as they arrived.
//   - F61-L2-3: the name a fragment states is a call's identity only at an
//     index this stream has already stated. A fresh index is the wire's own
//     "a call begins here", so a name-bearing fragment stating one is a call of
//     its own and not a continuation of a same-named call open elsewhere.
//   - F61-L2-5: a complete object after a FREE-FORM line is not more of that
//     line. Freeform is the model's whole command, delivered once (round 51's
//     G1) — appending an object to it handed the client
//     `{"_raw":"echo hi{\"c\":3}"}`, a call the model never wrote, where the
//     whole-list arm answers the line alone.
//
// Every case asks the same body of both arms of this leg — the fragments the
// upstream streamed and the whole completion the same upstream writes — and
// requires the same calls with the same ids, names and inputs. Each is
// fail-first: RED against the tree before this round's fix.

import (
	"encoding/json"
	"testing"
)

// TestTheChunkedSecondObjectAtOneIndexIsTheNextCallOnThisLeg is F61-L2-1.
func TestTheChunkedSecondObjectAtOneIndexIsTheNextCallOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}`),
		r59L2Frame(`{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"b\":"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"2}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"b\":2}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r59L2SameAsWhole(t, frag, whole)
}

// TestTheCallsIntroducedInOneDeltaGetTheirOwnArgumentsOnThisLeg is F61-L2-2.
func TestTheCallsIntroducedInOneDeltaGetTheirOwnArgumentsOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":""}},{"index":0,"id":"b","function":{"name":"Read","arguments":""}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"{\"p\":1}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},`+
		`{"index":0,"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r59L2SameAsWhole(t, frag, whole)
}

// TestANameAtAFreshIndexIsItsOwnCallOnThisLeg is F61-L2-3. The name a fragment
// states is a call's identity only at an index this stream has already stated,
// so the second Read — a fresh index, the same tool, its own object half-written
// — is a call of its own and not more of the first Read's open object.
func TestANameAtAFreshIndexIsItsOwnCallOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"Read","arguments":"{\"p\":"}}`),
		r59L2Frame(`{"index":1,"function":{"name":"Read","arguments":"{\"q\":"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"1}"}}`),
		r59L2Frame(`{"index":1,"function":{"arguments":"1}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}},`+
		`{"index":1,"type":"function","function":{"name":"Read","arguments":"{\"q\":1}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r59L2SameAsWhole(t, frag, whole)
}

// TestAnObjectAfterAFreeformLineIsNotMoreOfItOnThisLeg is F61-L2-5.
func TestAnObjectAfterAFreeformLineIsNotMoreOfItOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"echo hi"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"{\"c\":3}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"echo hi"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r59L2SameAsWhole(t, frag, whole)
	if len(frag) != 1 {
		b, _ := json.Marshal(frag)
		t.Fatalf("the model's one command reached the client as %s, want the call alone", b)
	}
}

// The controls: the pinned wires this round's change must not disturb. Freeform
// arrives once and whole (round 51's G1), a call the upstream never named is
// relayed as text rather than appended to a call (round 39's B-F8), and the
// index-not-updated continuation still reaches the open call beside the
// finished slot (round 59's F59-L2-1).
func TestThePinnedFragmentWiresStillAnswerOnThisLeg(t *testing.T) {
	t.Run("a chunked freeform line stays one call", func(t *testing.T) {
		frag, whole := r59L2Arms(t, []string{
			r59L2Frame(`{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"echo hel"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":"lo world"}}`),
			r59L2Fin, `data: [DONE]`,
		}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"id":"call_1","function":{"name":"Bash","arguments":"echo hello world"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
		r59L2SameAsWhole(t, frag, whole)
	})

	t.Run("the continuation at an earlier index is the open call", func(t *testing.T) {
		frag, whole := r59L2Arms(t, []string{
			r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`),
			r59L2Frame(`{"index":1,"id":"b","function":{"name":"Read"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":"{\"p\":1}"}}`),
			r59L2Fin, `data: [DONE]`,
		}, r59L2WholeTwoCalls)
		r59L2SameAsWhole(t, frag, whole)
	})

	t.Run("the whole call listed again at its own slot stays one call", func(t *testing.T) {
		frag, whole := r59L2Arms(t, []string{
			r59L2Frame(`{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"index":0,"id":"call_1"}`),
			r59L2Frame(`{"index":0,"function":{"name":"Bash"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":"{\"a\":1}"}}`),
			r59L2Fin, `data: [DONE]`,
		}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
		r59L2SameAsWhole(t, frag, whole)
	})
}
