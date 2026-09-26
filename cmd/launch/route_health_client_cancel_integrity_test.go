package launch

// route_health_client_cancel_integrity_test.go — a request the CALLER hung up
// on was fed to the circuit breaker and to the `auto` policy's per-session
// escalation as an upstream failure (2026-09-26 audit).
//
// Both route-health feeds read "no response" as "the leg is down": the
// translated path's transport-error branch recorded a failure unconditionally,
// and feedPassthroughRouteHealth's `status == 0` case cannot tell "the upstream
// never answered" from "the caller cancelled". But the upstream request is
// built on r.Context(), so a Ctrl-C'd turn — or any client that hangs up
// mid-flight — surfaces as exactly that transport error. Three of them in a
// row take the leg's circuit out for breakerOpenFor, and two under `auto`
// steer the whole session onto another tier, so a user abandoning three slow
// turns on a healthy leg is served from a different one for the next 90
// seconds.
//
// The retry helper already draws this line on the wire ("Transport errors
// before any response are safe to retry EXCEPT when the caller themselves hung
// up (context canceled) -- that is not an upstream failure"); the health feeds
// did not.

import (
	"bytes"
	"context"
	"encoding/json"
	"net"
	"net/http"
	"sync"
	"testing"
	"time"
)

// breakerFails reads a leg's consecutive-failure count without creating the
// breaker entry a read of a healthy leg must not create.
func breakerFails(table proxyRouteTable, baseURL string) int {
	if table.breakers == nil {
		return 0
	}
	table.breakers.mu.Lock()
	defer table.breakers.mu.Unlock()
	b := table.breakers.m[baseURL]
	if b == nil {
		return 0
	}
	return int(b.fails.Load())
}

// escalationFails reads a session's consecutive-failure count under the same
// rule.
func escalationFails(table proxyRouteTable, sessionID string) int {
	if table.escalations == nil {
		return 0
	}
	table.escalations.mu.Lock()
	e := table.escalations.m[sessionID]
	table.escalations.mu.Unlock()
	if e == nil {
		return 0
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.fails
}

// awaitRouteHealth waits for the request's bookkeeping to land: the feeds run
// within microseconds of the handler's return, but the request-log row is
// written BEFORE the feed on the passthrough path and after it on the
// translated one, so neither order can be assumed.
func awaitRouteHealth(table proxyRouteTable, sessionID, baseURL string) (int, int) {
	deadline := time.Now().Add(750 * time.Millisecond)
	for {
		b, e := breakerFails(table, baseURL), escalationFails(table, sessionID)
		if b != 0 || e != 0 || time.Now().After(deadline) {
			return b, e
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// sendMessageWithCancel posts a /v1/messages body to the proxy on a context
// this test controls, waits for the upstream to have been reached, then
// cancels — a client hanging up mid-flight.
func sendMessageWithCancel(t *testing.T, proxy string, hit <-chan struct{}) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	body, err := json.Marshal(map[string]any{
		"model": "glm-5.3", "max_tokens": 16, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxy+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")

	go func() {
		resp, err := http.DefaultClient.Do(req)
		if err == nil {
			resp.Body.Close()
		}
	}()

	select {
	case <-hit:
	case <-time.After(10 * time.Second):
		t.Fatal("the upstream was never reached, so nothing was cancelled mid-flight")
	}
	cancel()
}

// hangingUpstream is a raw listener that accepts connections and never answers
// them: an upstream that has been reached and holds. A listener rather than an
// httptest server because the point is that this leg NEVER responds — an HTTP
// handler that returns, or a server that gives up on a disconnected client,
// would hand the proxy a status and the request would stop being the
// "no response at all" case these tests are about.
//
// Each accepted connection is held until the test's cleanup closes it, which
// is deliberately after the assertions: releasing the connection earlier would
// turn the cancellation under test into an ordinary connection reset.
func hangingUpstream(t *testing.T) (string, <-chan struct{}) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	hit := make(chan struct{}, 8)
	var mu sync.Mutex
	var conns []net.Conn
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, c)
			mu.Unlock()
			hit <- struct{}{}
		}
	}()
	t.Cleanup(func() {
		ln.Close()
		mu.Lock()
		defer mu.Unlock()
		for _, c := range conns {
			c.Close()
		}
	})
	return "http://" + ln.Addr().String(), hit
}

// The translated (OpenAI-wire) leg.
func TestACancelledRequestDoesNotFeedTheTranslatedLegsHealth(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream, hit := hangingUpstream(t)

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai", BaseURL: upstream + "/v1", Token: "sk", UpstreamModel: "glm-5.3", Wire: "openai",
	}})
	table := proxyRouteTable{
		Default: route, Policy: RouteAuto, SessionID: "sess-cancel-openai",
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
	proxy := startProxyWithOversize(t, table)

	sendMessageWithCancel(t, proxy, hit)

	fails, escFails := awaitRouteHealth(table, table.SessionID, route.BaseURL)
	if fails != 0 {
		t.Errorf("a request the client cancelled mid-flight was counted as %d consecutive failure(s) of %s — %d of those (what breakerFailsToOpen is) take the leg's circuit out for %v, so a user abandoning slow turns on a HEALTHY leg is failed over on the next one",
			fails, route.BaseURL, breakerFailsToOpen, breakerOpenFor)
	}
	if escFails != 0 {
		t.Errorf("a cancelled request was counted as %d consecutive failure(s) toward the `auto` escalation of session %q, which arms at %d and holds for %v",
			escFails, table.SessionID, autoEscalateAfterFails, autoEscalateHoldFor)
	}
}

