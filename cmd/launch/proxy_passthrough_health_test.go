package launch

// proxy_passthrough_health_test.go — four behaviours a passthrough leg lost by
// returning from the /v1/messages handler before the translated path's
// bookkeeping ran (2026-09-26 audit, second round):
//
//  1. the circuit breaker / `auto` escalation never saw a passthrough leg's
//     upstream status, so docs/CLAUDE_TIERS.md's failover promise ("3
//     consecutive failures … open the circuit … fail over when the selected
//     leg's breaker is OPEN") was false for every native and Anthropic-wire
//     leg;
//  2. the request log recorded the upstream's status rather than the client's,
//     so an upstream error sent over HTTP 200 (which this proxy turns into a
//     502) was logged as a success and `oaica usage` read failed sessions as
//     clean;
//  3. X-Session-Id — docs/CLAUDE_TIERS.md promises it on "every request that
//     launch's proxy forwards" — was never set on a passthrough leg, the one
//     leg it exists for;
//  4. GET /v1/models ignored a row's declared ModelsURL.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// readRequestLogRows returns every parsed row of this HOME's requests.log.
func readRequestLogRows(t *testing.T) []requestLogEntry {
	t.Helper()
	path, err := requestLogPath()
	if err != nil {
		t.Fatalf("requestLogPath: %v", err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("no requests.log was written, so this test proves nothing: %v", err)
	}
	var rows []requestLogEntry
	for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		var e requestLogEntry
		if err := json.Unmarshal([]byte(line), &e); err != nil {
			t.Fatalf("requests.log line is not JSON: %v\n%s", err, line)
		}
		rows = append(rows, e)
	}
	return rows
}

// Q1: a failing passthrough leg must actually be failed over — asserted on the
// leg the CLIENT is routed to, not on a breaker key the test picked itself.
//
// That distinction is the whole point: the round-7 fix filed native legs under
// a constant nothing read, so a test asserting `breakers.open(thatConstant)`
// passed while a native primary still served every failing turn (2026-09-26
// audit, third round). These tests go through the proxy.
func TestNativePrimaryFailsOverToItsFallback(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	nativeHits, fallbackHits := 0, 0
	nativeErr := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		nativeHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer nativeErr.Close()
	fallback := openAITestUpstream(&fallbackHits)
	defer fallback.Close()

	prevUpstream := nativeAnthropicUpstream
	nativeAnthropicUpstream = nativeErr.URL
	t.Cleanup(func() { nativeAnthropicUpstream = prevUpstream })
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")

	nativeRoute := routeFor(launchEndpoint{Source: sourceNativeAnthropic, RemoteEndpoint: RemoteEndpoint{
		Name: "native-anthropic", UpstreamModel: "claude-opus-5", Wire: "anthropic",
	}})
	if nativeRoute.BaseURL != "" {
		t.Fatalf("premise: a native route now carries BaseURL %q — the empty-BaseURL key is what this test is about", nativeRoute.BaseURL)
	}
	fbRoute := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai", BaseURL: fallback.URL + "/v1", Token: "sk", UpstreamModel: "glm-5.3", Wire: "openai",
	}})

	proxy := startProxyWithOversize(t, proxyRouteTable{
		// weighted, not auto: this test isolates the CIRCUIT BREAKER. Under
		// auto, the escalation counter (which keys on route.BaseURL and so was
		// never broken) fails over on its own and would hide a breaker written
		// under a key nobody reads — which is exactly how the round-7 gap
		// survived its own test.
		Default: nativeRoute, Fallbacks: []proxyRoute{fbRoute}, Policy: RouteWeighted,
	})

	for i := 0; i < 3; i++ {
		postEntitlementTestMessage(t, proxy, nativeRoute.UpstreamModel)
	}
	_, body, hdr := postEntitlementTestMessage(t, proxy, nativeRoute.UpstreamModel)
	if fallbackHits == 0 {
		t.Errorf("a native primary served 4 consecutive upstream 500s and the configured fallback was never used (nativeHits=%d, route=%q) — CLAUDE_TIERS.md's failover section promises a dead leg is replaced; the breaker the feed writes is not the breaker selectRoute reads\nbody: %s",
			nativeHits, hdr.Get("X-Oaica-Route"), body)
	}
}

