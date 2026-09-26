package launch

// models_passthrough_keyless_remote_integrity_test.go — GET /v1/models
// answered an anthropic-wire REMOTE out of api.anthropic.com, ungated
// (2026-09-26 audit).
//
// The /v1/models handler branches on table.Default.NativePassthrough, and
// routeFor sets that flag for an anthropic-WIRE remote as well as for the
// native claude/* leg (it has to: the flag is what makes /v1/messages skip
// OpenAI translation on those rows). It then asked
// anthropicRemoteModelsTarget() for that row's own list, and on `ok == false`
// — which is what a vendor row whose key is not resolvable returns — fell
// through to nativeAnthropicModelsPassthrough:
//
//   - the ENTITLEMENT GATE was skipped, although entitlement.go's rule names
//     "the Anthropic-wire remote in the NativePassthrough branch" as covered
//     and the same row IS gated on /v1/messages;
//   - the answer came from api.anthropic.com under the user's own Anthropic
//     credential, so a vendor's leg was described by Anthropic's catalogue;
//   - and it said nothing about the actual problem, where /v1/messages for the
//     same keyless row answers 401 "no credential for <model> — run `oaica
//     auth login`".
//
// The native leg is ungated by contract and stays that way: it is
// api.anthropic.com under the user's own credential, which is exactly what
// made the fall-through indistinguishable from it at the call site.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// startKeylessModelsProxy runs the proxy for a default leg built from ep, and
// returns its base URL.
func startKeylessModelsProxy(t *testing.T, ep launchEndpoint) string {
	t.Helper()
	route := routeFor(ep)
	if !route.NativePassthrough {
		t.Fatalf("setup: an anthropic-wire remote is no longer NativePassthrough — this test no longer covers the branch: %+v", route)
	}
	return startEntitlementTestProxy(t, route, map[string]proxyRoute{ep.UpstreamModel: route})
}

func getModels(t *testing.T, proxyURL string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, proxyURL+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("x-api-key", "proxy-client-token")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// anthropicCatalogStub stands in for api.anthropic.com and counts what reached
// it.
func anthropicCatalogStub(t *testing.T) *int {
	t.Helper()
	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5","type":"model"},{"id":"claude-opus-5-5","type":"model"}]}`))
	}))
	t.Cleanup(srv.Close)
	old := nativeAnthropicModelsUpstream
	nativeAnthropicModelsUpstream = srv.URL + "/v1/models"
	t.Cleanup(func() { nativeAnthropicModelsUpstream = old })
	return &hits
}

// A keyless vendor row is not Anthropic's leg, so its catalogue request never
// becomes one — even when the machine DOES have an Anthropic credential that
// would let the request succeed.
func TestAKeylessAnthropicWireRemoteIsNotAnsweredFromAnthropic(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	// The credential that made the fall-through invisible: it succeeds.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-the-users-own-login")
	nativeHits := anthropicCatalogStub(t)

	proxy := startKeylessModelsProxy(t, launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: "http://127.0.0.1:1", Token: "",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})

	code, body := getModels(t, proxy)
	if *nativeHits != 0 {
		t.Errorf("api.anthropic.com was asked for the catalogue %d time(s) on behalf of a vendor's leg — its list is not that leg's list", *nativeHits)
	}
	if code == http.StatusOK {
		t.Errorf("a keyless anthropic-wire remote answered /v1/models with %d and %s", code, truncateForError(body))
	}
	if !strings.Contains(body, "credential") {
		t.Errorf("the client is not told the leg has no credential, which is what /v1/messages for the same row says: %s", truncateForError(body))
	}
}

// And with the gate armed, the row is denied on this endpoint exactly as it is
// on /v1/messages — with a resolvable key, so the only thing that can stop it
// is the gate.
func TestTheEntitlementGateCoversModelsOnAnAnthropicWireRemote(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3","type":"model"}]}`))
	}))
	t.Cleanup(upstream.Close)

	nativeHits := anthropicCatalogStub(t)
	withEntitlementGate(t, true, denyAllEntitlementCheck)

	proxy := startKeylessModelsProxy(t, launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-plan-key",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})

	code, body := getModels(t, proxy)
	if code != http.StatusForbidden {
		t.Fatalf("armed deny-all gate: /v1/models on an anthropic-wire remote answered %d, want 403 — the row is gated on /v1/messages and not here.\nbody: %s", code, truncateForError(body))
	}
	if upstreamHits != 0 {
		t.Errorf("the denial still spent upstream time: %d hit(s)", upstreamHits)
	}
	if *nativeHits != 0 {
		t.Errorf("a denied catalogue was still fetched from api.anthropic.com (%d hit(s))", *nativeHits)
	}
}

// With the gate off — its real default — the row's own list is served and
// nothing about that changed.
func TestAnAnthropicWireRemoteServesItsOwnModelList(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstreamHits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3","type":"model"}]}`))
	}))
	t.Cleanup(upstream.Close)

	nativeHits := anthropicCatalogStub(t)

	proxy := startKeylessModelsProxy(t, launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-plan-key",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})

	code, body := getModels(t, proxy)
	if code != http.StatusOK {
		t.Fatalf("/v1/models on a keyed anthropic-wire remote answered %d, want 200\nbody: %s", code, truncateForError(body))
	}
	if !strings.Contains(body, "glm-5.3") {
		t.Errorf("the row's own list did not come back: %s", truncateForError(body))
	}
	if upstreamHits != 1 {
		t.Errorf("the row's own endpoint was hit %d time(s), want 1", upstreamHits)
	}
	if *nativeHits != 0 {
		t.Errorf("a working remote's catalogue was fetched from api.anthropic.com %d time(s)", *nativeHits)
	}
}
