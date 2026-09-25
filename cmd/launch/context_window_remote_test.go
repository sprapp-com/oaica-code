package launch

// context_window_remote_test.go — the context-window probe is the third
// consumer of a remote's model-list URL, and for exactly the rows models_path
// exists for, asking "<base>/models" gets a 404: the launch then runs with no
// real window (no CLAUDE_CODE_MAX_CONTEXT_TOKENS hint, no context-fit clamp
// ceiling in the proxy).

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestContextWindowProbeUsesModelsPath pins the probe to the row's models_path.
// The chat/list version prefix is per-surface, so a row can be version "none"
// (correct for every request) and still need "/v1/models" for the list —
// without the override the probe asks a URL the vendor does not serve
// (2026-09-26 audit).
func TestContextWindowProbeUsesModelsPath(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	const ctxLen = 999999
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, fmt.Sprintf(`{"data":[{"id":"m","context_length":%d}]}`, ctxLen))
	}))
	defer srv.Close()

	writeRemotes(t, fmt.Sprintf(
		`{"remotes":[{"name":"pbox","base_url":%q,"version":"none","models_path":"/v1/models","api_key":"k"}]}`,
		srv.URL))

	r, _, ok := findUserRemoteForModel("pbox/m")
	if !ok {
		t.Fatal("the fixture remote must resolve")
	}
	// Sanity: the list path IS discoverable — the picker sweep uses it.
	ids, err := fetchRemoteModels(r)
	if err != nil || len(ids) != 1 || ids[0] != "m" {
		t.Fatalf("fetchRemoteModels(models_path) = %v, %v — the fixture is wrong, the list lives at %s/v1/models", ids, err, srv.URL)
	}

	plan, err := buildTierPlan("pbox/m", "", "", false)
	if err != nil {
		t.Fatalf("buildTierPlan: %v", err)
	}
	if got := plan.Routes.Default.BaseURL; got != srv.URL {
		t.Fatalf("route base = %q, want %q (version none)", got, srv.URL)
	}
	plan.withContextWindows()
	if plan.PrimaryContext != ctxLen {
		t.Errorf("PrimaryContext = %d, want %d: the context-window probe asked %s/models instead of the row's models_path %s/v1/models",
			plan.PrimaryContext, ctxLen, srv.URL, srv.URL)
	}
}
