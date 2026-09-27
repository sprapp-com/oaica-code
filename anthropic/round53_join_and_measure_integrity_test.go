package anthropic

// round53_join_and_measure_integrity_test.go — round 53's findings on the local
// leg's joins, all of them one rule: the estimate charges what the converter
// WRITES, and the converter writes one assembly of a body — not two.
//
//   - L2: convertToolResultContent wrote an empty text block, and wrote the
//     separator in front of it, so a result of ["one", ""] reached the model as
//     "one\n" while the gateway leg's tool_result join writes "one" for the same
//     blocks (its text case takes a string that carries bytes, and nothing
//     else). A tool result that ends in a blank line reads as an unfinished
//     answer.
//
//   - L2b/L3: both joins — the tool_result's "\n" between the parts it
//     assembles, and convertMessage's "\n\n" between a message's text blocks —
//     are prompt bytes, and nothing charged them. A message of three text
//     blocks reached the model as 20 bytes and was charged 12; a tool result of
//     two carried 7 and was charged 6. The estimate seeds the client-visible
//     input_tokens on the local leg, so the session's meter and its
//     auto-compaction read a prompt smaller than the one that was sent.
//
// Every case below measures the converter's own output and compares it with the
// charge, so the two cannot drift apart again without a test going red.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// round53Items decodes a JSON content array the way the request body arrives.
func round53Items(t *testing.T, raw string) []any {
	t.Helper()
	var items []any
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		t.Fatalf("decode %s: %v", raw, err)
	}
	return items
}

// TestAnEmptyToolResultTextBlockIsNotWritten is L2.
func TestAnEmptyToolResultTextBlockIsNotWritten(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
		want    string
	}{
		{"empty after text", `[{"type":"text","text":"one"},{"type":"text","text":""}]`, "one"},
		{"empty between", `[{"type":"text","text":"one"},{"type":"text","text":""},{"type":"text","text":"two"}]`, "one\ntwo"},
		{"empty first", `[{"type":"text","text":""},{"type":"text","text":"one"}]`, "one"},
		{"empty only", `[{"type":"text","text":""}]`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := round53Items(t, tc.content)
			text, _, err := convertToolResultContent(items)
			if err != nil {
				t.Fatalf("convertToolResultContent: %v", err)
			}
			if text != tc.want {
				t.Errorf("the tool result reached the model as %q, want %q\nthe gateway leg's tool_result join writes the blocks that carry bytes and nothing for the ones that do not, so a blank line here is a byte the model sees on this leg alone",
					text, tc.want)
			}
			if charged := countItemsIn(items, chargeToolResult); charged != len(text) {
				t.Errorf("the result was charged %d bytes for the %d the converter wrote (%q)", charged, len(text), text)
			}
		})
	}
}

// TestTheEstimateChargesWhatTheConverterWrites is L2b and L3: every byte the
// two joins write, on both spellings a content array arrives in.
func TestTheEstimateChargesWhatTheConverterWrites(t *testing.T) {
	for _, tc := range []struct {
		name    string
		content string
	}{
		{"two text blocks", `[{"type":"text","text":"aaaa"},{"type":"text","text":"bbbb"}]`},
		{"three text blocks", `[{"type":"text","text":"aaaa"},{"type":"text","text":"bbbb"},{"type":"text","text":"cccc"}]`},
		{"empty between", `[{"type":"text","text":"aaaa"},{"type":"text","text":""},{"type":"text","text":"bbbb"}]`},
		{"empty first", `[{"type":"text","text":""},{"type":"text","text":"bbbb"}]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			items := round53Items(t, tc.content)
			raw, err := json.Marshal(items)
			if err != nil {
				t.Fatalf("re-encode: %v", err)
			}
			var typed []ContentBlock
			if err := json.Unmarshal(raw, &typed); err != nil {
				t.Fatalf("decode into blocks: %v", err)
			}

			msgs, err := convertMessage(MessageParam{Role: "user", Content: typed})
			if err != nil {
				t.Fatalf("convertMessage: %v", err)
			}
			wire := 0
			for _, m := range msgs {
				wire += len(m.Role) + len(m.Content)
			}
			if got := len("user") + countAnyContent(typed); got != wire {
				t.Errorf("the typed body was charged %d bytes for a prompt of %d\n%s", got, wire, describeRound53Msgs(msgs))
			}
			if got := len("user") + countAnyContent(items); got != wire {
				t.Errorf("the same body as decoded JSON was charged %d bytes for a prompt of %d\n%s", got, wire, describeRound53Msgs(msgs))
			}
		})
	}
}

// TestAToolResultsJoinIsChargedItsSeparator is L2b on its own: the newline
// between the parts of one tool_result.
func TestAToolResultsJoinIsChargedItsSeparator(t *testing.T) {
	items := round53Items(t, `[{"type":"text","text":"one"},{"type":"text","text":"two"},{"type":"text","text":"three"}]`)
	text, _, err := convertToolResultContent(items)
	if err != nil {
		t.Fatalf("convertToolResultContent: %v", err)
	}
	if charged := countItemsIn(items, chargeToolResult); charged != len(text) {
		t.Errorf("three text parts reached the model as %q (%d bytes) and were charged %d: the join's newlines are prompt bytes like any other",
			text, len(text), charged)
	}
}

// TestASeparatorIsNotChargedAcrossAToolResult pins the walk's run rule: the
// blank line belongs between the text blocks of ONE run, and a tool result ends
// the run (convertMessage's cur = nil), so the text after it takes none. The
// charge for such a body is checked against the wire in the test above only for
// text-only content, because a tool result's frame is charged as the JSON the
// converter writes for it and not as the message's role — this test pins the
// separator half of that walk on its own.
func TestASeparatorIsNotChargedAcrossAToolResult(t *testing.T) {
	sameRun := round53Items(t, `[{"type":"text","text":"aaaa"},{"type":"text","text":"bbbb"}]`)
	if got := messageJoinSeparatorBytes(sameRun); got != 2 {
		t.Errorf("two text blocks in one run were charged %d separator bytes, want 2", got)
	}
	acrossResult := round53Items(t, `[{"type":"text","text":"aaaa"},{"type":"tool_result","tool_use_id":"c1","content":"r"},{"type":"text","text":"bbbb"}]`)
	if got := messageJoinSeparatorBytes(acrossResult); got != 0 {
		t.Errorf("the text after a tool result was charged %d separator bytes, want 0 — the result ends the run, so the text that follows opens one of its own", got)
	}
}

func describeRound53Msgs(msgs []api.Message) string {
	raw, err := json.Marshal(msgs)
	if err != nil {
		return "(unprintable)"
	}
	return string(raw)
}
