package launch

// round63_arrival_order_integrity_test.go — round 63's finding on the client
// proxy's flush order (F63-L2-1).
//
// This accumulator keys fragments by a slot that IS the upstream's own index
// whenever the fragment states one, and it handed the accumulated calls to the
// client in ascending slot order. Sorting by the index is sorting by the
// position the vendor declares — which agrees with the order the fragments
// arrived only while the vendor's indices ascend with its stream. An upstream
// whose fragment order and stated positions disagree (the call it states index 1
// for written before the call it states index 0 for) was answered in an order
// none of the other three arms use: this leg's whole-list arm reads the
// document's array and answers [a, b], and the gateway leg keeps the wire's own
// order on both of its arms, where this arm answered [b, a].
//
// The order is not cosmetic. Claude Code runs the calls the model asked for, and
// a turn whose second call is executed first is a different turn — the same
// class of divergence rounds 58-63 closed for the calls' CONTENTS.
//
// Fail-first: RED against the tree before this round's fix.

import (
	"testing"
)

// TestTheCallsReachTheClientInTheOrderTheStreamWroteThem is F63-L2-1: the call
// the stream introduces first, at a HIGHER index than the call that follows it.
func TestTheCallsReachTheClientInTheOrderTheStreamWroteThem(t *testing.T) {
	whole := `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[` +
		`{"index":1,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}},` +
		`{"index":0,"id":"b","type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`
	frag, wh := r59L2Arms(t, []string{
		r59L2Frame(`{"index":1,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`),
		r59L2Frame(`{"index":0,"id":"b","function":{"name":"Read","arguments":"{\"p\":1}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, whole)
	// PREMISE: the document arm is the comparison, so the case is only
	// meaningful while IT answers the order the fragments arrived in. A wire
	// whose whole-completion arm reordered too would pass a comparison against
	// itself.
	if len(wh) != 2 || wh[0]["id"] != "a" || wh[1]["id"] != "b" {
		t.Fatalf("PREMISE: the whole-list arm answers %v, want the call the document lists first (a) then (b)", wh)
	}
	r59L2SameAsWhole(t, frag, wh)
	if len(frag) == 2 && frag[0]["id"] != "a" {
		t.Errorf("the call the stream introduced FIRST was handed to the client second: the calls were ordered by the index each fragment stated, and this upstream's stated positions disagree with the order it wrote them — its own whole-completion document and the gateway leg both answer [a, b] (2026-09-28 audit, round 63, F63-L2-1)")
	}
}

// The control: the ordinary wire, where the indices ascend with the stream, is
// untouched — the arrivals and the indices agree, so neither order has anything
// to say against the other.
func TestTheOrdinaryIndexedWireIsUnchangedByTheArrivalOrder(t *testing.T) {
	frag, wh := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`),
		r59L2Frame(`{"index":1,"id":"b","function":{"name":"Read","arguments":"{\"p\":1}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, r59L2WholeTwoCalls)
	r59L2SameAsWhole(t, frag, wh)
}
