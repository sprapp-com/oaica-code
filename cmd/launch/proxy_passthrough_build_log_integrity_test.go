package launch

// proxy_passthrough_build_log_integrity_test.go — the passthrough leg's
// request-build failure returned without writing a request-log row
// (2026-09-26 audit, round 16).
//
// anthropicPassthrough logs every outcome, and its own comment says so: the
// entry is built up front precisely because the deferred row cannot cover a
// return that happens before the relay. Two early returns honour that — the
// transport failure after Do() writes its own row, and the translated
// sibling's http.NewRequestWithContext failure writes one at
// anthropic_openai_proxy.go's "build upstream request" branch. The passthrough
// leg's own NewRequestWithContext failure did not: `entry` exists by then and
// `w` has not been written to, so the row was simply absent.
//
// The reachable case is a malformed upstream for the leg — a URL the vendored
// remote's configuration produced — where `oaica usage` reports no traffic for
// a session whose every turn answered 500.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// logRowsIfAny returns the parsed rows of this HOME's requests.log, or none if
// the file does not exist — the difference between "no row" and "no file" is
// the finding here, so the reader must not fail on the second.
func logRowsIfAny(t *testing.T) []requestLogEntry {
	t.Helper()
	path, err := requestLogPath()
	if err != nil {
		t.Fatalf("requestLogPath: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	var rows []requestLogEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e requestLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("requests.log line is not JSON: %v\n%s", err, line)
		}
		rows = append(rows, e)
	}
	return rows
}

func TestPassthroughRequestBuildFailureIsLogged(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	w := httptest.NewRecorder()
	r := httptest.NewRequest(http.MethodPost, "/v1/messages", nil)
	body := []byte(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`)

	// "://bad" has no scheme, so http.NewRequestWithContext fails before any
	// network work — the branch under test.
	status, relayed := anthropicPassthrough(w, r, body, "://bad", "x-api-key", "k", "")
	if status != 0 || relayed {
		t.Fatalf("anthropicPassthrough = (%d, %v), want (0, false) for a request that was never built", status, relayed)
	}
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("the client got HTTP %d, want 500 — premise: this is the build-failure branch", w.Code)
	}

	rows := logRowsIfAny(t)
	if len(rows) != 1 {
		t.Fatalf("requests.log holds %d row(s) for a turn that answered 500, want 1 — a session whose every turn failed at this branch reports no traffic at all in `oaica usage`", len(rows))
	}
	if rows[0].StatusCode != http.StatusInternalServerError {
		t.Errorf("the logged status is %d, want 500 — the row must carry the status the CLIENT ended up with", rows[0].StatusCode)
	}
	if rows[0].Model != "kat-awq" {
		t.Errorf("the logged model is %q, want the model the request named", rows[0].Model)
	}
}
