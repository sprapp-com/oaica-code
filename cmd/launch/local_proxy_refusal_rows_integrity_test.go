package launch

// local_proxy_refusal_rows_integrity_test.go — a turn the logging proxy refused
// locally wrote no row at all (2026-09-26 audit, ninth round, auditor B).
//
// RunLocalLoggingProxy builds its row behind the body read, so every refusal
// that happens before it returned with no evidence: a body over
// httpbody.DefaultMax answered 413 and left requests.log empty, and `oaica
// usage` then printed "No launch traffic logged yet" — ERR 0 — for a session
// whose every turn was refused. That is the same reading the transport-failure
// and truncated-turn fixes were made for, one branch further up.
//
// The row is now created at the top of the handler and written from its defer,
// so the body-derived fields are simply absent on the refusals that never got
// one.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// httptestNewCountingServer answers every request with a clean turn and counts
// the hits, so a test can assert that a refused request never reached it.
func httptestNewCountingServer(t *testing.T, hits *atomic.Int32) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
}

// A body over the cap is unambiguously a turn attempt: it must be in the report
// as an error, and it must not have cost the upstream anything.
func TestAnOversizedTurnIsReported(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	oldMax := localLoggingProxyMaxBytes
	localLoggingProxyMaxBytes = 1024
	t.Cleanup(func() { localLoggingProxyMaxBytes = oldMax })

	var hits atomic.Int32
	upstream := httptestNewCountingServer(t, &hits)
	defer upstream.Close()

	proxy := startLocalLoggingProxy(t, upstream.URL)

	// One byte over the cap, so this is the refusal and not a rounding error.
	body := bytes.Repeat([]byte("x"), int(localLoggingProxyMaxBytes)+1)
	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST an oversized turn: %v", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)

	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("premise: the proxy answered %d for a body over its cap, want 413", resp.StatusCode)
	}
	if n := hits.Load(); n != 0 {
		t.Errorf("a refused body still reached the upstream (%d hit(s))", n)
	}

	requests, errors := reportedTurn(t)
	if requests != 1 {
		t.Errorf("`oaica usage` reports %d requests for a turn the proxy refused with a 413, want 1 — the row was built behind the body read, so the refusal left no evidence and the report says the session sent nothing", requests)
	}
	if errors != 1 {
		t.Errorf("`oaica usage` reports %d errors for a refused turn, want 1 — ERR 0 is the reading a user opens the report to rule out", errors)
	}
}
