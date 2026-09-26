package launch

// upstream_compression_redaction_integrity_test.go — an upstream that answered
// with a Content-Encoding defeated the redaction on both Anthropic-wire relays
// (2026-09-26 audit, ninth round).
//
// The relays below hand the client an upstream's own failure text with the
// injected credential removed — relayUpstreamResponse redacts the body and
// every header it copies. That work is text work, and both relays forwarded the
// CLIENT's accept-encoding upstream, so a vendor behind a CDN was free to answer
// gzip: the redaction ran over compressed bytes (where a key is not a substring
// of anything), Content-Encoding was copied back verbatim, and the launched
// client — Claude Code, which decodes what it asked for — read the working
// credential in clear text. The client's key ended up in an LLM's context window
// and in the transcript the user pastes into a ticket, which is the surface
// every one of these redactions exists for.
//
// Both halves of the fix are pinned here: the relays must not ask for a
// compression they cannot sanitize, and a body that arrives encoded anyway
// (a vendor that compresses unconditionally) must still be decoded before it is
// redacted, or refused rather than relayed when the codec is one this process
// cannot read.

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const gzipEchoKey = "sk-GZIP-ECHO-3131313131"

// gzipEchoMessage carries both halves a client-visible diagnostic must keep:
// the credential (to be removed) and the host and reason (to survive).
const gzipEchoMessage = "invalid api key " + gzipEchoKey + " sent to api.example.com"

// gzipUpstream answers an upstream failure. When onlyIfAsked is true it
// compresses exactly as a normal vendor does — only for a caller that
// announced gzip — which is the whole point: the client's accept-encoding was
// the proxy's to decide. When it is false the body is compressed
// unconditionally, so the proxy cannot dodge the decode by asking nicely.
func gzipUpstream(t *testing.T, status int, body string, onlyIfAsked bool) (*httptest.Server, *bool) {
	t.Helper()
	compressed := new(bool)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !onlyIfAsked || strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
			var buf bytes.Buffer
			zw := gzip.NewWriter(&buf)
			_, _ = zw.Write([]byte(body))
			_ = zw.Close()
			w.Header().Set("Content-Encoding", "gzip")
			w.WriteHeader(status)
			_, _ = w.Write(buf.Bytes())
			*compressed = true
			return
		}
		w.WriteHeader(status)
		_, _ = io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv, compressed
}

// anthropicWireEchoRoute is an Anthropic-wire user remote — the leg that relays
// the upstream's text rather than translating it.
func anthropicWireEchoRoute(up *httptest.Server) proxyRoute {
	return proxyRoute{
		BaseURL: up.URL, Key: gzipEchoKey, NativePassthrough: true, Wire: "anthropic",
		UpstreamModel: "glm-5.3", Label: "remote:zai",
	}
}

// clientReadable is what the launched client actually reads: a client decodes
// the encoding it asked for, so a redaction that ran on compressed bytes is
// invisible until here.
func clientReadable(t *testing.T, body []byte, hdr http.Header) string {
	t.Helper()
	switch strings.ToLower(strings.TrimSpace(hdr.Get("Content-Encoding"))) {
	case "":
		return string(body)
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return string(body)
		}
		defer zr.Close()
		out, _ := io.ReadAll(zr)
		return string(out)
	default:
		// An encoding this test cannot decode is still a body a real client
		// would be handed; returning it raw keeps the key assertion honest.
		return string(body)
	}
}

// gzipClientRequest sends a request that announces gzip, the way Claude Code
// and curl do, and returns the raw relayed body plus the headers. Setting the
// header by hand also stops Go's client from decoding the answer behind the
// assertion's back.
func gzipClientRequest(t *testing.T, method, url string, payload []byte) (int, []byte, http.Header) {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("Accept-Encoding", "gzip")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, raw, resp.Header
}

func echoMessagesPayload(t *testing.T) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model": "zai-coding-plan/glm-5.3", "max_tokens": 64, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func assertNoGzippedKey(t *testing.T, what string, status int, text string) {
	t.Helper()
	if strings.Contains(text, gzipEchoKey) {
		t.Errorf("%s: the upstream compressed its refusal and the compression defeated the redaction — the client decoded what it asked for and read the injected credential in clear text (HTTP %d)\n%s",
			what, status, text)
	}
	if !strings.Contains(text, "api.example.com") {
		t.Errorf("%s: the diagnostic lost the host it exists to carry, so support cannot tell which upstream refused\n%s", what, text)
	}
}

