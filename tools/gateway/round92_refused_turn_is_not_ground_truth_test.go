package main

// round92_refused_turn_is_not_ground_truth_test.go — leg 3, F92-L3-1
// (2026-09-29 audit, round 92).
//
// The finding: the ground-truth guard that a session's next request is measured
// against — `lastOKAt` for /health's recent-traffic verdict, and the prompt
// calibration ratio for the context-fit clamp — asked `rec.status`, the
// UPSTREAM's status, while the ledger row for the same turn asked the status the
// CLIENT was told. For every other turn those agree; for a turn the /v1/messages
// bridge REFUSES they do not, because the bridge answers 502 over an upstream 200
// (see ledgerStatusWriter). So a refused turn — one the row books with nothing at
// all, 0/0, `usage_seen=false`, `cost_usd=0` — was still the session's ground
// truth.
//
// Measured before the fix, both halves, with the upstream stating usage for a
// document the bridge cannot serve (empty content):
//
//   /health   upstream chat probe 500 → the probe fails, but the refused turn's
//             own usage made it "ok" with `recent_traffic_ok: true` and
//             "a real completion succeeded 0s ago" — a false-healthy verdict on
//             the surface that gates /v1/models. Byte-identical upstream body
//             minus the `usage` object → the same 502 and the same row, but
//             /health said "down".
//   fit clamp turn 1 refused (502, nothing booked) in both arms; turn 2, same
//             session, a 40030-byte body → 400 "prompt is too long: 35026 tokens
//             > 19984 maximum" when the refused turn stated usage, and 200 when
//             it did not. 35026 is exactly the refused turn's own ratio applied
//             to turn 2, so one refused turn changed the client-visible outcome
//             of a later request of that session.
//
// The fix is a hoist: `usageRecorder.clientStatus()` resolves the client's status
// in one place, and both the guard and the row ask it. The controls below are the
// other half of the pin — a genuinely served turn MUST still be ground truth, so
// the guard cannot be closed by simply never firing.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

// r92GroundTruthGateway is round39Gateway with a smaller context (so the fit
// clamp is reachable) and with the health endpoint served from the same gateway.
func r92GroundTruthGateway(t *testing.T, upstream *httptest.Server) (*httptest.Server, *gateway, string) {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{{
			ID: "kat-awq", OwnedBy: "oaica", ContextLength: 20000, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", g.messagesHandler)
	mux.HandleFunc("/health", g.healthHandler)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return srv, g, ledger
}

// r92Upstream serves the gateway's own health probe with 500, a small body with
// `small` and a large one with `large` — so the ONLY evidence /health can fall
// back on is what a real completion did.
func r92Upstream(t *testing.T, small, large string) *httptest.Server {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		if strings.Contains(string(raw), `"ping"`) {
			w.WriteHeader(http.StatusInternalServerError)
			io.WriteString(w, `{"error":{"message":"probe refused"}}`)
			return
		}
		if len(raw) < 1000 {
			io.WriteString(w, small)
			return
		}
		io.WriteString(w, large)
	}))
	t.Cleanup(up.Close)
	return up
}

