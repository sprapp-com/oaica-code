package launch

// proxy_upstream_error_redaction_integrity_test.go — an upstream's own error
// message reached the launched client in the clear when it arrived over HTTP
// 200 (2026-09-26 audit, fifth round).
//
// The proxy has two ways to hand an upstream failure to the client, and only
// one of them sanitized. The non-200 branch (writeUpstreamError) runs the body
// through redactCredentials — deliberately, because the upstream quotes the
// request URL back and that URL can carry the remote's credential as userinfo
// (`https://sk-…@host/v1`) or in the query string (`?api_key=…`). The
// HTTP-200-with-a-JSON-error-object branch, which this fleet's own gateway and
// vLLM both produce, passed `upstreamErrorMessage`'s result straight to
// writeAnthropicError on all four of its paths: the non-streaming body
// (anthropic_openai_proxy.go:1637), the two streaming recognition sites
// (:1801, :1816) and the two emit sites fed from them (:1901, :1912).
//
// It matters more here than in a log line: the client is a launched coding
// agent, so the message lands in an LLM's context window, in the transcript
// the user pastes into a ticket, and in whatever the agent decides to do with
// it next.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstreamLeakSecrets are the credential shapes an upstream error message can
// repeat, as <secret value>|<error message it appears in>.
var upstreamLeakSecrets = []struct{ value, message string }{
	{
		"sk-UPSTREAM-USERINFO-11223344",
		`Get "https://sk-UPSTREAM-USERINFO-11223344@api.example.com/v1/models": dial tcp: connection refused`,
	},
	{
		"sk-UPSTREAM-QUERY-55667788",
		`Get "https://api.example.com/v1?api_key=sk-UPSTREAM-QUERY-55667788": 429 rate limit exceeded`,
	},
	{
		"sk-UPSTREAM-CLIENTSECRET-99001122",
		`upstream refused: no entitlement for https://api.example.com/v1?client_secret=sk-UPSTREAM-CLIENTSECRET-99001122`,
	},
}

// errorBody is the HTTP-200 JSON error object the upstream answers with.
func upstreamErrorOver200(t *testing.T, message string) string {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"error": map[string]any{"type": "invalid_request_error", "message": message},
	})
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// clientErrorText POSTs to the proxy and returns everything the client could
// read: the body alone on a non-200, and the body plus the status when the
// proxy answered an error shape.
func proxyClientErrorText(t *testing.T, proxyURL string, stream bool) (int, string) {
	t.Helper()
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json",
		bytes.NewReader(calibMessagesBody(t, 4096, 64, stream)))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// The error text the client reads must not carry the credential, on either the
// streaming or the non-streaming path, for each shape an upstream repeats it.
func TestUpstreamErrorOverHTTP200IsRedacted(t *testing.T) {
	for _, tc := range upstreamLeakSecrets {
		for _, stream := range []bool{false, true} {
			up := jsonUpstream(t, upstreamErrorOver200(t, tc.message))
			proxy := startCalibProxy(t, up.URL, "sess-upstream-redact")
			status, body := proxyClientErrorText(t, proxy, stream)
			up.Close()

			what := "non-streaming"
			if stream {
				what = "streaming"
			}
			if strings.Contains(body, tc.value) {
				t.Errorf("%s: upstream answered an error object over HTTP 200 carrying %q and the proxy handed it to the launched client in the clear (HTTP %d) — the message reaches an LLM's context window and the transcript the user pastes into a ticket. The non-200 branch redacts the same body through writeUpstreamError\n%s",
					what, tc.value, status, body)
			}
			// The diagnostic itself must survive: a redacted message that
			// dropped the host or the status tells support nothing.
			if !strings.Contains(body, "api.example.com") {
				t.Errorf("%s: the redacted error lost the host, which is the diagnostic the message exists to carry\n%s", what, body)
			}
		}
	}
}

// The stream that never starts is the other shape of the same four sites: the
// scanner reached the end of a body that was one JSON error object and no SSE
// frames, so the message is written as the response's status instead of into an
// open stream. Same text, same redaction.
func TestStreamBodyWithNoSSEFramesIsRedacted(t *testing.T) {
	const secret = "sk-NOFRAMES-USERINFO-77889900"
	msg := `upstream returned an error body instead of a stream: https://` + secret + `@api.example.com/v1/models gave 503`
	up := jsonUpstream(t, upstreamErrorOver200(t, msg))
	proxy := startCalibProxy(t, up.URL, "sess-stream-noframes")
	status, body := proxyClientErrorText(t, proxy, true)
	up.Close()

	if strings.Contains(body, secret) {
		t.Errorf("a 200 whose body is a bare JSON error object reached the client with the credential intact (HTTP %d)\n%s", status, body)
	}
}

// The non-200 branch is the control: it already redacted, and a regression
// there is the same defect.
func TestNon200UpstreamErrorIsStillRedacted(t *testing.T) {
	const secret = "sk-NON200-USERINFO-33445566"
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"error":{"message":"Get \"https://`+secret+`@api.example.com/v1\": 429"}}`)
	}))
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-non200-redact")
	_, body := proxyClientErrorText(t, proxy, false)

	if strings.Contains(body, secret) {
		t.Errorf("the non-200 branch printed the credential; writeUpstreamError's redactCredentials is the only thing standing between an upstream body and the client\n%s", body)
	}
}
