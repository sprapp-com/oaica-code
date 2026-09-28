package main

// round75_nameless_fragment_park_integrity_test.go — leg 3, a fragment the
// upstream never named is not a call on any arm of this bridge (2026-09-28
// audit, round 75, F75-L3-1).
//
// The body: a nameless entry — an entry whose `function.name` is missing —
// carrying argument bytes, followed by a named, id-less call. Such an entry is
// not a call on any arm here: this bridge relays its arguments as TEXT (the
// client reads prose). Before the fix the FRAGMENT arm alone disagreed with
// both document arms: with a block open (the entry restated a call the wire had
// already named) the nameless fragment's bytes were written as the CALL's input
// — `{"_raw":"zzz"}`, or the whole `{"q":7}` object — and, when the fragment
// stated an id of its own, that id was stolen by the call that followed. The
// same bytes reached a document client as text. One body may not answer two
// shapes because it was spelled in fragments.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r75TextOf reads the text blocks a relayed streaming turn writes, in order.
// A call is not text: only text block starts and their deltas count.
func r75TextOf(t *testing.T, body string) string {
	t.Helper()
	var out strings.Builder
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			raw = strings.TrimSpace(line)
		}
		if !strings.HasPrefix(raw, "{") {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			ContentBlock struct {
				Type string `json:"type"`
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
			if ev.ContentBlock.Type == "text" {
				out.WriteString(ev.ContentBlock.Text)
			}
		case "content_block_delta":
			if ev.Delta.Type == "text_delta" {
				out.WriteString(ev.Delta.Text)
			}
		}
	}
	return out.String()
}

// r75PlainText reads the text blocks of a non-streaming answer, in order.
func r75PlainText(t *testing.T, body string) string {
	t.Helper()
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("the non-stream answer is not a message: %v\n%s", err, body)
	}
	var out strings.Builder
	for _, b := range msg.Content {
		if b.Type == "text" {
			out.WriteString(b.Text)
		}
	}
	return out.String()
}

// r75NamelessBodies are the three spellings of "a nameless entry carrying
// argument bytes, then a named id-less call".
func r75NamelessBodies() []struct {
	note    string
	entries string
	text    string
} {
	return []struct {
		note    string
		entries string
		text    string
	}{
		{
			"a nameless fragment with argument bytes, then a named id-less call",
			`{"type":"function","function":{"arguments":"zzz"}},` +
				`{"type":"function","function":{"name":"Read","arguments":"{}"}}`,
			"zzz",
		},
		{
			"a nameless fragment holding an id of its own, then a named id-less call",
			`{"id":"call_other","type":"function","function":{"arguments":"zzz"}},` +
				`{"type":"function","function":{"name":"Read","arguments":"{}"}}`,
			"zzz",
		},
		{
			"a nameless fragment whose bytes are an object, then a named id-less call",
			`{"type":"function","function":{"arguments":"{\"q\":7}"}},` +
				`{"type":"function","function":{"name":"Read","arguments":"{}"}}`,
			`{"q":7}`,
		},
	}
}

// TestANamelessFragmentIsRelayedAsTextOnEveryArm is the F75-L3-1 pin. The
// fragment arm must answer what both document arms answer: one call, the one
// the NAMED entry states, carrying that entry's own arguments — and the
// nameless fragment's bytes relayed as text, not as that call's input.
func TestANamelessFragmentIsRelayedAsTextOnEveryArm(t *testing.T) {
	for _, tc := range r75NamelessBodies() {
		t.Run(tc.note, func(t *testing.T) {
			doc := r71Leg3Doc(tc.entries)
			adoptedBody := r72DocTurn(t, doc)
			plainBody := r74PlainTurn(t, doc)
			frameBody := r72FrameTurn(t, r71Leg3Frame(tc.entries))

			adopted := r72Calls(t, adoptedBody)
			plain := r74PlainCalls(t, plainBody)
			frame := r72Calls(t, frameBody)

			if len(adopted) != 1 || plain != 1 || len(frame) != 1 {
				t.Fatalf("one nameless entry plus one named call answers one call: adopted=%d plain=%d frame=%d\n%s\nframe body:\n%s",
					len(adopted), plain, len(frame), tc.note, frameBody)
			}
			for _, arm := range []struct {
				name string
				call r72Call
			}{{"adopted", adopted[0]}, {"frame", frame[0]}} {
				if arm.call.input != "{}" {
					t.Errorf("%s: the call %q carries %q as its input, want the named entry's own `{}` — the nameless fragment's bytes are not a call's arguments (2026-09-28 audit, round 75, F75-L3-1)\n%s",
						arm.name, arm.call.id, arm.call.input, frameBody)
				}
			}
			if frame[0].id != adopted[0].id {
				t.Errorf("one call is %q as adopted entries and %q as fragments — the fragment's own id is not a call's id (2026-09-28 audit, round 75, F75-L3-1)\nframe body:\n%s",
					adopted[0].id, frame[0].id, frameBody)
			}

			// The fragment's bytes reach the client either way; as fragments
			// they must reach it as TEXT, the way the document arms relay them.
			if got := r75TextOf(t, frameBody); got != tc.text {
				t.Errorf("the fragment arm relayed the nameless entry's bytes as text %q, want %q (2026-09-28 audit, round 75, F75-L3-1)\nframe body:\n%s",
					got, tc.text, frameBody)
			}
			if got := r75TextOf(t, adoptedBody); got != tc.text {
				t.Errorf("PREMISE: the adopted document arm relays those bytes as text %q, want %q\nbody:\n%s", got, tc.text, adoptedBody)
			}
			if got := r75PlainText(t, plainBody); got != tc.text {
				t.Errorf("PREMISE: the plain document arm relays those bytes as text %q, want %q\nbody:\n%s", got, tc.text, plainBody)
			}
		})
	}
}
