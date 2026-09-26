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
)

// deadUpstreamAddress returns a 127.0.0.1 address nothing is listening on: the
// port was bound and closed, so a connect to it is refused immediately (no
// timeout to wait out, and no dependence on the machine's firewall).
func deadUpstreamAddress(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	return addr
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
