package launch

// entitlement_native_crossover_ungated_integrity_test.go — the oversize
// crossover gated the one leg the contract excludes (2026-09-26 audit, ninth
// round, auditor B — raised PLAUSIBLE, confirmed here).
//
// entitlement.go's rule: every request to a "self-hosted or user-remote" model
// is judged, while "the native claude/* leg is deliberately NOT gated — it is
// api.anthropic.com under the user's own credential, so it is neither
// self-hosted nor user-remote". The crossover applied the gate to the oversize
// leg's label unconditionally, so with a native --oversize leg (BaseURL "",
// Wire "anthropic" — the shape the plan builds, see
// oversize_native_leg_alias_test.go) an armed gate denied a crossover that the
// primary path would have served: the gate judged a leg the contract excludes.
//
// The crossover's own fix is unaffected and still pinned elsewhere: a leg is
// judged by the label that SERVES it (entitlement_oversize_test.go), which is
// what the control below re-checks for the classes the rule does cover.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// setupNativeOversizeTable builds the production shape: a tiny-window
// OpenAI-wire remote as the primary, and the plan-built claude/* leg as the
// Oversize target.
func nativeOversizeLeg(t *testing.T) proxyRoute {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-native-crossover")
	stubNativeModelCatalog(t, map[string]string{"fable": "claude-fable-5-1"})

	ep, err := resolveLaunchEndpoint("claude/fable")
	if err != nil {
		t.Fatalf("resolveLaunchEndpoint(claude/fable) = %v, want the native tier", err)
	}
	r := routeFor(ep)
	if r.Wire != "anthropic" || r.BaseURL != "" || !r.NativePassthrough {
		t.Fatalf("premise changed: the plan's native leg is Wire=%q BaseURL=%q NativePassthrough=%t, want the anthropic-wire, base-URL-less native passthrough",
			r.Wire, r.BaseURL, r.NativePassthrough)
	}
	return r
}

func postOverflowingTurn(t *testing.T, proxy, token, model string) int {
	t.Helper()
	long := strings.Repeat("padding content to exceed a tiny context window ", 50)
	body := `{"model":"` + model + `","max_tokens":10,"messages":[{"role":"user","content":"` + long + `"}]}`
	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// A crossover onto the native claude/* leg must NOT be denied: the contract
// excludes that leg from the gate.
func TestTheNativeOversizeCrossoverIsNotGated(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	over := nativeOversizeLeg(t)

	nativeHits := 0
	nativeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer nativeUpstream.Close()
	origNative := nativeAnthropicUpstream
	nativeAnthropicUpstream = nativeUpstream.URL
	defer func() { nativeAnthropicUpstream = origNative }()

	smallHits := 0
	small := openAITestUpstream(&smallHits)
	defer small.Close()
	base := proxyRoute{Label: "remote:small", BaseURL: small.URL + "/v1", Key: "k",
		UpstreamModel: "small-m", ContextWindow: 8, Wire: "openai"}

	withEntitlementGate(t, true, denyOnlyLabel(over.Label))

	proxy := startProxyWithOversize(t, proxyRouteTable{
		Default: base, ByModel: map[string]proxyRoute{"m": base}, Oversize: over})

	// No client token on this table: authorized() allows every caller then.
	code := postOverflowingTurn(t, proxy, "", "m")
	if code == http.StatusForbidden {
		t.Errorf("an armed gate denying the native claude/* label answered 403 on the oversize crossover — entitlement.go's contract excludes that leg (api.anthropic.com under the user's own credential), so the gate judged a leg it deliberately does not cover and refused a turn the primary path would have served")
	}
	if code != http.StatusOK {
		t.Errorf("the native crossover answered %d, want 200 (served by the native leg)\nupstream hits: %d", code, nativeHits)
	}
	if nativeHits == 0 {
		t.Errorf("the native leg was never reached, so this test proves nothing about the gate")
	}
}

// Control: the classes the rule DOES name are still judged — a crossover onto
// an anthropic-wire REMOTE must still be denied by the same armed gate.
func TestTheRemoteOversizeCrossoverIsStillGated(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	bigHits := 0
	big := anthropicTestUpstream(&bigHits)
	defer big.Close()
	smallHits := 0
	small := openAITestUpstream(&smallHits)
	defer small.Close()

	base := proxyRoute{Label: "remote:small", BaseURL: small.URL + "/v1", Key: "k",
		UpstreamModel: "small-m", ContextWindow: 8, Wire: "openai"}
	over := proxyRoute{Label: "remote:zai-coding-plan", BaseURL: big.URL, Key: "sk-remote",
		UpstreamModel: "glm-5.3", Wire: "anthropic", NativePassthrough: true}

	withEntitlementGate(t, true, denyOnlyLabel(over.Label))

	proxy := startProxyWithOversize(t, proxyRouteTable{
		Default: base, ByModel: map[string]proxyRoute{"m": base}, Oversize: over})
	if code := postOverflowingTurn(t, proxy, "", "m"); code != http.StatusForbidden {
		t.Errorf("the remote oversize crossover answered %d, want 403 — narrowing the gate to the classes the rule names must not stop judging them\nupstream hits: %d", code, bigHits)
	}
	if bigHits != 0 {
		t.Errorf("the denial still spent upstream time: %d hit(s)", bigHits)
	}
}
