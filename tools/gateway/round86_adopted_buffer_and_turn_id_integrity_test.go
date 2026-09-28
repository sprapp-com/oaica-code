package main

// round86_adopted_buffer_and_turn_id_integrity_test.go — leg 3, F86-L3-1 and
// F86-L3-2 (2026-09-28 audit, round 86).
//
// F86-L3-1. The gateway reads one upstream body three ways: as a stream of
// fragments, as a stream whose frames each carry a whole completion, and as a
// plain document. The two streamed spellings buffer the turn so a whole
// completion can be recognised inside the stream — that is `adoptWholeStream` —
// and the buffer was measured against a 1 MiB limit meant for how much of an
// SSE *tail* to keep, while the plain path measures its buffer against
// `bufCapOK` (8<<20). One turn whose body passed 1 MiB was therefore REFUSED on
// both streaming spellings and served on the plain one, and the ledger rows
// disagreed about the same body: the two streamed arms wrote no usage at all
// where the plain arm metered the turn. The limit the buffering is measured
// against is the size the plain arm already accepted. The meter's own
// partial-line buffer carried the same 1 MiB bound, so once the refusal was
// lifted the streamed rows still read usage_seen=false for a turn the plain arm
// booked usage_seen=true — the same body served must be the same body billed
// (F86-L3-1).
//
// F86-L3-2. The id the turn is announced under. The document arms mint it from
// the upstream body's own id, so both spellings of the same upstream agree; the
// frame arm minted one of its own, and every consumer that carries the id — the
// client's transcript, the ledger's request_id — saw two names for one body
// (F86-L3-2).
//
// Measured on 2026-09-28 before the fix. One 1200 KiB turn: adopt 502, frame
// 502, plain 200. Message id for an upstream that states none: adopt
// msg_18d98533d4978145, frame msg_18d98533d4a34b7d, plain msg_18d98533d4b4ef6e
// — a fresh id per request is right when the upstream states none, and the pin
// below asks only about the stated case.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// r86Leg3Doc is a whole completion carrying one Read call whose one argument is
// `pad` bytes long. The usage is stated so the metering rows have something to
// agree about.
func r86Leg3Doc(id, pad string) string {
	return `{"id":"` + id + `","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hi",` +
		`"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"p\":\"` + pad + `\"}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
}

// r86Leg3Turn answers one request from one upstream body and reports the status,
// the whole client body, and the ledger row written for it.
func r86Leg3Turn(t *testing.T, ct, upstreamBody, ask string) (int, string, ledgerEntry) {
	t.Helper()
	up := round44Upstream(t, nil, ct, upstreamBody)
	srv, ledger := round39Gateway(t, up, nil)
	status, body := round44Raw(t, srv, ask)
	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatalf("no ledger row was written for a turn the client was served (status %d):\n%s", status, body)
	}
	return status, body, rows[0]
}

// r86Leg3Arg is the arguments the client was handed for the turn's one call, in
// one spelling for both media: the assembled `partial_json` of a stream, or the
// tool_use block's input of a document, each canonically remarshalled.
func r86Leg3Arg(t *testing.T, body string) string {
	t.Helper()
	var raw string
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		var doc struct {
			Content []struct {
				Type  string          `json:"type"`
				Input json.RawMessage `json:"input"`
			} `json:"content"`
		}
		if json.Unmarshal([]byte(body), &doc) != nil {
			t.Fatalf("the document arm's own body does not parse:\n%s", body)
		}
		for _, c := range doc.Content {
			if c.Type == "tool_use" {
				raw = string(c.Input)
				break
			}
		}
	} else {
		for _, line := range strings.Split(body, "\n") {
			raw2, ok := strings.CutPrefix(line, "data: ")
			if !ok || !strings.Contains(raw2, "input_json_delta") {
				continue
			}
			var ev struct {
				Delta struct {
					PartialJSON string `json:"partial_json"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(raw2), &ev) != nil {
				continue
			}
			raw += ev.Delta.PartialJSON
		}
	}
	if raw == "" {
		return ""
	}
	var any1 any
	if json.Unmarshal([]byte(raw), &any1) != nil {
		return raw
	}
	b, err := json.Marshal(any1)
	if err != nil {
		return raw
	}
	return string(b)
}

