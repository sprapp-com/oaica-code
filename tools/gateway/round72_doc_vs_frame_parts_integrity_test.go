package main

// round72_doc_vs_frame_parts_integrity_test.go — leg 3, one body written three
// ways: the whole message in one chunk, the same entries one chunk each, and the
// non-stream document. All three must answer the same calls, in the same order,
// with the same INPUT — the text the client assembles for each call, which is
// the partial_json its deltas accumulate, or the input the block start states
// when nothing follows it.
//
// The body: three entries sharing one stated id and one name — a call that is
// introduced, restated, and restated again — where the argument text of the
// entries is empty, whitespace, and the empty object `{}`. Round 71's identity
// route (F71-L3-2) reaches these entries through `blockCarrying` and hands the
// carried block the fragment it recognises as the same call restated. What the
// bridge then WRITES for that recognition used to differ by spelling: the
// fragment arm delivered the block's whole accumulated text at close —
// `"      {}"`, whitespace prefix and all — where the one-list arm delivered
// `{}`. Both parse to the empty object, but the doctrine asks the two spellings
// of one answer for the same bytes, and the client leg answers `{}` under both
// (2026-09-28 audit, round 72, F72-L3-1).

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"
)

// r72Entry spells one tool-call entry of the turn.
func r72Entry(index int, args string) string {
	entry := `{"index":` + itoa(index) + `,"id":"call_1","type":"function","function":{"name":"Read"`
	if args != "" {
		entry += `,"arguments":"` + args + `"`
	}
	return entry + `}}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	out := ""
	for n > 0 {
		out = string(rune('0'+n%10)) + out
		n /= 10
	}
	return out
}

// r72Body spells the three entries as one message with the given indexes.
func r72Body(indexes [3]int, first, second string) string {
	entries := r72Entry(indexes[0], first) + "," + r72Entry(indexes[1], second) + "," + r72Entry(indexes[2], "{}")
	return r71Leg3Doc(entries)
}

// r72Call is one tool_use block as the client ends up with it: the id the block
// opened with and the input it assembles.
type r72Call struct {
	id    string
	input string
}

// r72Calls reads a relayed turn the way the client reads it: in block order, the
// id each block opened with and the input it ends with — the partial_json its
// deltas accumulated, or the input the start event stated when none followed.
func r72Calls(t *testing.T, body string) []r72Call {
	t.Helper()
	type open struct {
		id     string
		stated string
		parts  strings.Builder
	}
	blocks := map[int]*open{}
	order := []int{}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				ID    string          `json:"id"`
				Type  string          `json:"type"`
				Input json.RawMessage `json:"input"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(line), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type != "tool_use" {
				continue
			}
			stated := ""
			if len(ev.ContentBlock.Input) > 0 {
				stated = string(ev.ContentBlock.Input)
			}
			blocks[ev.Index] = &open{id: ev.ContentBlock.ID, stated: stated}
			order = append(order, ev.Index)
		case "content_block_delta":
			if ev.Delta.Type != "input_json_delta" {
				continue
			}
			if b := blocks[ev.Index]; b != nil {
				b.parts.WriteString(ev.Delta.PartialJSON)
			}
		}
	}
	out := make([]r72Call, 0, len(order))
	for _, idx := range order {
		b := blocks[idx]
		input := b.parts.String()
		if input == "" {
			input = b.stated
		}
		out = append(out, r72Call{id: b.id, input: input})
	}
	return out
}

// r72DocTurn relays the whole-message spelling and hands back the raw turn.
func r72DocTurn(t *testing.T, doc string) string {
	t.Helper()
	up := round45Upstream(t, "application/json", doc)
	srv, _ := round39Gateway(t, up, nil)
	_, body := round45Ask(t, srv, round45AskStream)
	return body
}

// r72FrameTurn relays the fragment spelling and hands back the raw turn.
func r72FrameTurn(t *testing.T, frames ...string) string {
	t.Helper()
	_, body := r58FrameCalls(t, frames...)
	return body
}

// TestTheSameRestatedCallIsWrittenTheSameWhicheverSpellingCarriesIt is the
// F72-L3-1 pin: one answer, two spellings, the same calls with the same inputs.
func TestTheSameRestatedCallIsWrittenTheSameWhicheverSpellingCarriesIt(t *testing.T) {
	for _, shape := range []struct {
		indexes       [3]int
		first, second string
	}{
		{[3]int{0, 0, 1}, "   ", "   "},
		{[3]int{0, 0, 0}, "", ""},
		{[3]int{0, 0, 1}, "   ", ""},
		{[3]int{0, 0, 1}, "", ""},
		{[3]int{1, 1, 1}, "", ""},
		{[3]int{0, 0, 1}, "", "   "},
		{[3]int{1, 1, 0}, "", ""},
		{[3]int{0, 0, 0}, "   ", "   "},
	} {
		note := "three entries stated at " + itoa(shape.indexes[0]) + "," + itoa(shape.indexes[1]) + "," + itoa(shape.indexes[2]) +
			" share one id and one name, with the argument text " + strconv.Quote(shape.first) + " then " + strconv.Quote(shape.second) +
			" then `{}`: one call restated, whose last stated arguments are `{}`"

		body := r72Body(shape.indexes, shape.first, shape.second)
		docCalls := r72Calls(t, r72DocTurn(t, body))
		frameCalls := r72Calls(t, r72FrameTurn(t,
			r71Leg3Frame(r72Entry(shape.indexes[0], shape.first)),
			r71Leg3Frame(r72Entry(shape.indexes[1], shape.second)),
			r71Leg3Frame(r72Entry(shape.indexes[2], "{}")),
		))

		// Premise: the entries share one id and one name, so both spellings
		// really do answer ONE call — the comparison below is about the input
		// written for it, not about a call that went missing.
		if len(docCalls) != 1 || len(frameCalls) != 1 {
			t.Fatalf("PREMISE: one call per spelling for %s, got one-list=%d fragments=%d\none list %v\nfragments %v",
				note, len(docCalls), len(frameCalls), docCalls, frameCalls)
		}
		if frameCalls[0].id != docCalls[0].id {
			t.Errorf("the same call is %q as fragments and %q as one list\n%s", frameCalls[0].id, docCalls[0].id, note)
		}
		if frameCalls[0].input != docCalls[0].input {
			t.Errorf("call %q assembles %q as fragments and %q as one list\n%s\n%s",
				frameCalls[0].id, frameCalls[0].input, docCalls[0].input, note, body)
		}
	}
}

// TestTheRestatedCallKeepsTheArgumentsTheWireStated is the same reading asked
// from the other side: the call's input is the arguments the wire last stated
// for it, and never a concatenation of the restatements' bytes.
func TestTheRestatedCallKeepsTheArgumentsTheWireStated(t *testing.T) {
	body := r72Body([3]int{0, 0, 1}, "   ", "   ")
	doc := r72DocTurn(t, body)
	frame := r72FrameTurn(t,
		r71Leg3Frame(r72Entry(0, "   ")),
		r71Leg3Frame(r72Entry(0, "   ")),
		r71Leg3Frame(r72Entry(1, "{}")),
	)

	for _, arm := range []struct {
		name string
		body string
	}{{"one list", doc}, {"fragments", frame}} {
		for _, call := range r72Calls(t, arm.body) {
			if strings.Contains(call.input, "   ") {
				t.Fatalf("as %s, call %q assembles %q: the restatements' whitespace was folded into its input (2026-09-28 audit, round 72, F72-L3-1)\n%s",
					arm.name, call.id, call.input, arm.body)
			}
		}
	}
}
