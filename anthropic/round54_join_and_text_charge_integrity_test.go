package anthropic

// round54_join_and_text_charge_integrity_test.go — round 54's findings on the
// local leg's estimate, both of them the round-53 rule ("the estimate charges
// what the converter WRITES") still not held in two places.
//
//   - C4/B3: convertMessage writes a document with a text source and a
//     search_result into the same text run as a text block, with the same "\n\n"
//     join, a trailing "\n" of its own, and — for a passage — a newline after
//     its title and its reference. messageJoinSeparatorBytes walked only the
//     text arm, so a prompt whose newest blocks were an attachment or a search
//     passage was charged a few bytes short of what was sent, and the estimate
//     seeds the client-visible input_tokens whenever the upstream states no
//     usage.
//
//   - B4: block.Text was charged for EVERY block type, though convertMessage
//     reads it only in its "text" case. A body carrying a "text" key on a block
//     of another type was billed for bytes no converter ever writes: an image
//     block with a 41 000-byte "text" key took the estimate from 4 096 bytes to
//     45 096 for a prompt that did not change.
//
// Both tests measure the converter's own output and compare it with the charge,
// so the two cannot drift apart again without a test going red.

import (
	"encoding/json"
	"strings"
	"testing"
)

// round54Blocks decodes a content array both ways a body arrives: as typed
// blocks and as decoded JSON.
func round54Blocks(t *testing.T, raw string) ([]ContentBlock, []any) {
	t.Helper()
	var items []any
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	re, err := json.Marshal(items)
	if err != nil {
		t.Fatalf("re-encode: %v", err)
	}
	var typed []ContentBlock
	if err := json.Unmarshal(re, &typed); err != nil {
		t.Fatalf("decode into blocks: %v", err)
	}
	return typed, items
}

func TestTheEstimateChargesWhatTheConverterWritesForAttachments(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"document after text", `[{"type":"text","text":"aaaa"},{"type":"document","source":{"type":"text","data":"dddd"}}]`},
		{"document ending in newline", `[{"type":"text","text":"aaaa"},{"type":"document","source":{"type":"text","data":"dddd\n"}}]`},
		{"document alone", `[{"type":"document","source":{"type":"text","data":"dddd"}}]`},
		{"search_result after text", `[{"type":"text","text":"aaaa"},{"type":"search_result","title":"T","source":"http://s","content":[{"type":"text","text":"passage"}]}]`},
		{"search_result alone", `[{"type":"search_result","title":"T","source":"http://s","content":[{"type":"text","text":"passage"}]}]`},
		{"search_result with no title or ref", `[{"type":"search_result","content":[{"type":"text","text":"passage"}]}]`},
		{"search_result whose passage ends in a newline", `[{"type":"search_result","title":"T","content":[{"type":"text","text":"passage\n"}]}]`},
		{"two documents in one run", `[{"type":"document","source":{"type":"text","data":"aaaa"}},{"type":"document","source":{"type":"text","data":"bbbb"}}]`},
		{"a search result after a document", `[{"type":"document","source":{"type":"text","data":"aaaa"}},{"type":"search_result","title":"T","content":[{"type":"text","text":"p"}]}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			typed, items := round54Blocks(t, tc.content)

			msgs, err := convertMessage(MessageParam{Role: "user", Content: typed})
			if err != nil {
				t.Fatalf("convertMessage: %v", err)
			}
			wire := 0
			for _, m := range msgs {
				wire += len(m.Role) + len(m.Content)
			}
			if got := len("user") + countAnyContent(typed); got != wire {
				t.Errorf("the typed body was charged %d bytes for a prompt of %d\n%s\nthe converter writes a document's text and a search result's title, reference and passages into the same run as a text block, with the same blank line in front of each and a trailing newline of its own, and every one of those bytes is prompt the upstream counts",
					got, wire, describeRound53Msgs(msgs))
			}
			if got := len("user") + countAnyContent(items); got != wire {
				t.Errorf("the same body as decoded JSON was charged %d bytes for a prompt of %d\n%s", got, wire, describeRound53Msgs(msgs))
			}
		})
	}
}

// Test an attachment's run rule on its own. The wire comparison above cannot
// see it — a tool result's frame is charged as the JSON the converter writes for
// it, not as the message's role (round 53's note on the same walk). The bytes
// this walk charges a document are the blank line it takes inside a run PLUS the
// trailing newline the converter adds when its data does not end in one, so two
// documents in one run carry 2 (the join) + 1 + 1, and a document that opens a
// run of its own (after a tool result, which ends the previous one) carries only
// its own trailing newline.
func TestADocumentsSeparatorRespectsTheRunRule(t *testing.T) {
	_, sameRun := round54Blocks(t, `[{"type":"document","source":{"type":"text","data":"aaaa"}},{"type":"document","source":{"type":"text","data":"bbbb"}}]`)
	if got := messageJoinSeparatorBytes(sameRun); got != 4 {
		t.Errorf("two documents in one run were charged %d separator bytes, want 4 (the 2-byte join plus each document's own trailing newline)", got)
	}
	_, acrossResult := round54Blocks(t, `[{"type":"tool_result","tool_use_id":"c1","content":"r"},{"type":"document","source":{"type":"text","data":"aaaa"}}]`)
	if got := messageJoinSeparatorBytes(acrossResult); got != 1 {
		t.Errorf("the document after a tool result was charged %d separator bytes, want 1 — the result ends the run, so the document that follows takes no blank line and carries only its own trailing newline", got)
	}
}

func TestANonTextBlocksTextKeyIsNotCharged(t *testing.T) {
	const src = `{"type":"base64","media_type":"image/png","data":"iVBORw0KGgoAAAANSUhEUg"}`

	with, _ := round54Blocks(t, `[{"type":"image","source":`+src+`,"text":"`+strings.Repeat("x", 41000)+`"}]`)
	without, _ := round54Blocks(t, `[{"type":"image","source":`+src+`}]`)
	if got, want := countAnyContent(with), countAnyContent(without); got != want {
		t.Errorf("an image block carrying a 41 000-byte \"text\" key was charged %d bytes against %d for the same block without it: convertMessage reads block.Text only in its \"text\" case, so those bytes are a field of no block this converter writes and the model never sees them\nthe estimate seeds the client-visible input_tokens whenever the upstream states no usage, so a session carrying one such block read as a prompt 45 KB long and compacted early",
			got, want)
	}

	// The decoded arm, which reads the same key out of a map.
	var withItems, withoutItems []any
	if err := json.Unmarshal([]byte(`[{"type":"image","source":`+src+`,"text":"`+strings.Repeat("y", 41000)+`"}]`), &withItems); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if err := json.Unmarshal([]byte(`[{"type":"image","source":`+src+`}]`), &withoutItems); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got, want := countAnyContent(withItems), countAnyContent(withoutItems); got != want {
		t.Errorf("decoded: the same block with a \"text\" key was charged %d bytes against %d without it", got, want)
	}

	// And the text arm still charges its own text — the gate is a gate, not a
	// removal.
	text, _ := round54Blocks(t, `[{"type":"text","text":"0123456789"}]`)
	if got := countAnyContent(text); got != 10 {
		t.Errorf("a text block of 10 bytes was charged %d", got)
	}
}
