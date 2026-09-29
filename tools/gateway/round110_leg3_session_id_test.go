package main

// round110_leg3_session_id_test.go — leg 3, round 110 (2026-09-29 audit), F110-L3-1.
//
// X-Session-Id is chosen by the client and was stored at full length — up to the
// 64 KiB header limit — in the ledger row, the upstream error log and the context
// calibrator's key. A holder of any valid key could grow the ledger about 180x
// faster than an ordinary request does, and pin 4096 keys of that size in the
// calibrator map, whose bound counts entries and not bytes. The id is kept as
// the client's own when it is a sane length, and otherwise replaced by a bounded
// form that is still stable per id, so a session keeps its identity.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMine110AHugeSessionIdIsBoundedInTheLedger(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	t.Cleanup(up.Close)
	srv, ledger := r107Gw(t, up, nil)
	post := func(sid string) {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`))
		req.Header.Set("Authorization", "Bearer sk")
		req.Header.Set("Content-Type", "application/json")
		if sid != "" {
			req.Header.Set("X-Session-Id", sid)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	huge := strings.Repeat("s", 60000)
	post(huge)
	post(huge[:59999] + "t") // a different id with the same long prefix
	post("ordinary-session-1")
	// The row is appended when the handler returns, after the client has its
	// response, so wait for all three rather than read a partial ledger.
	var rows []string
	for deadline := time.Now().Add(3 * time.Second); ; {
		b, _ := os.ReadFile(ledger)
		rows = strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(rows) >= 3 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if len(rows) != 3 {
		t.Fatalf("premise: %d ledger rows", len(rows))
	}
	for i, r := range rows[:2] {
		if len(r) > 2048 {
			t.Errorf("row %d is %d bytes — a client-chosen session id must not decide how large a ledger row is (2026-09-29 audit, round 110, F110-L3-1)", i, len(r))
		}
	}
	if !strings.Contains(rows[2], `"ordinary-session-1"`) {
		t.Errorf("an ordinary session id was not kept as the client sent it: %s (2026-09-29 audit, round 110, F110-L3-1)", rows[2])
	}
	// Two different long ids stay two different sessions.
	if sid := func(r string) string { i := strings.Index(r, `"session_id":"`); return r[i:min(len(r), i+400)] }; sid(rows[0]) == sid(rows[1]) {
		t.Errorf("two different long session ids collapsed to one identity in the ledger (2026-09-29 audit, round 110, F110-L3-1)")
	}
}

func TestMine110SessionIdBoundKeepsTheIdentityStable(t *testing.T) {
	a, b := boundedSessionID(strings.Repeat("x", 500)), boundedSessionID(strings.Repeat("x", 500))
	c := boundedSessionID(strings.Repeat("x", 499) + "y")
	if a != b || a == c || len(a) > 128 {
		t.Errorf("bounded ids: same=%v distinct=%v len=%d, want stable, distinct and at most 128 (2026-09-29 audit, round 110, F110-L3-1)", a == b, a != c, len(a))
	}
	if got := boundedSessionID("short-id"); got != "short-id" {
		t.Errorf("a short id was rewritten to %q (2026-09-29 audit, round 110, F110-L3-1)", got)
	}
}

// TestMine110AHugeSessionIdIsBoundedInTheCalibrator pins the calibrator's key: it
// is stored per session, and its bound counts entries, not bytes.
func TestMine110AHugeSessionIdIsBoundedInTheCalibrator(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"}}],"usage":{"prompt_tokens":10,"completion_tokens":2}}`)
	}))
	t.Cleanup(up.Close)
	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "ledger.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:  []gwModel{{ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768, Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	t.Cleanup(srv.Close)
	for i := 0; i < 3; i++ {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`))
		req.Header.Set("Authorization", "Bearer sk")
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("X-Session-Id", strings.Repeat("s", 50000)+string(rune('a'+i)))
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
	}
	c := g.calibrator()
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.samples) != 3 {
		t.Fatalf("premise: the calibrator holds %d sessions, want the 3 distinct ones", len(c.samples))
	}
	for k := range c.samples {
		if len(k) > 512 {
			t.Errorf("a calibrator key is %d bytes — its bound counts entries, so the key's size must be bounded too (2026-09-29 audit, round 110, F110-L3-1)", len(k))
		}
	}
}