// The passthrough (Anthropic-wire) leg — the path where "no response" and
// "caller hung up" are both the status 0 the feed is handed.
func TestACancelledPassthroughRequestDoesNotFeedTheLegsHealth(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream, hit := hangingUpstream(t)

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream, Token: "sk-remote", UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	if !route.NativePassthrough {
		t.Fatalf("setup: an anthropic-wire remote is no longer NativePassthrough, so this test no longer covers the passthrough path: %+v", route)
	}
	table := proxyRouteTable{
		Default: route, Policy: RouteAuto, SessionID: "sess-cancel-passthrough",
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
	proxy := startProxyWithOversize(t, table)

	sendMessageWithCancel(t, proxy, hit)

	fails, escFails := awaitRouteHealth(table, table.SessionID, route.BaseURL)
	if fails != 0 {
		t.Errorf("a passthrough request the client cancelled mid-flight was counted as %d failure(s) of %s — the feed is handed status 0 by the transport error and cannot tell it from a dead upstream",
			fails, route.BaseURL)
	}
	if escFails != 0 {
		t.Errorf("a cancelled passthrough request was counted as %d consecutive failure(s) toward the `auto` escalation of session %q",
			escFails, table.SessionID)
	}
}

// The controls: a genuine transport failure — no caller cancellation — still
// fails the leg, on both paths, so these tests cannot be passed by never
// feeding the route health at all.
func TestADeadUpstreamStillFailsTheTranslatedLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai", BaseURL: "http://" + deadUpstreamAddress(t) + "/v1", Token: "sk", UpstreamModel: "glm-5.3", Wire: "openai",
	}})
	table := proxyRouteTable{
		Default: route, Policy: RouteAuto, SessionID: "sess-dead-openai",
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
	proxy := startProxyWithOversize(t, table)

	if code, _, _ := postEntitlementTestMessage(t, proxy, route.UpstreamModel); code == http.StatusOK {
		t.Fatalf("premise: a refused upstream answered %d", code)
	}

	fails, escFails := awaitRouteHealth(table, table.SessionID, route.BaseURL)
	if fails == 0 {
		t.Errorf("a refused connection did not count against %s — the breaker can never open", route.BaseURL)
	}
	if escFails == 0 {
		t.Errorf("a refused connection did not count toward the `auto` escalation of session %q", table.SessionID)
	}
}

func TestADeadUpstreamStillFailsAPassthroughLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: "http://" + deadUpstreamAddress(t), Token: "sk-remote", UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	if !route.NativePassthrough {
		t.Fatalf("setup: an anthropic-wire remote is no longer NativePassthrough: %+v", route)
	}
	table := proxyRouteTable{
		Default: route, Policy: RouteAuto, SessionID: "sess-dead-passthrough",
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
	proxy := startProxyWithOversize(t, table)

	if code, _, _ := postEntitlementTestMessage(t, proxy, route.UpstreamModel); code == http.StatusOK {
		t.Fatalf("premise: a refused passthrough upstream answered %d", code)
	}

	fails, escFails := awaitRouteHealth(table, table.SessionID, route.BaseURL)
	if fails == 0 {
		t.Errorf("a refused passthrough connection did not count against %s", route.BaseURL)
	}
	if escFails == 0 {
		t.Errorf("a refused passthrough connection did not count toward the `auto` escalation of session %q", table.SessionID)
	}
}
