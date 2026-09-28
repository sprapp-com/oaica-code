package main

// round98_leg3_truncated_freeform_call_test.go — leg 3, round 98 (2026-09-29
// audit), F98-L3-1 and F98-L3-2.
//
// One upstream turn, three spellings: the whole document under stream:false, the
// whole document under stream:true, and the same entries one fragment per delta.
// Round 80's F80-L3-3 and round 81's F81-L3-1 both rule that a call the client
// cannot run is not a call: a truncated one is DROPPED, a mid-object one is
// wrapped under `_raw`. The fragment arm applied that rule only to arguments
// that had begun an object — `argsAreMidObject` — so a call whose stated
// arguments never began one (`echo hi`, `"s"`, `[1]`, `42`) opened at the
// fragment and stayed open, and a turn cut at the token limit handed the client
// a runnable call the document arms of the same body had dropped:
//
//	plain/adopt (whole document)   [text:hi]
//	framed   (one entry per delta) [text:hi, call call_1|Bash|{"_raw":"echo hi"}]
//
// The same hold is what the mint sees: an entry that states NO id is numbered
// from its name and arguments, and on the fragmented spelling the mint was taken
// while only a PREFIX of the freeform line had arrived, so one upstream call
// reached the client under two ids — `call_c0daae27` split against
// `call_16fa1c74` whole, and the client ran the model's one call twice.
//
// The fix has two halves, and each was measured alone on the frozen tree:
//
//	a  the hold widened from `argsAreMidObject` to `!finishedObjectArgs`
//	b  a call another call has MOVED ON from is closed, when it holds a
//	   freeform line (not when it holds a half-written object)
//
// `a` alone breaks round 81's F81-L3-3 (`TestANamelessFragmentAfterAClosedCallIsProse`):
// a held freeform call is never `b.cur`, so closeOpen never reaches it and the
// nameless fragment after the NEXT call folded into it. `b` alone fixes nothing
// (31 failures on the frozen tree before it, 31 after). Together: 31 -> 21
// failures and the fuzz differential 15/300 -> 12/300, with no case newly
// disagreeing. `b`'s predicate is the freeform line on purpose: closing a held
// half-written OBJECT there measured RED on three pins of the one-index-per-call
// wire (round 39's B-F7 family, rounds 63 and 77) —
// `TestALateFragmentCompletesTheCallItBelongsTo`,
// `TestACallInProgressIsNotClosedForTheNextOne`,
// `TestASecondNameMidObjectIsTheNextCall`.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round98Blocks reads either an Anthropic SSE stream or a whole message document
// into the ordered blocks a client ends up with: `text:<bytes>` and
// `call:<id>|<name>|<input>`. Chunk granularity (how many deltas carried the
// bytes) is not part of the reading.
func round98Blocks(t *testing.T, body string) []string {
	t.Helper()
	if !strings.Contains(body, "event: ") {
		var doc struct {
			Content []struct {
				Type  string          `json:"type"`
				Text  string          `json:"text"`
				ID    string          `json:"id"`
				Name  string          `json:"name"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if err := json.Unmarshal([]byte(body), &doc); err != nil {
			return []string{"<unparseable>"}
		}
		var out []string
		for _, b := range doc.Content {
			switch b.Type {
			case "text":
				out = append(out, "text:"+b.Text)
			case "tool_use":
				out = append(out, "call:"+b.ID+"|"+b.Name+"|"+string(b.Input))
			default:
				out = append(out, b.Type+":?")
			}
		}
		return out
	}
	type blk struct {
		kind string
		id   string
		name string
		text strings.Builder
		part strings.Builder
	}
	var order []*blk
	byIdx := map[int]*blk{}
	get := func(i int) *blk {
		if b, ok := byIdx[i]; ok {
			return b
		}
		b := &blk{}
		byIdx[i] = b
		order = append(order, b)
		return b
	}
	for _, line := range strings.Split(body, "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Index   int    `json:"index"`
			Content struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
				Text string `json:"text"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				Text        string `json:"text"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(payload), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			b := get(ev.Index)
			b.kind, b.id, b.name = ev.Content.Type, ev.Content.ID, ev.Content.Name
			b.text.WriteString(ev.Content.Text)
		case "content_block_delta":
			b := get(ev.Index)
			if ev.Delta.Type == "text_delta" {
				b.text.WriteString(ev.Delta.Text)
			}
			b.part.WriteString(ev.Delta.PartialJSON)
		}
	}
	var out []string
	for _, b := range order {
		if b.kind == "text" {
			out = append(out, "text:"+b.text.String())
			continue
		}
		raw := b.part.String()
		if strings.TrimSpace(raw) == "" {
			raw = "{}"
		}
		out = append(out, "call:"+b.id+"|"+b.name+"|"+raw)
	}
	return out
}

// round98Spellings runs one upstream body three ways and returns the ordered
// blocks the client was handed: plain (whole document, stream:false), adopted
// (whole document, stream:true) and framed (one fragment per delta).
func round98Spellings(t *testing.T, doc string, frames []string) (blocks [][]string) {
	t.Helper()
	var writes []string
	for _, f := range frames {
		writes = append(writes, f+"\n\n")
	}
	arms := []*httptest.Server{
		round45Upstream(t, "application/json", doc),
		round45Upstream(t, "text/event-stream", doc),
		round45Frames(t, writes...),
	}
	for i, up := range arms {
		srv, _ := round39Gateway(t, up, nil)
		ask := round45AskPlain
		if i > 0 {
			ask = round45AskStream
		}
		status, body := round45Ask(t, srv, ask)
		if status != http.StatusOK {
			t.Fatalf("status %d\n%s", status, body)
		}
		blocks = append(blocks, round98Blocks(t, body))
	}
	return blocks
}

// round98Doc builds a whole OpenAI completion carrying one content and one list
// of entries.
func round98Doc(finish, content, entries string) string {
	tcs := ""
	if entries != "" {
		tcs = `,"tool_calls":[` + entries + `]`
	}
	return `{"id":"chatcmpl-r98","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":` +
		content + tcs + `},"finish_reason":"` + finish +
		`"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
}

func round98Tail(finish string) string {
	return `data: {"choices":[{"index":0,"delta":{},"finish_reason":"` + finish + `"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
}

func round98Frame(inner string) string {
	return `data: {"choices":[{"index":0,"delta":{"tool_calls":[` + inner + `]}}]}`
}

// TestATruncatedCallWithNoObjectArgumentsIsNoCallOnAnyArm is F98-L3-1's pin: a
// turn cut at the token limit whose named call's argument text never began an
// object. Every arm hands the client the turn's prose and no call at all —
// round 80's F80-L3-3, which the fragment arm had applied only to objects.
func TestATruncatedCallWithNoObjectArgumentsIsNoCallOnAnyArm(t *testing.T) {
	for _, stated := range []string{`"echo hi"`, `"\"s\""`, `"[1]"`, `"42"`} {
		t.Run(stated, func(t *testing.T) {
			doc := round98Doc("length", `"hi"`,
				`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":`+stated+`}}`)
			frames := []string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				round98Frame(`{"index":0,"id":"call_1","function":{"name":"Bash","arguments":` + stated + `}}`),
				round98Tail("length"),
			}
			blocks := round98Spellings(t, doc, frames)
			names := []string{"plain", "adopted", "framed"}
			for i := range blocks {
				if strings.Join(blocks[i], "|") != strings.Join(blocks[0], "|") {
					t.Errorf("%s: one truncated turn, two client readings:\n  plain   %v\n  %s %v\n"+
						"  (2026-09-29 audit, round 98, F98-L3-1 — a call whose stated arguments never became an object is not a call, round 80's F80-L3-3)",
						names[i], blocks[0], names[i], blocks[i])
				}
				for _, b := range blocks[i] {
					if strings.HasPrefix(b, "call:") {
						t.Errorf("%s: the client was handed a tool call for a turn cut before its arguments became an object: %s",
							names[i], b)
					}
				}
			}
			t.Logf("blocks %v", blocks[2])
		})
	}
}

// TestAFreeformCallSplitAcrossFragmentsIsOneCall is F98-L3-2's pin: an entry
// that states NO id is numbered by this bridge from the call's name and its
// arguments, so the fragmented spelling must mint the SAME id the whole document
// mints — otherwise the client runs the model's one call twice.
func TestAFreeformCallSplitAcrossFragmentsIsOneCall(t *testing.T) {
	doc := round98Doc("tool_calls", "null",
		`{"index":0,"type":"function","function":{"name":"Bash","arguments":"echo hi"}}`)
	whole := []string{
		round98Frame(`{"index":0,"function":{"name":"Bash","arguments":"echo hi"}}`),
		round98Tail("tool_calls"),
	}
	split := []string{
		round98Frame(`{"index":0,"function":{"name":"Bash","arguments":"echo "}}`),
		round98Frame(`{"index":0,"function":{"arguments":"hi"}}`),
		round98Tail("tool_calls"),
	}
	three := []string{
		round98Frame(`{"index":0,"function":{"name":"Bash","arguments":"ec"}}`),
		round98Frame(`{"index":0,"function":{"arguments":"ho "}}`),
		round98Frame(`{"index":0,"function":{"arguments":"hi"}}`),
		round98Tail("tool_calls"),
	}
	for i, name := range []string{"plain", "adopted", "framed"} {
		w := strings.Join(round98Spellings(t, doc, whole)[i], "|")
		two := strings.Join(round98Spellings(t, doc, split)[i], "|")
		th := strings.Join(round98Spellings(t, doc, three)[i], "|")
		if w != two || w != th {
			t.Errorf("%s: one call, three client readings (2026-09-29 audit, round 98, F98-L3-2 — the fragmented spelling minted a second id and the client ran the model's one call twice):\n  whole   %s\n  two     %s\n  three   %s",
				name, w, two, th)
		}
		if !strings.Contains(w, "echo hi") {
			t.Errorf("%s: the freeform call's bytes reached no client at all: %s", name, w)
		}
		if !strings.Contains(w, "call:") {
			t.Errorf("%s: the freeform call reached no client at all: %s", name, w)
		}
	}
	// The document arms and the fragment arm answer the same wire the same way.
	docArm := strings.Join(round98Spellings(t, doc, split)[0], "|")
	fragArm := strings.Join(round98Spellings(t, doc, split)[2], "|")
	if docArm != fragArm {
		t.Errorf("one call, the document arms handed %s and the fragment arm %s", docArm, fragArm)
	}
	t.Logf("split %s", fragArm)
}

// TestAHeldFreeformCallStillReachesTheClient keeps the freeform line's own
// reading: a call the wire never moved on from is delivered whole, with its
// bytes, when the turn ends (rounds 60 and 81's freeform family) — the hold is
// not a drop.
func TestAHeldFreeformCallStillReachesTheClient(t *testing.T) {
	doc := round98Doc("tool_calls", "null",
		`{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hi"}}`)
	frames := []string{
		round98Frame(`{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"echo hi"}}`),
		round98Tail("tool_calls"),
	}
	for i, name := range []string{"plain", "adopted", "framed"} {
		got := strings.Join(round98Spellings(t, doc, frames)[i], "|")
		if !strings.Contains(got, "echo hi") || !strings.Contains(got, "call:call_1|Bash|") {
			t.Errorf("%s: the freeform call never reached the client: %s", name, got)
		}
	}
}
