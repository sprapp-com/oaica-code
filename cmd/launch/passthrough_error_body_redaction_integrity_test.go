package launch

// passthrough_error_body_redaction_integrity_test.go — the Anthropic-wire
// passthrough relayed a FAILING upstream's diagnosis to the client raw
// (2026-09-26 audit, eighth round).
//
// Every other relay in this package redacts before it forwards: the sibling
// /v1/models relay (relayUpstreamResponse, which also drops and recomputes
// Content-Length), the translated path's writeUpstreamError, and the stream
// error recogniser (upstreamErrorMessage). The byte-for-byte /v1/messages
// passthrough copied the upstream's headers and then wrote its body through
// with no redaction at all — so a vendor error that quotes the URL it was
// called with (`{"error": "denied https://mirror.example/v1?api_key=sk-…"}`,
// the shape net/http itself prints in transport errors) handed the client a
// working credential, and a header did the same without even being read.
// That client is Claude Code: the text lands in its UI, in the session
// transcript, and in whatever bug report the user pastes it into.
//
// The rule is now split by outcome, which is the split this proxy already
// makes everywhere else: a 2xx body is the MODEL'S OWN ANSWER and is relayed
// byte-for-byte (redacting it would corrupt the user's content, and a
// credential straddling two reads of a stream cannot be redacted safely at
// all), while a non-2xx body is the vendor's diagnosis — exactly where a
// credential gets quoted back — and gets the same treatment as every other
// failure path here.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// leakyVendor answers with the vendor's own diagnosis, quoting the URL it was
// called with. bodySecret and headerSecret are the shapes a real credential
// takes on the way back: a query-string key in an error message, and a
// credential-bearing URL echoed into a custom header.
const (
	bodySecret   = "sk-live-BODYLEAK-99887766"
	headerSecret = "sk-live-HEADERLEAK-11223344"
)

func leakyVendor(t *testing.T, status int) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Upstream-Route", "https://mirror.example.com/v1/messages?api_key="+headerSecret)
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"rate_limit_error","message":"denied https://mirror.example.com/v1/messages?api_key=` + bodySecret + `"}}`))
	}))
}

// The defect: a failing passthrough leg's body and headers must not carry a
// credential to the client.
func TestAPassthroughErrorBodyIsRedacted(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := leakyVendor(t, http.StatusTooManyRequests)
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-remote",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	if !route.NativePassthrough {
		t.Fatalf("setup: an anthropic-wire user remote is no longer NativePassthrough — this test no longer covers the leg it was written for: %+v", route)
	}
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	code, body, hdr := postEntitlementTestMessage(t, proxy, "zai-coding-plan/glm-5.3")
	if code != http.StatusTooManyRequests {
		t.Fatalf("premise: the client got HTTP %d, want the upstream's 429 relayed\nbody: %s", code, body)
	}
	for _, secret := range []string{bodySecret, headerSecret} {
		if strings.Contains(body, secret) {
			t.Errorf("the upstream's error body reached the client with the credential in it (%s) — the client is Claude Code, which prints this into its transcript and into any bug report it is pasted into\nbody: %s", secret, body)
		}
	}
	if got := hdr.Get("X-Upstream-Route"); strings.Contains(got, headerSecret) {
		t.Errorf("the upstream's X-Upstream-Route header reached the client with the credential in it: %q", got)
	}
	// The diagnosis itself must survive: redaction is not a blank page.
	if !strings.Contains(body, "rate_limit_error") {
		t.Errorf("the upstream's error type was lost along with the credential — the client is told even less now than before\nbody: %s", body)
	}
	// And the relay must still be a valid HTTP response.
	if cl := hdr.Get("Content-Length"); cl != "" && cl != itoa(len(body)) {
		t.Errorf("Content-Length = %q but the redacted body is %d bytes — a stale length truncates or hangs the client", cl, len(body))
	}
}

// The control: our own upstream key is the OTHER credential this leg could
// echo, and it must not come back either.
func TestAPassthroughErrorDoesNotEchoOurCredential(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const ourKey = "sk-our-upstream-key-ZZZ999"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":{"message":"bad key: ` + r.Header.Get("x-api-key") + `"}}`))
	}))
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: ourKey,
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	_, body, _ := postEntitlementTestMessage(t, proxy, "zai-coding-plan/glm-5.3")
	if !strings.Contains(body, ourKey) {
		t.Skipf("premise: the fixture no longer receives our credential on the request, so there is nothing for it to echo\nbody: %s", body)
	}
	t.Errorf("the upstream echoed the credential we sent it and the client received it verbatim — a redaction rule that only knows URL-shaped secrets misses this, so the proxy must redact the value it injected itself\nbody: %s", body)
}

// And the other half of the rule: a 2xx body is the model's own answer. It is
// relayed unchanged even when it contains something credential-shaped, because
// redacting it would corrupt the user's content.
func TestAPassthroughSuccessBodyIsRelayedByteForByte(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	answer := `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"the key is at https://mirror.example.com/v1?api_key=` + bodySecret + `"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-remote",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	code, body, _ := postEntitlementTestMessage(t, proxy, "zai-coding-plan/glm-5.3")
	if code != http.StatusOK {
		t.Fatalf("premise: the client got HTTP %d, want 200\nbody: %s", code, body)
	}
	if body != answer {
		t.Errorf("a successful passthrough answer was not relayed byte-for-byte:\n got: %s\nwant: %s", body, answer)
	}
}

// itoa avoids pulling strconv in for one call.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	return string(b[i:])
}
