package launch

// oversize_native_leg_alias_test.go — the oversize-to-native crossover must
// resolve the Claude Code tier alias on the route the PLAN builds (round-5
// audit, 2026-09-26).
//
// The /v1/messages handler rewrites the body's "model" before handing an
// over-window request to a native --oversize leg, and used to do it only when
// `over.Wire != "anthropic"`. Every NATIVE leg is built with Wire =
// "anthropic" (resolveLaunchEndpoint's sourceNativeAnthropic branch, copied
// verbatim by routeFor), so the condition was false on exactly the leg the
// rewrite exists for: the alias ("fable") went to api.anthropic.com unresolved
// — the 2026-09-02 incident resolveNativeModelAlias's own doc records. The
// discriminator is BaseURL: the native passthrough is the leg WITHOUT one.
//
// The pre-existing pin (TestOversizeSwap_ResolvesBareAliasAgainstRealCatalog)
// hand-builds its oversize route with Wire unset, a shape no plan produces,
// which is why the defect survived it.

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startRouteTestProxy starts the real routed proxy (RunAnthropicOpenAIProxyRoutes)
// on a loopback port and returns its base URL and client token.
func startRouteTestProxy(t *testing.T, table proxyRouteTable) (proxyURL, token string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	tok, err := newProxyClientToken()
	if err != nil {
		t.Fatal(err)
	}
	table.ClientToken = tok
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, table) }()
	t.Cleanup(func() { ln.Close() })
	return "http://" + ln.Addr().String(), tok
}

// postOverflowingAnthropicRequest posts a body that cannot fit a tiny
// ContextWindow, so the handler's context-fit clamp runs oversizeSwap.
func postOverflowingAnthropicRequest(t *testing.T, proxyURL, token, model string) int {
	t.Helper()
	long := strings.Repeat("padding content to exceed a tiny context window ", 50)
	body, _ := json.Marshal(map[string]any{
		"model":      model,
		"max_tokens": 10,
		"messages":   []map[string]any{{"role": "user", "content": long}},
	})
	req, err := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode
}

// The contract: an oversize crossover onto a claude/* leg sends the model id
// Anthropic's wire API accepts, not the CLI tier word — on the route built by
// resolveLaunchEndpoint + routeFor, which is the shape production uses.
func TestOversizeNativeCrossoverResolvesTheTierAliasOnThePlanBuiltLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-oversize-alias")
	// Deterministic, offline alias resolution: "fable" → "claude-fable-5-1".
	stubNativeModelCatalog(t, map[string]string{"fable": "claude-fable-5-1"})

	// The oversize leg EXACTLY as the plan builds it.
	ep, err := resolveLaunchEndpoint("claude/fable")
	if err != nil {
		t.Fatalf("resolveLaunchEndpoint(claude/fable) = %v, want the native tier", err)
	}
	prodRoute := routeFor(ep)
	if prodRoute.Wire != "anthropic" || prodRoute.BaseURL != "" {
		t.Fatalf("premise changed: the plan's native leg is Wire=%q BaseURL=%q, want the anthropic-wire, base-URL-less native passthrough",
			prodRoute.Wire, prodRoute.BaseURL)
	}

	// CONTROL: a leg with a BaseURL — a REMOTE anthropic-wire endpoint, whose
	// UpstreamModel is the vendor's real id and must go through untouched.
	remoteRoute := prodRoute
	remoteRoute.BaseURL = "http://remote.invalid/v1"

	oaicaUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer oaicaUpstream.Close()

	// seen records the model id each upstream was handed.
	run := func(label string, oversize proxyRoute) (nativeModel, remoteModel string) {
		nativeUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &req)
			nativeModel = req.Model
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
		}))
		defer nativeUpstream.Close()
		remoteUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &req)
			remoteModel = req.Model
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"id":"msg_x","type":"message","role":"assistant","content":[{"type":"text","text":"ok"}]}`))
		}))
		defer remoteUpstream.Close()

		origMsg := nativeAnthropicUpstream
		nativeAnthropicUpstream = nativeUpstream.URL
		defer func() { nativeAnthropicUpstream = origMsg }()

		// anthropicPassthroughTarget resolves a BaseURL leg's credential from
		// the route; give the remote leg one, and point it at the stub.
		target := oversize
		if target.BaseURL != "" {
			target.BaseURL = remoteUpstream.URL
			target.Key = "remote-key"
		}

		smallRoute := proxyRoute{BaseURL: oaicaUpstream.URL, UpstreamModel: "oaica-35b-a3b-vision",
			Label: "router:oaica", ContextWindow: 100}
		table := proxyRouteTable{
			Default:  smallRoute,
			ByModel:  map[string]proxyRoute{"oaica-35b-a3b-vision": smallRoute},
			Oversize: target,
		}
		proxyURL, token := startRouteTestProxy(t, table)
		if code := postOverflowingAnthropicRequest(t, proxyURL, token, "oaica-35b-a3b-vision"); code != http.StatusOK {
			t.Fatalf("%s: proxy returned %d, want 200 (the crossover should have saved this request)", label, code)
		}
		return nativeModel, remoteModel
	}

	nativeModel, _ := run("native", prodRoute)
	if nativeModel != "claude-fable-5-1" {
		t.Errorf("the plan-built native oversize leg sent model=%q to api.anthropic.com, want the resolved id %q: "+
			"the CLI tier word (%q) is what Anthropic's wire API answers \"model: fable not found\" to — the body's "+
			"model was rewritten from the client's valid id into an unaccepted alias instead of resolving it "+
			"(resolveNativeModelAlias; 2026-09-02 incident).", nativeModel, "claude-fable-5-1", prodRoute.UpstreamModel)
	}

	// The remote anthropic-wire leg is the exception: its UpstreamModel is
	// already the vendor's id and must not be rewritten into an Anthropic one.
	_, remoteModel := run("remote", remoteRoute)
	if remoteModel != prodRoute.UpstreamModel {
		t.Errorf("a REMOTE anthropic-wire oversize leg sent model=%q, want its own upstream id %q — the rewrite must "+
			"key on the leg having no BaseURL, not on its wire", remoteModel, prodRoute.UpstreamModel)
	}
}
