package launch

// context_window_remote_anthropic_auth_integrity_test.go — finding #16: the
// context-window probe authenticates for itself, and it sent
// "Authorization: Bearer" to every route it probed. An Anthropic-wire remote
// (wire "anthropic": zai-coding-plan, minimax-coding-plan, a raw
// api.anthropic.com row) does not accept a Bearer — it wants x-api-key plus
// anthropic-version, which is exactly the branch the two other consumers of
// the same /models URL already take (fetchRemoteModels in user_remotes.go,
// probeRemote in doctor.go) and the credential shape the proxy's own
// passthrough injects for these rows (proxyRoute.anthropicPassthroughTarget,
// anthropic_openai_proxy.go). The probe got a 401, so the launch ran with no
// real window: no CLAUDE_CODE_MAX_CONTEXT_TOKENS hint and no context-fit
// clamp ceiling in the proxy — the same consequence
// TestContextWindowProbeUsesModelsPath pins for a wrong list URL.

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// anthropicWireModelsProbe answers like an Anthropic endpoint: it refuses a
// Bearer, needs the key under x-api-key, and reports a context window once the
// credential is right. The headers it received are handed back so the test can
// assert on what the probe actually sent.
func anthropicWireModelsProbe(ctxLen int, got *http.Header) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*got = r.Header.Clone()
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		if r.Header.Get("x-api-key") != "sk-anthropic-key" || r.Header.Get("anthropic-version") == "" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"data":[{"id":"m","context_length":%d}]}`, ctxLen))
	}))
}

func TestContextWindowProbeAnthropicWireUsesXAPIKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	const ctxLen = 424242
	var got http.Header
	srv := anthropicWireModelsProbe(ctxLen, &got)
	defer srv.Close()

	writeRemotes(t, fmt.Sprintf(
		`{"remotes":[{"name":"zai","base_url":%q,"wire":"anthropic","api_key":"sk-anthropic-key"}]}`,
		srv.URL))

	plan, err := buildTierPlan("zai/m", "", "", false)
	if err != nil {
		t.Fatalf("buildTierPlan: %v", err)
	}
	// Fixture sanity: the route really is the anthropic-wire row.
	if plan.Routes.Default.Wire != "anthropic" {
		t.Fatalf("route wire = %q, want anthropic — the fixture is wrong", plan.Routes.Default.Wire)
	}

	plan.withContextWindows()

	if got == nil {
		t.Fatal("the context-window probe never reached the remote's /models")
	}
	if key := got.Get("x-api-key"); key != "sk-anthropic-key" {
		t.Errorf("probe sent x-api-key = %q, want the remote's key: an Anthropic-wire remote authenticates the way fetchRemoteModels and probeRemote already do", key)
	}
	if got.Get("anthropic-version") == "" {
		t.Error("probe omitted anthropic-version, which every other caller of this URL sends to an Anthropic-wire remote")
	}
	if auth := got.Get("Authorization"); auth != "" {
		t.Errorf("probe sent Authorization: %q to an Anthropic-wire remote — api.anthropic.com and the coding-plan vendors answer a Bearer with 401", auth)
	}
	if plan.PrimaryContext != ctxLen {
		t.Errorf("PrimaryContext = %d, want %d: a 401 leaves the launch with no CLAUDE_CODE_MAX_CONTEXT_TOKENS hint and no context-fit clamp ceiling",
			plan.PrimaryContext, ctxLen)
	}
}
