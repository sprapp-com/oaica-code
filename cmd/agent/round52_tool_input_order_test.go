package agent

// round52_tool_input_order_test.go — round 52's finding on the agent shim.
//
// A tool_use block's accumulated input_json_delta text was decoded into a plain
// map and written into the call's arguments by ranging that map, whose
// iteration order Go randomizes. The agent loop writes the call straight back
// into the next request body, so one upstream stream became several different
// argument orders across runs for the same conversation — and every prefix
// cache keyed on those prompt bytes was recomputed (2026-09-27 audit, round
// 52).

import (
	"encoding/json"
	"testing"
)

func TestAccumulatedToolInputRendersInOneOrder(t *testing.T) {
	// The order the upstream wrote, which is the order both other legs render:
	// this decode has the stream's own bytes, so it keeps them rather than
	// normalizing them.
	const want = `{"zeta":1,"alpha":2,"mu":3,"beta":4}`
	feed := func(a *anthropicSSEAccumulator, t *testing.T, i int, ev, data string) []string {
		t.Helper()
		deltas, _, err := a.Feed(ev, []byte(data))
		if err != nil {
			t.Fatalf("run %d: Feed(%s): %v", i, ev, err)
		}
		out := make([]string, 0, len(deltas))
		for _, d := range deltas {
			for _, tc := range d.Message.ToolCalls {
				b, err := json.Marshal(tc.Function.Arguments)
				if err != nil {
					t.Fatalf("run %d: marshal arguments: %v", i, err)
				}
				out = append(out, string(b))
			}
		}
		return out
	}

	seen := map[string]bool{}
	for i := 0; i < 40; i++ {
		a := newAnthropicSSEAccumulator()
		feed(a, t, i, "content_block_start",
			`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"Write","input":{}}}`)
		feed(a, t, i, "content_block_delta",
			`{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"zeta\":1,\"alpha\":2,\"mu\":3,\"beta\":4}"}}`)
		got := feed(a, t, i, "content_block_stop", `{"type":"content_block_stop","index":0}`)

		if len(got) != 1 {
			t.Fatalf("run %d: the closed block emitted %d calls, want 1", i, len(got))
		}
		if got[0] != want {
			t.Fatalf("run %d: the accumulated input renders as %s, want %s\nthe call goes straight back into the next request body, so a map's iteration order re-renders one upstream stream as different prompt bytes and every prefix cache keyed on them is recomputed",
				i, got[0], want)
		}
		seen[got[0]] = true
	}
	if len(seen) != 1 {
		t.Errorf("one stream rendered %d different argument JSONs", len(seen))
	}
}
