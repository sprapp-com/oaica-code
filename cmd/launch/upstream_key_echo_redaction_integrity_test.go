package launch

// upstream_key_echo_redaction_integrity_test.go — the translated leg handed the
// launched client a working credential when the upstream quoted it back
// (2026-09-26 audit, ninth round).
//
// This proxy injects a bearer into every translated request
// (anthropic_openai_proxy.go:1582, `Authorization: Bearer route.resolveKey()`).
// A vendor that rejects the call routinely names what it rejected —
// `{"error":{"message":"invalid api key sk-live-…"}}` — and an upstream that
// answers a non-200, or an error object over HTTP 200, has its text re-emitted
// to the client by writeUpstreamError and upstreamErrorMessage.
//
// Both sanitize with redactCredentials, whose rules are SHAPE-based: URL
// userinfo, a credential-bearing query value, a parse-error fragment. A bare
// echoed key is a word to that function. The client is Claude Code, so the key
// lands in an LLM's context window and in the transcript the user pastes into a
// ticket — the same surface relayUpstreamResponse and the anthropic-wire
// passthrough already redact literally (see their `secret` parameter), which is
// the reason this is an omission and not a design choice.
//
// The diagnostic must survive: a redacted message that also dropped the host
// tells support nothing, which is why every case below asserts both halves.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// upstreamEchoKey is the credential the proxy injects on the translated leg.
// Every fixture quotes it back bare, outside any URL, which is exactly the
// shape redactCredentials cannot see.
const upstreamEchoKey = "sk-UPSTREAM-ECHO-4242424242"

// upstreamEchoMessage is a vendor diagnostic carrying the key verbatim plus the
// host, so a test can tell "redacted" from "gutted".
const upstreamEchoMessage = "invalid api key " + upstreamEchoKey + " sent to api.example.com — check the credential for this route"

// echoProxy starts the OpenAI-translation leg against up, with upstreamEchoKey
// as the route's credential.
func echoProxy(t *testing.T, up *httptest.Server) string {
	t.Helper()
	return startEntitlementTestProxy(t, proxyRoute{
		BaseURL: up.URL, Key: upstreamEchoKey, UpstreamModel: "glm-5.3",
		Label: "remote:echo", ContextWindow: 262144,
	}, nil)
}

// postEchoMessage POSTs /v1/messages and returns everything the launched client
// could read.
func postEchoMessage(t *testing.T, proxyURL string, stream bool) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": "glm-5.3", "max_tokens": 64, "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// assertKeyNotEchoed is the shared verdict: the credential must not reach the
// client, the host must.
func assertKeyNotEchoed(t *testing.T, what string, status int, body string) {
	t.Helper()
	if strings.Contains(body, upstreamEchoKey) {
		t.Errorf("%s: the upstream quoted the key this proxy injected and the client was handed it in the clear (HTTP %d) — the child is Claude Code, so the credential lands in an LLM's context window and the transcript the user pastes into a ticket\n%s",
			what, status, body)
	}
	if !strings.Contains(body, "api.example.com") {
		t.Errorf("%s: the redacted diagnostic lost the host it exists to carry, so the message no longer tells support which upstream refused\n%s", what, body)
	}
	if strings.Contains(body, "invalid api key") {
		return // the reason survived alongside the redaction
	}
	t.Errorf("%s: the redacted diagnostic lost the reason it exists to carry\n%s", what, body)
}

// errorObjectOver is an upstream answer: the given status, with a JSON error
// object whose message is upstreamEchoMessage.
func errorObjectOver(t *testing.T, status int) *httptest.Server {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"error": map[string]any{"type": "invalid_request_error", "message": upstreamEchoMessage},
	})
	if err != nil {
		t.Fatal(err)
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		_, _ = io.WriteString(w, string(b))
	}))
}

// The non-200 branch: an upstream that refuses the call and names the key it
// refused.
func TestUpstreamNon200EchoedKeyIsRedacted(t *testing.T) {
	for _, status := range []int{http.StatusUnauthorized, http.StatusTooManyRequests, http.StatusInternalServerError} {
		up := errorObjectOver(t, status)
		proxy := echoProxy(t, up)
		code, body := postEchoMessage(t, proxy, false)
		up.Close()
		assertKeyNotEchoed(t, "non-200 upstream error", code, body)
	}
}

// The same text over HTTP 200, which vLLM and this fleet's own gateway both
// produce: the body never becomes a turn, but it does become the client's
// error message.
func TestUpstreamErrorObjectOver200EchoedKeyIsRedacted(t *testing.T) {
	for _, stream := range []bool{false, true} {
		up := errorObjectOver(t, http.StatusOK)
		proxy := echoProxy(t, up)
		code, body := postEchoMessage(t, proxy, stream)
		up.Close()

		what := "non-streaming 200"
		if stream {
			what = "streaming 200 with no SSE frames"
		}
		assertKeyNotEchoed(t, what, code, body)
	}
}

// The mid-stream failure: the answer starts, then the upstream reports an error
// as an SSE frame instead of a choice.
func TestMidStreamErrorFrameEchoedKeyIsRedacted(t *testing.T) {
	payload, err := json.Marshal(map[string]any{
		"error": map[string]any{"message": upstreamEchoMessage},
	})
	if err != nil {
		t.Fatal(err)
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n")
		fl, _ := w.(http.Flusher)
		if fl != nil {
			fl.Flush()
		}
		_, _ = io.WriteString(w, "data: "+string(payload)+"\n\n")
	}))
	defer up.Close()

	proxy := echoProxy(t, up)
	code, body := postEchoMessage(t, proxy, true)
	assertKeyNotEchoed(t, "mid-stream error frame", code, body)
}
