package main

// round76_nameless_run_and_order_integrity_test.go — leg 3, three ways this
// bridge's own three arms answered ONE upstream body differently when the body
// carried an entry the upstream never named (2026-09-28 audit, round 76,
// F76-L3-1 to F76-L3-3).
//
// The rule all three now keep, stated once: an entry whose `function.name` is
// empty is not a call on this leg — no content_block_start can carry a name it
// does not have, so its bytes are the model's prose and are relayed to the
// client as TEXT. The relay is one text block per CONTIGUOUS RUN of such
// entries (the span between the calls the wire names), and it goes out with
// every arm at the same point of the turn — after the blocks that name
// themselves, which is where the frame and non-stream arms have always put it.
//
//  1. The frame arm DROPPED the bytes of a nameless fragment whose arguments
//     could not extend the arguments of the block the wire put it on — the same
//     id restated, or the slot the previous fragment left. The block's own
//     write refuses those bytes (callArgsExtend, round 61's F61-L3-2 sibling),
//     and no other arm refused them: adoption and the non-stream path both
//     relay them as prose, so a body the model wrote reached a streaming client
//     with its output missing. The fragment now gets a block of its own, which
//     the nameless arm of finishStream relays.
//  2. The two document arms disagreed about the SPLIT. Adoption grouped the
//     nameless entries of one body into a single text block whatever sat
//     between them, and the non-stream path wrote one block per entry, so one
//     body reached the client as one text block under `stream:true` and two
//     under `stream:false` (and the frame arm, which merges adjacent nameless
//     fragments into one block, sided with adoption). One block per RUN now,
//     on every arm.
//  3. Adoption relayed that text WHERE IT READ IT, before the calls it had not
//     opened yet — a call with no arguments is held until the turn ends — so a
//     body whose nameless entry preceded a call reached the client as
//     [text][tool_use] when it streamed and [tool_use][text] when it did not.
//     The bytes are parked and relayed by the nameless arm, in one pass with
//     the frame path's own nameless blocks.
//
// Each test asks the SAME body of all three arms — the two document spellings
// (adopted inside a streaming request, and the plain non-stream answer) and the
// frame spelling — and each is fail-first against the tree before this round's
// fix. Round 39's B-F7 pin and round 61's F61-L3-2 pin are REVISED by this
// round: both asked the frame arm against a document body that omitted the
// fragment, so a drop on one arm passed as agreement (see their own comments).

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// r76BlockOrder reads a relayed turn the way the CLIENT reads it: the block
// types in wire order, with each text block's own text and each tool_use
// block's id. A streaming turn is read from its events, a message from its
// content — the two spellings of the same answer.
func r76BlockOrder(t *testing.T, body string) string {
	t.Helper()
	if !strings.Contains(body, "event: ") {
		var msg struct {
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
				ID   string `json:"id"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &msg); err != nil {
			t.Fatalf("the relayed turn is not a message: %v\n%s", err, body)
		}
		out := ""
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				out += fmt.Sprintf("<text %q>", b.Text)
			case "tool_use":
				out += fmt.Sprintf("<tool_use %s>", b.ID)
			default:
				out += "<" + b.Type + ">"
			}
		}
		return out
	}
	type blk struct{ typ, id, text string }
	blocks := map[int]*blk{}
	var order []int
	for _, line := range strings.Split(body, "\n") {
		raw := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(raw, "{") {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			blocks[ev.Index] = &blk{typ: ev.ContentBlock.Type, id: ev.ContentBlock.ID, text: ev.ContentBlock.Text}
			order = append(order, ev.Index)
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				if b := blocks[ev.Index]; b != nil {
					b.text += ev.Delta.Text
				}
			}
		}
	}
	out := ""
	for _, i := range order {
		b := blocks[i]
		switch b.typ {
		case "text":
			out += fmt.Sprintf("<text %q>", b.text)
		case "tool_use":
			out += fmt.Sprintf("<tool_use %s>", b.id)
		default:
			out += "<" + b.typ + ">"
		}
	}
	return out
}

// r76ThreeArms relays one body down all three arms of this bridge: the document
// adopted inside a streaming request, the same document answered to a
// non-streaming request, and the fragments the same upstream streams.
func r76ThreeArms(t *testing.T, entries []string) (doc, plain, frame string) {
	t.Helper()
	body := r71Leg3Doc(strings.Join(entries, ","))
	frames := make([]string, 0, len(entries))
	for _, e := range entries {
		frames = append(frames, r71Leg3Frame(e))
	}
	return r72DocTurn(t, body), r74PlainTurn(t, body), r72FrameTurn(t, frames...)
}

// TestANamelessFragmentsBytesReachTheClientOnEveryArm is F76-L3-1: a nameless
// fragment whose bytes cannot extend the arguments of the block the wire puts
// them on. The frame arm dropped them; both document arms relay them as prose.
func TestANamelessFragmentsBytesReachTheClientOnEveryArm(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
		want    string
	}{
		{
			"the same id restated, then a nameless continuation",
			[]string{
				`{"id":"call_1","type":"function","function":{"arguments":"zzz"}}`,
				`{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{}"}}`,
				`{"id":"call_1","type":"function","function":{"arguments":"{\"q\":7}"}}`,
			},
			`<tool_use call_1><text "zzz"><text "{\"q\":7}">`,
		},
		{
			"one slot, then a nameless continuation",
			[]string{
				`{"index":0,"type":"function","function":{"arguments":"zzz"}}`,
				`{"index":0,"type":"function","function":{"name":"Read","arguments":"{}"}}`,
				`{"index":0,"type":"function","function":{"arguments":"{\"q\":7}"}}`,
			},
			`<tool_use call_eb2776e1><text "zzz"><text "{\"q\":7}">`,
		},
		{
			"a nameless object after a call whose block had closed",
			[]string{
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}`,
				`{"index":0,"type":"function","function":{"arguments":"{\"more\":1}"}}`,
			},
			`<tool_use call_1><text "{\"more\":1}">`,
		},
		{
			"a nameless object after a free-form line the slot holds",
			[]string{
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hi"}}`,
				`{"index":0,"type":"function","function":{"arguments":"{\"c\":3}"}}`,
			},
			`<tool_use call_1><text "{\"c\":3}">`,
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			doc, plain, frame := r76ThreeArms(t, tc.entries)
			gotDoc, gotPlain, gotFrame := r76BlockOrder(t, doc), r76BlockOrder(t, plain), r76BlockOrder(t, frame)
			if gotFrame != gotDoc || gotPlain != gotDoc {
				t.Errorf("one body, three arms: adopted=%s plain=%s frames=%s — an entry that names no call is prose on every arm, so its bytes reach the client whichever way the request is answered (2026-09-28 audit, round 76, F76-L3-1)\n%s\n%s\n%s",
					gotDoc, gotPlain, gotFrame, doc, plain, frame)
			}
			if gotDoc != tc.want {
				t.Errorf("the turn answered %s, want %s — the nameless entries' bytes are the model's output and the call is the only block it named (2026-09-28 audit, round 76, F76-L3-1)\n%s",
					gotDoc, tc.want, doc)
			}
		})
	}
}

