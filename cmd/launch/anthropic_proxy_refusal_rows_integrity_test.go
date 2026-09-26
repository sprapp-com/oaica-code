package launch

// anthropic_proxy_refusal_rows_integrity_test.go — a turn the Anthropic proxy
// refuses locally wrote no row at all (2026-09-26 audit, ninth round, auditor B).
//
// RunAnthropicOpenAIProxyRoutes builds its request-log row well past every gate
// it applies to a turn — the proxy token, the body cap, the request parse, the
// entitlement check, the context-fit clamp — so a refusal returned with no
// evidence: `oaica usage` reported ERR 0 for a session whose every turn was
// refused here. The prompt-too-long 400 is the case that matters most, since it
// is the auto-compaction call the report exists to show.
//
// The row is now created at the top of the handler and written only by refuse(),
// so a turn that reaches a leg still logs exactly one row — the translated path
// logs its own, and the passthrough legs log inside their own functions. That
// "exactly one" is the control below: an over-eager refusal row would double
// every successful turn.

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// A denied turn is a turn: it must be in the report, as an error.
func TestALocallyRefusedAnthropicTurnIsReported(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	hits := 0
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}]}`))
	}))
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "deepseek", BaseURL: upstream.URL + "/v1", Token: "sk-remote",
		UpstreamModel: "deepseek-v4-flash", Wire: "openai",
	}})

	withEntitlementGate(t, true, denyAllEntitlementCheck)

	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"deepseek/deepseek-v4-flash": route})
	code, body, _ := postEntitlementTestMessage(t, proxy, "deepseek/deepseek-v4-flash")
	if code != http.StatusForbidden {
		t.Fatalf("premise: an armed deny-all gate answered %d, want 403\nbody: %s", code, body)
	}
	if hits != 0 {
		t.Fatalf("premise: the denial still spent upstream time (%d hit(s))", hits)
	}

	requests, errors := reportedTurn(t)
	if requests != 1 {
		t.Errorf("`oaica usage` reports %d requests for a turn this proxy refused with a 403, want 1 — the row was built behind every gate, so the refusal left no evidence and the report says the session sent nothing", requests)
	}
	if errors != 1 {
		t.Errorf("`oaica usage` reports %d errors for a refused turn, want 1 — ERR 0 is the reading a user opens the report to rule out", errors)
	}
}

// Control: the row must be written ONCE per turn. A refusal row that also fired
// on the successful path would double every request the report counts.
func TestAServedAnthropicTurnIsCountedOnce(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`))
	}))
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "deepseek", BaseURL: upstream.URL + "/v1", Token: "sk-remote",
		UpstreamModel: "deepseek-v4-flash", Wire: "openai",
	}})

	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"deepseek/deepseek-v4-flash": route})
	code, body, _ := postEntitlementTestMessage(t, proxy, "deepseek/deepseek-v4-flash")
	if code != http.StatusOK {
		t.Fatalf("premise: a served turn answered %d, want 200\nbody: %s", code, body)
	}

	requests, errors := reportedTurn(t)
	if requests != 1 {
		t.Errorf("`oaica usage` reports %d requests for one served turn, want exactly 1 — the refusal row is written for turns that reached no leg", requests)
	}
	if errors != 0 {
		t.Errorf("`oaica usage` reports %d errors for a turn that was served, want 0", errors)
	}
}
