package launch

// entitlement_oversize_test.go — the oversize crossover's own half of the
// entitlement contract (entitlement.go). The gate's doc promises it sits
// "after routing is resolved (route, reqModel) but before the upstream call is
// made — so a denial never spends upstream GPU time", for "every request to a
// self-hosted or user-remote model".
//
// The crossover (route_policy.go's oversizeSwap, reached from the context-fit
// clamp) breaks that in two ways, both fixed 2026-09-26:
//
//  1. A NativePassthrough oversize leg — an anthropic-wire REMOTE, the plan
//     rows — returned straight into anthropicPassthrough with no gate at all,
//     so the one class of backend the rule names could be served while an
//     armed gate denied it.
//  2. On the translated crossover the gate had already run against the
//     PRE-swap label by the time `route = over` moved the request, so the
//     leg that actually spent upstream time was never the leg evaluated.
//
// Both are the same defect as the round-5 finding one level up: a leg is
// judged by the label it was ASKED for, not the label that SERVES it.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"testing"
)

// startProxyWithOversize serves the proxy with a whole table, so a test can
// supply an Oversize leg (startEntitlementTestProxy builds its own table and
// cannot).
func startProxyWithOversize(t *testing.T, table proxyRouteTable) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, table) }()
	return "http://" + ln.Addr().String()
}

// openAITestUpstream answers the OpenAI-shaped response a translated leg's
// client expects, counting hits.
func openAITestUpstream(hits *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"PONG"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
}

// anthropicTestUpstream answers the Anthropic-shaped response an
// anthropic-wire leg's client expects, counting hits.
func anthropicTestUpstream(hits *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
}

// denyOnlyLabel denies exactly one label and allows every other — the shape
// that answers "was the leg that SERVES the request the one judged?", as
// opposed to a deny-all gate that a pre-swap label would also trip.
func denyOnlyLabel(denied string) EntitlementCheckFn {
	return func(r *http.Request, routeLabel, reqModel string) EntitlementDecision {
		if routeLabel == denied {
			return EntitlementDecision{Allowed: false, Reason: "denied: " + routeLabel}
		}
		return EntitlementDecision{Allowed: true}
	}
}

// TestEntitlementGateCoversTheOversizeCrossoverLeg pins both halves: the
// Anthropic-wire oversize leg is gated at all, and the translated crossover
// judges the leg that serves.
func TestEntitlementGateCoversTheOversizeCrossoverLeg(t *testing.T) {
	t.Run("anthropic-wire oversize leg is denied before its upstream is called", func(t *testing.T) {
		setLaunchTestHome(t, t.TempDir())

		smallHits, bigHits := 0, 0
		small := openAITestUpstream(&smallHits)
		defer small.Close()
		big := anthropicTestUpstream(&bigHits)
		defer big.Close()

		base := proxyRoute{Label: "remote:small", BaseURL: small.URL + "/v1", Key: "k",
			UpstreamModel: "small-m", ContextWindow: 8, Wire: "openai"}
		// The rule's own subject, reached by the crossover rather than by
		// the primary lookup: an anthropic-wire user remote.
		over := proxyRoute{Label: "remote:zai-coding-plan", BaseURL: big.URL, Key: "sk-remote",
			UpstreamModel: "glm-5.3", Wire: "anthropic", NativePassthrough: true}

		withEntitlementGate(t, true, denyOnlyLabel(over.Label))

		proxy := startProxyWithOversize(t, proxyRouteTable{
			Default: base, ByModel: map[string]proxyRoute{"m": base}, Oversize: over})
		code, body, _ := postEntitlementTestMessage(t, proxy, "m")
		if code != http.StatusForbidden {
			t.Fatalf("armed gate denying %q: the oversize crossover answered %d, want 403 — the crossover leg reaches its upstream without ever being evaluated.\nbody: %s\nupstream hits: %d",
				over.Label, code, body, bigHits)
		}
		if bigHits != 0 {
			t.Fatalf("the denial still spent upstream time: %d hit(s)", bigHits)
		}
	})

	t.Run("translated crossover judges the leg that serves, not the one asked for", func(t *testing.T) {
		setLaunchTestHome(t, t.TempDir())

		smallHits, bigHits := 0, 0
		small := openAITestUpstream(&smallHits)
		defer small.Close()
		big := openAITestUpstream(&bigHits)
		defer big.Close()

		base := proxyRoute{Label: "router:small", BaseURL: small.URL + "/v1", Key: "k",
			UpstreamModel: "small-m", ContextWindow: 8, Wire: "openai"}
		over := proxyRoute{Label: "remote:big", BaseURL: big.URL + "/v1", Key: "k",
			UpstreamModel: "big-m", ContextWindow: 200000, Wire: "openai"}

		// The primary label is NOT denied, so the pre-swap verdict is
		// "allow" — only a gate that re-evaluates after the swap can stop
		// this request, and only the swapped-to leg spends upstream time.
		withEntitlementGate(t, true, denyOnlyLabel(over.Label))

		proxy := startProxyWithOversize(t, proxyRouteTable{
			Default: base, ByModel: map[string]proxyRoute{"m": base}, Oversize: over})
		code, body, _ := postEntitlementTestMessage(t, proxy, "m")
		if code != http.StatusForbidden {
			t.Fatalf("gate denied %q but the crossover answered %d, want 403 — checkEntitlement ran against the PRE-swap label, so the leg that served was never evaluated.\nbody: %s\nupstream hits: %d",
				over.Label, code, body, bigHits)
		}
		if bigHits != 0 {
			t.Fatalf("the denied leg spent upstream time: %d hit(s)", bigHits)
		}
	})
}
