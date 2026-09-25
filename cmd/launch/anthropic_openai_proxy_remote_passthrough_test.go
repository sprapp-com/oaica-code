package launch

// anthropic_openai_proxy_remote_passthrough_test.go — a user remote that
// declares wire "anthropic" (the plan rows: zai-coding-plan, and any vendor
// whose documented integration is ANTHROPIC_BASE_URL) is forwarded
// UNTRANSLATED to its own /v1/messages with x-api-key, exactly the shape
// opencode's Anthropic-SDK integration gets. Before this existed the
// translation path POSTed <base>/chat/completions at an Anthropic-shaped
// endpoint and the vendor answered 404 — Claude Code showed "502 upstream
// HTTP 404" (zai-coding-plan, 2026-09-25).

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRouteFor_MarksAnthropicWireRemoteAsPassthrough(t *testing.T) {
	wireAnthropic := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: "https://api.z.ai/api/anthropic/v1",
		Token: "sk-x", UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	if !wireAnthropic.NativePassthrough {
		t.Error("an anthropic-wire remote must route through the passthrough — the translation path cannot reach its endpoint")
	}
	if wireAnthropic.Wire != "anthropic" {
		t.Errorf("Wire = %q, want anthropic", wireAnthropic.Wire)
	}

	wireOpenAI := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "deepseek", BaseURL: "https://api.deepseek.com/v1", Token: "sk-x",
		UpstreamModel: "deepseek-v4-flash", Wire: "openai",
	}})
	if wireOpenAI.NativePassthrough {
		t.Error("an openai-wire remote must keep using the translation path")
	}
	if wireOpenAI.Wire != "openai" {
		t.Errorf("Wire = %q, want openai", wireOpenAI.Wire)
	}
}

// The whole point: Claude Code's request arrives at the remote's /v1/messages
// byte-for-byte (its own headers, its own body), with the picker-namespaced
// model id rewritten to the vendor's real one and OUR credential injected.
func TestRemoteAnthropicWireForwardsToMessagesUntranslated(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	type captured struct {
		path          string
		apiKey        string
		authz         string
		anthropicVer  string
		model         string
		rawBody       string
		betaHeaderGot string
	}
	got := make(chan captured, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var m struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(raw, &m)
		got <- captured{
			path: r.URL.Path, apiKey: r.Header.Get("x-api-key"),
			authz: r.Header.Get("authorization"), anthropicVer: r.Header.Get("anthropic-version"),
			model: m.Model, rawBody: string(raw), betaHeaderGot: r.Header.Get("anthropic-beta"),
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	ep := launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL + "/v1", Token: "sk-remote",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}}
	route := routeFor(ep)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: route, ByModel: map[string]proxyRoute{"zai-coding-plan/glm-5.3": route}}) }()
	time.Sleep(50 * time.Millisecond)

	// A Claude-Code-shaped request: its own x-api-key (the per-launch proxy
	// token, which must NOT reach the vendor), its own anthropic-version and
	// anthropic-beta headers (which must), and the picker's model string.
	body, _ := json.Marshal(map[string]any{
		"model": "zai-coding-plan/glm-5.3", "max_tokens": 16, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	req, err := http.NewRequest(http.MethodPost, "http://"+ln.Addr().String()+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "proxy-client-token")
	req.Header.Set("anthropic-version", "2023-06-01")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, body %s", resp.StatusCode, respBody)
	}
	if !bytes.Contains(respBody, []byte("PONG")) {
		t.Fatalf("response body not relayed: %s", respBody)
	}

	select {
	case c := <-got:
		if c.path != "/v1/messages" {
			t.Errorf("upstream path = %q, want /v1/messages (never /chat/completions)", c.path)
		}
		if c.apiKey != "sk-remote" {
			t.Errorf("upstream x-api-key = %q, want the remote's own key", c.apiKey)
		}
		if c.authz != "" {
			t.Errorf("upstream authorization = %q, want unset (client's proxy token must not leak)", c.authz)
		}
		if c.model != "glm-5.3" {
			t.Errorf("upstream model = %q, want glm-5.3 (picker prefix stripped)", c.model)
		}
		if c.anthropicVer != "2023-06-01" {
			t.Errorf("anthropic-version = %q, want the client's own value forwarded", c.anthropicVer)
		}
		if c.betaHeaderGot != "prompt-caching-2024-07-31" {
			t.Errorf("anthropic-beta = %q, want it forwarded verbatim", c.betaHeaderGot)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never received the request")
	}
}

// No credential anywhere: a clear 401 naming the fix, not a mysterious 502.
func TestRemoteAnthropicWireWithoutCredentialIs401(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("upstream must not be called without a credential")
	}))
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL + "/v1",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: route}) }()
	time.Sleep(50 * time.Millisecond)

	body, _ := json.Marshal(map[string]any{"model": "glm-5.3", "max_tokens": 16, "messages": []map[string]any{{"role": "user", "content": "ping"}}})
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	respBody, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("status = %d, want 401; body %s", resp.StatusCode, respBody)
	}
	if !bytes.Contains(respBody, []byte("zai-coding-plan")) {
		t.Errorf("401 body should name the remote so the fix is obvious: %s", respBody)
	}
}
