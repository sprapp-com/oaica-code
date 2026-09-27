package launch

// native_oauth_beta_passthrough_integrity_test.go — the /v1/messages
// passthrough forwarded a native OAuth bearer to api.anthropic.com with no
// anthropic-beta header (2026-09-26 audit, eleventh round).
//
// anthropicPassthrough forwards the CLIENT's headers verbatim and replaces only
// the credential, on the premise that "Claude Code set these correctly
// already". That premise does not hold for the header this leg depends on: the
// client here is a child oaica launched against the proxy, its Authorization
// header is the proxy's own token (replaced, see the header loop), and the
// anthropic-beta set Claude Code emits is a function of how IT is
// authenticated — not of the leg oaica is pointing it at. oaica injects the
// credential the leg actually needs, so it has to inject that credential's beta
// too; native_anthropic_auth.go's own comment records what a bearer without it
// earns ("Anthropic rejects the bearer without it"), and applyNativeAnthropicAuth
// already sets it for the one request oaica builds on its own behalf (the
// native /v1/models GET). Every POST on a native OAuth leg is the same
// situation, one header short: a 401 on every turn, for exactly the users the
// native tiers exist for.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// betaCaptureUpstream records the anthropic-beta header it was sent.
func betaCaptureUpstream(got *string) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Get("anthropic-beta")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("{}"))
	}))
}

func passthroughBody() []byte {
	return []byte(`{"model":"fable","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`)
}

func TestANativeOAuthPassthroughCarriesTheOAuthBeta(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var beta string
	srv := betaCaptureUpstream(&beta)
	defer srv.Close()

	body := passthroughBody()
	rec := httptest.NewRecorder()
	// The client is a child oaica launched: it sends the proxy's own token as
	// its Authorization, and no beta of its own.
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer proxy-token-not-the-upstream-credential")

	status, _ := anthropicPassthrough(rec, req, body, srv.URL, "Authorization", "Bearer sk-ant-oat-notareal", "sess-native-beta", false)
	if status != http.StatusOK {
		t.Fatalf("premise: the stub upstream answered %d, want 200", status)
	}
	if !strings.Contains(beta, oauthBetaHeaderValue) {
		t.Errorf("anthropic-beta reaching the upstream = %q, want it to contain %q — the credential oaica injected is an OAuth bearer, and api.anthropic.com rejects it without that beta (401 on every turn)", beta, oauthBetaHeaderValue)
	}
}

// The client's own betas must survive: they carry prompt caching, and dropping
// one is a silent downgrade of every request on the leg.
func TestANativeOAuthPassthroughKeepsTheClientsOwnBetas(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var beta string
	srv := betaCaptureUpstream(&beta)
	defer srv.Close()

	body := passthroughBody()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

	if status, _ := anthropicPassthrough(rec, req, body, srv.URL, "Authorization", "Bearer sk-ant-oat-notareal", "sess-native-beta-merge", false); status != http.StatusOK {
		t.Fatalf("premise: the stub upstream answered %d, want 200", status)
	}
	if !strings.Contains(beta, "prompt-caching-2024-07-31") {
		t.Errorf("anthropic-beta = %q — the client's own beta was replaced instead of merged, which silently disables prompt caching on the leg", beta)
	}
	if !strings.Contains(beta, oauthBetaHeaderValue) {
		t.Errorf("anthropic-beta = %q, want the oauth value merged in alongside the client's", beta)
	}
}

// Control: a vendor key sent as x-api-key needs no beta, and must not be given
// one — this is every third-party anthropic-wire remote.
func TestAnApiKeyPassthroughGetsNoOAuthBeta(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var beta string
	srv := betaCaptureUpstream(&beta)
	defer srv.Close()

	body := passthroughBody()
	rec := httptest.NewRecorder()
	req := httptest.NewRequest("POST", "/v1/messages", bytes.NewReader(body))

	if status, _ := anthropicPassthrough(rec, req, body, srv.URL, "x-api-key", "sk-zai-notareal", "sess-apikey", false); status != http.StatusOK {
		t.Fatalf("premise: the stub upstream answered %d, want 200", status)
	}
	if beta != "" {
		t.Errorf("anthropic-beta = %q on an x-api-key leg, want unset — the client's beta set is the client's to choose here", beta)
	}
}
