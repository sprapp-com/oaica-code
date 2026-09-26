package launch

// request_log_transport_failure_integrity_test.go — a request that failed at
// the transport layer (upstream refusing connections, an unresolvable host, a
// TLS failure, a timeout) never reached the request log at all. The row was
// written from behind the upstream call, so `oaica usage` reported ERR 0 for a
// session in which every single turn failed — the exact failures a user opens
// the report to find, and the ones an uptime check computes from it, counted
// as if the machine had sent no traffic (2026-09-26 audit).
//
// The status logged is what the CLIENT received (502), matching the rule the
// rest of the log already follows: the report must name the experience, not
// the upstream's (absent) answer.

import (
	"bytes"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// deadUpstreamAddress returns a 127.0.0.1 address nothing is listening on: the
// port was bound and closed, so a connect to it is refused immediately (no
// timeout to wait out, and no dependence on the machine's firewall).
//
// The port is then dialled to confirm it actually refuses before it is handed
// out, and another one is tried if not. Binding port 0 and closing hands the
// port straight back to the ephemeral pool, and this package has a dozen tests
// starting httptest servers at the same time — one of them can be given that
// exact port in between, and then the "dead" address reaches a LIVE server.
// That is not a hypothetical: it made TestADeadUpstreamStillFailsTheTranslatedLeg
// fail once in a full-package run (2026-09-26 audit) with "a refused connection
// did not count against …", because the request reached somebody else's server,
// whose 4xx is correctly not a leg failure.
//
// The check narrows the window from "the rest of the test run" to the
// microseconds between the dial and the request. It cannot close it: no
// ephemeral port is reserved, and a port that is refused now can be taken a
// moment later.
func deadUpstreamAddress(t *testing.T) string {
	t.Helper()
	// A timeout counts as "nothing answers here" too: a filtered loopback port
	// is as dead to the caller as a refusing one.
	for attempt := 0; attempt < 20; attempt++ {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := ln.Addr().String()
		ln.Close()

		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err != nil {
			return addr
		}
		c.Close()
	}
	t.Fatal("could not find an address that refuses connections — twenty ports in a row were taken between closing them and dialling them, so this test's premise cannot be established")
	return ""
}

func usageRowsByModel(t *testing.T) map[string]UsageStatsRow {
	t.Helper()
	rows, err := LoadUsageStats(UsageStatsFilter{})
	if err != nil {
		t.Fatal(err)
	}
	out := map[string]UsageStatsRow{}
	for _, r := range rows {
		out[r.Model] = r
	}
	return out
}

func TestAnUnreachableUpstreamIsCountedAsAnError(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	dead := deadUpstreamAddress(t)
	proxy := startCalibProxy(t, "http://"+dead, "sess-transport-fail")

	status, body := proxyClientErrorText(t, proxy, false)
	if status != 502 {
		t.Fatalf("HTTP %d from the proxy, want 502 — the premise of this test is that the client was told the leg failed:\n%s", status, body)
	}

	rows := usageRowsByModel(t)
	row, ok := rows["kat-awq"]
	if !ok {
		t.Fatalf("`oaica usage` has no row at all for a request whose upstream was unreachable (%+v) — the row is written behind the upstream call, so a session in which every turn failed reports zero requests and ERR 0", rows)
	}
	if row.Errors != 1 || row.Requests != 1 {
		t.Errorf("row = %+v, want 1 request and 1 error: the failure the user most wants counted is the one the report omits", row)
	}
	if !strings.Contains(row.Backend, dead) {
		t.Errorf("row backend = %q, want it to name the leg that failed (%s)", row.Backend, dead)
	}
}

// The same defect on the native Anthropic passthrough leg — a second
// completion path, and the one a Claude Code session lands on when its plan
// row is anthropic-wire. It logged nothing at all from behind its transport
// call either.
func TestAPassthroughTransportFailureIsCounted(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	dead := deadUpstreamAddress(t)

	body := []byte(`{"model":"fable","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))

	if got, _ := anthropicPassthrough(rec, req, body, "http://"+dead, "x-api-key", "sk-ant-notarealkey", "sess-passthrough-fail"); got != 0 {
		t.Errorf("anthropicPassthrough returned %d, want 0 for a leg it never reached", got)
	}
	if rec.Code != 502 {
		t.Fatalf("client status %d, want 502", rec.Code)
	}

	rows := usageRowsByModel(t)
	row, ok := rows["fable"]
	if !ok {
		t.Fatalf("no usage row for the failed passthrough turn (%+v) — a plan row on the anthropic wire produced no evidence of its failure", rows)
	}
	if row.Errors != 1 {
		t.Errorf("row = %+v, want the failed turn counted as an error", row)
	}
	if !strings.Contains(row.Backend, dead) {
		t.Errorf("row backend = %q, want it to name the upstream (%s)", row.Backend, dead)
	}
}
