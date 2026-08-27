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

// withEntitlementGate temporarily arms the entitlement check with fn (or
// leaves it disabled if fn is nil), restoring the package defaults after
// the test — mirrors the remoteContextWindowFn stub pattern used elsewhere.
func withEntitlementGate(t *testing.T, enabled bool, fn EntitlementCheckFn) {
	t.Helper()
	oldEnabled, oldFn := entitlementCheckEnabled, entitlementCheckFn
	entitlementCheckEnabled = enabled
	if fn != nil {
		entitlementCheckFn = fn
	}
	t.Cleanup(func() {
		entitlementCheckEnabled, entitlementCheckFn = oldEnabled, oldFn
	})
}

func TestCheckEntitlement_DisabledByDefault(t *testing.T) {
	// No withEntitlementGate call: proves the package's real zero-value
	// default (entitlementCheckEnabled) is false without test interference.
	if entitlementCheckEnabled {
		t.Fatal("entitlementCheckEnabled must default to false — an unconfigured deployment must never deny requests")
	}
	allowed, reason := checkEntitlement(&http.Request{}, "test:route", "test-model")
	if !allowed || reason != "" {
		t.Fatalf("checkEntitlement with gate disabled = (%v, %q), want (true, \"\")", allowed, reason)
	}
}

func TestCheckEntitlement_EnabledButUnconfiguredAllowsEverything(t *testing.T) {
	// Flipping the flag alone (env var, no real check plugged in) must not
	// start denying traffic — allowAllEntitlementCheck is the package
	// default entitlementCheckFn.
	withEntitlementGate(t, true, nil)
	allowed, _ := checkEntitlement(&http.Request{}, "test:route", "test-model")
	if !allowed {
		t.Fatal("enabling the gate without configuring a real check must still allow requests")
	}
}

func TestCheckEntitlement_DeniesWhenArmedWithDenyAll(t *testing.T) {
	withEntitlementGate(t, true, denyAllEntitlementCheck)
	allowed, reason := checkEntitlement(&http.Request{}, "test:route", "test-model")
	if allowed {
		t.Fatal("expected deny with denyAllEntitlementCheck armed")
	}
	if reason == "" {
		t.Fatal("expected a non-empty denial reason")
	}
}

func TestCheckEntitlement_ReceivesRouteAndModel(t *testing.T) {
	var gotRoute, gotModel string
	withEntitlementGate(t, true, func(r *http.Request, routeLabel, reqModel string) EntitlementDecision {
		gotRoute, gotModel = routeLabel, reqModel
		return EntitlementDecision{Allowed: true}
	})
	checkEntitlement(&http.Request{}, "remote:kat-awq", "kat-awq")
	if gotRoute != "remote:kat-awq" || gotModel != "kat-awq" {
		t.Fatalf("checkEntitlement passed (%q, %q), want (\"remote:kat-awq\", \"kat-awq\")", gotRoute, gotModel)
	}
}

// TestProxy_EntitlementGateEndToEnd proves the gate actually blocks a real
// /v1/messages request through RunAnthropicOpenAIProxyRoutes when armed —
// not just the checkEntitlement helper in isolation — and that a disabled
// gate (the shipped default) never touches a real request's outcome.
func TestProxy_EntitlementGateEndToEnd(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstreamHit := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer upstream.Close()

	table := proxyRouteTable{Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", Label: "test:kat-awq"}}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	proxyURL := "http://" + ln.Addr().String()

	post := func() *http.Response {
		body, _ := json.Marshal(map[string]any{
			"model": "kat-awq", "max_tokens": 10,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	// Gate disabled (default): request succeeds, upstream is hit.
	resp := post()
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("gate disabled: status = %d, want 200", resp.StatusCode)
	}
	if !upstreamHit {
		t.Fatal("gate disabled: upstream was never called")
	}

	// Gate armed with deny-all: request is rejected BEFORE reaching upstream.
	upstreamHit = false
	withEntitlementGate(t, true, denyAllEntitlementCheck)
	resp = post()
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("gate armed: status = %d, want 403; body=%s", resp.StatusCode, body)
	}
	if upstreamHit {
		t.Fatal("gate armed with deny-all: upstream was called despite the denial — GPU time was spent on a rejected request")
	}
}