// The same, for an Anthropic-wire REMOTE primary (BaseURL set): the shape that
// already worked, kept as the control that isolates the native key.
func TestAnthropicWireRemotePrimaryFailsOver(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	deadHits, liveHits := 0, 0
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		deadHits++
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer dead.Close()
	live := openAITestUpstream(&liveHits)
	defer live.Close()

	deadRoute := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: dead.URL, Token: "sk", UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	liveRoute := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "minimax-coding-plan", BaseURL: live.URL + "/v1", Token: "sk", UpstreamModel: "minimax-m3", Wire: "openai",
	}})

	proxy := startProxyWithOversize(t, proxyRouteTable{
		// weighted for the same reason as the native case above: isolate the breaker.
		Default: deadRoute, Fallbacks: []proxyRoute{liveRoute}, Policy: RouteWeighted,
		ByModel: map[string]proxyRoute{deadRoute.UpstreamModel: deadRoute},
	})

	for i := 0; i < 3; i++ {
		postEntitlementTestMessage(t, proxy, deadRoute.UpstreamModel)
	}
	_, body, hdr := postEntitlementTestMessage(t, proxy, deadRoute.UpstreamModel)
	if liveHits == 0 {
		t.Errorf("an Anthropic-wire remote primary served 4 consecutive 500s without failing over (deadHits=%d, route=%q)\nbody: %s", deadHits, hdr.Get("X-Oaica-Route"), body)
	}
}

// The native OVERSIZE leg is filed under the key oversizeSwap itself checks —
// route_policy.go's own rule, not the primary's key.
func TestNativeOversizeLegIsFiledUnderTheOversizeKey(t *testing.T) {
	nativeOversize := proxyRoute{Label: "native-anthropic-oversize", NativePassthrough: true}
	if passthroughBreakerKey(nativeOversize, true) != nativeOversizeBreakerKey {
		t.Errorf("a native oversize leg is filed under %q, but oversizeSwap checks %q — it would be selected for every oversize crossover forever",
			passthroughBreakerKey(nativeOversize, true), nativeOversizeBreakerKey)
	}
	// And not the primary's identity: a dead oversize leg must not open the
	// primary's circuit, nor the primary's failures open the oversize's.
	if passthroughBreakerKey(nativeOversize, true) == passthroughBreakerKey(proxyRoute{Label: "native-primary", NativePassthrough: true}, false) {
		t.Error("the native oversize leg shares a breaker key with the native primary — one being down would take the other out")
	}
	// An ordinary remote leg is filed under its own BaseURL, which is what
	// selectRoute reads.
	remote := proxyRoute{BaseURL: "https://zai.example/v1", Label: "zai"}
	if passthroughBreakerKey(remote, false) != remote.BaseURL {
		t.Errorf("a remote passthrough leg is filed under %q, want its BaseURL %q", passthroughBreakerKey(remote, false), remote.BaseURL)
	}
}

// And behaviourally: a native oversize leg that has failed is no longer handed
// an oversize crossover, because oversizeSwap checks the key the feed writes.
func TestDeadNativeOversizeLegIsNotChosenForCrossover(t *testing.T) {
	table := proxyRouteTable{
		breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
		Policy:      RouteAuto,
		Oversize:    proxyRoute{Label: "native-anthropic-oversize", NativePassthrough: true},
	}
	primary := proxyRoute{Label: "remote-primary", BaseURL: "https://zai.example/v1", ContextWindow: 100}

	for i := 0; i < 3; i++ {
		feedPassthroughRouteHealth(table, table.Oversize, "sess-1",
			passthroughBreakerKey(table.Oversize, true), 0, false) // transport failures
	}
	if _, ok := table.oversizeSwap(primary, 50_000, 0); ok {
		t.Error("a native oversize leg with 3 consecutive transport failures was still chosen for the crossover — oversizeSwap checks nativeOversizeBreakerKey, so the feed has to write that same key")
	}
}

// A 4xx is the leg working (bad request, shedding) and must not count.
func TestPassthrough4xxDoesNotOpenTheBreaker(t *testing.T) {
	table := proxyRouteTable{breakers: &routeBreakers{}, escalations: &routeEscalations{}}
	route := proxyRoute{BaseURL: "https://zai.example/v1", Label: "zai"}
	for i := 0; i < 5; i++ {
		feedPassthroughRouteHealth(table, route, "sess-1", passthroughBreakerKey(route, false), http.StatusTooManyRequests, false)
	}
	if table.breakers.open(route.BaseURL) {
		t.Error("429s opened a passthrough leg's breaker — shedding is the leg working, not failing")
	}
}

