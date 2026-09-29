package main

// round104_leg3_same_name_and_choices_test.go — leg 3, round 104 (2026-09-29
// audit), F104-L3-1 and F104-L3-2.
//
// F104-L3-1 is a regression from round 103's own clause. That clause reads an
// id-less named fragment whose name equals the open block's, over a MID-OBJECT
// call, as more of that call — and asked nothing of the slot. Two id-less `Read`
// calls at DIFFERENT indexes, the first cut off at `{"a":`, folded into one on the
// framed and adopted arms (`{"_raw":"{\"a\":{\"b\":2}"}`) where the plain document
// lists two: the client owed two tool_results and was told of one, and the second
// call the model wrote never ran. The clause is now asked only of a fragment, and
// only when the slot it states is one the open call was written at.
//
// F104-L3-2: the framed arm skipped a later choice by its POSITION inside a
// chunk, so a vendor streaming each choice in a chunk of its own had every choice
// relayed — "hiyo" against the documents' "hi", and a second choice's call handed
// to the client under a tool_use verdict the documents never reach. Round 103's
// F103-L2-1 fixed the same reading on the client proxy; it is now the choice the
// turn's first element names here too.

import (
	"strings"
	"testing"
)

// TestMine104ASecondSameNameCallAtAnotherIndexIsNotFolded is F104-L3-1's pin.
func TestMine104ASecondSameNameCallAtAnotherIndexIsNotFolded(t *testing.T) {
	doc := round98Doc("tool_calls", `"hi"`, `{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"a\":"}},{"index":1,"type":"function","function":{"name":"Read","arguments":"{\"b\":2}"}}`)
	blocks, _ := r100RunArms(t, doc, []string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		round98Frame(`{"index":0,"function":{"name":"Read","arguments":"{\"a\":"}}`),
		round98Frame(`{"index":1,"function":{"name":"Read","arguments":"{\"b\":2}"}}`),
		round98Tail("tool_calls"),
	})
	want := strings.Join(blocks[0], " ")
	if strings.Count(want, "call:") != 2 {
		t.Fatalf("premise: the plain document lists %q, want two calls", want)
	}
	for i, arm := range [3]string{"plain", "adopted", "framed"} {
		if got := strings.Join(blocks[i], " "); got != want {
			t.Errorf("the %s arm reads %q, want what the plain document reads (%q) — a second call of the same name at ANOTHER slot is a call of its own, not more of the first (2026-09-29 audit, round 104, F104-L3-1)", arm, got, want)
		}
	}
}

// TestMine104AChoiceInAChunkOfItsOwnIsNotRelayed is F104-L3-2's pin.
func TestMine104AChoiceInAChunkOfItsOwnIsNotRelayed(t *testing.T) {
	doc := `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"},{"index":1,"message":{"role":"assistant","content":"yo","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Read","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
	blocks, bodies := r100RunArms(t, doc, []string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"index":1,"delta":{"content":"yo"}}]}`,
		`data: {"choices":[{"index":1,"delta":{"tool_calls":[{"index":0,"id":"c1","function":{"name":"Read","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`,
	})
	want := strings.Join(blocks[0], " ")
	if want != "text:hi" {
		t.Fatalf("premise: the plain document reads %q, want the first choice's text alone", want)
	}
	if got := strings.Join(blocks[2], " "); got != want || strings.Contains(bodies[2], `"stop_reason":"tool_use"`) {
		t.Errorf("the framed arm reads %q, want %q with the first choice's verdict — the turn's choice is the one its first element names, not the element that opens its own chunk (2026-09-29 audit, round 104, F104-L3-2)", got, want)
	}
}

// TestMine104AListStatingTwoSameNameCallsAtOneIndexKeepsBoth is the control for
// the adopted arm: a whole list STATES its calls (round 82's F82-L3-1), so a
// second entry of the same name at the SAME index is a second call there. It
// holds without an adopted-arm exclusion in the clause (measured), which is why
// none ships.
func TestMine104AListStatingTwoSameNameCallsAtOneIndexKeepsBoth(t *testing.T) {
	doc := round98Doc("tool_calls", `"hi"`, `{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"a\":"}},{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"b\":2}"}}`)
	blocks, _ := r100RunArms(t, doc, nil)
	plain, adopted := strings.Join(blocks[0], " "), strings.Join(blocks[1], " ")
	if plain != adopted {
		t.Errorf("the adopted arm reads %q, want what the plain document reads (%q) — a whole list states its calls (2026-09-29 audit, round 104, F104-L3-1)", adopted, plain)
	}
}
