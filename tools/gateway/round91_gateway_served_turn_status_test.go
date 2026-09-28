package main

// round91_gateway_served_turn_status_test.go — leg 3, F91-L3-1 (2026-09-29
// audit, round 91). RECORDED, NOT FIXED.
//
// The finding: an upstream that answers a sub-400 non-200 status with a whole,
// valid completion is served to the client as a normal 200 turn — the bridge
// swallows codes below 400 and commits 200 — while the ledger row for that same
// turn books the UPSTREAM's code and wipes the metering (`main.go`:
// `if status != http.StatusOK { u, cached, seen = usage{}, 0, false; cost,
// tier = 0, 0 }`). So a turn the client was handed, and which the upstream
// reported usage for, is recorded with a status the client never read, 0/0
// tokens, `usage_seen=false` and `cost_usd=0`.
//
// Measured (round 91, my own probe, both the plain and the streaming arm):
//   upstream 200 + document (control) -> client 200; row 200, 11/4, seen, cost>0
//   upstream 4xx/5xx                  -> client and row both that code, nothing booked
//   upstream 204 and 304 + document   -> client 502 "unparseable upstream response",
//                                        row agrees with the client (those codes
//                                        carry no body, so no turn is served)
//   every other sub-400 non-200       -> client 200 with the answer; the row books
//                                        that code, 0/0, seen=false, cost=0
//
// Why recorded rather than fixed: no producer of this shape lives in this
// repository. Every upstream this gateway is tested against — vLLM and ollama —
// answers 200 for a served turn and 4xx/5xx for a refused one, which the row
// already books correctly. The divergent band is reachable only through a
// third-party `upstream_addr`: a load balancer or CDN in front of the model that
// rewrites a success into 2xx-non-200 or 3xx, or a redirect that lands on the
// document. Per the audit doctrine a finding with no live producer is recorded
// with its reasoning instead of fixed (F68-L1-1 precedent).
//
// This test does not bless the divergence. It holds the measurement so a later
// change to this path must confront it: it fails if any status in the band is
// repaired — or if the controls stop behaving — and repairing the band without
// updating this record is the failure the doctrine intends. A fix must decide
// what a served turn under a sub-400 non-200 upstream is booked as, and must
// leave the 200, 204/304 and 4xx/5xx behaviour unchanged.
//
// Two statuses are deliberately absent from the band: 204 and 304, which carry
// no body and so serve no turn. They are asserted as controls below, so a change
// that moves them into the band is caught too.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r91recDoc is a whole, valid completion: one turn, with the usage the upstream
// reported for it.
func r91recDoc() string {
	return `{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`
}

// r91recUpstream answers every request with `status` and that document.
func r91recUpstream(t *testing.T, status int) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		fmt.Fprint(w, r91recDoc())
	}))
	t.Cleanup(up.Close)
	return up
}

// r91recRow drives one arm through the gateway and returns the status the client
// was told, the body it read, and the ledger row booked for the same turn.
func r91recRow(t *testing.T, upstreamStatus int, ask string) (int, string, ledgerEntry) {
	t.Helper()
	srv, ledger := round39Gateway(t, r91recUpstream(t, upstreamStatus), nil)
	code, body := round44Raw(t, srv, ask)
	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatalf("upstream %d: no ledger row was booked for a turn the client read as %d: %s", upstreamStatus, code, body)
	}
	return code, body, rows[0]
}

var r91recArms = []struct{ name, ask string }{
	{"plain", round44AskPlain},
	{"stream", round44AskStream},
}

