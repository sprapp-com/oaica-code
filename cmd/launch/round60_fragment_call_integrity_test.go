package launch

// round60_fragment_call_integrity_test.go — round 60's findings on the client
// proxy's fragment arm (F60-L2-1, F60-L2-2, and the two cross-leg wires
// F60-X-1 / F60-X-2).
//
// Round 59 taught this accumulator to re-point an index at the call it splits
// off, and to route a continuation at a finished slot to the call the stream
// last wrote to. Neither rule reached the two shapes below, and both are the
// same question: what a fragment belongs to when the index and the fragment's
// own words disagree with what the slot already holds.
//
//   - F60-L2-1: the index arm splits a call the upstream states this slot for a
//     second time on a stated id or a stated name alone, so two calls of ONE
//     tool at one index — the vendor that writes index 0 for every call, which
//     round 36's B-F2 is about — were one call again: the whole-list arm answers
//     that body with two calls, and the fragment arm handed the client a single
//     Read whose input was `{"p":"a.go"}{"p":"b.go"}`, JSON no tool can parse,
//     under a stop_reason of tool_use.
//   - F60-L2-2: a continuation that states the id (or the name) of a call the
//     stream left OPEN at another index was routed by the index the vendor did
//     not update, so its arguments landed in the block that index held and the
//     open call reached the client with an empty input — a Read run with no
//     arguments while the model's arguments were delivered as a second, nameless
//     block (the name-spelled wire) or dropped whole (the id-spelled one).
//   - F60-X-1 / F60-X-2: an argument-only fragment at a slot whose call is
//     finished, with no open call left to route it to, was appended to that call
//     anyway — `{"b":2}{"c":3}`, which is not JSON — while this leg's own
//     whole-list arm and the gateway leg both DROP it. The two arms of one leg
//     answered one body differently, and the client could not run the call it
//     was handed.
//
// Every case asks the same body of both arms of this leg — the fragments the
// upstream streamed and the whole completion the same upstream writes — and
// requires the same calls with the same ids, names and inputs. Each is
// fail-first: RED against the tree before this round's fix.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r60L2Calls is the whole-list spelling of the two-call turn F60-L2-1 streams:
// two Reads, one index each, no ids — the vendor's own answer to the same model
// turn.
const r60L2TwoReadsWhole = `{"id":"c","object":"chat.completion","model":"glm-5.3",` +
	`"choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[` +
	`{"function":{"name":"Read","arguments":"{\"p\":\"a.go\"}"}},` +
	`{"function":{"name":"Read","arguments":"{\"p\":\"b.go\"}"}}]},` +
	`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`

