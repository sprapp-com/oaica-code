package launch

// models_passthrough_redaction_integrity_test.go — GET /v1/models relayed the
// upstream's response byte for byte, headers and body, so a mirror that echoes
// the request credential back — in an error page, or in a diagnostic header —
// handed that credential straight to the launched client, which prints it
// (2026-09-26 audit, seventh round).
//
// Every other upstream-facing path in this proxy runs its text through
// redactCredentials before it can reach the client or a log; the two
// passthroughs (proxyPassThrough for an OpenAI-wire plan row, and
// anthropicModelsPassthrough for a native Anthropic upstream) were the
// exceptions, and they are exactly the paths where the upstream is NOT
// api.anthropic.com — it is a user's mirror, reached with a user's key in the
// base URL.

import (
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

// secretEchoUpstream answers every request with a body and a header that quote
// the request URL, credential and all — what a proxy's error page or a
// diagnostic header actually looks like.
func secretEchoUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Route", "https://mirror.example.com/v1/models?api_key=sk-live-BODYSECRET")
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestModelsPassThroughDoesNotRelayAnUpstreamSecret(t *testing.T) {
	srv := secretEchoUpstream(t, `{"error":"upstream refused https://mirror.example.com/v1/models?api_key=sk-live-BODYSECRET"}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxyPassThrough(rec, req, srv.URL+"/v1/models", "sk-live-THEKEY")

	body := rec.Body.String()
	if strings.Contains(body, "sk-live-BODYSECRET") {
		t.Errorf("the upstream body was relayed verbatim: %s\n— the client prints this, and a mirror's error page quotes the request credential", body)
	}
	if h := rec.Header().Get("X-Upstream-Route"); strings.Contains(h, "sk-live-BODYSECRET") {
		t.Errorf("the upstream header was relayed verbatim: X-Upstream-Route: %s", h)
	}
	// The rest of the body must survive: this is a passthrough, not a filter.
	if !strings.Contains(body, "upstream refused") {
		t.Errorf("the diagnostic text was dropped along with the secret: %s", body)
	}
}

func TestAnthropicModelsPassthroughDoesNotRelayAnUpstreamSecret(t *testing.T) {
	srv := secretEchoUpstream(t, `{"error":"denied https://mirror.example.com/v1/models?api_key=sk-live-BODYSECRET"}`)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	anthropicModelsPassthrough(rec, req, srv.URL+"/v1/models", "x-api-key", "sk-live-THEKEY")

	body := rec.Body.String()
	if strings.Contains(body, "sk-live-BODYSECRET") {
		t.Errorf("the upstream body was relayed verbatim: %s", body)
	}
	if h := rec.Header().Get("X-Upstream-Route"); strings.Contains(h, "sk-live-BODYSECRET") {
		t.Errorf("the upstream header was relayed verbatim: X-Upstream-Route: %s", h)
	}
}

// The control: an ordinary model list must reach the client byte-identical and
// with a correct length. Redaction that corrupts the response would break the
// SDK-side model validation this endpoint exists for.
func TestModelsPassThroughLeavesAnOrdinaryBodyIdentical(t *testing.T) {
	list := `{"data":[{"id":"claude-fable-5-1","display_name":"Claude Fable 5.1"},{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"}]}`
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(list))
	}))
	t.Cleanup(srv.Close)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	proxyPassThrough(rec, req, srv.URL+"/v1/models", "sk-live-THEKEY")

	if rec.Body.String() != list {
		t.Errorf("the model list came back changed:\n got: %s\nwant: %s", rec.Body.String(), list)
	}
	if got := rec.Header().Get("Content-Type"); !strings.Contains(got, "application/json") {
		t.Errorf("Content-Type = %q, want it preserved", got)
	}
	if got := rec.Header().Get("Content-Length"); got != "" && got != strconv.Itoa(len(list)) {
		t.Errorf("Content-Length = %q, want %d or absent — a stale length truncates or hangs the client", got, len(list))
	}
}
