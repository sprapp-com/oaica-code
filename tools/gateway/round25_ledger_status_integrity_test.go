package main

// round25_ledger_status_integrity_test.go — the ledger recorded the upstream's
// status for a turn the client read as a failure (2026-09-27 audit, round 25).
//
// Round 24 gave the /v1/messages bridge a status it chooses itself: an upstream
// 200 whose body carries no answer is answered 502, and the bridge holds the
// commit until it knows the outcome. The decision lives in finalize(), which
// messagesHandler calls AFTER completionHandler returns — and completionHandler
// is what writes the ledger row, from usageRecorder.status, which is the
// upstream's own status. So a turn the client read as "502, retry" was billed
// and reported as a successful 200: the ledger is the record operators and the
// cost math read, and it claimed an answer that was never sent.

import (
	"encoding/json"
	"net/http"
	"os"
	"strings"
	"testing"
)

// ledgerStatusOf posts one /v1/messages request against a broken upstream and
// returns the client's status beside the status the ledger recorded for it.
func ledgerStatusOf(t *testing.T, upstreamStatus int, contentType, body string, stream bool) (int, int, string) {
	t.Helper()
	up := brokenUpstream(t, upstreamStatus, contentType, body)
	ledger := t.TempDir() + "/ledger.jsonl"
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-test"), Label: "test"}},
		Models: []gwModel{{
			ID: "oaica-35b-a3b-vision", UpstreamID: "oaica-35b-a3b-vision", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	req := map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	}
	if stream {
		req["stream"] = true
	}
	w := postMessages(t, g, "sk-test", req)

	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	line := strings.TrimSpace(string(raw))
	if line == "" {
		t.Fatalf("no ledger row was written for a turn that reached the upstream (client status %d)", w.Code)
	}
	var row struct {
		Status int `json:"status"`
	}
	if err := json.Unmarshal([]byte(line), &row); err != nil {
		t.Fatalf("ledger row is not JSON (%v): %s", err, line)
	}
	return w.Code, row.Status, line
}

// TestLedgerRecordsTheFailureTheClientSaw: an upstream 200 with no answer is
// answered 502, and the ledger row for that turn must say 502 too — a ledger
// that reports 200 for a turn that sent nothing is a false success in the one
// record kept of what happened.
func TestLedgerRecordsTheFailureTheClientSaw(t *testing.T) {
	client, recorded, line := ledgerStatusOf(t, http.StatusOK, "application/json", `{"id":"chatcmpl-x","choices":[]}`, false)
	if client != http.StatusBadGateway {
		t.Fatalf("premise: the client saw %d, want the bridge's 502", client)
	}
	if recorded != client {
		t.Errorf("ledger status = %d for a turn the client read as %d: the row records the upstream's 200 for a failure the bridge itself decided (%s)", recorded, client, line)
	}
}

// TestLedgerRecordsAnUntranslatableBodyTheSameWay is the same defect via the
// other branch: the body is not JSON at all.
func TestLedgerRecordsAnUntranslatableBodyTheSameWay(t *testing.T) {
	client, recorded, line := ledgerStatusOf(t, http.StatusOK, "application/json", "<html>gateway timeout</html>", false)
	if client != http.StatusBadGateway {
		t.Fatalf("premise: the client saw %d, want 502", client)
	}
	if recorded != client {
		t.Errorf("ledger status = %d for a turn the client read as %d (%s)", recorded, client, line)
	}
}

// TestLedgerRecordsAnEmptyStreamTheSameWay is the streaming branch of the same
// row: the upstream ended with no event at all.
func TestLedgerRecordsAnEmptyStreamTheSameWay(t *testing.T) {
	client, recorded, line := ledgerStatusOf(t, http.StatusOK, "text/event-stream", "", true)
	if client < 400 {
		t.Fatalf("premise: the client saw %d for an empty stream, want a failure status", client)
	}
	if recorded != client {
		t.Errorf("ledger status = %d for a turn the client read as %d (%s)", recorded, client, line)
	}
}

// TestLedgerStillRecordsTheUpstreamsOwnError is the control: when the upstream
// itself fails, the bridge surfaces that status verbatim (translated into the
// Anthropic envelope) and the ledger must keep recording it — the fix may not
// flatten every failure into 502.
func TestLedgerStillRecordsTheUpstreamsOwnError(t *testing.T) {
	client, recorded, line := ledgerStatusOf(t, http.StatusTooManyRequests, "application/json",
		`{"error":{"message":"rate limited by the box","type":"rate_limit_error"}}`, false)
	if client != http.StatusTooManyRequests {
		t.Fatalf("premise: the client saw %d, want the upstream's 429", client)
	}
	if recorded != http.StatusTooManyRequests {
		t.Errorf("ledger status = %d, want the upstream's own 429 (%s)", recorded, line)
	}
}

// TestLedgerStillRecordsASuccessfulTurn is the other control: a normal
// completion keeps its 200 in the ledger, so the fix is about failures only.
func TestLedgerStillRecordsASuccessfulTurn(t *testing.T) {
	client, recorded, line := ledgerStatusOf(t, http.StatusOK, "application/json",
		`{"id":"chatcmpl-ok","choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`, false)
	if client != http.StatusOK {
		t.Fatalf("premise: the client saw %d, want 200", client)
	}
	if recorded != http.StatusOK {
		t.Errorf("ledger status = %d for a successful turn (%s)", recorded, line)
	}
	if !strings.Contains(line, `"prompt_tokens":11`) {
		t.Errorf("the successful turn's usage is missing from the row: %s", line)
	}
}
