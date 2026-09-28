package launch

// round73_fresh_index_restated_call_integrity_test.go — leg 2's fragment arm,
// and the call a fragment states its own identity for at a FRESH index
// (2026-09-28 audit, round 73, F73-L2-2).
//
// A fragment that states an id this stream has already opened belongs to that
// call whatever index it carries (round 60's F60-L2-2), because the vendor that
// does not update its index on a continuation writes the call's own id at a
// stale one. That route is asked "at any index" — and the fresh index is this
// leg's own signal for a NEW call (round 61's F61-L2-3, which the name route
// already honours). A fragment that states its own id AND its own name AND a
// complete object of its own at an index this stream has never written is not a
// continuation of anything: the call it names has received none of its
// arguments, so there is nothing for it to be more of. Read as a continuation,
// the second call's whole object was grafted onto the argument-less call that
// stated the same id, and the second call did not exist — one Bash carrying the
// other call's input, which signals nothing to the client.
//
// Every other arm answers two calls for this wire: this leg's non-stream list,
// both of leg 1's arms, and the gateway leg's document and frame arms.

import (
	"testing"
)

// r73FragWire spells F73-L2-2's two calls as fragments, one frame each.
func r73FragWire(secondIndex int) []string {
	return []string{
		r59L2Frame(`{"index":0,"id":"call_c","function":{"name":"Bash","arguments":""}}`),
		r59L2Frame(`{"index":` + itoa73(secondIndex) + `,"id":"call_c","function":{"name":"Bash","arguments":"{\"a\": 1}"}}`),
		r59L2Fin, `data: [DONE]`,
	}
}

func itoa73(n int) string {
	if n == 0 {
		return "0"
	}
	return "1"
}

// r73ArglessHolderWhole is the same two calls as one whole completion.
const r73ArglessHolderWhole = `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,` +
	`"message":{"role":"assistant","content":"","tool_calls":[` +
	`{"id":"call_c","function":{"name":"Bash","arguments":""}},` +
	`{"id":"call_c","function":{"name":"Bash","arguments":"{\"a\": 1}"}}]},` +
	`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`

// TestACallRestatedAtAFreshIndexIsItsOwnCall is the F73-L2-2 pin.
func TestACallRestatedAtAFreshIndexIsItsOwnCall(t *testing.T) {
	const note = "two calls: an argument-less one stating an id, and a second stating the same id, the same name and a complete object of its own at a FRESH index — a slot the stream had never written"
	// What the other arms answer for this wire: leg 1's document translator and
	// its streaming arm, the gateway leg's document arm and its frame arm.
	const want = 2

	frag, whole := r59L2Arms(t, r73FragWire(1), r73ArglessHolderWhole)
	if len(whole) != want {
		t.Errorf("this leg's whole-list arm answers %d call(s) for %s, want %d — the same body reaches leg 1's document translator and the gateway's document arm as %d (2026-09-28 audit, round 73, F73-L2-2)\nwhole: %v",
			len(whole), note, want, want, whole)
	}
	if len(frag) != want {
		t.Errorf("this leg's fragment arm answers %d call(s) for %s, want %d: the fragment that stated its own id, name and object at a fresh index was read as a continuation of the argument-less call holding that id, so the client runs ONE tool carrying the other call's input and the second call's identity is gone (2026-09-28 audit, round 73, F73-L2-2)\nfragments: %v",
			len(frag), note, want, frag)
	}
	if len(frag) == want && len(whole) == want {
		r59L2SameAsWhole(t, frag, whole)
	}

	// The same two entries in ONE delta carry the same two calls: the index the
	// second entry states is fresh there too.
	frag, whole = r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"call_c","function":{"name":"Bash","arguments":""}},` +
			`{"index":1,"id":"call_c","function":{"name":"Bash","arguments":"{\"a\": 1}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, r73ArglessHolderWhole)
	if len(frag) != want || len(whole) != want {
		t.Errorf("the same two calls in ONE delta are %d call(s) as fragments and %d whole, want %d each (2026-09-28 audit, round 73, F73-L2-2)\nfragments: %v\nwhole:     %v",
			len(frag), len(whole), want, frag, whole)
	}
}