// r86Leg3StartID is the id the turn is announced under — the message_start of a
// stream, or the top-level id of a document.
func r86Leg3StartID(t *testing.T, body string) string {
	t.Helper()
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		var m struct {
			ID string `json:"id"`
		}
		_ = json.Unmarshal([]byte(body), &m)
		return m.ID
	}
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type    string `json:"type"`
			Message struct {
				ID string `json:"id"`
			} `json:"message"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil || ev.Type != "message_start" {
			continue
		}
		return ev.Message.ID
	}
	return "<none>"
}

// TestALargeTurnIsServedTheSameOnEverySpelling is the F86-L3-1 pin: the size a
// streamed turn may reach is the size the plain arm already accepts, and the
// three spellings of it agree on the call and on what was metered.
func TestALargeTurnIsServedTheSameOnEverySpelling(t *testing.T) {
	for _, n := range []int{900 << 10, 1200 << 10} {
		pad := strings.Repeat("a", n)
		doc := r86Leg3Doc("chatcmpl-big", pad)
		t.Run(map[bool]string{true: "over the meter's old tail limit", false: "under the meter's old tail limit"}[n > 1<<20], func(t *testing.T) {
			// The plain arm is the reference: it is the spelling the buffer limit
			// must not be smaller than.
			plainStatus, plainBody, plainRow := r86Leg3Turn(t, "application/json", doc, round44AskPlain)
			if plainStatus != http.StatusOK {
				t.Fatalf("premise: the plain arm refused a %d-byte turn (status %d):\n%s", len(doc), plainStatus, plainBody)
			}
			want := r86Leg3Arg(t, plainBody)
			if len(want) < n {
				t.Fatalf("premise: the plain arm's call holds %d bytes of argument, want at least the %d the upstream stated", len(want), n)
			}

			for _, arm := range []struct {
				name string
				ct   string
				body string
				ask  string
			}{
				{"adopted", "text/event-stream", doc, round44AskStream},
				{"framed", "text/event-stream", "data: " + doc + "\n\ndata: [DONE]\n\n", round44AskStream},
			} {
				status, body, row := r86Leg3Turn(t, arm.ct, arm.body, arm.ask)
				if status != http.StatusOK {
					t.Errorf("the %s spelling of the same %d-byte turn answered %d while the plain one answered 200 — the buffer a streamed turn is measured against is the size the plain arm accepts (2026-09-28 audit, round 86, F86-L3-1):\n%s",
						arm.name, len(doc), status, body)
					continue
				}
				if got := r86Leg3Arg(t, body); got != want {
					t.Errorf("the %s spelling handed the client %d bytes of argument where the plain one handed %d — one body, one call, with its own bytes (2026-09-28 audit, round 86, F86-L3-1)",
						arm.name, len(got), len(want))
				}
				if row.UsageSeen != plainRow.UsageSeen || row.PromptTokens != plainRow.PromptTokens ||
					row.CompletionTokens != plainRow.CompletionTokens || row.CostUSD != plainRow.CostUSD {
					t.Errorf("the %s spelling metered usage_seen=%v prompt=%d completion=%d cost=%v where the plain one recorded usage_seen=%v prompt=%d completion=%d cost=%v — the same body served must be the same body billed (2026-09-28 audit, round 86, F86-L3-1)",
						arm.name, row.UsageSeen, row.PromptTokens, row.CompletionTokens, row.CostUSD,
						plainRow.UsageSeen, plainRow.PromptTokens, plainRow.CompletionTokens, plainRow.CostUSD)
				}
			}
		})
	}
}

// TestAStatedTurnIDIsTheIDEverySpellingAnnounces is the F86-L3-2 pin: when the
// upstream states the id of the turn, the streamed spelling announces that id
// rather than minting one of its own.
func TestAStatedTurnIDIsTheIDEverySpellingAnnounces(t *testing.T) {
	doc := r86Leg3Doc("chatcmpl-abc", "x")
	_, adopt, _ := r86Leg3Turn(t, "text/event-stream", doc, round44AskStream)
	_, framed, _ := r86Leg3Turn(t, "text/event-stream", "data: "+doc+"\n\ndata: [DONE]\n\n", round44AskStream)
	_, plain, _ := r86Leg3Turn(t, "application/json", doc, round44AskPlain)

	want := r86Leg3StartID(t, plain)
	if want == "" || want == "<none>" {
		t.Fatalf("premise: the plain arm announced the turn under no id at all:\n%s", plain)
	}
	for _, arm := range []struct{ name, body string }{
		{"the adopted spelling", adopt},
		{"the framed spelling", framed},
	} {
		if got := r86Leg3StartID(t, arm.body); got != want {
			t.Errorf("%s announced the turn as %q where the upstream stated %q — every consumer that carries the id (the client transcript, the ledger's request_id) must read the same name for the same body (2026-09-28 audit, round 86, F86-L3-2)",
				arm.name, got, want)
		}
	}
}
