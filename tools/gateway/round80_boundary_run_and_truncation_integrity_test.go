package main

// round80_boundary_run_and_truncation_integrity_test.go — leg 3, the places a
// call boundary the wire drew was answered differently by the arms of one body
// (2026-09-28 audit, round 80).
//
//  1. F80-L3-1. An entry that names a call this bridge already carries and
//     states NOTHING else — no id, no arguments — over a call that holds no
//     arguments either is the NEXT call, not that one restated: an empty
//     argument list is a COMPLETE one (round 39's B-F9), so the call the slot
//     carries is whole, and an entry that names a call is a call (round 77).
//     The restatement rule read the argument-less repeat as the same call and
//     dropped it: `[{index 0,id call_1,name Read},{index 0,name Read}]` reached
//     the client as ONE call on the frame arm where both document arms of the
//     same body answer two.
//
//  2. F80-L3-2. A run of nameless entries is one text block, and it ends where
//     a CALL is NAMED and nowhere else. The document arms group the run by
//     `args == ""` and count an entry whose bytes are whitespace as a member of
//     the run it sits in; the frame arm's join test trimmed the bytes first, so
//     `[{index 0,"A"},{index 1," "},{index 0,"B"}]` was routed to a block of its
//     own and SPLIT the run — `<text "A"><text " B">` against the document arms'
//     `<text "A B">`.
//
//  3. F80-L3-3. A truncated turn whose named call never became parseable JSON
//     is not a call: the non-stream path drops it and so does the adoption arm
//     (callInput), and both answer that body with no tool block and a
//     stop_reason of max_tokens. The frame arm opened the block and handed the
//     client half an object under a stop_reason of tool_use.
//
//  4. F80-L3-4. Argument bytes that never became an object — the turn is over
//     and they are still mid-object — are kept as the single-key _raw object on
//     every other arm of the body: the client leg's fragment fold, this bridge's
//     own non-stream path through callInput, and the adoption arm by the same
//     call. The frame arm delivered them as they stood, so one body reached the
//     client as `{"r":1` when it streamed and `{"_raw":"{\"r\":1"}` when it did
//     not, the first of which no client can parse into an input.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r80Leg3ToolInputs reads the accumulated input of each tool_use block, in
// block order, on either framing. A start block's `input` is REPLACED by the
// first partial_json (the client accumulates the deltas, it does not append
// them to the start's empty object), and a whole-document answer carries the
// input itself.
func r80Leg3ToolInputs(t *testing.T, body string) []string {
	t.Helper()
	if !strings.Contains(body, "event: ") {
		var msg struct {
			Content []struct {
				Type  string          `json:"type"`
				ID    string          `json:"id"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &msg); err != nil {
			t.Fatalf("the relayed turn is not a message: %v\n%s", err, body)
		}
		var out []string
		for _, b := range msg.Content {
			if b.Type == "tool_use" {
				out = append(out, string(b.Input))
			}
		}
		return out
	}
	acc := map[int]*strings.Builder{}
	var order []int
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(raw), &ev); err != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			if ev.ContentBlock.Type == "tool_use" {
				acc[ev.Index] = &strings.Builder{}
				order = append(order, ev.Index)
			}
		case "content_block_delta":
			if ev.Delta.Type == "input_json_delta" && acc[ev.Index] != nil {
				acc[ev.Index].WriteString(ev.Delta.PartialJSON)
			}
		}
	}
	var out []string
	for _, i := range order {
		out = append(out, acc[i].String())
	}
	return out
}

// TestAnArglessRestatementOfACallIsTheNextCall is F80-L3-1.
func TestAnArglessRestatementOfACallIsTheNextCall(t *testing.T) {
	for _, tc := range []struct {
		note    string
		entries []string
	}{
		{
			"the repeat states the id",
			[]string{
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Read"}}`,
				`{"index":0,"type":"function","function":{"name":"Read"}}`,
			},
		},
		{
			"the repeat states no id and no index",
			[]string{
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Read"}}`,
				`{"type":"function","function":{"name":"Read"}}`,
			},
		},
	} {
		t.Run(tc.note, func(t *testing.T) {
			doc, plain, frame := r76ThreeArms(t, tc.entries)
			want := `<tool_use call_1><tool_use call_eb2776e1>`
			for _, arm := range []struct {
				name string
				body string
			}{{"document", doc}, {"non-stream", plain}, {"frame", frame}} {
				if got := r76BlockOrder(t, arm.body); got != want {
					t.Errorf("the %s arm answered %s, want %s — a call named again with no arguments, over a call that holds none, is the NEXT call (an empty argument list is a complete one), and every arm of one body has to say so (2026-09-28 audit, round 80, F80-L3-1)\n%s",
						arm.name, got, want, arm.body)
				}
			}
		})
	}
}

