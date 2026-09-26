package launch

// proxy_non_object_body_integrity_test.go — a literal `null` body panicked the
// /v1/messages handler on an Anthropic-wire remote leg (2026-09-26 audit,
// thirteenth round).
//
// The handler reads the body, then does `json.Unmarshal(body, &anthReq)` into
// an anthropic.MessagesRequest. That unmarshal ACCEPTS `null`: decoding null
// into a struct is a documented no-op that reports no error. selectRoute("")
// then returns the Default route, and when that route is an Anthropic-wire
// REMOTE — the zai-coding-plan / MiniMax /anthropic class,
// NativePassthrough && Wire == "anthropic" && BaseURL != "" — the handler
// rewrites the body's model field before forwarding it. That rewrite
// unmarshals into a map[string]json.RawMessage, which for `null` is left NIL
// with no error either, and the next line assigns into it:
//
//	assignment to entry in nil map
//
// net/http recovers the panic per-connection, so what a client sees is a
// dropped connection, not an error — and because the request-log defer is
// registered further down, the failure writes no usage row and feeds no
// breaker sample: a failed request with nothing, anywhere, to record it.
//
// This is the same input class as the round-nine fix in the sibling file
// (local_proxy.go's normalizeSystemMessages got a `parsed == nil` guard); the
// guard was never added here.

import (
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// countingUpstream counts how many requests actually reached it.
func countingUpstream(t *testing.T, hits *int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"PONG"},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":1,"total_tokens":6}}`)
	}))
}

// startAnthropicWireProxy boots the routes handler with an Anthropic-wire
// remote as the default leg — the shape that runs the model rewrite.
func startAnthropicWireProxy(t *testing.T, upstreamURL, sessionID string) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	table := proxyRouteTable{
		SessionID: sessionID,
		Default: proxyRoute{
			Label: "remote:zai-coding-plan", BaseURL: upstreamURL, Key: "k",
			UpstreamModel: "glm-5.3", ContextWindow: 262144,
			Wire: "anthropic", NativePassthrough: true,
		},
	}
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	url := "http://" + ln.Addr().String()
	time.Sleep(50 * time.Millisecond)
	return url
}

// A body that is not a JSON object must be refused with a readable 400 — and
// nothing may be forwarded upstream.
func TestANonObjectRequestBodyIsRefusedNotPanicked(t *testing.T) {
	hits := 0
	up := countingUpstream(t, &hits)
	defer up.Close()
	proxy := startAnthropicWireProxy(t, up.URL, "sess-non-object-body")

	for _, body := range []string{"null", "[]", `"hello"`, "123", ""} {
		t.Run(body, func(t *testing.T) {
			resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatalf("POST %s: %v — the handler panicked and net/http dropped the connection instead of answering", body, err)
			}
			defer resp.Body.Close()
			raw, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusBadRequest {
				t.Errorf("status = %d for a %s body, want 400 with a readable error; a body that is not a JSON object is not an Anthropic Messages request\n%s", resp.StatusCode, body, raw)
			}
			if resp.StatusCode == http.StatusBadRequest {
				var e struct {
					Error struct {
						Message string `json:"message"`
					} `json:"error"`
				}
				if json.Unmarshal(raw, &e) != nil || e.Error.Message == "" {
					t.Errorf("the 400 carried no JSON error message:\n%s", raw)
				}
			}
		})
	}
	if hits != 0 {
		t.Errorf("the upstream was hit %d time(s) by a malformed body; nothing about these requests is forwardable", hits)
	}

	// Control: a real Anthropic request on this same leg still works, so the
	// refusal is about the body's shape and not about the leg.
	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 16, 32, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("control: a valid request answered %d\n%s", resp.StatusCode, raw)
	}
	if hits != 1 {
		t.Errorf("control: the upstream was hit %d time(s), want 1", hits)
	}
}

// The rewrite helper itself must refuse a non-object body rather than build a
// document out of nothing: it is also called on the oversize crossover path.
func TestRewritingTheModelOfANonObjectBodyErrors(t *testing.T) {
	for _, body := range []string{"null", "[]", `"hello"`, ""} {
		got, err := rewriteAnthropicRequestModel([]byte(body), "glm-5.3")
		if err == nil {
			t.Errorf("rewriteAnthropicRequestModel(%q) returned %q and no error; a non-object body has no model field to replace, and assigning into the nil map it decodes to panics", body, got)
		}
	}
}
