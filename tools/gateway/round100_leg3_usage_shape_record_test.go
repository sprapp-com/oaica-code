package main

// round100_leg3_usage_shape_record_test.go — leg 3, round 100 (2026-09-29 audit),
// F100-L3-2. RECORDED, not changed.
//
// The ledger row is built from the `usage` object an upstream states, and its
// token counts are read as plain ints (`main.go`'s usage struct). A count spelled
// as a STRING, or as a float, is refused by that decode, and the refusal lands on
// the whole body: the two document arms answer 502 "unparseable upstream
// response" — a client that asked for `stream:false` is refused a turn the
// streaming spelling of the same body serves WHOLE:
//
//	"usage":{"prompt_tokens":"11","completion_tokens":4}
//	  plain    502 {"error":{"message":"unparseable upstream response",...}}
//	  adopted  502 same
//	  framed   200 text "hi", call:call_1|Read|{"cmd":"ls"}, usage input_tokens 8
//
// Round 90's F90-L3-2 doctrine — one upstream body states one cause, whichever
// spelling the client asked for — is broken in the direction that round did not
// measure (there it was the fragment arm that refused and the document arms that
// served). The same measurement holds for `"prompt_tokens":11.0`. A `usage` object
// that states only fields the typed struct does not carry — `"cached_tokens":"7"`,
// measured — is served on every arm, which is what makes this the integer-typed
// fields' reading rather than the whole object's.
//
// Recorded rather than changed: no producer of this leg's fixtures states a
// non-integer count. The OpenAI wire's usage fields are integers — this tree's
// own writer states them as ints (`openai`'s `Usage` struct and every upstream
// the tree has a fixture for), and a foreign server that stringifies or floats
// its counts would have to exist for this reading to reach a client. That is a
// rank-c spelling by the round's own report. What a later round should know before
// "fixing" it: the tolerance belongs in the usage decode alone and must not widen
// to the counts the meter is TOLD (the ledger's own numbers are ints), so a
// tolerant read has to state what it costs — the same counts, parsed — rather than
// a second estimate.

import (
	"strings"
	"testing"
)

func TestAUsageCountThatIsNotAnIntegerRefusesTheWholeBody(t *testing.T) {
	entry := `{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"cmd\":\"ls\"}"}}`
	base := round98Doc("tool_calls", `"hi"`, entry)
	for _, c := range []struct {
		name  string
		spell string
	}{
		{"a count spelled as a string", `"prompt_tokens":"11"`},
		{"a count spelled as a float", `"prompt_tokens":11.0`},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := strings.Replace(base, `"prompt_tokens":11`, c.spell, 1)
			frames := []string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				round98Frame(`{"index":0,"id":"call_1","function":{"name":"Read","arguments":"{\"cmd\":\"ls\"}"}}`),
				`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
				`data: {"choices":[],"usage":{` + c.spell + `,"completion_tokens":4}}`,
			}
			arms, bodies := r100RunArms(t, doc, frames)

			// The fragment arm serves the turn whole — the reading the two
			// document arms refuse the same body for.
			if got := strings.Join(arms[2], " "); got != `text:hi call:call_1|Read|{"cmd":"ls"}` {
				t.Errorf("the framed arm reads %q, want the whole turn — this pin states the split, so it moves if the split moves (2026-09-29 audit, round 100, F100-L3-2)", got)
			}
			// The document arms refuse it: 502, no blocks. Both are the reading
			// this record states, and round 90's doctrine is the reason a later
			// round would change it.
			for i, name := range [2]string{"plain", "adopted"} {
				if len(arms[i]) != 0 {
					t.Errorf("the %s arm now reads %v — if the document arms have been taught to serve a non-integer count, this record is spent and the fix it describes has been made (2026-09-29 audit, round 100, F100-L3-2)", name, arms[i])
				}
			}
			for i, name := range [2]string{"plain", "adopted"} {
				if !strings.Contains(bodies[i], "unparseable upstream response") {
					t.Errorf("the %s arm answered %q, want the 502 this record states (2026-09-29 audit, round 100, F100-L3-2)", name, strings.TrimSpace(bodies[i]))
				}
			}
		})
	}
}
