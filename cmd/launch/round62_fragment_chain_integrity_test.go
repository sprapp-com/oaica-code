package launch

// round62_fragment_chain_integrity_test.go — round 62's finding on the client
// proxy's fragment arm (F62-L2-1) and the pin for its nested-object reading
// (F62-L2-2).
//
// A vendor that writes ONE index for every call of the turn leaves that index
// naming the call it wrote there last — the NEWEST. The argument fragments it
// streams afterwards state no id and no name, so the only thing that decides
// which call they belong to is which calls the index has named and which of them
// is still waiting for its arguments. Round 61's chain answers that for the
// newest call still EMPTY; the vendor feeds its calls in the order it introduced
// them, so by the time the FIRST call's arguments arrive the newest one may
// already be finished — a call whose argument object arrived whole on its own
// fragment — and then the chain did not fire: the fragment was matched to the
// slot the index names, refused as bytes a finished object cannot take, and
// dropped, so the first call reached the client with an EMPTY input while the
// whole-list arm of the same body answered it with its arguments.
//
// F62-L2-2 is two cases: the pin and the fix. canExtend reads `{"b":` ++
// `{"c":3}` as a nested value in progress, so a whole object arriving onto one
// the model is still writing is more of that call rather than a second one —
// and the whole-list arm of the same upstream holds the fragments of one call as
// the pieces of that call's argument string, so the two agree exactly when the
// document holds their concatenation (`{"b":{"c":3}2}`), which the first case
// asserts (green on both readings: this leg appends a fragment's bytes whatever
// the predicate says, and canExtend only decides whether a new call is SPLIT off
// — which is what the second case pins, the whole object restated under the
// call's own id and name). At that site a nested value and a second call are the
// same bytes; the identity the fragment states is what tells them apart.
//
// Each case asks the same body of both arms of this leg, and each of F62-L2-1
// and the restated nested value is fail-first: RED against the tree before this
// round's fix.

import (
	"testing"
)

// TestTheFragmentContinuesTheOpenCallTheIndexNamedOnThisLeg is F62-L2-1: one
// index for the turn, the newest call finished, an earlier call still waiting.
func TestTheFragmentContinuesTheOpenCallTheIndexNamedOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":""}}`),
		r59L2Frame(`{"index":0,"id":"b","function":{"name":"Read","arguments":"{\"p\":1}"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"{\"cmd\":\"ls\"}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},`+
		`{"index":0,"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r59L2SameAsWhole(t, frag, whole)
	if len(frag) == 1 && frag[0]["input"] != nil {
		if in, _ := frag[0]["input"].(map[string]any); len(in) == 0 {
			t.Errorf("the first call this index named reached the client with an EMPTY input while the newest call it named was already finished: the fragment was matched to the slot the index names and dropped as bytes a finished object cannot take (2026-09-28 audit, round 62, F62-L2-1)")
		}
	}
}

// TestTheFragmentsOfOneCallAreThePiecesOfItsArgumentStringOnThisLeg is
// F62-L2-2, the pin: a fragment that is itself a whole object extends an object
// the model is still writing, and the arm that holds the whole list agrees
// exactly when the document holds the concatenation.
func TestTheFragmentsOfOneCallAreThePiecesOfItsArgumentStringOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"b\":"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"{\"c\":3}"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"2}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"b\":{\"c\":3}2}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r59L2SameAsWhole(t, frag, whole)

	t.Run("the nested value restated under the call's own identity is still one call", func(t *testing.T) {
		nested, nestedWhole := r59L2Arms(t, []string{
			r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"query\":"}}`),
			r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"sql\":\"select 1\"}"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":",\"limit\":10}"}}`),
			r59L2Fin, `data: [DONE]`,
		}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"query\":{\"sql\":\"select 1\"},\"limit\":10}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
		r59L2SameAsWhole(t, nested, nestedWhole)
		if len(nested) != 1 {
			t.Fatalf("the model's one call came out as %d call(s) on the fragment arm, want one", len(nested))
		}
		if in, _ := nested[0]["input"].(map[string]any); in["limit"] == nil || in["query"] == nil {
			t.Errorf("the call reached the client with input %v, want the nested object the model wrote: the whole object restated under the call's own id and name was read as a second call and the call left behind held its unfinished prefix (2026-09-28 audit, round 62, F62-L2-2)", nested[0]["input"])
		}
	})
}
