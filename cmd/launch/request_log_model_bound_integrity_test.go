package launch

// request_log_model_bound_integrity_test.go — an unbounded `model` string made a
// row permanently unreadable (2026-09-26 audit, ninth round, auditor B).
//
// The model id a row carries comes from the CLIENT's request body, which the
// proxy accepts up to httpbody.DefaultMax. A body naming a multi-megabyte model
// wrote a JSON line past maxLogLineBytes, and the reader drops such a line as
// unreadable — so the turn vanished from `oaica usage` entirely, and the report
// warned about a truncated row on a log with nothing wrong with it. The bound is
// applied in appendRequestLog, the one point every row goes through.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
	"unicode/utf8"
)

// A multi-megabyte model id must still produce a readable row.
func TestAHugeModelIdStillWritesAReadableRow(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	appendRequestLog(requestLogEntry{
		Timestamp:  "2026-09-26T00:00:00Z",
		Model:      strings.Repeat("m", 4<<20),
		Path:       "/v1/messages",
		Backend:    "test",
		StatusCode: http.StatusOK,
	})

	path, err := requestLogPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw := string(b)
	if i := strings.IndexByte(raw, '\n'); i >= 0 {
		raw = raw[:i]
	}
	if len(raw) > maxLogLineBytes {
		t.Errorf("the row is %d bytes, past the %d-byte line cap — the reader drops such a line as unreadable, so this turn is missing from the report", len(raw), maxLogLineBytes)
	}

	rows, unreadable, err := LoadUsageStatsCountingUnreadable(UsageStatsFilter{})
	if err != nil {
		t.Fatalf("LoadUsageStatsCountingUnreadable: %v", err)
	}
	if unreadable != 0 {
		t.Errorf("unreadable = %d for a row this package wrote itself — the report warns about a corrupt log for a healthy one", unreadable)
	}
	if len(rows) != 1 {
		t.Fatalf("got %d aggregated row(s), want 1", len(rows))
	}
	if rows[0].Requests != 1 {
		t.Errorf("requests = %d for a logged turn, want 1 — an over-long model id removed the turn from the report", rows[0].Requests)
	}
}

// End to end: a client that names a giant model is still a turn in the report.
func TestAHugeModelIdIsStillCountedAsATurn(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := jsonUpstream(t, `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
	defer upstream.Close()

	model := strings.Repeat("m", 4<<20)
	body, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 8, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	proxy := startLocalLoggingProxy(t, upstream.URL)
	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()

	rows := waitForRequestLogRows(t, 1)
	if len(rows) != 1 {
		t.Fatalf("got %d row(s) for one turn, want 1", len(rows))
	}
	if len(rows[0].Model) > maxLoggedModelBytes+len("…") {
		t.Errorf("the logged model is %d bytes, want it bounded to ~%d", len(rows[0].Model), maxLoggedModelBytes)
	}
	if !utf8.ValidString(rows[0].Model) {
		t.Errorf("the bounded model is not valid UTF-8: %q", rows[0].Model)
	}
	requests, errors := reportedTurn(t)
	if requests != 1 || errors != 0 {
		t.Errorf("`oaica usage` reports %d requests / %d errors for a served turn whose model id was huge, want 1 / 0", requests, errors)
	}
}
