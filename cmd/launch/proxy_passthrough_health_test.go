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

// Q1: a passthrough leg that fails must feed the breaker, not read healthy.
func TestPassthroughFailuresOpenTheCircuit(t *testing.T) {
	table := proxyRouteTable{
		breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
	}
	route := proxyRoute{BaseURL: "https://zai.example/v1", Label: "zai"}

	for i := 0; i < 3; i++ {
		feedPassthroughRouteHealth(table, route, "sess-1", http.StatusInternalServerError)
	}
	if !table.breakers.open(route.BaseURL) {
		t.Errorf("after 3 upstream 500s on a passthrough leg the breaker is still closed — a dead passthrough primary is never failed over, contradicting CLAUDE_TIERS.md's failover section")
	}

	// A 4xx is the leg working (bad request, shedding) — must not open it.
	fresh := proxyRouteTable{breakers: &routeBreakers{}, escalations: &routeEscalations{}}
	for i := 0; i < 5; i++ {
		feedPassthroughRouteHealth(fresh, route, "sess-1", http.StatusTooManyRequests)
	}
	if fresh.breakers.open(route.BaseURL) {
		t.Errorf("429s opened a passthrough leg's breaker — shedding is the leg working, not failing")
	}

	// A native leg has no BaseURL; it must still be tracked under its own key
	// rather than silently collapsing onto the empty-string key.
	native := proxyRoute{Label: "claude-native"}
	nativeTable := proxyRouteTable{breakers: &routeBreakers{}, escalations: &routeEscalations{}}
	for i := 0; i < 3; i++ {
		feedPassthroughRouteHealth(nativeTable, native, "sess-1", 0) // transport failure
	}
	if !nativeTable.breakers.open(nativeAnthropicBreakerKey) {
		t.Errorf("transport failures on the native claude/* leg did not open its breaker")
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
