package main

// round89_refused_turn_meter_integrity_test.go — leg 3, F89-L3-1 and F89-L3-3
// (2026-09-29 audit, round 89).
//
// The meter scans the upstream's own bytes for usage, and it scans them whether
// or not the turn those bytes spell is one the bridge can serve. Round 88 taught
// the row to book the turn the client was SERVED (usage_seen, the served
// document); this is the other end of the same rule: a turn the client was told
// FAILED was never served at all, so there is no served turn to meter.
//
// F89-L3-1. A stream cut between its usage frame and the sentinel that ends it
// (the gateway asks for stream_options.include_usage, so the usage object IS in
// the last frame before [DONE]) reached the client as a failure and was booked
// as the upstream's own counts with usage_seen=true. Three spellings of the same
// refusal — a usage-only frame with no choices, an error frame carrying usage,
// and a 429 whose body is an SSE usage frame — each booked 9000/500 for a turn
// that produced nothing. The 429 body showed it as two records of one body as
// well: the streamed arm booked 9000/500/seen=true and the buffered arm
// 0/0/seen=false for the same bytes.
//
// F89-L3-3. The round-88 guard asked whether a document had choices; an empty
// completion WITH a choice passed it, so the buffered arm booked 7/3 for a body
// the arm refused as "upstream returned an empty completion" while the streamed
// arm booked nothing for the same body.
//
// One rule at the one place the row is built, where cost is already zeroed for a
// failure (round 45, B45-14): a non-200 row states no counts, no cache and no
// usage_seen.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r89UsageFrame is a frame that states usage and relays nothing.
const r89UsageFrame = `data: {"id":"x","choices":[],"usage":{"prompt_tokens":9000,"completion_tokens":500}}` + "\n\n"

// r89UpstreamStatus serves one body under one status.
func r89UpstreamStatus(t *testing.T, status int, ct, body string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(up.Close)
	return up
}

// r89Turn answers one request from one upstream body served under one status and
// reports the client's status, the whole client body, and the ledger row.
func r89Turn(t *testing.T, status int, ct, body, ask string) (int, string, ledgerEntry) {
	t.Helper()
	srv, ledger := round39Gateway(t, r89UpstreamStatus(t, status, ct, body), nil)
	code, out := round44Raw(t, srv, ask)
	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatalf("no ledger row was written (status %d):\n%s", code, out)
	}
	return code, out, rows[0]
}

