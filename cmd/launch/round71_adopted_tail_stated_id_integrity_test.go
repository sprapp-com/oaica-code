package launch

// round71_adopted_tail_stated_id_integrity_test.go — leg 2, the ids stated by a
// fragment the ADOPTED TAIL drops.
//
// Round 70 established that a minted id must never land on an id the turn
// already stated, and enforced it where the flush drops an entry: the nameless
// fragment (whose arguments relay as text) and the truncated fragment the
// truncation gate refuses are collected into droppedStatedIDs and reserved on
// the converter before the flush returns.
//
// One drop site was not covered: a fragment arriving AFTER a whole completion
// this leg adopted, which the adopted-tail block drops outright — before any
// accumulator exists, so the flush never sees it and the id it stated is never
// reserved. A later id-less call in the same turn whose mint happens to be that
// string then took it, and the same turn reached the client under a different
// id depending on whether the upstream wrote its first call inside the adopted
// frame or as a delta (2026-09-28 audit, round 71, F71-L2-1).

import (
	"testing"

	"github.com/ollama/ollama/anthropic"
)

// r71TailMint is the id this leg mints for an id-less "Read" carrying {"a":1}.
// The wire states exactly this string on the fragment the adopted tail drops,
// which is what makes the collision reachable: the turn states an id a mint
// would otherwise produce.
var r71TailMint = anthropic.ToolCallIDFor("Read", `{"a":1}`)

// r71CallA is the call the adopted frame carries: its block is closed on the
// client's side, so the fragment that follows it cannot be delivered.
func r71CallA() string {
	return `{"id":"call_a","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`
}

// r71NamelessEntry states r71TailMint on an entry the upstream never named, and
// states no index for it either: an entry with neither a name nor an index
// cannot be told from a continuation of the calls the adopted frame wrote, so
// the adopted-tail block drops it outright — before any accumulator exists, and
// therefore before the flush that reserves a dropped entry's id.
func r71NamelessEntry() string {
	return `{"type":"function","id":"` + r71TailMint + `","function":{"name":"","arguments":""}}`
}

// r71IdlessRead is the call that collides: it states no id of its own, so the
// arm mints one, and the mint is r71TailMint.
func r71IdlessRead() string {
	return `{"index":5,"type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}`
}

func r71ToolCallsFrame(calls string) string {
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[` +
		calls + `]}}]}`
}

// TestRound71DroppedTailFragmentStatedIDIsNotMintedOver is the F71-L2-1 pin.
// ONE turn, TWO spellings: the upstream writes the Bash call inside a whole
// completion this leg adopts, or as an ordinary delta. Which frame carried it
// is the vendor's business, so both spellings must hand the client the same
// ids — and neither may hand the id-less Read the id the wire stated for an
// entry the client never sees.
func TestRound71DroppedTailFragmentStatedIDIsNotMintedOver(t *testing.T) {
	adopted := round68Spellings(t, []string{
		round68WholeFrame(r71CallA()),
		r71ToolCallsFrame(r71NamelessEntry()),
		r71ToolCallsFrame(r71IdlessRead()),
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	})
	deltaOnly := round68Spellings(t, []string{
		r71ToolCallsFrame(round68Delta(0, r71CallA())),
		r71ToolCallsFrame(r71NamelessEntry()),
		r71ToolCallsFrame(r71IdlessRead()),
		`data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	})

	// Premise: the adopted spelling really does relay both calls (the adopted
	// one, then the model's own). Without it the id comparison is meaningless.
	if len(adopted) != 2 || len(deltaOnly) != 2 {
		t.Fatalf("PREMISE: want two tool_use blocks per arm, got adopted=%+v delta=%+v", adopted, deltaOnly)
	}
	if adopted[0].ID != "call_a" || adopted[1].Name != "Read" {
		t.Fatalf("PREMISE: adopted spelling relayed %+v, want the adopted Bash call then the model's Read", adopted)
	}

	for _, arm := range []struct {
		name  string
		got   []round68Block
		spell string
	}{{"adopted", adopted, "inside the frame this leg adopted"}, {"delta", deltaOnly, "as a delta"}} {
		if arm.got[1].ID == r71TailMint {
			t.Errorf("the %s arm (Bash call written %s) handed the client %s for its id-less Read — the id the wire stated for an entry the client never sees: a minted id landed on a stated one",
				arm.name, arm.spell, arm.got[1].ID)
		}
	}
	if adopted[1].ID != deltaOnly[1].ID {
		t.Errorf("the same turn reaches the client under %s when the Bash call is written inside the adopted frame and under %s when it is written as a delta: which frame carried it decided the client's ids",
			adopted[1].ID, deltaOnly[1].ID)
	}
}