// TestAServedTurnUnderASubFourHundredNon200IsRecordedNotMetered is the record of
// F91-L3-1. Each case is a turn the client was served; what the row says about
// it is the divergence this test exists to keep visible.
func TestAServedTurnUnderASubFourHundredNon200IsRecordedNotMetered(t *testing.T) {
	band := []struct {
		status int
		name   string
	}{
		{http.StatusCreated, "201"},
		{http.StatusAccepted, "202"},
		{http.StatusNonAuthoritativeInfo, "203"},
		{http.StatusResetContent, "205"},
		{http.StatusPartialContent, "206"},
		{http.StatusMultiStatus, "207"},
		{http.StatusAlreadyReported, "208"},
		{http.StatusIMUsed, "226"},
		{http.StatusMultipleChoices, "300"},
		{http.StatusMovedPermanently, "301"},
		{http.StatusFound, "302"},
		{http.StatusSeeOther, "303"},
		{http.StatusTemporaryRedirect, "307"},
		{http.StatusPermanentRedirect, "308"},
	}
	for _, s := range band {
		for _, arm := range r91recArms {
			code, body, row := r91recRow(t, s.status, arm.ask)

			// The client's half: the turn really was served, so this is a turn
			// that happened and not a refusal.
			if code != http.StatusOK || !strings.Contains(body, "hello") {
				t.Fatalf("premise changed: upstream %s/%s answered the client %d without the served turn: %.200s",
					s.name, arm.name, code, body)
			}
			// The row's half: the divergence F91-L3-1 records.
			if row.Status == code {
				t.Errorf("F91-L3-1 looks repaired: upstream %s/%s, the client read %d and the row now books %d too. "+
					"The row's status is the status the client was told — update this record (tools/gateway/round91_gateway_served_turn_status_test.go) and close the finding.",
					s.name, arm.name, code, row.Status)
			}
			if row.UsageSeen || row.PromptTokens != 0 || row.CompletionTokens != 0 || row.CostUSD != 0 {
				t.Errorf("F91-L3-1 looks repaired: upstream %s/%s, a served turn with upstream usage was booked %d/%d seen=%v cost=%v "+
					"(the client read %d and was told the answer). A turn that is served is now metered — update this record and close the finding.",
					s.name, arm.name, row.PromptTokens, row.CompletionTokens, row.UsageSeen, row.CostUSD, code)
			}
		}
	}
}

// TestTheServedTurnStatusBandStopsWhereTheBodyStops pins the two edges of the
// record: a 200 is metered (so the divergence is the band's, not the document's),
// a 4xx books nothing on both sides, and the two bodyless sub-400 non-200 codes
// serve no turn at all and must stay out of the band.
func TestTheServedTurnStatusBandStopsWhereTheBodyStops(t *testing.T) {
	// The 200 control: the same document, metered.
	for _, arm := range r91recArms {
		code, body, row := r91recRow(t, http.StatusOK, arm.ask)
		if code != http.StatusOK || !strings.Contains(body, "hello") {
			t.Fatalf("the 200/%s control answered %d: %.200s", arm.name, code, body)
		}
		if row.Status != http.StatusOK || !row.UsageSeen || row.PromptTokens != 11 || row.CompletionTokens != 4 || row.CostUSD <= 0 {
			t.Errorf("the 200/%s control booked status=%d %d/%d seen=%v cost=%v, want 200 11/4 seen=true cost>0 — "+
				"a served turn under a 200 is metered", arm.name, row.Status, row.PromptTokens, row.CompletionTokens, row.UsageSeen, row.CostUSD)
		}
	}

	// A refusal is the client's status on both sides and books nothing.
	for _, arm := range r91recArms {
		code, body, row := r91recRow(t, http.StatusTooManyRequests, arm.ask)
		if code != http.StatusTooManyRequests || row.Status != code {
			t.Errorf("a 429/%s reached the client as %d and the row as %d, want 429 on both — the row's status is the status the client was told: %.120s",
				arm.name, code, row.Status, body)
		}
		if row.UsageSeen || row.PromptTokens != 0 || row.CompletionTokens != 0 || row.CostUSD != 0 {
			t.Errorf("a refused turn (429/%s) was metered %d/%d seen=%v cost=%v, want nothing booked",
				arm.name, row.PromptTokens, row.CompletionTokens, row.UsageSeen, row.CostUSD)
		}
	}

	// 204 and 304 carry no body, so even with a document written after the
	// status line no turn is served: the client is told a 502 and the row agrees.
	// If either ever starts serving the document it joins the band above.
	for _, status := range []int{http.StatusNoContent, http.StatusNotModified} {
		for _, arm := range r91recArms {
			code, _, row := r91recRow(t, status, arm.ask)
			if code == http.StatusOK {
				t.Errorf("upstream %d/%s now serves a turn (client read 200); it belongs in the recorded band above — update this record",
					status, arm.name)
			}
			if row.Status != code {
				t.Errorf("upstream %d/%s: the client read %d and the row booked %d; for a body that carries no turn the two must agree",
					status, arm.name, code, row.Status)
			}
		}
	}
}
