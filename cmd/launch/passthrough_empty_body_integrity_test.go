package launch

// passthrough_empty_body_integrity_test.go — a 200 with NO body was counted as
// a delivered turn on the passthrough legs (2026-09-26 audit, eighth round).
//
// anthropicPassthrough reports "the body reached the client's END" from
// `errors.Is(readErr, io.EOF)` alone, and an upstream that answers 200 and
// closes without writing a byte reaches EOF on its first read. So a leg that
// answers every turn with an empty 200 — a misconfigured gateway, a route that
// matched nothing, a proxy in front of a backend that is gone — relayed nothing
// to the client and was recorded HEALTHY. The breaker never opened, the session
// never failed over, and the client saw a dead turn on every request.
//
// The other two relay paths already refuse this shape, which is what makes it a
// defect here rather than a design choice: handleNonStreamResponse fails to
// decode an empty body ("unexpected end of JSON input") and answers 502, and
// handleStreamResponse never sets `completed` without an upstream frame, so
// both feed the route health `false`. Only the byte-for-byte relay called an
// empty body complete — "the body was relayed to its END" is true of nothing at
// all.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// emptyOKUpstream answers every request 200 with no body at all.
func emptyOKUpstream(hits *int) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			*hits++
		}
		w.WriteHeader(http.StatusOK)
	}))
}

func TestAnEmptyUpstreamBodyIsNotADeliveredTurn(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	empty := emptyOKUpstream(nil)
	defer empty.Close()

	body := []byte(`{"model":"glm-5.3","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))

	status, relayed := anthropicPassthrough(rec, req, body, empty.URL, "x-api-key", "sk-notareal", "sess-empty-body")
	if status != http.StatusOK {
		t.Fatalf("premise: upstream status relayed was %d, want 200 — this test is about a 200 that carried nothing", status)
	}
	if relayed {
		t.Errorf("anthropicPassthrough reported relayed=true for an upstream that sent zero bytes — nothing reached the client, and the route health feed records that as a healthy leg, so the circuit never opens and a session on it never fails over")
	}
}

// And behaviourally, through the proxy: an Anthropic-wire primary that answers
// every turn with an empty 200 must be failed over, exactly as one answering
// 500 is (proxy_passthrough_health_test.go).
func TestAnEmptyBodyPassthroughPrimaryFailsOver(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	emptyHits, liveHits := 0, 0
	empty := emptyOKUpstream(&emptyHits)
	defer empty.Close()
	live := openAITestUpstream(&liveHits)
	defer live.Close()

	emptyRoute := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: empty.URL, Token: "sk", UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	liveRoute := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "minimax-coding-plan", BaseURL: live.URL + "/v1", Token: "sk", UpstreamModel: "minimax-m3", Wire: "openai",
	}})
	if !emptyRoute.NativePassthrough {
		t.Fatalf("setup: an anthropic-wire user remote is no longer NativePassthrough — this test no longer covers the leg it was written for: %+v", emptyRoute)
	}

	proxy := startProxyWithOversize(t, proxyRouteTable{
		// weighted, to isolate the BREAKER: under auto the escalation counter
		// would fail over on its own and hide a health record written wrong.
		Default: emptyRoute, Fallbacks: []proxyRoute{liveRoute}, Policy: RouteWeighted,
		ByModel: map[string]proxyRoute{emptyRoute.UpstreamModel: emptyRoute},
	})

	for i := 0; i < 3; i++ {
		postEntitlementTestMessage(t, proxy, emptyRoute.UpstreamModel)
	}
	_, body, hdr := postEntitlementTestMessage(t, proxy, emptyRoute.UpstreamModel)
	if liveHits == 0 {
		t.Errorf("an Anthropic-wire primary that answered 4 empty 200s was never failed over (emptyHits=%d, route=%q) — an empty body is a turn no client can use, and CLAUDE_TIERS.md's failover section promises a dead leg is replaced\nbody: %s",
			emptyHits, hdr.Get("X-Oaica-Route"), body)
	}
}
