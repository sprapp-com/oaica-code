package launch

// request_log_duration_integrity_test.go — the row's duration_ms must measure
// the REQUEST, not the moment the row was built. The row is now constructed
// before the upstream call (so a transport failure can be logged at all), and
// a duration captured at construction reports the request-marshalling time and
// nothing else: a field meaning "how long this took" would read as 0ms for
// every successful request, and every request that actually got slow would
// look identical to one that returned instantly (2026-09-26 audit).
//
// Nothing consumes duration_ms yet, which is exactly why this is worth
// pinning now: the field has one meaning and a later reader must be able to
// trust it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestTheLoggedDurationCoversTheUpstreamCall(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const upstreamDelay = 400 * time.Millisecond
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(upstreamDelay)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"cmpl-1","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	t.Cleanup(srv.Close)

	proxy := startCalibProxy(t, srv.URL, "sess-duration")
	status, body := proxyClientErrorText(t, proxy, false)
	if status != 200 {
		t.Fatalf("HTTP %d from a healthy upstream:\n%s", status, body)
	}

	path, err := requestLogPath()
	if err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no request log written: %v", err)
	}
	var entry requestLogEntry
	found := false
	for _, line := range strings.Split(strings.TrimRight(string(raw), "\n"), "\n") {
		var e requestLogEntry
		if json.Unmarshal([]byte(line), &e) == nil && e.Model == "kat-awq" {
			entry, found = e, true
		}
	}
	if !found {
		t.Fatalf("no row for the request that just succeeded:\n%s", raw)
	}
	if entry.DurationMs < int64(upstreamDelay/time.Millisecond)-100 {
		t.Errorf("duration_ms = %d for a %s upstream call — the row is built before the call, so the duration has to be taken at the END of it, or every slow request reports the same near-zero time as an instant one", entry.DurationMs, upstreamDelay)
	}
}
