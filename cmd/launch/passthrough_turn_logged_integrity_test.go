package launch

// passthrough_turn_logged_integrity_test.go — the Anthropic-wire passthrough
// leg wrote a request-log row only when its upstream refused the connection
// (2026-09-26 audit).
//
// The translated path logs every attempt (request_log.go, entry built before
// the upstream call, row written from a deferred status capture). The
// passthrough legs returned from the /v1/messages handler before any of that,
// so a remote on the anthropic wire — a plan row like zai-coding-plan, which
// is exactly the leg this proxy's passthrough exists for — was invisible to
// `oaica usage` in the two states the report is opened for:
//
//   - a session where every turn SUCCEEDED wrote no rows at all, so the whole
//     day of traffic read as "no traffic logged yet";
//   - a session where every turn failed with an upstream 5xx wrote no rows
//     either, so ERR stayed 0 for it.
//
// Both are asserted here through the proxy, on the rows a user reads.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// waitForRequestLogRows polls for want rows: the proxy logs from a defer inside
// the handler goroutine, and the client's last read of the body can land before
// that goroutine has run it, so a single read races (and the row is written,
// not missing). Returns what it found when the deadline passes either way.
func waitForRequestLogRows(t *testing.T, want int) []requestLogEntry {
	t.Helper()
	path, err := requestLogPath()
	if err != nil {
		t.Fatalf("requestLogPath: %v", err)
	}
	deadline := time.Now().Add(3 * time.Second)
	var rows []requestLogEntry
	for time.Now().Before(deadline) {
		rows = nil
		if b, rerr := os.ReadFile(path); rerr == nil {
			for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
				if strings.TrimSpace(line) == "" {
					continue
				}
				var e requestLogEntry
				if jerr := json.Unmarshal([]byte(line), &e); jerr != nil {
					t.Fatalf("requests.log line is not JSON: %v\n%s", jerr, line)
				}
				rows = append(rows, e)
			}
		}
		if len(rows) >= want {
			return rows
		}
		time.Sleep(10 * time.Millisecond)
	}
	return rows
}

const anthropicWireAnswer = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`

func anthropicWireProxy(t *testing.T, status int, body string) string {
	t.Helper()
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(upstream.Close)

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-remote",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	return startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})
}

// A turn that succeeded must be counted: one row, and its status the client got.
func TestASuccessfulPassthroughTurnIsLogged(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	proxy := anthropicWireProxy(t, http.StatusOK, anthropicWireAnswer)

	code, body, _ := postEntitlementTestMessage(t, proxy, "zai-coding-plan/glm-5.3")
	if code != http.StatusOK {
		t.Fatalf("premise: HTTP %d for a healthy passthrough turn, so there is no success to log\nbody: %s", code, body)
	}

	rows := waitForRequestLogRows(t, 1)
	if len(rows) != 1 {
		t.Fatalf("a delivered passthrough turn wrote %d request-log rows, want 1 — an Anthropic-wire remote can serve a whole session and `oaica usage` reports \"no traffic logged yet\" for it", len(rows))
	}
	if rows[0].StatusCode != http.StatusOK {
		t.Errorf("the row says status_code = %d for a turn the client got as 200", rows[0].StatusCode)
	}
	if rows[0].Model == "" {
		t.Errorf("the row has no model — `oaica usage` groups by this column, and a blank one is a row the report cannot attribute")
	}
	if rows[0].DurationMs < 0 {
		t.Errorf("duration_ms = %d", rows[0].DurationMs)
	}
}

// A failing upstream must be counted too, with its status.
func TestAFailedPassthroughTurnIsLogged(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	proxy := anthropicWireProxy(t, http.StatusInternalServerError, `{"error":{"message":"backend exploded"}}`)

	code, _, _ := postEntitlementTestMessage(t, proxy, "zai-coding-plan/glm-5.3")
	if code != http.StatusInternalServerError {
		t.Fatalf("premise: HTTP %d from a 500ing upstream, want the 500 relayed", code)
	}

	rows := waitForRequestLogRows(t, 1)
	if len(rows) != 1 {
		t.Fatalf("a passthrough turn whose upstream answered 500 wrote %d request-log rows, want 1 — `oaica usage` reads ERR off this column, and the 5xx path returned from the handler before any row was written", len(rows))
	}
	if rows[0].StatusCode != http.StatusInternalServerError {
		t.Errorf("the row says status_code = %d for a turn the client got as 500", rows[0].StatusCode)
	}
}
