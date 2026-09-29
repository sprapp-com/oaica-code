package launch

// round101_leg2_indexless_waiting_call_test.go — leg 2, round 101
// (2026-09-29 audit), F101-L2-1.
//
// An index-less upstream states its calls with nothing but sequence, and this
// leg's fragment arm resolved an argument-only fragment to the call the stream
// had written to LAST. That is right for a token stream, which writes name1,
// args1, name2, args2 — but not for a server that parses a whole tool-call array
// and re-emits it, the shape Go's `json:"index,omitempty"` produces for an
// index-0 call: every call's header arrives in one delta and the arguments
// follow, one fragment each, in the order the calls were introduced.
//
//	{"id":"c1","function":{"name":"Bash"}},{"id":"c2","function":{"name":"Read"}}
//	{"function":{"arguments":"{\"a\":1}"}}
//	{"function":{"arguments":"{\"f\":2}"}}
//
// The first fragment was written into the second call, so the client ran
// `Bash {}` — a tool the model never asked to run empty — and `Read {"a":1}`
// wearing Bash's arguments, while the second call's own bytes arrived as TEXT:
// this leg's whole-list arm answers the same body `Bash {"a":1}` then
// `Read {"f":2}`, and so does every other arm of the turn. An argument-only
// fragment now takes the EARLIEST call this stream has named and not yet fed —
// the answer the indexed path has given through indexChain since round 61 — and
// every fragment no waiting call can take is still decided by the rules below
// (round 76's prose fold, round 58's last-named continuation).

import (
	"encoding/json"
	"testing"
)

// r101WholeTwoCalls is the whole-list spelling of the turn every case below
// streams: Bash{"a":1} then Read{"f":2}, stated with the ids the fragments state.
const r101WholeTwoCalls = `{"id":"c","object":"chat.completion","model":"glm-5.3",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[` +
	`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},` +
	`{"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"f\":2}"}}]},` +
	`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`

// r101SameTurn is the whole-list comparison with the blocks named, so a failure
// says which call was misread rather than only that the lists differ.
func r101SameTurn(t *testing.T, frames []string) {
	t.Helper()
	frag, whole := r59L2Arms(t, frames, r101WholeTwoCalls)
	if len(frag) != len(whole) {
		t.Fatalf("the same turn is %d call(s) as fragments and %d whole:\nfragments: %v\nwhole:     %v", len(frag), len(whole), frag, whole)
	}
	for i := range whole {
		fj, _ := json.Marshal(frag[i])
		wj, _ := json.Marshal(whole[i])
		if string(fj) != string(wj) {
			t.Errorf("call %d is %s as fragments and %s whole — an argument-only fragment with no index is the earliest call still waiting for its arguments, not the call the stream wrote to last (2026-09-29 audit, round 101, F101-L2-1)", i, fj, wj)
		}
	}
}

// TestMine101AnIndexlessArgumentFragmentIsTheEarliestWaitingCall is F101-L2-1,
// spelled the three ways such an upstream writes it.
func TestMine101AnIndexlessArgumentFragmentIsTheEarliestWaitingCall(t *testing.T) {
	t.Run("both headers in one delta", func(t *testing.T) {
		r101SameTurn(t, []string{
			r59L2Frame(`{"id":"c1","type":"function","function":{"name":"Bash"}},{"id":"c2","type":"function","function":{"name":"Read"}}`),
			r59L2Frame(`{"function":{"arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"function":{"arguments":"{\"f\":2}"}}`),
			r59L2Fin, `data: [DONE]`,
		})
	})
	t.Run("each header in its own delta", func(t *testing.T) {
		r101SameTurn(t, []string{
			r59L2Frame(`{"id":"c1","type":"function","function":{"name":"Bash"}}`),
			r59L2Frame(`{"id":"c2","type":"function","function":{"name":"Read"}}`),
			r59L2Frame(`{"function":{"arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"function":{"arguments":"{\"f\":2}"}}`),
			r59L2Fin, `data: [DONE]`,
		})
	})
	t.Run("the first call's index omitted as Go's omitempty writes it", func(t *testing.T) {
		r101SameTurn(t, []string{
			r59L2Frame(`{"id":"c1","type":"function","function":{"name":"Bash"}},{"index":1,"id":"c2","type":"function","function":{"name":"Read"}}`),
			r59L2Frame(`{"function":{"arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"index":1,"function":{"arguments":"{\"f\":2}"}}`),
			r59L2Fin, `data: [DONE]`,
		})
	})
}

// TestMine101TheIndexlessRulesThisFixDidNotMove pins the two readings the new
// rule sits in front of: the token stream, where each call is named and fed in
// turn (the reading round 58's last-named rule was written for), and the
// index-less call whose arguments arrive with its own header.
func TestMine101TheIndexlessRulesThisFixDidNotMove(t *testing.T) {
	t.Run("a token stream names each call and feeds it in turn", func(t *testing.T) {
		r101SameTurn(t, []string{
			r59L2Frame(`{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"id":"c2","type":"function","function":{"name":"Read","arguments":"{\"f\":2}"}}`),
			r59L2Fin, `data: [DONE]`,
		})
	})
	t.Run("the introducer carries the index and the continuation does not", func(t *testing.T) {
		r101SameTurn(t, []string{
			r59L2Frame(`{"index":0,"id":"c1","type":"function","function":{"name":"Bash"}}`),
			r59L2Frame(`{"index":1,"id":"c2","type":"function","function":{"name":"Read"}}`),
			r59L2Frame(`{"function":{"arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"function":{"arguments":"{\"f\":2}"}}`),
			r59L2Fin, `data: [DONE]`,
		})
	})
}