// TestTwoCallsOfOneToolAtOneIndexStayTwoOnThisLeg is F60-L2-1. A vendor that
// states one index for every call of the turn states it for two calls of the
// same tool as well, and the second call's own complete object is not more of
// the first's.
func TestTwoCallsOfOneToolAtOneIndexStayTwoOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"function":{"name":"Read","arguments":"{\"p\":\"a.go\"}"}}`),
		r59L2Frame(`{"index":0,"function":{"name":"Read","arguments":"{\"p\":\"b.go\"}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, r60L2TwoReadsWhole)
	r59L2SameAsWhole(t, frag, whole)

	// The same wire with NO index anywhere is the shape round 38 pinned, and it
	// must stay two calls: the split above is the index arm's, not a new rule for
	// fragments that carry no slot at all.
	frag, whole = r59L2Arms(t, []string{
		r59L2Frame(`{"function":{"name":"Read","arguments":"{\"p\":\"a.go\"}"}}`),
		r59L2Frame(`{"function":{"name":"Read","arguments":"{\"p\":\"b.go\"}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, r60L2TwoReadsWhole)
	r59L2SameAsWhole(t, frag, whole)
}

// TestTheContinuationStatedAtAnotherIndexStaysTheOpenCallOnThisLeg is F60-L2-2,
// spelled both ways a vendor writes a call's own name.
func TestTheContinuationStatedAtAnotherIndexStaysTheOpenCallOnThisLeg(t *testing.T) {
	whole := r59L2WholeTwoCalls
	for _, tc := range []struct {
		name  string
		third string
	}{
		{"the continuation states the id", r59L2Frame(`{"index":0,"id":"b","function":{"arguments":"{\"p\":1}"}}`)},
		{"the continuation states the name", r59L2Frame(`{"index":0,"function":{"name":"Read","arguments":"{\"p\":1}"}}`)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.name == "the continuation states the name" {
				// REVERSED by round 79 (F79-L2-A), for the same reason as the
				// round-59 row of this wire: a fragment that names the open call
				// again BESIDE arguments, over a call that has accumulated none,
				// begins the NEXT call (round 78's F78-L3-2 — an empty argument
				// list is a complete one). The reference is this leg's own
				// whole-list arm for the same entries, which answers three calls
				// here; the hand-written two-call document this row compared
				// against agreed only while the fragment arm folded them.
				// Measured 2026-09-28.
				entries := []string{
					`{"index":0,"id":"a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`,
					`{"index":1,"id":"b","type":"function","function":{"name":"Read"}}`,
					`{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"p\":1}"}}`,
				}
				frag, wholeArm, _, _, _, _ := r75Arms(t, entries)
				r59L2SameAsWhole(t, frag, wholeArm)
				return
			}
			frag, wholeBlocks := r59L2Arms(t, []string{
				r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`),
				r59L2Frame(`{"index":1,"id":"b","function":{"name":"Read"}}`),
				tc.third,
				r59L2Fin, `data: [DONE]`,
			}, whole)
			r59L2SameAsWhole(t, frag, wholeBlocks)
		})
	}

	// The control: the index the vendor DID update — the ordinary interleaved
	// wire — is untouched by the routing above.
	frag, wholeBlocks := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":"}}`),
		r59L2Frame(`{"index":1,"id":"b","function":{"name":"Read","arguments":"{\"p\":"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"\"ls\"}"}}`),
		r59L2Frame(`{"index":1,"function":{"arguments":"1}"}}`),
		r59L2Fin, `data: [DONE]`,
	}, whole)
	r59L2SameAsWhole(t, frag, wholeBlocks)
}

// TestAnArgumentFragmentAfterAFinishedCallIsDroppedOnThisLeg is F60-X-1 and
// F60-X-2: an argument-only fragment at a slot whose call is finished, with no
// open call beside it to carry the arguments. Dropping it is what this leg's own
// whole-list arm does with the same body, and what the gateway leg does with the
// same bytes.
func TestAnArgumentFragmentAfterAFinishedCallIsDroppedOnThisLeg(t *testing.T) {
	// F60-X-1: two calls at one index, the second's slot finished when the
	// argument-only fragment arrives.
	t.Run("two calls at one index", func(t *testing.T) {
		frag, whole := r59L2Arms(t, []string{
			r59L2Frame(`{"index":0,"function":{"name":"Bash","arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"index":0,"function":{"name":"Read","arguments":"{\"b\":2}"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":"{\"c\":3}"}}`),
			r59L2Fin, `data: [DONE]`,
		}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"function":{"name":"Read","arguments":"{\"b\":2}"}},`+
			`{"function":{"arguments":"{\"c\":3}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
		r59L2SameAsWhole(t, frag, whole)
	})

	// F60-X-2: two calls at two indexes, both finished, and the fragment states
	// the index of the one that was written first.
	t.Run("the index written first", func(t *testing.T) {
		frag, whole := r59L2Arms(t, []string{
			r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"a\":1}"}}`),
			r59L2Frame(`{"index":1,"id":"b","function":{"name":"Read","arguments":"{\"b\":2}"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":"{\"c\":3}"}}`),
			r59L2Fin, `data: [DONE]`,
		}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
			`{"id":"a","function":{"name":"Bash","arguments":"{\"a\":1}"}},`+
			`{"id":"b","function":{"name":"Read","arguments":"{\"b\":2}"}},`+
			`{"function":{"arguments":"{\"c\":3}"}}]},`+
			`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
		r59L2SameAsWhole(t, frag, whole)
	})

	// The control round 59 pinned: the same fragment while the call it belongs to
	// is still OPEN is that call's own arguments, on this leg and on every other.
	t.Run("the open call beside it", func(t *testing.T) {
		frag, whole := r59L2Arms(t, []string{
			r59L2Frame(`{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`),
			r59L2Frame(`{"index":1,"id":"b","function":{"name":"Read"}}`),
			r59L2Frame(`{"index":0,"function":{"arguments":"{\"p\":1}"}}`),
			r59L2Fin, `data: [DONE]`,
		}, r59L2WholeTwoCalls)
		r59L2SameAsWhole(t, frag, whole)
	})
}

// TestAFreeformContinuationStaysOneCallOnThisLeg is the control for every
// finished-list test above: freeform is NOT a finished argument list just
// because it is not the beginning of an object. The model's whole command is
// the call's input, delivered once (round 51's G1 on the gateway leg), and a
// fragment that follows it is more of that same line — not a second call
// carrying half a command, and not bytes with nowhere left to go.
func TestAFreeformContinuationStaysOneCallOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"echo hel"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"lo world"}}`),
		r59L2Fin, `data: [DONE]`,
	}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"id":"call_1","function":{"name":"Bash","arguments":"echo hello world"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r59L2SameAsWhole(t, frag, whole)
	if len(frag) != 1 {
		b, _ := json.Marshal(frag)
		t.Fatalf("the model's one command reached the client as %s, want one call", b)
	}
	args, _ := json.Marshal(frag[0])
	if !strings.Contains(string(args), "echo hello world") {
		t.Errorf("the freeform command reached the client as %s, want the model's whole line", args)
	}
}

// TestTheLateIdFragmentNamesTheCallOnThisLeg pins what this leg answers when a
// call's id arrives after its arguments: the client is handed the id the
// upstream stated, and the whole-list arm of the same leg agrees. It is the
// control for the routing above — a fragment stating an id is never read as a
// second call when the call it names is the one open at its slot.
func TestTheLateIdFragmentNamesTheCallOnThisLeg(t *testing.T) {
	frag, whole := r59L2Arms(t, []string{
		r59L2Frame(`{"index":0,"function":{"name":"Bash"}}`),
		r59L2Frame(`{"index":0,"function":{"arguments":"{\"a\":1}"}}`),
		r59L2Frame(`{"index":0,"id":"z"}`),
		r59L2Fin, `data: [DONE]`,
	}, `{"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[`+
		`{"id":"z","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},`+
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":10}}`)
	r59L2SameAsWhole(t, frag, whole)
	if len(frag) != 1 || frag[0]["id"] != "z" {
		b, _ := json.Marshal(frag)
		t.Errorf("the call a late id names reached the client as %s, want one call under the stated id z", b)
	}
}