func r92Post(t *testing.T, srv *httptest.Server, body string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Session-Id", "sess-92")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

func r92Health(t *testing.T, srv *httptest.Server) (int, map[string]any) {
	t.Helper()
	resp, err := http.Get(srv.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var body map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&body)
	return resp.StatusCode, body
}

// r92ServedDoc is a whole, valid completion with usage the upstream stated.
const r92ServedDoc = `{"id":"ok","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10000,"completion_tokens":4}}`

// A refused turn is refused: the client reads a failure, the row books nothing,
// the session's health does not count it as a success, and the session's next
// request is not measured with it.
func TestARefusedTurnIsNotTheSessionsGroundTruth(t *testing.T) {
	// The same refused turn, spelled two ways: with the upstream's usage object
	// and without it. The row is identical in both; everything downstream must
	// be too.
	for _, spelling := range []struct{ name, body string }{
		{"the upstream states usage",
			`{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}],"usage":{"prompt_tokens":28,"completion_tokens":4}}`},
		{"the upstream states none",
			`{"id":"c","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`},
	} {
		srv, _, ledger := r92GroundTruthGateway(t, r92Upstream(t, spelling.body, r92ServedDoc))

		code, body := r92Post(t, srv, `{"model":"kat-awq","max_tokens":8,"messages":[{"role":"user","content":"aaaa"}]}`)
		rows := waitLedger(t, ledger, 1)
		if len(rows) == 0 {
			t.Fatalf("%s: no row was booked", spelling.name)
		}
		row := rows[0]
		if code == http.StatusOK {
			t.Fatalf("%s: premise — the turn was served (%d), so it is not a refusal to test: %.160s", spelling.name, code, body)
		}
		if row.Status != code || row.UsageSeen || row.PromptTokens != 0 || row.CompletionTokens != 0 || row.CostUSD != 0 {
			t.Fatalf("%s: premise — the row does not say nothing happened (status=%d %d/%d seen=%v cost=%v, client read %d)",
				spelling.name, row.Status, row.PromptTokens, row.CompletionTokens, row.UsageSeen, row.CostUSD, code)
		}

		// /health may not call this a recent success: the probe is failing, so
		// the honest answer is down.
		hcode, hbody := r92Health(t, srv)
		if hcode == http.StatusOK {
			t.Errorf("%s: a turn the client was REFUSED (row status %d, nothing booked) was counted as this session's recent success: /health said %d %v. "+
				"A refused turn is not ground truth (F92-L3-1)",
				spelling.name, row.Status, hcode, hbody)
		}

		// And the next request of the same session is not measured with it: a
		// body far past chars/4 (10000 tokens of 40000 bytes, plus margin) fits
		// under the refused turn's own 0.875 ratio and must not be refused.
		big := `{"model":"kat-awq","max_tokens":8,"messages":[{"role":"user","content":"` + strings.Repeat("b", 40000) + `"}]}`
		c2, b2 := r92Post(t, srv, big)
		if c2 != http.StatusOK {
			t.Errorf("%s: a LATER request of the same session answered %d: %.160s\n"+
				"the refused turn's own prompt ratio is not this session's calibration (F92-L3-1)",
				spelling.name, c2, b2)
		}
	}
}

// The other half of the pin: a turn that really was served IS the session's
// ground truth. Without this the guard could be "fixed" by never firing, and the
// recent-traffic fallback this gateway relies on under probe load would be gone.
func TestAServedTurnIsStillTheSessionsGroundTruth(t *testing.T) {
	served := r92ServedDoc
	srv, g, ledger := r92GroundTruthGateway(t, r92Upstream(t, served, served))

	code, body := r92Post(t, srv, `{"model":"kat-awq","max_tokens":8,"messages":[{"role":"user","content":"aaaa"}]}`)
	rows := waitLedger(t, ledger, 1)
	if code != http.StatusOK || len(rows) == 0 || rows[0].Status != http.StatusOK {
		t.Fatalf("premise: the turn was not served (client %d, rows %d): %.160s", code, len(rows), body)
	}
	if g.lastOKAt.Load() == 0 {
		t.Errorf("a served turn (%d, usage stated) did not become the session's ground truth", code)
	}

	// The probe is failing, so /health may only be up on the strength of that
	// served turn — which is exactly the fallback this guard exists for.
	hcode, hbody := r92Health(t, srv)
	if hcode != http.StatusOK || hbody["recent_traffic_ok"] != true {
		t.Errorf("a served turn did not keep /health up while the probe failed: %d %v", hcode, hbody)
	}
	if detail, _ := hbody["detail"].(string); !strings.Contains(detail, "real completion succeeded") {
		t.Errorf("the recent-traffic fallback answered %v, want it to name the real completion", hbody)
	}
}
