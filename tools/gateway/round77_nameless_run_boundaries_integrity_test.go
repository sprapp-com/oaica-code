package main

// round77_nameless_run_boundaries_integrity_test.go — leg 3, where one
// contiguous run of entries the upstream never named begins and ends
// (2026-09-28 audit, round 77, F77-L3-1 and F77-L3-2).
//
// Round 76 settled that such an entry's bytes are prose relayed as TEXT and
// that the relay is one text block per CONTIGUOUS run. Its fix routed those
// bytes by asking whether the block they were about to be written into could
// EXTEND them — callArgsExtend, the CALL's own predicate — and a run of prose
// is not a call's argument list: two whole objects the wire sent as two
// nameless entries were split into two blocks on the frame arm where both
// document arms answered one, and the fragment that split off opened a block
// relayed last, so the bytes after it went back into the run's first block and
// the client read the model's prose in an order the model never wrote
// (AAA + {"c":3} + BBB reached it as AAABBB then {"c":3}).
//
// Two things follow, and both are now stated once:
//
//  1. An argument predicate belongs to a CALL. A nameless block is prose: it
//     takes any bytes, which is what the write does and what the wrapper now
//     lets it do.
//  2. A run ends where a CALL is named, and nowhere else. Round 76's wrapper
//     also let a nameless fragment rejoin an earlier run across a call the wire
//     named in between, because the routing key it was handed was that run's
//     block — one run reached the client as its two halves joined where both
//     document arms wrote two. The bridge now remembers which nameless block is
//     currently taking bytes (`namelessRunKey`) and a block that names itself
//     clears it, which is the same span the document arms group into one text
//     block.
//
// Every row asks the SAME body of all three arms — the document adopted inside
// a streaming request, the same document answered to a non-streaming request,
// and the fragments the upstream streams — and asserts they agree, block for
// block.

// Recorded, not fixed (same round): F77-L3-3, a nameless fragment whose bytes
// CAN extend a call the wire already named is folded into that call's arguments
// on the frame arm, so `[Bash "echo hi", nameless "zzz"]` reaches a streaming
// client as one call with `{"_raw":"echo hizzz"}` where both document arms
// answer the call as the model wrote it with `zzz` relayed beside it as text
// (and the argument-less spelling takes a different minted id for the call as
// well). The auditor measured it byte-identical before and after round 76, so
// it is not a regression of this round. It is the recorded trade-off that
// round 60's gate and round 51's G1 already pin: a fragment that names no call
// is indistinguishable on THIS arm from the continuation of a free-form
// argument list a vendor splits across fragments (`{"name":"Bash","arguments":
// "echo "}` then `{"arguments":"hi"}` is one call on every arm, and relaying
// that half as prose would hand the client a tool_use whose command is
// truncated). The reading that keeps the model's command runnable wins, as it
// did at round 60, and the tie-break direction for the two-entry spelling
// belongs to a round that can tell the two wires apart — nothing on this arm
// can.

import (
	"testing"
)

// TestANamelessRunTakesAnyBytesOnEveryArm is F77-L3-1: a run of nameless
// entries is one text block whatever its bytes are, including bytes no call's
// argument list could extend.
func TestANamelessRunTakesAnyBytesOnEveryArm(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
		want    string
	}{
		{
			"a free-form line then a whole object",
			[]string{
				`{"type":"function","function":{"arguments":"zzz"}}`,
				`{"type":"function","function":{"arguments":"{\"c\":3}"}}`,
			},
			`<text "zzz{\"c\":3}">`,
		},
		{
			"two whole objects, no call named at all",
			[]string{
				`{"type":"function","function":{"arguments":"{\"c\":3}"}}`,
				`{"type":"function","function":{"arguments":"{\"d\":4}"}}`,
			},
			`<text "{\"c\":3}{\"d\":4}">`,
		},
		{
			"two whole objects at one stated slot",
			[]string{
				`{"index":0,"type":"function","function":{"arguments":"{\"a\":1}"}}`,
				`{"index":0,"type":"function","function":{"arguments":"{\"b\":2}"}}`,
			},
			`<text "{\"a\":1}{\"b\":2}">`,
		},
		{
			"a run then the call that ends it",
			[]string{
				`{"type":"function","function":{"arguments":"zzz"}}`,
				`{"type":"function","function":{"arguments":"{\"c\":3}"}}`,
				`{"type":"function","function":{"name":"Read"}}`,
			},
			`<tool_use call_eb2776e1><text "zzz{\"c\":3}">`,
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			doc, plain, frame := r76ThreeArms(t, tc.entries)
			gotDoc, gotPlain, gotFrame := r76BlockOrder(t, doc), r76BlockOrder(t, plain), r76BlockOrder(t, frame)
			if gotFrame != gotDoc || gotPlain != gotDoc {
				t.Errorf("one body, three arms: adopted=%s plain=%s frames=%s — a nameless block is prose and takes any bytes, so a run of them is one text block on every arm (2026-09-28 audit, round 77, F77-L3-1)\n%s\n%s\n%s",
					gotDoc, gotPlain, gotFrame, doc, plain, frame)
			}
			if gotDoc != tc.want {
				t.Errorf("the turn answered %s, want %s (2026-09-28 audit, round 77, F77-L3-1)\n%s", gotDoc, tc.want, doc)
			}
		})
	}
}