// Q2: the logged status is the one the CLIENT received.
func TestRequestLogRecordsTheStatusTheClientGot(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The shape vLLM and this fleet's gateway both use: an error object
		// over HTTP 200.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"error":{"message":"model is gone","type":"invalid_request_error"}}`))
	}))
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "deepseek", BaseURL: upstream.URL + "/v1", Token: "sk-remote",
		UpstreamModel: "deepseek-v4-flash", Wire: "openai",
	}})
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"deepseek/deepseek-v4-flash": route})

	code, body, _ := postEntitlementTestMessage(t, proxy, "deepseek/deepseek-v4-flash")
	if code == http.StatusOK {
		t.Fatalf("premise: the proxy returned 200 for an upstream error object, so there is no status mismatch to log\nbody: %s", body)
	}

	rows := readRequestLogRows(t)
	if len(rows) == 0 {
		t.Fatal("no request log row was written")
	}
	last := rows[len(rows)-1]
	if last.StatusCode != code {
		t.Errorf("requests.log recorded status_code = %d but the client received %d — `oaica usage` reports failed turns as successful", last.StatusCode, code)
	}
}

// Q3: the affinity header reaches a passthrough upstream.
func TestPassthroughLegsSendTheSessionHeader(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	got := make(chan string, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got <- r.Header.Get("X-Session-Id")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-remote",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	if !route.NativePassthrough {
		t.Fatalf("setup: an anthropic-wire user remote is no longer NativePassthrough — this test no longer covers the leg it was written for: %+v", route)
	}

	ln := startProxyWithOversize(t, proxyRouteTable{
		Default: route, ByModel: map[string]proxyRoute{"zai-coding-plan/glm-5.3": route},
		SessionID: "sess-affinity-1",
	})
	code, body, _ := postEntitlementTestMessage(t, ln, "zai-coding-plan/glm-5.3")
	if code != http.StatusOK {
		t.Fatalf("passthrough leg answered %d, want 200\nbody: %s", code, body)
	}
	if h := <-got; h != "sess-affinity-1" {
		t.Errorf("X-Session-Id = %q on a passthrough request, want %q — CLAUDE_TIERS.md promises it on every request launch's proxy forwards, and a consistent-hash remote is the leg it exists for", h, "sess-affinity-1")
	}
}

// Q4: /v1/models honours a row's declared ModelsURL.
func TestModelsEndpointHonoursTheDeclaredModelsURL(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	served := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- r.URL.Path
		if r.URL.Path == "/custom/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"perplexity/sonar"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	route := proxyRoute{
		BaseURL: upstream.URL, Label: "perplexity", Key: "sk-remote",
		ModelsURL: upstream.URL + "/custom/models", Wire: "openai",
	}
	proxy := startEntitlementTestProxy(t, route, nil)

	resp, err := http.Get(proxy + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/models answered %d, want 200 — the row declares models_path %q but the proxy hard-wired <base>/models and got the host's 404", resp.StatusCode, route.ModelsURL)
	}
	if got := <-served; got != "/custom/models" {
		t.Errorf("the models request went to %q, want /custom/models", got)
	}
}

// The same rule on the Anthropic-wire branch: a plan row that declares
// models_path serves its list there, not at the sibling of its messages path.
// Latent today (the one shipped models_path row is wire "openai") — pinned so
// the second branch does not have to be found twice.
func TestAnthropicWireModelsBranchHonoursTheDeclaredModelsURL(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	served := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		served <- r.URL.Path
		if r.URL.Path == "/custom/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3"}]}`))
			return
		}
		http.NotFound(w, r)
	}))
	defer upstream.Close()

	route := proxyRoute{
		BaseURL: upstream.URL, Label: "zai-coding-plan", Key: "sk-remote",
		Wire: "anthropic", ModelsURL: upstream.URL + "/custom/models", NativePassthrough: true,
	}
	if !route.NativePassthrough {
		t.Fatalf("setup: this route is not NativePassthrough: %+v", route)
	}
	proxy := startEntitlementTestProxy(t, route, nil)

	resp, err := http.Get(proxy + "/v1/models")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /v1/models on an anthropic-wire row declaring models_path answered %d, want 200 — this branch hard-wired <base>/models", resp.StatusCode)
	}
	if got := <-served; got != "/custom/models" {
		t.Errorf("the anthropic-wire models request went to %q, want /custom/models", got)
	}
}
