package launch

// local_logging_proxy_rows_integrity_test.go — the local logging proxy (the
// one every `oaica launch claude` routes through, whether the destination is
// the cloud router or a local `oaica serve`) recorded two kinds of failed turn
// as clean ones (2026-09-26 audit):
//
//  1. a transport failure wrote no row at all — the row was created behind the
//     upstream call, so a refused connection / DNS / TLS failure returned early
//     and `oaica usage` reported ERR 0 for a session whose every turn failed;
//  2. a 2xx whose body was cut short mid-answer was recorded with the status
//     byte (200) rather than the outcome, so a session in which every turn was
//     truncated also reported ERR 0. An upstream that answers 200 and closes
//     without a byte is the same thing by the same argument.
//
// The assertions here are on the report a user reads (LoadUsageStats), not on
// the log line: Requests/Errors are the columns the symptom was reported in.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func startLocalLoggingProxy(t *testing.T, target string) string {
	t.Helper()
	ln, port, err := ListenLocalLoggingProxy()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = RunLocalLoggingProxy(ln, target) }()
	return fmt.Sprintf("http://127.0.0.1:%d", port)
}

// postProxyTurn sends one /v1/messages turn and reads the answer to its end.
// A read error is tolerated (a truncated relay is one of the cases under test);
// the read must still happen, or the handler's relay has nowhere to put the
// body and the turn becomes a client-gone instead.
func postProxyTurn(t *testing.T, proxyURL string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "m", "max_tokens": 8, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// reportedTurn returns the Requests/Errors the report shows for the one turn
// this test sent. It polls: the proxy writes its row from a defer inside the
// handler goroutine, which can run after the client has already read the last
// byte. A missing row is therefore what it returns when the deadline passes,
// not an error — that is one of the cases under test.
func reportedTurn(t *testing.T) (int, int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	requests, errors := 0, 0
	for time.Now().Before(deadline) {
		requests, errors = 0, 0
		stats, err := LoadUsageStats(UsageStatsFilter{})
		if err != nil {
			t.Fatalf("LoadUsageStats: %v", err)
		}
		for _, r := range stats {
			requests += r.Requests
			errors += r.Errors
		}
		if requests >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	return requests, errors
}

// Control: a turn that arrived must stay a clean turn, or the two fixes below
// would be indistinguishable from "log everything as an error".
func TestACleanLocalProxyTurnIsReportedClean(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	postProxyTurn(t, startLocalLoggingProxy(t, upstream.URL))
	requests, errors := reportedTurn(t)
	if requests != 1 || errors != 0 {
		t.Errorf("`oaica usage` reports %d requests / %d errors for one clean turn, want 1 / 0", requests, errors)
	}
}

// (1) A turn whose upstream refused the connection must be in the report.
func TestATransportFailureIsReported(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	// The package's conventional dead address: nothing listens on port 1.
	postProxyTurn(t, startLocalLoggingProxy(t, "http://127.0.0.1:1"))

	requests, errors := reportedTurn(t)
	if requests != 1 {
		t.Errorf("`oaica usage` reports %d requests for a turn that failed to reach its upstream, want 1 — the row was written only after a successful upstream call, so a dead backend left no evidence at all", requests)
	}
	if errors != 1 {
		t.Errorf("`oaica usage` reports %d errors, want 1 — ERR 0 is the reading a user opens the report to rule out", errors)
	}
}

// (2) A body cut short after the headers must not read as a clean turn.
func TestATruncatedTurnIsNotReportedClean(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Declared longer than what is written, then the handler returns: the
		// connection closes mid-body, which is what a backend that dies during
		// an answer looks like from here.
		w.Header().Set("Content-Length", "4096")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","content":[`))
	}))
	defer upstream.Close()

	postProxyTurn(t, startLocalLoggingProxy(t, upstream.URL))
	requests, errors := reportedTurn(t)
	if requests != 1 || errors != 1 {
		t.Errorf("`oaica usage` reports %d requests / %d errors for a turn truncated after its 200 headers, want 1 / 1 — the row recorded the status byte rather than whether the turn arrived", requests, errors)
	}
}

// ...and neither must a 200 that carries nothing at all.
func TestAnEmptyTurnIsNotReportedClean(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	postProxyTurn(t, startLocalLoggingProxy(t, upstream.URL))
	requests, errors := reportedTurn(t)
	if requests != 1 || errors != 1 {
		t.Errorf("`oaica usage` reports %d requests / %d errors for a 200 with no body, want 1 / 1 — a Messages response with no bytes is not a turn any client can use", requests, errors)
	}
}
