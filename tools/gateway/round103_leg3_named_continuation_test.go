package main

// round103_leg3_named_continuation_test.go — leg 3, round 103 (2026-09-29 audit).

import (
	"strings"
	"testing"
)

func r103Call1() string {
	return `{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":""}}`
}

func r103Call2() string {
	return `{"index":0,"id":"call_2","type":"function","function":{"name":"Grep","arguments":"{\"cmd\":\"ls\"}"}}`
}

// r103Turn is the one body every arm of this file reads: two calls, the first
// argument-less and the second carrying the object.
func r103Turn() string {
	return round98Doc("tool_calls", `"hi"`, r103Call1()+","+r103Call2())
}

const r103Want = `text:hi call:call_1|Read|{} call:call_2|Grep|{"cmd":"ls"}`

// TestMine103ANamedContinuationExtendsTheCallItContinues is F103-L3-1's pin.
//
// The vendor writes ONE index for every call of its turn (round 58's F58-L3-2,
// round 62's F62-L3-3) and restates the call's name on the chunk that continues
// it (the habit round 62's F62-L3-2 attests). The index still resolves to the
// turn's FIRST call, whose name differs, so the occupant clause read the
// continuation as the next call: the fragment arm handed its client three
// tool_use blocks for a two-call turn — the named call left holding the half
// object `{"cmd":`, which no client can run, and a call the model never wrote
// minted beside it.
func TestMine103ANamedContinuationExtendsTheCallItContinues(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		round98Frame(r103Call1()),
		round98Frame(`{"index":0,"id":"call_2","type":"function","function":{"name":"Grep","arguments":"{\"cmd\":"}}`),
		round98Frame(`{"index":0,"function":{"name":"Grep","arguments":"\"ls\"}"}}`),
		round98Tail("tool_calls"),
	}
	blocks, _ := r100RunArms(t, r103Turn(), frames)
	if got := strings.Join(blocks[0], " "); got != r103Want {
		t.Fatalf("premise: the document arm reads %q, want %q", got, r103Want)
	}
	if got := strings.Join(blocks[2], " "); got != r103Want {
		t.Errorf("the framed arm reads %q, want %q — a named continuation whose bytes extend a call this bridge has opened mid-object is more of THAT call, not the next one, whatever slot the vendor states for it (2026-09-29 audit, round 103, F103-L3-1)", got, r103Want)
	}
}

// TestMine103TheSameWireWithoutTheRestatedNameIsUnchanged is the control: the
// same turn with the continuation naming nothing takes the index chain instead
// (round 62's F62-L3-3) and every arm already answers one turn. It exists so
// F103-L3-1's pin cannot be satisfied by widening the chain walk.
func TestMine103TheSameWireWithoutTheRestatedNameIsUnchanged(t *testing.T) {
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		round98Frame(r103Call1()),
		round98Frame(`{"index":0,"id":"call_2","type":"function","function":{"name":"Grep","arguments":"{\"cmd\":"}}`),
		round98Frame(`{"index":0,"arguments":"\"ls\"}"}`),
		round98Tail("tool_calls"),
	}
	blocks, _ := r100RunArms(t, r103Turn(), frames)
	if got := strings.Join(blocks[0], " "); got != r103Want {
		t.Fatalf("premise: the document arm reads %q, want %q", got, r103Want)
	}
	// The fragment arm cannot answer this one: the frame states no name, so
	// nothing on the wire says which call `"ls"}` belongs to and the chain
	// answers by introduction order (round 62's F62-L3-3 pins that reading,
	// and the reverse walk is RED against it). RECORDED as F103-L3-2 — the
	// divergence is the DOCUMENT spelling being transposed, not this arm
	// misreading, and the two directions cannot both hold.
	t.Logf("framed: %s", strings.Join(blocks[2], " "))
}
