package launch

// models_relay_oauth_beta_integrity_test.go — the GET /v1/models relay
// injected an OAuth bearer without the beta that announces it (2026-09-26
// audit, thirteenth round).
//
// The eleventh round fixed this for every POST the /v1/messages passthrough
// forwards, and native_anthropic_auth.go's comment says why it has to be
// fixed wherever oaica INJECTS such a credential rather than hoped for from
// the client: the client on these legs is a child oaica whose own
// Authorization is the proxy's token, and the beta set the client emits
// depends on how IT is authenticated, not on the credential the leg needs.
//
// The models relay is a third injection site and kept a bare
// `req.Header.Set(headerName, headerValue)`. On an OAuth-only machine —
// exactly the users the native picker rows exist for — the catalog request is
// then authenticated with a token api.anthropic.com will not accept without
// oauth-2025-04-20, so the model list comes back 401 and Claude Code falls
// back to its built-in list even though `oaica auth login` succeeded.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestTheModelsRelayCarriesTheOAuthBeta(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var beta string
	srv := betaCaptureUpstream(&beta)
	defer srv.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)
	// The client is a child oaica: the proxy's own token, and its own beta.
	req.Header.Set("Authorization", "Bearer proxy-token-not-the-upstream-credential")
	req.Header.Set("anthropic-beta", "prompt-caching-2024-07-31")

	anthropicModelsPassthrough(rec, req, srv.URL, "Authorization", "Bearer sk-ant-oat-notareal")

	if rec.Code != http.StatusOK {
		t.Fatalf("premise: the stub upstream answered %d, want 200", rec.Code)
	}
	if !strings.Contains(beta, oauthBetaHeaderValue) {
		t.Errorf("anthropic-beta reaching the upstream on GET /v1/models = %q, want it to contain %q — oaica injected an OAuth bearer here, and api.anthropic.com rejects it without that beta, so an OAuth-only machine's model list 401s and Claude Code falls back to its built-in one", beta, oauthBetaHeaderValue)
	}
	if !strings.Contains(beta, "prompt-caching-2024-07-31") {
		t.Errorf("anthropic-beta = %q — the client's own beta was replaced rather than merged", beta)
	}
}

// Control: a vendor key sent as x-api-key needs no beta and must not be given
// one — this is every third-party anthropic-wire remote's model list.
func TestTheModelsRelayGetsNoBetaForAnApiKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var beta string
	srv := betaCaptureUpstream(&beta)
	defer srv.Close()

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("GET", "/v1/models", nil)

	anthropicModelsPassthrough(rec, req, srv.URL, "x-api-key", "sk-zai-notareal")

	if rec.Code != http.StatusOK {
		t.Fatalf("premise: the stub upstream answered %d, want 200", rec.Code)
	}
	if beta != "" {
		t.Errorf("anthropic-beta = %q on an x-api-key models relay, want unset", beta)
	}
}