// TestOneNamelessRunIsOneTextBlockOnEveryArm is F76-L3-2: two nameless entries
// with nothing between them are ONE run, and a run is one text block — the
// reading adoption and the frame arm take, and the one the non-stream path
// refused (it wrote a block per entry).
func TestOneNamelessRunIsOneTextBlockOnEveryArm(t *testing.T) {
	entries := []string{
		`{"type":"function","function":{"arguments":"A"}}`,
		`{"type":"function","function":{"arguments":"B"}}`,
		`{"type":"function","function":{"name":"Read","arguments":"{}"}}`,
	}
	doc, plain, frame := r76ThreeArms(t, entries)
	want := `<tool_use call_eb2776e1><text "AB">`
	if got := r76BlockOrder(t, doc); got != want {
		t.Errorf("the adopted arm answered %s, want %s (2026-09-28 audit, round 76, F76-L3-2)", got, want)
	}
	if got := r76BlockOrder(t, plain); got != want {
		t.Errorf("the non-stream arm answered %s, want %s — two nameless entries side by side are one run, and a run is one text block (2026-09-28 audit, round 76, F76-L3-2)\n%s", got, want, plain)
	}
	if got := r76BlockOrder(t, frame); got != want {
		t.Errorf("the fragment arm answered %s, want %s (2026-09-28 audit, round 76, F76-L3-2)", got, want)
	}
}

// TestANamelessEntriesTextPrecedesTheCallItPreceded is F76-L3-3: the nameless
// entry came BEFORE the call that follows it, and a call with no arguments is
// held to the end of the turn — so adoption relayed the text first, and the
// client was shown the model's prose and its call in a different order
// depending on whether the request streamed.
func TestANamelessEntriesTextPrecedesTheCallItPreceded(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
		want    string
	}{
		{
			"an argument-less call after the nameless entry",
			[]string{
				`{"type":"function","function":{"arguments":"zzz"}}`,
				`{"type":"function","function":{"name":"Read"}}`,
			},
			`<tool_use call_eb2776e1><text "zzz">`,
		},
		{
			"the same, both entries at one slot",
			[]string{
				`{"index":0,"type":"function","function":{"arguments":"zzz"}}`,
				`{"index":0,"type":"function","function":{"name":"Read"}}`,
			},
			`<tool_use call_eb2776e1><text "zzz">`,
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			doc, plain, frame := r76ThreeArms(t, tc.entries)
			gotDoc, gotPlain, gotFrame := r76BlockOrder(t, doc), r76BlockOrder(t, plain), r76BlockOrder(t, frame)
			if gotDoc != gotPlain || gotFrame != gotPlain {
				t.Errorf("one body, three arms: adopted=%s plain=%s frames=%s — the turn's blocks go out in one order whichever way the request is answered (2026-09-28 audit, round 76, F76-L3-3)\n%s\n%s\n%s",
					gotDoc, gotPlain, gotFrame, doc, plain, frame)
			}
			if gotPlain != tc.want {
				t.Errorf("the turn answered %s, want %s — the nameless entry's bytes are relayed with the other nameless text, after the blocks that name themselves (2026-09-28 audit, round 76, F76-L3-3)\n%s",
					gotPlain, tc.want, plain)
			}
		})
	}
}