// The messages relay, with a vendor that compresses only when asked — the
// client's own accept-encoding must not become the upstream's reason to
// compress a body this proxy has to sanitize.
func TestGzippedAnthropicWireErrorIsRedacted(t *testing.T) {
	up, compressed := gzipUpstream(t, http.StatusInternalServerError,
		`{"type":"error","error":{"type":"api_error","message":"`+gzipEchoMessage+`"}}`, true)
	route := anthropicWireEchoRoute(up)
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	code, raw, hdr := gzipClientRequest(t, http.MethodPost, proxy+"/v1/messages", echoMessagesPayload(t))
	if !*compressed {
		t.Logf("note: the fixture upstream did not compress this answer, so the encoded path was not exercised")
	}
	assertNoGzippedKey(t, "gzipped /v1/messages error relay", code, clientReadable(t, raw, hdr))
}

// The models relay — the other path that copies the upstream's headers and its
// body back to the client.
func TestGzippedAnthropicWireModelsErrorIsRedacted(t *testing.T) {
	up, _ := gzipUpstream(t, http.StatusUnauthorized,
		`{"type":"error","error":{"type":"authentication_error","message":"`+gzipEchoMessage+`"}}`, true)
	route := anthropicWireEchoRoute(up)
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	code, raw, hdr := gzipClientRequest(t, http.MethodGet, proxy+"/v1/models", nil)
	assertNoGzippedKey(t, "gzipped /v1/models error relay", code, clientReadable(t, raw, hdr))
}

// A vendor that compresses even when it was not asked leaves no way to dodge
// the decode: the body must be decoded and then redacted.
func TestUnconditionallyCompressedUpstreamErrorIsRedacted(t *testing.T) {
	up, compressed := gzipUpstream(t, http.StatusInternalServerError,
		`{"type":"error","error":{"type":"api_error","message":"`+gzipEchoMessage+`"}}`, false)
	route := anthropicWireEchoRoute(up)
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	code, raw, hdr := gzipClientRequest(t, http.MethodPost, proxy+"/v1/messages", echoMessagesPayload(t))
	if !*compressed {
		t.Fatalf("premise: the fixture upstream did not compress the body, so this test proves nothing")
	}
	assertNoGzippedKey(t, "unconditionally gzipped error relay", code, clientReadable(t, raw, hdr))
}

// A body under an encoding this process cannot decode must not be relayed
// under the encoding header anyway: the redaction would run over bytes that are
// not the text the client will see, and a client that ignores the header reads
// the upstream's message verbatim.
func TestUndecodableUpstreamBodyIsNotRelayed(t *testing.T) {
	// Declared gzip, and not gzip at all.
	const marker = "MARKER-ZD41"
	const payloadText = `{"error":{"message":"` + gzipEchoMessage + ` ` + marker + `"}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = io.WriteString(w, payloadText)
	}))
	defer up.Close()

	route := anthropicWireEchoRoute(up)
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	code, raw, hdr := gzipClientRequest(t, http.MethodPost, proxy+"/v1/messages", echoMessagesPayload(t))
	text := clientReadable(t, raw, hdr)
	if strings.Contains(text, marker) {
		t.Errorf("an upstream declared an encoding this process cannot decode and its body was relayed under that header (HTTP %d) — the redaction ran over bytes that are not the text a client reads\n%s", code, text)
	}
}

// The control: a plaintext success body still reaches the client intact. The
// fix must not cost the relay its artifact.
func TestPlaintextAnthropicWireSuccessStillRelays(t *testing.T) {
	const answer = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG-ZD41"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, answer)
	}))
	defer up.Close()

	route := anthropicWireEchoRoute(up)
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	code, raw, hdr := gzipClientRequest(t, http.MethodPost, proxy+"/v1/messages", echoMessagesPayload(t))
	text := clientReadable(t, raw, hdr)
	if code != http.StatusOK || !strings.Contains(text, "PONG-ZD41") {
		t.Fatalf("a plaintext success stopped reaching the client after the encoding fix (HTTP %d)\n%s", code, text)
	}
}