// TestAWhitespaceEntryDoesNotSplitANamelessRun is F80-L3-2.
func TestAWhitespaceEntryDoesNotSplitANamelessRun(t *testing.T) {
	entries := []string{
		`{"index":0,"type":"function","function":{"arguments":"A"}}`,
		`{"index":1,"type":"function","function":{"arguments":" "}}`,
		`{"index":0,"type":"function","function":{"arguments":"B"}}`,
	}
	doc, plain, frame := r76ThreeArms(t, entries)
	want := `<text "A B">`
	for _, arm := range []struct {
		name string
		body string
	}{{"document", doc}, {"non-stream", plain}, {"frame", frame}} {
		if got := r76BlockOrder(t, arm.body); got != want {
			t.Errorf("the %s arm answered %s, want %s — a run of nameless entries is one text block and ends where a CALL is named, and an entry whose bytes are a space is a member of the run it sits in, not a block of its own (2026-09-28 audit, round 80, F80-L3-2)\n%s",
				arm.name, got, want, arm.body)
		}
	}
}

// TestATruncatedCallTheClientCannotParseIsNotACall is F80-L3-3. The turn is cut
// at the token limit with a named call whose arguments are half an object: the
// document arms relay no tool block and report max_tokens, and so must this arm.
func TestATruncatedCallTheClientCannotParseIsNotACall(t *testing.T) {
	entries := []string{
		`{"index":0,"type":"function","function":{"arguments":"A"}}`,
		`{"index":1,"type":"function","function":{"name":"Read","arguments":"{\"r\":1"}}`,
	}
	whole := r80Leg3Doc(strings.Join(entries, ","), "length")
	doc := r72DocTurn(t, whole)
	plain := r74PlainTurn(t, whole)
	frames := make([]string, 0, len(entries)+2)
	for _, e := range entries {
		frames = append(frames, r71Leg3Frame(e))
	}
	frames = append(frames,
		`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`)
	// Framed by hand: r58FrameCalls states its own finish_reason of tool_calls
	// after these, and the turn's verdict is the LAST reason the wire states.
	up := round45Frames(t, frames...)
	srv, _ := round39Gateway(t, up, nil)
	_, frame := round45Ask(t, srv, round45AskStream)

	for _, arm := range []struct {
		name string
		body string
	}{{"document", doc}, {"non-stream", plain}} {
		if n := r76BlockOrder(t, arm.body); strings.Contains(n, "tool_use") {
			t.Fatalf("the %s arm opened a tool_use block for an unparseable truncated call — control drifted: %s", arm.name, n)
		}
	}
	if got := r76BlockOrder(t, frame); strings.Contains(got, "tool_use") {
		t.Errorf("the frame arm opened %s where both document arms of the same body open no tool block — a truncated call whose arguments never parsed is not a call on any arm (2026-09-28 audit, round 80, F80-L3-3)\n%s",
			got, frame)
	}
	if !strings.Contains(frame, `"stop_reason":"max_tokens"`) {
		t.Errorf("the frame arm does not report max_tokens for the truncated turn (2026-09-28 audit, round 80, F80-L3-3)\n%s", frame)
	}
}

// TestAMidObjectTurnEndsAsTheRawObjectEveryArmKeeps is F80-L3-4.
func TestAMidObjectTurnEndsAsTheRawObjectEveryArmKeeps(t *testing.T) {
	whole := r80Leg3Doc(`{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"r\":1"}}`, "tool_calls")
	doc := r72DocTurn(t, whole)
	plain := r74PlainTurn(t, whole)
	frames := []string{
		r71Leg3Frame(`{"index":0,"type":"function","function":{"name":"Read","arguments":"{\"r\":1"}}`),
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
	frame := r72FrameTurn(t, frames...)

	want := `{"_raw":"{\"r\":1"}`
	for _, arm := range []struct {
		name string
		body string
	}{{"document", doc}, {"non-stream", plain}, {"frame", frame}} {
		got := r80Leg3ToolInputs(t, arm.body)
		if len(got) != 1 || got[0] != want {
			t.Errorf("the %s arm handed the client %v, want [%s] — arguments the model never closed into an object are kept as the single-key _raw object on every arm, because no client can parse the bare text into an input (2026-09-28 audit, round 80, F80-L3-4)\n%s",
				arm.name, got, want, arm.body)
		}
	}
}

// r80Leg3Doc is the whole-completion document for a list of entries.
func r80Leg3Doc(entries, fin string) string {
	return `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[` + entries + `]},"finish_reason":"` + fin + `"}],` +
		`"usage":{"prompt_tokens":10,"completion_tokens":10}}`
}

