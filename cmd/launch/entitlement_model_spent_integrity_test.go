package launch

// entitlement_model_spent_integrity_test.go — the entitlement gate was asked
// about the model the CLIENT named, not the one the request would spend
// (2026-09-26 audit, eighth round).
//
// entitlement.go's contract is that the check "sees exactly what a real
// entitlement decision would need: which model, which route/backend label". On
// the Anthropic-wire passthrough branch the request body is REWRITTEN to
// route.UpstreamModel after the gate has run, so for any spelling that
// selectRoute does not recognise the two differ: the gate judged the spelling
// while the upstream was sent - and the user was billed for - the leg's own
// model. A policy keyed on the model ("this account may not spend glm-5.3")
// was therefore bypassed by asking for "vendor/glm-5.3-not-this", which
// resolves to the same leg and is rewritten to glm-5.3 on the way out.
//
// The translated path is already correct — it passes reqModel, which IS what
// it puts in the upstream body (chatRequestToOpenAI, :1347) — and so is the
// oversize crossover, which passes route.UpstreamModel after re-pointing the
// request. This branch was the one that named one model and spent another.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// anthropicWireFixture answers a valid Anthropic response and records the model
// it was asked for, so the test can say what was actually spent.
func anthropicWireFixture(t *testing.T, seen *string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req struct {
			Model string `json:"model"`
		}
		_ = json.Unmarshal(b, &req)
		*seen = req.Model
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`))
	}))
}

func TestTheEntitlementGateIsAskedAboutTheModelThatIsSpent(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	spent := ""
	upstream := anthropicWireFixture(t, &spent)
	defer upstream.Close()

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: upstream.URL, Token: "sk-remote",
		UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"zai-coding-plan/glm-5.3": route})

	// A policy keyed on the model that is actually spent.
	withEntitlementGate(t, true, func(r *http.Request, routeLabel, reqModel string) EntitlementDecision {
		if reqModel == "glm-5.3" {
			return EntitlementDecision{Allowed: false, Reason: "this account may not spend glm-5.3"}
		}
		return EntitlementDecision{Allowed: true}
	})

	// Premise: the picker spelling that names the model directly is denied.
	code, body, _ := postEntitlementTestMessage(t, proxy, "zai-coding-plan/glm-5.3")
	if code != http.StatusForbidden {
		t.Fatalf("premise: HTTP %d for the spelling that names the model, want 403 — the gate no longer denies glm-5.3, so this test proves nothing\nbody: %s", code, body)
	}

	// The bypass: an unrecognised spelling resolves to the same leg, and the
	// body is rewritten to glm-5.3 on the way upstream.
	code, body, _ = postEntitlementTestMessage(t, proxy, "vendor/glm-5.3-not-this")
	if code != http.StatusForbidden {
		t.Errorf("HTTP %d for a spelling the gate never judged — the request was rewritten to and billed as %q (upstream got %q), which the policy denies\nbody: %s",
			code, route.UpstreamModel, spent, body)
	}
	if spent == "glm-5.3" {
		t.Errorf("the upstream was sent %q despite a policy denying it — GPU time and the user's balance were spent on a request the gate would have refused had it been asked about the right model", spent)
	}
}