// TestARefusedTurnIsBookedAsNothing is the F89-L3-1 and F89-L3-3 pin. The
// spellings of one refusal are listed together because the row must not depend
// on which of them the upstream used: a turn the client was told failed states
// nothing, and the streamed and buffered arms of one body state the same nothing.
func TestARefusedTurnIsBookedAsNothing(t *testing.T) {
	const (
		contentFrame = `data: {"id":"c","choices":[{"index":0,"delta":{"content":"hi"}}]}` + "\n\n"
		finFrame     = `data: {"choices":[{"index":0,"finish_reason":"stop"}]}` + "\n\n"
		errorUsage   = `data: {"error":{"message":"boom","type":"server_error"},"usage":{"prompt_tokens":9000,"completion_tokens":500}}` + "\n\n"
		emptyDoc     = `{"id":"d","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	)
	for _, tc := range []struct {
		name   string
		status int
		ct     string
		body   string
		ask    string
	}{
		{"a usage-only frame and nothing else", 200, "text/event-stream", r89UsageFrame, round44AskStream},
		{"an error frame carrying usage", 200, "text/event-stream", errorUsage, round44AskStream},
		{"content, then a usage frame, then nothing", 200, "text/event-stream", contentFrame + r89UsageFrame, round44AskStream},
		{"a 429 whose body is a usage frame, streamed", 429, "text/event-stream", r89UsageFrame, round44AskStream},
		{"a 429 whose body is a usage frame, buffered", 429, "text/event-stream", r89UsageFrame, round44AskPlain},
		{"an empty completion with a choice, buffered", 200, "application/json", emptyDoc, round44AskPlain},
		{"an empty completion with a choice, streamed", 200, "text/event-stream", emptyDoc, round44AskStream},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, row := r89Turn(t, tc.status, tc.ct, tc.body, tc.ask)
			if row.Status == http.StatusOK {
				t.Fatalf("premise: this turn was served (client %d) — the row below is about a refusal:\n%s", status, body)
			}
			if row.PromptTokens != 0 || row.CompletionTokens != 0 || row.CachedTokens != 0 || row.UsageSeen {
				t.Errorf("a refused turn was booked prompt=%d completion=%d cached=%d usage_seen=%v. The client was answered %d and told no usage at all: the row and the client are two records of ONE turn, and a turn nobody was served has none (2026-09-29 audit, round 89, F89-L3-1/F89-L3-3)",
					row.PromptTokens, row.CompletionTokens, row.CachedTokens, row.UsageSeen, status)
			}
			if row.CostUSD != 0 || row.PriceTier != 0 {
				t.Errorf("a refused turn was charged cost=%g tier=%d (2026-09-29 audit, round 89, F89-L3-1)", row.CostUSD, row.PriceTier)
			}
		})
	}

	// The control: the same frames, terminated, ARE the turn — the row books the
	// counts the client was told, so the rule above zeroes failures and not
	// successful turns that happen to state usage in one frame.
	t.Run("control: the same frames with their terminator are the turn", func(t *testing.T) {
		status, body, row := r89Turn(t, 200, "text/event-stream", contentFrame+r89UsageFrame+finFrame, round44AskStream)
		if status != http.StatusOK {
			t.Fatalf("the served turn answered %d:\n%s", status, body)
		}
		in, _, out := round40Delta(t, body)
		if in != 9000 || out != 500 {
			t.Fatalf("premise: the client was told %d/%d, want the upstream's 9000/500", in, out)
		}
		if row.PromptTokens != 9000 || row.CompletionTokens != 500 || !row.UsageSeen {
			t.Errorf("a served turn was booked prompt=%d completion=%d usage_seen=%v, want the upstream's own 9000/500 and seen=true — zeroing a failure must not zero the turn that was served (2026-09-29 audit, round 89, F89-L3-1)",
				row.PromptTokens, row.CompletionTokens, row.UsageSeen)
		}
	})

	// And the two arms of ONE upstream body are one row, which is what the rule
	// is for: the buffered arm could not have read the SSE body's usage at all.
	t.Run("one 429 body, two arms, one row", func(t *testing.T) {
		_, _, streamed := r89Turn(t, 429, "text/event-stream", r89UsageFrame, round44AskStream)
		_, _, buffered := r89Turn(t, 429, "text/event-stream", r89UsageFrame, round44AskPlain)
		// Stream is the arm the row records, not a reading of the turn: the
		// usage-bearing fields are what the two arms must agree on.
		if streamed.Status != buffered.Status || streamed.PromptTokens != buffered.PromptTokens ||
			streamed.CompletionTokens != buffered.CompletionTokens || streamed.CachedTokens != buffered.CachedTokens ||
			streamed.UsageSeen != buffered.UsageSeen || streamed.CostUSD != buffered.CostUSD {
			t.Errorf("one upstream body metered two ways: streamed %d/%d cache=%d seen=%v status=%d cost=%g, buffered %d/%d cache=%d seen=%v status=%d cost=%g. Which arm carried it must not decide what the row says about a turn the upstream refused (2026-09-29 audit, round 89, F89-L3-1)",
				streamed.PromptTokens, streamed.CompletionTokens, streamed.CachedTokens, streamed.UsageSeen, streamed.Status, streamed.CostUSD,
				buffered.PromptTokens, buffered.CompletionTokens, buffered.CachedTokens, buffered.UsageSeen, buffered.Status, buffered.CostUSD)
		}
	})
}

// TestABodyWithChoicesAndNoAnswerIsRefusedByBothArms is the F89-L3-3 control
// pair: the refused body above and a body that says something differ only in
// whether the completion carries an answer.
func TestABodyWithChoicesAndNoAnswerIsRefusedByBothArms(t *testing.T) {
	const empty = `{"id":"d","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	for _, arm := range []struct{ name, ct, ask string }{
		{"buffered", "application/json", round44AskPlain},
		{"streamed", "text/event-stream", round44AskStream},
	} {
		t.Run(arm.name, func(t *testing.T) {
			status, body, row := r89Turn(t, 200, arm.ct, empty, arm.ask)
			if status != 502 {
				t.Errorf("a completion with no answer answered %d, want 502 (2026-09-29 audit, round 89, F89-L3-3):\n%s", status, body)
			}
			if row.PromptTokens != 0 || row.CompletionTokens != 0 || row.UsageSeen {
				t.Errorf("the %s arm booked prompt=%d completion=%d usage_seen=%v for a turn whose completion carries no answer (2026-09-29 audit, round 89, F89-L3-3)",
					arm.name, row.PromptTokens, row.CompletionTokens, row.UsageSeen)
			}
		})
	}

	t.Run("control: an answer in the same document is the turn", func(t *testing.T) {
		const saysSomething = `{"id":"d","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
		if _, body, row := r89Turn(t, 200, "application/json", saysSomething, round44AskPlain); !strings.Contains(body, "hi") || row.PromptTokens != 7 || row.CompletionTokens != 3 {
			t.Errorf("a served document was booked prompt=%d completion=%d for a client that read %q, want 7/3 (2026-09-29 audit, round 89, F89-L3-3)", row.PromptTokens, row.CompletionTokens, body)
		}
	})
}
