package launch

// entitlement_passthrough_test.go — entitlement.go's rule, executable: the
// gate's own doc says it sits "after routing is resolved (route, reqModel) but
// before the upstream call is made — so a denial never spends upstream GPU
// time" for "every request to a self-hosted or user-remote model".
//
// Two defects made that false, both fixed 2026-09-26 and pinned here:
//
//  1. The Anthropic-wire user remote (a plan row: zai-coding-plan and friends)
//     returns from the NativePassthrough branch BEFORE the translated path's
//     checkEntitlement call — so the one class of backend the rule names could
//     not be denied, and an armed deny-all gate still spent its upstream time.
//  2. X-Oaica-Route was set only on the translated path, so a passthrough leg
//     — the leg a silent --sonnet-model/fallback swap is hardest to see on —
//     answered with no routing header at all.
//
// The native claude/* leg stays UNGATED on purpose (api.anthropic.com under
// the user's own credential: neither self-hosted nor user-remote), so its
// absence from this test is the contract, not a gap.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// startEntitlementTestProxy serves RunAnthropicOpenAIProxyRoutes on a random
// loopback port, with byModel the table the request's model id resolves
// through, and returns its base URL.
func startEntitlementTestProxy(t *testing.T, route proxyRoute, byModel map[string]proxyRoute) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() {
		_ = RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: route, ByModel: byModel})
	}()
	return "http://" + ln.Addr().String()
}

// postEntitlementTestMessage sends the smallest well-formed /v1/messages
// request and returns the status, body and response headers.
func postEntitlementTestMessage(t *testing.T, proxyURL, model string) (int, string, http.Header) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 16, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "proxy-client-token")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b), resp.Header
}

func TestEntitlementGateCoversAnthropicWireRemotes(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	openaiRoute := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "deepseek", BaseURL: upstream.URL + "/v1", Token: "sk-remote",
		UpstreamModel: "deepseek-v4-flash", Wire: "openai",
	}})
	// The rule's own subject: a user remote on the ANTHROPIC wire.
	anthRoute := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-remote",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	if !anthRoute.NativePassthrough {
		t.Fatalf("setup: an anthropic-wire user remote is no longer NativePassthrough — this test no longer covers the passthrough branch: %+v", anthRoute)
	}

	withEntitlementGate(t, true, denyAllEntitlementCheck)

	t.Run("anthropic-wire remote is denied before its upstream is called", func(t *testing.T) {
		proxy := startEntitlementTestProxy(t, anthRoute, map[string]proxyRoute{"zai-coding-plan/glm-5.3": anthRoute})
		code, body, hdr := postEntitlementTestMessage(t, proxy, "zai-coding-plan/glm-5.3")
		if code != http.StatusForbidden {
			t.Fatalf("armed deny-all gate: anthropic-wire remote answered %d, want 403 — the request reached its upstream instead of the gate.\nbody: %s\nupstream hits: %d", code, body, upstreamHits)
		}
		if upstreamHits != 0 {
			t.Fatalf("the denial still spent upstream time: %d hit(s)", upstreamHits)
		}
		if hdr.Get("X-Oaica-Route") != anthRoute.Label {
			t.Errorf("X-Oaica-Route = %q on a denied passthrough leg, want %q — the header is set only on the translated path, so a passthrough leg answers with no routing attribution", hdr.Get("X-Oaica-Route"), anthRoute.Label)
		}
	})

	t.Run("openai-wire remote is denied too (the path that always worked)", func(t *testing.T) {
		proxy := startEntitlementTestProxy(t, openaiRoute, map[string]proxyRoute{"deepseek/deepseek-v4-flash": openaiRoute})
		code, body, hdr := postEntitlementTestMessage(t, proxy, "deepseek/deepseek-v4-flash")
		if code != http.StatusForbidden {
			t.Fatalf("armed deny-all gate: openai-wire remote answered %d, want 403 (control)\nbody: %s", code, body)
		}
		if hdr.Get("X-Oaica-Route") != openaiRoute.Label {
			t.Errorf("X-Oaica-Route = %q, want %q", hdr.Get("X-Oaica-Route"), openaiRoute.Label)
		}
	})
}

// TestAnthropicWireRemoteServesAndAttributesItsRoute is the other half: with
// the gate off (its real default) the passthrough leg must still work, and the
// routing header must name the leg — moving the gate into the branch must not
// have cost the passthrough anything.
func TestAnthropicWireRemoteServesAndAttributesItsRoute(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-remote",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	code, body, hdr := postEntitlementTestMessage(t, proxy, "zai-coding-plan/glm-5.3")
	if code != http.StatusOK {
		t.Fatalf("passthrough leg answered %d with the gate disabled, want 200\nbody: %s", code, body)
	}
	if hits != 1 {
		t.Fatalf("upstream hits = %d, want 1", hits)
	}
	if hdr.Get("X-Oaica-Route") != route.Label {
		t.Errorf("X-Oaica-Route = %q, want %q", hdr.Get("X-Oaica-Route"), route.Label)
	}
}
