package launch

// Fork tests: the launch picker must never offer upstream Ollama's built-in
// launch catalog (ollamaCloudAliasCatalog — kimi-k2.6:cloud, qwen3.5:cloud,
// glm-5.1:cloud, minimax-m2.7:cloud, gemma4, qwen3.5) as if it were an OAICA
// offering. The picker is sourced from the OAICA router, user remotes
// (~/.oaica/remotes.json), OpenRouter and the local daemon only. The catalog
// survives solely as the seed of cloudModelLimits, so a real Ollama ":cloud"
// alias on a user's own daemon still resolves its context window.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"testing"
)

// ollamaCatalogNames returns every name in ollamaCloudAliasCatalog.
func ollamaCatalogNames() []string {
	out := make([]string, 0, len(ollamaCloudAliasCatalog))
	for _, item := range ollamaCloudAliasCatalog {
		out = append(out, item.Name)
	}
	return out
}

// fakeDaemon stands up a local Ollama daemon whose /api/tags lists exactly
// the given model names and whose /api/show answers for any of them.
func fakeDaemon(t *testing.T, models ...string) {
	t.Helper()
	tags := make([]string, 0, len(models))
	for _, m := range models {
		tags = append(tags, fmt.Sprintf(`{"name":%q}`, m))
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/tags":
			fmt.Fprintf(w, `{"models":[%s]}`, strings.Join(tags, ","))
		case "/api/show":
			fmt.Fprint(w, `{"model":"local"}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)
}

// A HOME with no remotes and no keys: the router returns nothing (the
// setLaunchTestHome seam), so the picker shows only what the local daemon
// has. Before this fork change the same setup listed the six-entry upstream
// catalog ahead of the local model.
func TestPicker_NoRemotesNoKeys_ShowsOnlyLocalDaemonModels(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	withLauncherHooks(t)
	stubBareIndex(t, map[string][]string{})
	fakeDaemon(t, "qwen3:8b")

	var gotNames []string
	DefaultSingleSelector = func(title string, items []SelectionItem, current string) (string, error) {
		for _, item := range items {
			gotNames = append(gotNames, item.Name)
		}
		return "qwen3:8b", nil
	}

	model, err := ResolveRunModel(context.Background(), RunModelRequest{ForcePicker: true})
	if err != nil {
		t.Fatalf("ResolveRunModel returned error: %v", err)
	}
	if model != "qwen3:8b" {
		t.Fatalf("model = %q, want qwen3:8b", model)
	}
	if !slices.Equal(gotNames, []string{"ollama/qwen3:8b"}) {
		t.Fatalf("picker items = %v, want exactly the local daemon model", gotNames)
	}
	for _, name := range ollamaCatalogNames() {
		if slices.Contains(gotNames, name) {
			t.Fatalf("picker offered upstream Ollama catalog entry %q: %v", name, gotNames)
		}
	}
}

// A rejected key (fresh install, OAICA_API_KEY unset) must degrade the same
// way: local models only, no catalog rows pretending to be launchable.
func TestPicker_RouterAuthError_StillNoCatalog(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	withLauncherHooks(t)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: http.StatusUnauthorized, Host: "https://api.oaica.com", Body: "invalid_api_key"})
	fakeDaemon(t, "qwen3:8b")

	var gotNames []string
	DefaultSingleSelector = func(title string, items []SelectionItem, current string) (string, error) {
		for _, item := range items {
			gotNames = append(gotNames, item.Name)
		}
		return "qwen3:8b", nil
	}

	if _, err := ResolveRunModel(context.Background(), RunModelRequest{ForcePicker: true}); err != nil {
		t.Fatalf("ResolveRunModel returned error: %v", err)
	}
	if !slices.Equal(gotNames, []string{"ollama/qwen3:8b"}) {
		t.Fatalf("picker items = %v, want exactly the local daemon model", gotNames)
	}
}

// Nothing local, no remotes, router empty: the picker must not be opened on
// a fabricated catalog. It errors, and the error names the router reason.
func TestPicker_NoModelsAnywhere_ErrorsInsteadOfCatalog(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	withLauncherHooks(t)
	stubBareIndex(t, map[string][]string{})
	fakeDaemon(t)

	selectorCalls := 0
	DefaultSingleSelector = func(title string, items []SelectionItem, current string) (string, error) {
		selectorCalls++
		return "", nil
	}

	_, err := ResolveRunModel(context.Background(), RunModelRequest{ForcePicker: true})
	if err == nil {
		t.Fatal("expected an error with no models from any source")
	}
	if !strings.Contains(err.Error(), "no models available") || !strings.Contains(err.Error(), "OAICA router") {
		t.Fatalf("error %q should say no models are available and name the router", err)
	}
	if selectorCalls != 0 {
		t.Fatalf("selector opened %d times on an empty inventory, want 0", selectorCalls)
	}
}

// recommendations() must fail open to an EMPTY list, not the catalog, and
// remember why; the deprecated-model prompt therefore cannot suggest an
// Ollama cloud alias this fork cannot launch.
func TestRecommendations_RouterFailureFallsBackToNothing(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubCloudFetch(t, nil, errors.New("couldn't reach https://api.oaica.com: dial tcp: i/o timeout"))

	c := &launcherClient{apiClient: deadClient(t)}
	if recs := c.recommendations(context.Background()); len(recs) != 0 {
		t.Fatalf("recommendations = %v, want none when the router is unreachable", recs)
	}
	if c.recommendationsErr == nil {
		t.Fatal("recommendationsErr should record why the router gave nothing")
	}
	if cloud, local := c.agentCapableRecommendations(context.Background()); cloud != "" || local != "" {
		t.Fatalf("agent-capable recommendations = (%q, %q), want none", cloud, local)
	}
}

// The limits table is the one thing the catalog still feeds: a genuine
// Ollama ":cloud" alias on a local daemon keeps its context window, and the
// Claude Code launch keeps exporting it as CLAUDE_CODE_AUTO_COMPACT_WINDOW.
func TestLookupCloudModelLimit_OllamaCloudAliasesStillResolve(t *testing.T) {
	setDynamicCloudModelLimits(nil)
	for _, item := range ollamaCloudAliasCatalog {
		if !isCloudModelName(item.Name) {
			continue
		}
		l, ok := lookupCloudModelLimit(item.Name)
		if !ok {
			t.Fatalf("lookupCloudModelLimit(%q) = false, want the catalog limit", item.Name)
		}
		if l.Context != item.Details.ContextLength || l.Output != item.MaxOutputTokens {
			t.Fatalf("limits for %q = %+v, want context %d output %d", item.Name, l, item.Details.ContextLength, item.MaxOutputTokens)
		}
		plan := tierPlan{PrimaryName: item.Name, SecondaryName: item.Name}
		want := "CLAUDE_CODE_AUTO_COMPACT_WINDOW=" + strconv.Itoa(l.Context)
		if !slices.Contains(plan.envVars("http://127.0.0.1:1", "tok"), want) {
			t.Fatalf("envVars for %q missing %s", item.Name, want)
		}
	}

	// Router / user-remote ids carry no cloud source tag and must not match.
	for _, name := range []string{"kat-awq", "box/kat-awq", "openrouter/deepseek/deepseek-chat", "qwen3.5"} {
		if _, ok := lookupCloudModelLimit(name); ok {
			t.Fatalf("lookupCloudModelLimit(%q) matched an Ollama cloud alias limit", name)
		}
	}
}
