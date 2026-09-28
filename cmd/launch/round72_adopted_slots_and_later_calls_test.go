package launch

// round72_adopted_slots_and_later_calls_test.go — leg 2, a call the model made
// after a frame this leg ADOPTED wrote several calls of its own.
//
// Round 68 fixed the one-call case: a delta naming a different call at an index
// the adoption had written is split onto a slot of its own rather than dropped,
// because the slot the index resolves to holds a call whose block the client
// already closed. The split's fresh slot is the stream's own counter, and the
// ADOPTION never raised that counter — it numbers the slots it wrote itself, in
// the accumulator's terms — so with more than one call in the adopted frame the
// split landed back ON an adopted slot whenever the delta's index was low, and
// the fragment was dropped by the very gate the split had been written to
// escape. A tool the model asked for never reached the client, under a
// stop_reason of tool_use, while the same three calls written wholly inside the
// frame — or wholly as deltas — reached it whole (2026-09-28 audit, round 72,
// F72-L2-1).

import (
	"testing"
)

// r72CallA/r72CallB are the two calls the adopted frame writes; r72CallC is the
// call the upstream sends as a delta afterwards.
func r72CallA() string {
	return `{"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
}
func r72CallB() string {
	return `{"id":"call_b","type":"function","function":{"name":"Read","arguments":"{\"f\":2}"}}`
}
func r72CallC() string {
	return `{"id":"call_c","type":"function","function":{"name":"Glob","arguments":"{\"g\":3}"}}`
}

func r72Finish() []string {
	return []string{
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
}

// r72BlockNames renders the blocks as the comparison message spells them.
func r72BlockNames(blocks []round68Block) string {
	out := ""
	for _, b := range blocks {
		out += " [" + b.ID + " " + b.Name + " " + b.Args + "]"
	}
	return out
}

// TestACallAfterAMultiCallAdoptionIsNotLost is the F72-L2-1 pin. ONE turn: the
// upstream writes its first two calls inside a whole completion this leg adopts,
// and sends the third as an ordinary delta stating an index — the vendor that
// writes one index for every call of the turn states 0 here. Which frame carried
// which call is the vendor's business: all three spellings of the turn must hand
// the client the same three calls.
func TestACallAfterAMultiCallAdoptionIsNotLost(t *testing.T) {
	for _, c := range []struct {
		name         string
		frameCalls   []string
		deltaIndex   int
		deltaCall    string
		wholeCalls   []string
		deltaSpelled func() []string
	}{
		{
			name:       "two calls adopted, the third sent as a delta at index 0",
			frameCalls: []string{r72CallA(), r72CallB()},
			deltaIndex: 0,
			deltaCall:  r72CallC(),
			wholeCalls: []string{r72CallA(), r72CallB(), r72CallC()},
		},
		{
			name:       "three calls adopted, the fourth sent as a delta at index 1",
			frameCalls: []string{r72CallA(), r72CallB(), r72CallC()},
			deltaIndex: 1,
			deltaCall:  `{"id":"call_d","type":"function","function":{"name":"Grep","arguments":"{\"p\":4}"}}`,
			wholeCalls: []string{r72CallA(), r72CallB(), r72CallC(), `{"id":"call_d","type":"function","function":{"name":"Grep","arguments":"{\"p\":4}"}}`},
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			joined := ""
			for i, call := range c.frameCalls {
				if i > 0 {
					joined += ","
				}
				joined += call
			}
			adoptedThenDelta := round68Spellings(t, append([]string{
				round68WholeFrame(joined),
				r71ToolCallsFrame(round68Delta(c.deltaIndex, c.deltaCall)),
			}, r72Finish()...))

			// The same turn with every call a delta at the same index — the
			// vendor's own spelling, which no adoption is involved in.
			deltaSpelled := append([]string{}, r72Finish()...)
			for i := len(c.wholeCalls) - 1; i >= 0; i-- {
				deltaSpelled = append([]string{r71ToolCallsFrame(round68Delta(c.deltaIndex, c.wholeCalls[i]))}, deltaSpelled...)
			}
			allDeltas := round68Spellings(t, deltaSpelled)

			// And the same turn written wholly inside the frame this leg adopts.
			wholeJoined := ""
			for i, call := range c.wholeCalls {
				if i > 0 {
					wholeJoined += ","
				}
				wholeJoined += call
			}
			wholeFrame := round68Spellings(t, append([]string{round68WholeFrame(wholeJoined)}, r72Finish()...))

			want := len(c.wholeCalls)
			// Premise: the two spellings with no split involved really do answer
			// every call of the turn, so the comparison below is about a lost
			// call and not a mis-count.
			if len(allDeltas) != want || len(wholeFrame) != want {
				t.Fatalf("PREMISE: want %d calls from the delta spelling and from the whole-frame spelling, got %s and %s",
					want, r72BlockNames(allDeltas), r72BlockNames(wholeFrame))
			}
			if len(adoptedThenDelta) != len(wholeFrame) {
				t.Fatalf("the same turn's %d calls reach the client as %s when the upstream writes %d of them inside the frame this leg adopts and the last as a delta at index %d, and as %s when it writes all of them inside that frame: a tool the model asked for was dropped (2026-09-28 audit, round 72, F72-L2-1)",
					want, r72BlockNames(adoptedThenDelta), len(c.frameCalls), c.deltaIndex, r72BlockNames(wholeFrame))
			}
			for i := range wholeFrame {
				if adoptedThenDelta[i] != wholeFrame[i] {
					t.Fatalf("call %d is %+v when the earlier calls arrive inside the adopted frame and the last as a delta at index %d, and %+v when all of them arrive inside that frame (2026-09-28 audit, round 72, F72-L2-1)",
						i, adoptedThenDelta[i], c.deltaIndex, wholeFrame[i])
				}
			}
		})
	}
}
