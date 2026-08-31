package launch

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// TestParseRoutePolicy covers the accepted vocabulary, the empty default,
// and loud failure on a typo.
func TestParseRoutePolicy(t *testing.T) {
	for in, want := range map[string]routePolicy{
		"":              RouteLocalFirst,
		"local-first":   RouteLocalFirst,
		"remote-first":  RouteRemoteFirst,
		"auto":          RouteAuto,
		"local-only":    RouteLocalOnly,
		"remote-only":   RouteRemoteOnly,
		"Remote-First":  RouteRemoteFirst, // lenient on case? No — strict:
	} {
		if in == "Remote-First" {
			continue
		}
		got, err := parseRoutePolicy(in)
		if err != nil || got != want {
			t.Errorf("parseRoutePolicy(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseRoutePolicy("locall-first"); err == nil {
		t.Error("typo'd policy must fail loudly, not degrade to the default")
	}
}

// TestLocalhostLocality pins the loopback rule the fallback ordering uses.
func TestLocalhostLocality(t *testing.T) {
	for url, want := range map[string]string{
		"http://127.0.0.1:11434/v1": "local",
		"http://localhost:8081/v1":  "local",
		"https://api.deepseek.com/v1": "remote",
	} {
		if got := routeLocality(url); got != want {
			t.Errorf("routeLocality(%q) = %q, want %q", url, got, want)
		}
	}
}

// End-to-end: the primary leg keeps answering 502 (post-retry), the breaker
// opens after breakerFailsToOpen failed requests, and subsequent traffic —
// under local-first — lands on the healthy local fallback with the
// X-Oaica-Route attribution header. Retries are cut to 1 so the test is fast.
func TestRoutePolicy_FallbackOnOpenBreaker(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	oldMax := proxyUpstreamMaxRetries
	proxyUpstreamMaxRetries = 1
	oldOpenFor := breakerOpenFor
	breakerOpenFor = time.Minute
	defer func() { proxyUpstreamMaxRetries, breakerOpenFor = oldMax, oldOpenFor }()

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("down"))
	}))
	defer down.Close()

	upOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The 30s health poll also hits /models; answer everything.
		if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-2", "model": "local-m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "local ok"}}},
		})
	}))
	defer upOK.Close()

	table := proxyRouteTable{
		Policy: RouteLocalFirst,
		Default: proxyRoute{BaseURL: down.URL, UpstreamModel: "m", Label: "remote:down"},
		Fallbacks: []proxyRoute{{BaseURL: upOK.URL, UpstreamModel: "local-m", Label: "daemon:local"}},
		breakers: &routeBreakers{},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, table) }()

	post := func() (string, http.Header) {
		body, _ := json.Marshal(map[string]any{
			"model": "m", "max_tokens": 4,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		content, _ := out["content"].([]any)
		text := ""
		if len(content) > 0 {
			m, _ := content[0].(map[string]any)
			text, _ = m["text"].(string)
		}
		return text, resp.Header
	}

	// First breakerFailsToOpen requests hit the dead primary (that's the
	// breaker learning it's dead, correctly — no way around paying for the
	// detection window).
	for i := 0; i < breakerFailsToOpen; i++ {
		text, hdr := post()
		if text == "local ok" || hdr.Get("X-Oaica-Route") != "remote:down" {
			t.Fatalf("request %d should still be on the (failing) primary, got %q", i+1, hdr.Get("X-Oaica-Route"))
		}
	}
	// Breaker now OPEN for the primary: the next request must land on the
	// local fallback, with the attribution header naming the real leg.
	text, hdr := post()
	if text != "local ok" {
		t.Fatalf("expected fallback response, got: %v", text)
	}
	if hdr.Get("X-Oaica-Route") != "daemon:local" {
		t.Fatalf("X-Oaica-Route = %q, want daemon:local", hdr.Get("X-Oaica-Route"))
	}
}

// TestRoutePolicy_LocalOnlyDoesNotCross verifies the pin at the selection
// layer (no wire needed — selectRoute only consults breaker state): with
// local-only and only a remote fallback, an OPEN local breaker still returns
// the local route (the request then fails visibly) instead of silently
// crossing to the remote. remote-first, by contrast, picks the remote.
func TestRoutePolicy_LocalOnlyDoesNotCross(t *testing.T) {
	localDown := proxyRoute{BaseURL: "http://127.0.0.1:9206/v1", UpstreamModel: "m", Label: "daemon:local"}
	remoteUp := proxyRoute{BaseURL: "https://remote.example/v1", UpstreamModel: "m", Label: "remote:x"}
	for _, tc := range []struct {
		policy routePolicy
		want   proxyRoute
	}{{RouteLocalOnly, localDown}, {RouteRemoteFirst, remoteUp}} {
		table := proxyRouteTable{
			Policy:   tc.policy,
			Default:  localDown,
			Fallbacks: []proxyRoute{remoteUp},
			breakers: &routeBreakers{},
		}
		for i := 0; i < breakerFailsToOpen; i++ {
			table.breakers.recordFail(localDown.BaseURL)
		}
		got, _, _ := table.selectRoute("m")
		if got.BaseURL != tc.want.BaseURL {
			t.Errorf("policy %s with dead local leg: selected %s, want %s", tc.policy, got.BaseURL, tc.want.BaseURL)
		}
	}
}