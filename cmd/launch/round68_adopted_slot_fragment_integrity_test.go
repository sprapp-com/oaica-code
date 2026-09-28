package launch

// round68_adopted_slot_fragment_integrity_test.go — leg 2's adopted-slot
// occupant, pinned.
//
// When an upstream answers a `stream:true` request with a WHOLE completion
// inside one `data:` frame, this leg adopts it (adoptNonSSECompletion) and
// writes the calls it carried. Any tool delta that arrives AFTER that frame is
// then read against a slot the adoption already filled, and the question is
// what "already filled" means: a bare bool ("this slot is spoken for") can only
// drop the fragment, while the rule the rest of the converter follows asks the
// slot what call it holds and treats a fragment naming a DIFFERENT call as a
// call of the model's own — which must open its own block.
//
// Round 68's F68-L2-1 is the difference: the same two calls reached the client
// as two blocks when the upstream stated them inside the frame it adopted, and
// as ONE when it stated the second a delta later. A tool the model asked for
// never reached the client, and which spelling the vendor chose decided it.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// round68Block is one tool_use block the client received, with the argument
// bytes it accumulated across the deltas of that block.
type round68Block struct {
	ID   string
	Name string
	Args string
}

// round68Spellings runs the streaming arm over one SSE script and returns the
// tool_use blocks the client received, in order.
func round68Spellings(t *testing.T, frames []string) []round68Block {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	script := strings.Join(frames, "\n\n") + "\n\n"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, script)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}

	var out []round68Block
	var cur *round68Block
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Name  string `json:"name"`
				Input any    `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				// The block's `input` at its start is the placeholder `{}`; the
				// call's arguments are the input_json_delta bytes that follow it,
				// which is what a client accumulates. Reading the placeholder as
				// the call's input would make every block look identical.
				out = append(out, round68Block{ID: ev.ContentBlock.ID, Name: ev.ContentBlock.Name})
				cur = &out[len(out)-1]
				_ = ev.ContentBlock.Input
			} else {
				cur = nil
			}
		case "content_block_delta":
			if cur != nil && ev.Delta.Type == "input_json_delta" {
				cur.Args += ev.Delta.PartialJSON
			}
		}
	}
	return out
}

const (
	round68CallA = `{"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}`
	round68CallB = `{"id":"call_b","type":"function","function":{"name":"Read","arguments":"{\"f\":2}"}}`
)

// round68Delta spells one delta-chunk tool-call entry. The index goes INSIDE
// the call object: written alongside it the frame is not JSON, and the
// tool_calls field is dropped by the parser with no error — which is how a
// first version of this probe appeared to reproduce nothing.
func round68Delta(index int, call string) string {
	return `{"index":` + strconv.Itoa(index) + `,` + strings.TrimPrefix(call, "{")
}

// round68WholeFrame is a whole completion carrying the given calls, inside one
// data frame — the shape adoptNonSSECompletion takes.
func round68WholeFrame(calls string) string {
	return `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"",` +
		`"tool_calls":[` + calls + `]},"finish_reason":"tool_calls"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":3}}`
}

// TestRound68AdoptedSlotFragmentAgreesWithInFrameSpelling is the F68-L2-1 pin.
// ONE turn, TWO spellings the upstream may choose between, the same two calls
// either way: which frame carries the second call is the vendor's business and
// must not be the client's.
func TestRound68AdoptedSlotFragmentAgreesWithInFrameSpelling(t *testing.T) {
	bothInTheFrame := round68Spellings(t, []string{
		round68WholeFrame(round68CallA + `,` + round68CallB),
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	})
	secondAsFragment := round68Spellings(t, []string{
		round68WholeFrame(round68CallA),
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[` +
			round68Delta(0, round68CallB) + `]}}]}`,
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	})

	// The premise: the in-frame spelling really does relay both calls. With that
	// broken the comparison below would pass for the wrong reason.
	want := []round68Block{
		{ID: "call_a", Name: "Bash", Args: `{"a":1}`},
		{ID: "call_b", Name: "Read", Args: `{"f":2}`},
	}
	if len(bothInTheFrame) != 2 {
		t.Fatalf("PREMISE: one frame carrying both calls relayed %d blocks (%+v), want 2 — the comparison is meaningless without it",
			len(bothInTheFrame), bothInTheFrame)
	}
	if bothInTheFrame[0] != want[0] || bothInTheFrame[1] != want[1] {
		t.Fatalf("PREMISE: the in-frame spelling relayed %+v, want %+v", bothInTheFrame, want)
	}
	if len(secondAsFragment) != len(bothInTheFrame) {
		t.Errorf("the same two calls reach the client as %d blocks when the upstream states both inside the frame this leg adopted (%+v) and as %d when it states the second one delta later (%+v): the model asked for a tool the client never sees, and which spelling the vendor chose decided it (2026-09-28 audit, round 68, F68-L2-1)",
			len(bothInTheFrame), bothInTheFrame, len(secondAsFragment), secondAsFragment)
		return
	}
	for i := range want {
		if secondAsFragment[i] != want[i] {
			t.Errorf("block %d differs between the two spellings of one turn: delta-later %+v, in-frame %+v (2026-09-28 audit, round 68, F68-L2-1)",
				i, secondAsFragment[i], want[i])
		}
	}
}

// TestRound68RestatingTheAdoptedCallStaysOneCall is the control in the other
// direction: the fragment a vendor sends for a call the adopted frame ALREADY
// wrote is not a second call. Widening the occupant rule so that a different
// call can open a block must not also split a call from itself, which is how
// round 56's restatement got its own block the last time this comparison was
// loosened.
func TestRound68RestatingTheAdoptedCallStaysOneCall(t *testing.T) {
	got := round68Spellings(t, []string{
		round68WholeFrame(round68CallA),
		// The same call, same id, same name, its arguments restated.
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[` +
			round68Delta(0, round68CallA) + `]}}]}`,
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	})
	if len(got) != 1 {
		t.Fatalf("a call the adopted frame already wrote, restated by a later delta, became %d blocks (%+v): the client is asked to run the same tool twice (2026-09-28 audit, round 68, F68-L2-1's control)",
			len(got), got)
	}
	if got[0].ID != "call_a" || got[0].Name != "Bash" {
		t.Errorf("the restated call was not the adopted one: %+v", got[0])
	}
}
