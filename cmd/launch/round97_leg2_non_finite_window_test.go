package launch

// round97_leg2_non_finite_window_test.go — leg 2, round 97 (2026-09-29 audit),
// F97-L2-4.
//
// `remoteLen` strips a JSON string's quotes and hands what is left to
// `strconv.ParseFloat` — which accepts "NaN", "Inf", "Infinity" and "+Inf"
// without error — and then converts to int. Measured, on a list naming the
// route's own model:
//
//	"NaN" (named entry, and its sibling too)  -> -9223372036854775808
//	9e18                                      -> 9000000000000000000
//
// where the probe is documented "0 if unknown". Round 96's comment on this
// type says "Garbage still fails the decode, which is this probe's fail-closed
// answer" — and the garbage it had in mind does, but these spellings do not: a
// non-finite float converts to int64's minimum (amd64), which every consumer
// gated on `> 0` reads as unknown only by accident, and an out-of-range length
// survives every such filter as a REAL window, taking the context-fit clamp
// ceiling with it. Both are lengths no model has, so both fail the decode now.
//
// The controls keep the round-96 readings: a spelled length is still the number
// it says, a negative or absent one is still unknown, and a sibling's retype
// still does not void the named entry.

import "testing"

func TestALengthNoModelHasIsNoWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{"NaN as a string, named entry and its sibling", `{"data":[{"id":"m","context_length":"NaN","max_model_len":"NaN"}]}`, 0},
		{"NaN as a bare number", `{"data":[{"id":"m","context_length":NaN}]}`, 0},
		{"Infinity as a string", `{"data":[{"id":"m","context_length":"Infinity"}]}`, 0},
		{"+Inf as a string", `{"data":[{"id":"m","context_length":"+Inf"}]}`, 0},
		{"a length past int32", `{"data":[{"id":"m","context_length":9e18}]}`, 0},
		{"a length past int64", `{"data":[{"id":"m","context_length":1e19}]}`, 0},
		{"a negative length", `{"data":[{"id":"m","context_length":-5}]}`, 0},
		{"control: a length the wire spelled as a string", `{"data":[{"id":"m","context_length":"262144"}]}`, 262144},
		{"control: a vLLM length", `{"data":[{"id":"m","max_model_len":262144}]}`, 262144},
		{"control: a large but real window", `{"data":[{"id":"m","context_length":10000000}]}`, 10000000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelsJSONUpstream(t, []byte(tc.body))
			if got := defaultRemoteContextWindow(proxyRoute{BaseURL: srv.URL, UpstreamModel: "m"}); got != tc.want {
				t.Errorf("window = %d, want %d (2026-09-29 audit, round 97, F97-L2-4):\n  %s", got, tc.want, tc.body)
			}
		})
	}
}
