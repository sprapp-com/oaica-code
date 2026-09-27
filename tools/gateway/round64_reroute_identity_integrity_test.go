package main

// round64_reroute_identity_integrity_test.go — round 64's two findings on the
// gateway bridge's id-reroute (F64-L3-2, F64-L3-3).
//
// Round 63's F63-L3-3 taught the index arm to follow a stated id to the call
// that already carries it, so a vendor that numbers its fragments ITSELF — one
// upstream index per chunk, id restated beside the bytes — finishes the call it
// introduced at another index. The reroute, as it landed, asked only whether the
// carried call's id was STATED and its arguments unfinished. Two things follow
// from that, both of them the same shape as the findings around them: a fragment
// the reroute must not take, and a fragment it takes but does not record.
//
//   - F64-L3-2: a fragment that states a DIFFERENT NAME is not that call's
//     continuation. An argument continuation carries arguments alone, so a
//     second name introduces a second call whatever id it states (round 40's
//     A40-8, and the client leg splits on exactly that after routing by id). The
//     reroute took it anyway when the first call was still mid-object, and the
//     second call's whole object was appended to the first's unfinished
//     arguments: the client held ONE call whose input was `{"a":{"b":2}1}` —
//     JSON no tool parses — under a stop_reason of tool_use, and the model's
//     second call did not exist.
//   - F64-L3-3: every other branch of the index arm records the index it was
//     reached by (indexKeys/indexChain). The reroute returned without recording,
//     so the vendor's next chunk — which for that wire states neither id nor
//     name — found no block at its index and opened a fresh one: the call
//     reached the client unterminated under stop_reason tool_use, and the byte
//     that finished it was relayed to the client as assistant prose.
//
// Each case asks the SAME conversation of both of this bridge's arms — the
// fragments the upstream streamed, and the whole completion that upstream would
// write — and requires the same calls with the same ids and the same inputs.
// Fail-first: RED against the tree before this round's fix.

import (
	"testing"
)

// TestASecondNameMidObjectIsTheNextCall is F64-L3-2. The first call is written
// mid-object; the second is whole on the wire, at another index, under an id the
// first already holds and a name of its own.
func TestASecondNameMidObjectIsTheNextCall(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"a\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Glob","arguments":"{\"b\":2}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
	)
	// The document arm of the same body: the same two calls, the same id stated
	// twice (the second is minted on both arms), the same two inputs.
	wantIDs, wantParts, _ := r60DocArm(t,
		`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"index":1,"id":"a","type":"function","function":{"name":"Glob","arguments":"{\"b\":2}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, "", "the second call is introduced while the first is mid-object: its own name says it is a call of its own, and its object must not be appended to the first's unfinished arguments (2026-09-28 audit, round 64, F64-L3-2)", "")
	if len(parts) > 0 && parts[0] != `{"a":1}` {
		t.Errorf("the first call's arguments are %s, want the object the wire finished writing for it", parts[0])
	}
}

// TestTheIndexThatFedTheCallFinishesIt is F64-L3-3. The vendor states the call's
// id and name once, then numbers its own fragments: the chunk that closes the
// argument object states the index alone.
func TestTheIndexThatFedTheCallFinishesIt(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Bash","arguments":"\"ls\""}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"function":{"arguments":"}"}}]}}]}`,
	)
	wantIDs, wantParts, _ := r60DocArm(t,
		`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, "", "the chunk that closes the argument object states only the index the reroute was reached by, and that index has to name the call it fed (2026-09-28 audit, round 64, F64-L3-3)", "")
}

// The control: the wire round 63 pinned — the id restated beside the bytes at a
// second index, the same name throughout — is still one call, and the ordinary
// OpenAI order (an id and a name on the introducing chunk, arguments at the
// index it opened) is still one call of its own.
func TestTheRerouteStillFinishesTheCallItWasPinnedFor(t *testing.T) {
	ids, parts, text := r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"a","function":{"name":"Bash","arguments":"\"ls\""}}]}}]}`,
	)
	wantIDs, wantParts, _ := r60DocArm(t,
		`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\""}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, "", "the vendor that restates the id beside its own numbered fragment is one call, which is the wire round 63 closed", "")

	ids, parts, text = r60FrameArm(t,
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
	)
	wantIDs, wantParts, _ = r60DocArm(t,
		`{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r60ArmSame(t, ids, parts, text, wantIDs, wantParts, "", "the ordinary chunk order is one call", "")
}
