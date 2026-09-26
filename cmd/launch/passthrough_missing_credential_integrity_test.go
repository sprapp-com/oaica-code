package launch

// passthrough_missing_credential_integrity_test.go — a passthrough request that
// never left this process was booked as a failure of the LEG (2026-09-26
// audit).
//
// Both passthrough wrappers answer 401 when this side has no credential to
// send — the native leg when neither ANTHROPIC_API_KEY nor a `claude /login`
// token is available, a remote leg whose plan row declares a credential it
// cannot resolve. The native one returned (0, false), which is the SAME shape
// as "the upstream did not answer", so the handler's health feed read a local
// configuration problem as evidence about the backend: three turns and the
// circuit opened on a leg that was never contacted, moving the session to a
// fallback the user's missing credential does not fix either, while the
// failover changed what they were being charged for.
//
// The distinction the feed already makes for a cancelled request (clientGone)
// is the one that applies here: the leg said nothing, so nothing is recorded.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
)

// noLocalAnthropicCredential makes resolveNativeAnthropicAuth fail the way a
// user who has never logged in and set no key does.
func noLocalAnthropicCredential(t *testing.T) {
	t.Helper()
	t.Setenv("ANTHROPIC_API_KEY", "")
	old := readClaudeOAuthAccessTokenFn
	readClaudeOAuthAccessTokenFn = func() (string, error) { return "", nil }
	t.Cleanup(func() { readClaudeOAuthAccessTokenFn = old })
}

func nativeAnthropicTestRoute() proxyRoute {
	return routeFor(launchEndpoint{Source: sourceNativeAnthropic, RemoteEndpoint: RemoteEndpoint{
		Name: "native-anthropic", UpstreamModel: "claude-opus-5", Wire: "anthropic",
	}})
}

// The behavioural half: the client keeps getting the actionable 401, and the
// leg's circuit stays closed.
func TestAMissingLocalCredentialDoesNotOpenTheLegsCircuit(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	noLocalAnthropicCredential(t)

	nativeHits, fallbackHits := 0, 0
	native := anthropicTestUpstream(&nativeHits) // stands in for api.anthropic.com: must never be reached
	defer native.Close()
	fallback := openAITestUpstream(&fallbackHits)
	defer fallback.Close()

	prevUpstream := nativeAnthropicUpstream
	nativeAnthropicUpstream = native.URL
	t.Cleanup(func() { nativeAnthropicUpstream = prevUpstream })

	nativeRoute := nativeAnthropicTestRoute()
	fbRoute := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai", BaseURL: fallback.URL + "/v1", Token: "sk", UpstreamModel: "glm-5.3", Wire: "openai",
	}})
	proxy := startProxyWithOversize(t, proxyRouteTable{
		Default: nativeRoute, Fallbacks: []proxyRoute{fbRoute}, Policy: RouteWeighted,
	})

	for i := 0; i < 4; i++ {
		code, body, hdr := postEntitlementTestMessage(t, proxy, nativeRoute.UpstreamModel)
		if code != http.StatusUnauthorized {
			t.Fatalf("turn %d answered %d, want the 401 that tells the user to log in\nbody: %s", i, code, body)
		}
		if hdr.Get("X-Oaica-Route") != "" && i == 0 {
			t.Logf("served by %q", hdr.Get("X-Oaica-Route"))
		}
	}
	if nativeHits != 0 {
		t.Errorf("the request reached %s (%d times) with no credential resolved for it — the premise of this test is that this side answers before contacting the leg", nativeAnthropicUpstream, nativeHits)
	}
	if fallbackHits != 0 {
		t.Errorf("4 turns that failed on a MISSING LOCAL CREDENTIAL moved the session to the fallback (%d hits): the leg being failed over was never contacted, and the fallback cannot use an Anthropic login either — the user is now on a different bill for the same 401", fallbackHits)
	}
}

// The unit-level half, pinned at the classification itself so the reason is
// named: the "never attempted" answer must record NOTHING, while a real
// transport failure (status 0, attempted) must still fail the leg — the two
// are indistinguishable by status alone, which is what made this a defect.
func TestANeverAttemptedPassthroughRecordsNoHealthSignal(t *testing.T) {
	neverAttempted := proxyRoute{Label: "native-anthropic", NativePassthrough: true}
	table := proxyRouteTable{breakers: &routeBreakers{}, escalations: &routeEscalations{}}
	key := passthroughBreakerKey(neverAttempted, false)

	for i := 0; i < 5; i++ {
		feedPassthroughRouteHealth(table, neverAttempted, "sess-cred", key, passthroughNotAttempted, false, false)
	}
	if table.breakers.open(key) {
		t.Error("a leg that was never contacted opened its circuit — five locally-refused requests (no credential to send) mark a working backend dead")
	}

	// Control: the same feed with a real transport failure still fails it.
	for i := 0; i < breakerFailsToOpen; i++ {
		feedPassthroughRouteHealth(table, neverAttempted, "sess-cred", key, 0, false, false)
	}
	if !table.breakers.open(key) {
		t.Errorf("a leg that really did not answer %d times was NOT failed — the fix for the credential case must not swallow the transport case", breakerFailsToOpen)
	}
}

// And the wrappers must actually return that answer rather than a 0 that reads
// as a dead leg.
func TestAMissingCredentialIsReportedAsNeverAttempted(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	noLocalAnthropicCredential(t)

	body := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	status, relayed, attempted := nativeAnthropicPassthrough(rec, req, body, "sess-cred")
	if attempted {
		t.Errorf("nativeAnthropicPassthrough reported %d/attempted=%t for a request it refused locally — nothing was sent to any upstream, so there is no upstream result to classify", status, attempted)
	}
	if status != passthroughNotAttempted {
		t.Errorf("status = %d, want passthroughNotAttempted (%d): a plain 0 is what a dead leg returns, which is the whole defect", status, passthroughNotAttempted)
	}
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("client status %d, want 401 with the actionable message", rec.Code)
	}
	if !bytes.Contains(rec.Body.Bytes(), []byte("ANTHROPIC_API_KEY")) {
		t.Errorf("the 401 must name what to do about it:\n%s", rec.Body.String())
	}
	_ = relayed
}