// TestANamelessRunsBytesKeepTheWiresOrder is F77-L3-2: the bytes of a run reach
// the client in the order the model wrote them. Round 76's split reordered
// them — the fragment that split off opened a block relayed last, so the bytes
// after it landed back in the run's first block.
func TestANamelessRunsBytesKeepTheWiresOrder(t *testing.T) {
	entries := []string{
		`{"type":"function","function":{"arguments":"AAA"}}`,
		`{"type":"function","function":{"arguments":"{\"c\":3}"}}`,
		`{"type":"function","function":{"arguments":"BBB"}}`,
	}
	doc, plain, frame := r76ThreeArms(t, entries)
	want := `<text "AAA{\"c\":3}BBB">`
	if got := r76BlockOrder(t, doc); got != want {
		t.Errorf("the adopted arm answered %s, want %s (2026-09-28 audit, round 77, F77-L3-2)", got, want)
	}
	if got := r76BlockOrder(t, plain); got != want {
		t.Errorf("the non-stream arm answered %s, want %s (2026-09-28 audit, round 77, F77-L3-2)", got, want)
	}
	if got := r76BlockOrder(t, frame); got != want {
		t.Errorf("the fragment arm answered %s, want %s — the client reads the model's prose in the order the model wrote it, and the bytes either side of a fragment the run cannot extend are still the run's own (2026-09-28 audit, round 77, F77-L3-2)\n%s", got, want, frame)
	}
}

// TestARunEndsWhereACallIsNamed is the boundary rule both findings turn on: two
// nameless entries with a call between them are TWO runs, because that is what
// the document arms write for the same body. Round 76's fix joined them into
// one text block on the frame arm by routing the later entry back to the run's
// block, which the wire had already interrupted.
func TestARunEndsWhereACallIsNamed(t *testing.T) {
	entries := []string{
		`{"id":"call_1","type":"function","function":{"arguments":"zzz"}}`,
		`{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{}"}}`,
		`{"id":"call_1","type":"function","function":{"arguments":"{\"q\":7}"}}`,
	}
	doc, plain, frame := r76ThreeArms(t, entries)
	want := `<tool_use call_1><text "zzz"><text "{\"q\":7}">`
	gotDoc, gotPlain, gotFrame := r76BlockOrder(t, doc), r76BlockOrder(t, plain), r76BlockOrder(t, frame)
	if gotDoc != want {
		t.Errorf("the adopted arm answered %s, want %s (2026-09-28 audit, round 77, F77-L3-1)", gotDoc, want)
	}
	if gotPlain != want {
		t.Errorf("the non-stream arm answered %s, want %s (2026-09-28 audit, round 77, F77-L3-1)", gotPlain, want)
	}
	if gotFrame != want {
		t.Errorf("the fragment arm answered %s, want %s — a run is the span between the calls the wire NAMES, so an entry before the call and an entry after it are two runs, and the document arms write two text blocks for this body (2026-09-28 audit, round 77, F77-L3-1)\n%s",
			gotFrame, want, frame)
	}
}
