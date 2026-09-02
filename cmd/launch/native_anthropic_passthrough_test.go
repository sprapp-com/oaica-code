package launch

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

func startNativePassthroughProxy(t *testing.T, upstreamURL string) (proxyURL, clientToken string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token, err := newProxyClientToken()
	if err != nil {
		t.Fatal(err)
	}
	route := proxyRoute{Label: "native-anthropic:native-anthropic", UpstreamModel: "opus", NativePassthrough: true}
	table := proxyRouteTable{
		ClientToken: token,
		Default:     route,
		ByModel:     map[string]proxyRoute{"claude/opus": route},
	}
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, table) }()
	t.Cleanup(func() { ln.Close() })
	return "http://" + ln.Addr().String(), token
}

// TestNativeAnthropicPassthrough_ForwardsRawBodyWithAPIKey verifies the
// core promise of nativeAnthropicPassthrough (anthropic_openai_proxy.go):
// no OpenAI translation happens (the upstream sees the EXACT bytes Claude
// Code sent), and the credential comes from ANTHROPIC_API_KEY as an
// x-api-key header, not our own proxy token.
func TestNativeAnthropicPassthrough_ForwardsRawBodyWithAPIKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-key")

	var gotBody []byte
	var gotAPIKeyHeader, gotAuthHeader, gotAnthropicVersion string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBody, _ = io.ReadAll(r.Body)
		gotAPIKeyHeader = r.Header.Get("x-api-key")
		gotAuthHeader = r.Header.Get("Authorization")
		gotAnthropicVersion = r.Header.Get("anthropic-version")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
	}))
	defer upstream.Close()
	orig := nativeAnthropicUpstream
	nativeAnthropicUpstream = upstream.URL
	t.Cleanup(func() { nativeAnthropicUpstream = orig })

	proxyURL, token := startNativePassthroughProxy(t, upstream.URL)

	// The exact bytes a real Claude Code native request sends — no
	// "max_completion_tokens", no OpenAI shape, just Anthropic wire.
	reqBody := []byte(`{"model":"claude/opus","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy returned %d: %s", resp.StatusCode, b)
	}

	if !bytes.Equal(gotBody, reqBody) {
		t.Errorf("upstream body = %s, want byte-identical to what the client sent (no translation)", gotBody)
	}
	if gotAPIKeyHeader != "sk-ant-test-key" {
		t.Errorf("x-api-key = %q, want the resolved ANTHROPIC_API_KEY", gotAPIKeyHeader)
	}
	if gotAuthHeader != "" {
		t.Errorf("Authorization header leaked to upstream = %q, want empty (native uses x-api-key, and our proxy's own bearer must never pass through)", gotAuthHeader)
	}
	if gotAnthropicVersion != "2023-06-01" {
		t.Errorf("anthropic-version = %q, want forwarded from the client", gotAnthropicVersion)
	}
}

// TestNativeAnthropicPassthrough_OAuthTokenWinsWhenNoAPIKey verifies the
// OAuth fallback: resolveNativeAnthropicAuth (native_anthropic_auth.go)
// reads ~/.claude/.credentials.json when ANTHROPIC_API_KEY is unset, and
// sends it as a Bearer Authorization header (Anthropic's OAuth scheme, not
// x-api-key).
func TestNativeAnthropicPassthrough_OAuthTokenWinsWhenNoAPIKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")

	orig := readClaudeOAuthAccessTokenFn
	readClaudeOAuthAccessTokenFn = func() (string, error) { return "sk-ant-oat-test-token", nil }
	t.Cleanup(func() { readClaudeOAuthAccessTokenFn = orig })

	var gotAPIKeyHeader, gotAuthHeader string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKeyHeader = r.Header.Get("x-api-key")
		gotAuthHeader = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[]}`))
	}))
	defer upstream.Close()
	orig2 := nativeAnthropicUpstream
	nativeAnthropicUpstream = upstream.URL
	t.Cleanup(func() { nativeAnthropicUpstream = orig2 })

	proxyURL, token := startNativePassthroughProxy(t, upstream.URL)
	reqBody := []byte(`{"model":"claude/opus","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy returned %d: %s", resp.StatusCode, b)
	}

	if gotAPIKeyHeader != "" {
		t.Errorf("x-api-key = %q, want empty (OAuth path uses Authorization)", gotAPIKeyHeader)
	}
	if gotAuthHeader != "Bearer sk-ant-oat-test-token" {
		t.Errorf("Authorization = %q, want Bearer sk-ant-oat-test-token", gotAuthHeader)
	}
}

// TestNativeAnthropicPassthrough_NoCredentialFailsClosed verifies the
// no-credential-available path never sends an empty/missing auth header
// upstream — it must refuse locally instead.
func TestNativeAnthropicPassthrough_NoCredentialFailsClosed(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	orig := readClaudeOAuthAccessTokenFn
	readClaudeOAuthAccessTokenFn = func() (string, error) { return "", nil }
	t.Cleanup(func() { readClaudeOAuthAccessTokenFn = orig })

	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()
	origURL := nativeAnthropicUpstream
	nativeAnthropicUpstream = upstream.URL
	t.Cleanup(func() { nativeAnthropicUpstream = origURL })

	proxyURL, token := startNativePassthroughProxy(t, upstream.URL)
	reqBody := []byte(`{"model":"claude/opus","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401 (no credential available)", resp.StatusCode)
	}
	if upstreamHit {
		t.Error("upstream was hit with no credential — must fail before ever making the request")
	}
}

// TestNativeAnthropicPassthrough_NoUsageLogged confirms the "no OAICA
// billing on this leg" contract: requestLogHardSignalRE etc still fire
// (client-side diagnostic log), but that log has no cost/usage fields to
// begin with — this test exists to pin the ABSENCE of any usage-tracking
// side effect specific to native passthrough, should one get added later
// by mistake. Kept lightweight: just confirms the request completes
// without the handler attempting the OpenAI-shaped usage bookkeeping that
// would panic/error against a bare Anthropic response body shape.
func TestNativeAnthropicPassthrough_NoUsageLogged(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test-key")

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		// Deliberately NOT an OpenAI-shaped usage object — if the handler
		// tried to parse this as chat-completions usage it would either
		// error or silently record zeros; either way this proves the path
		// never attempts that parse.
		w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":5,"output_tokens":2}}`))
	}))
	defer upstream.Close()
	orig := nativeAnthropicUpstream
	nativeAnthropicUpstream = upstream.URL
	t.Cleanup(func() { nativeAnthropicUpstream = orig })

	proxyURL, token := startNativePassthroughProxy(t, upstream.URL)
	reqBody := []byte(`{"model":"claude/opus","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`)
	req, _ := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(reqBody))
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy returned %d: %s", resp.StatusCode, body)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("response is not the raw upstream JSON: %v (%s)", err, body)
	}
	if parsed["id"] != "msg_1" {
		t.Errorf("response body was rewritten, want passthrough: %s", body)
	}
}
