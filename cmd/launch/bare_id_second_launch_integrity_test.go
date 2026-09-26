package launch

// bare_id_second_launch_integrity_test.go — a launch saved under a user
// remote's BARE upstream id was rewritten on every run after it (2026-09-27
// audit, round 19).
//
// A user-remote model has two spellings a launch can save: the namespaced
// picker name ("ds/deepseek-chat") and the bare upstream id ("deepseek-chat").
// droid's ownership rule accepts both on purpose; the stores that hold one
// model — cline and omp — translate the stored bare id back to a picker name,
// and they translate it to the NAMESPACED one, unconditionally. So a launch
// saved as "deepseek-chat" read back "ds/deepseek-chat", liveConfigMatches
// (slices.Equal(editor.Models(), models)) was false on every run, and each
// launch rewrote the config it had just written and reprinted the "configured"
// block — the same churn these translators were added to stop, for the other
// spelling.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"

	"github.com/ollama/ollama/cmd/config"
)

// bareIDClineSetup writes a user remote "ds" and a Cline store holding that
// remote's BARE upstream id, the way a launch saved under "deepseek-chat"
// leaves it, and records the state the launch would have saved.
func bareIDClineSetup(t *testing.T) (providersPath, legacyPath string, firstProviders, firstLegacy []byte) {
	t.Helper()
	home := t.TempDir()
	setLaunchTestHome(t, home)
	withLauncherHooks(t)

	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "cline")
	t.Setenv("PATH", binDir)
	stubBareIndex(t, map[string][]string{"deepseek-chat": {"ds/deepseek-chat"}})
	stubUserRemoteModels(t, []LaunchModel{{
		Name: "ds/deepseek-chat", Remote: true, Upstream: "ds",
	}}, nil)
	writeRemotes(t, `{"remotes":[{"name":"ds","base_url":"https://ds.example/v1","api_key":"sk-ds"}]}`)

	// Premise: both spellings resolve, to the same remote and the same model.
	namespaced, ok := resolveRemoteEndpoint("ds/deepseek-chat")
	if !ok {
		t.Fatal("premise: ds/deepseek-chat does not resolve")
	}
	bare, ok := resolveRemoteEndpoint("deepseek-chat")
	if !ok {
		t.Fatal("premise: the bare id does not resolve, so a launch cannot be saved under it")
	}
	if namespaced.BaseURL != bare.BaseURL || namespaced.UpstreamModel != bare.UpstreamModel {
		t.Fatalf("premise: the two spellings resolve to different endpoints: %+v vs %+v", namespaced, bare)
	}

	c := &Cline{}
	if err := c.Edit(testLaunchModels("deepseek-chat")); err != nil {
		t.Fatalf("seeding Cline's stores: %v", err)
	}
	// The state a launch under that name saves.
	if err := config.SaveIntegration("cline", []string{"deepseek-chat"}); err != nil {
		t.Fatalf("saving integration state: %v", err)
	}

	providersPath = clineProvidersPath(home)
	legacyPath = clineLegacyGlobalStatePath(home)
	var err error
	if firstProviders, err = os.ReadFile(providersPath); err != nil {
		t.Fatal(err)
	}
	if firstLegacy, err = os.ReadFile(legacyPath); err != nil {
		t.Fatal(err)
	}
	return providersPath, legacyPath, firstProviders, firstLegacy
}

func TestASecondLaunchUnderABareRemoteIDLeavesClinesFiles(t *testing.T) {
	providersPath, legacyPath, firstProviders, firstLegacy := bareIDClineSetup(t)

	var selections int
	DefaultMultiSelector = func(title string, items []SelectionItem, preChecked []string) ([]string, error) {
		selections++
		return nil, nil
	}
	// The write itself is what to count: a reconfigure that rewrites the same
	// bytes leaves the files identical, so byte equality cannot see it.
	editor := &countingClineEditor{Cline: &Cline{}}
	withIntegrationOverride(t, "cline", editor)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/experimental/model-recommendations":
			fmt.Fprint(w, `{"recommendations":[]}`)
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"sample-model"}]}`)
		case "/api/show":
			var req apiShowRequest
			_ = json.NewDecoder(r.Body).Decode(&req)
			fmt.Fprintf(w, `{"model":%q}`, req.Model)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)

	if err := LaunchIntegration(t.Context(), IntegrationLaunchRequest{Name: "cline"}); err != nil {
		t.Fatalf("LaunchIntegration returned error: %v", err)
	}

	if got, err := os.ReadFile(providersPath); err != nil || !bytes.Equal(got, firstProviders) {
		t.Errorf("the launch rewrote providers.json although the selection had not changed — the store holds the bare upstream id and Models() reported the namespaced picker name, so the two spellings were compared as different models:\n--- before\n%s\n--- after\n%s", firstProviders, got)
	}
	if got, err := os.ReadFile(legacyPath); err != nil || !bytes.Equal(got, firstLegacy) {
		t.Errorf("the launch rewrote globalState.json although the selection had not changed:\n--- before\n%s\n--- after\n%s", firstLegacy, got)
	}
	if selections != 0 {
		t.Errorf("the model picker ran %d times; the saved selection was already live", selections)
	}
	if editor.edits != 0 {
		t.Errorf("the launch rewrote Cline's config %d time(s): the store holds the bare upstream id \"deepseek-chat\" and Models() reported \"ds/deepseek-chat\", so the two spellings of the SAME model were compared as different selections", editor.edits)
	}
}

// countingClineEditor is the real Cline behind a counter on Edit.
type countingClineEditor struct {
	*Cline
	edits int
}

func (c *countingClineEditor) Edit(models []LaunchModel) error {
	c.edits++
	return c.Cline.Edit(models)
}

// The rule the comparison rests on, at the level both call sites share: the two
// spellings of one remote model are one model, and anything else is not.
func TestBareAndNamespacedSpellingsOfOneModelCompareEqual(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
		{"name":"ds","base_url":"https://ds.example/v1","api_key":"sk-ds"},
		{"name":"other","base_url":"https://other.example/v1","api_key":"sk-other"}]}`)
	stubBareIndex(t, map[string][]string{"deepseek-chat": {"ds/deepseek-chat"}, "shared-model": {"ds/shared-model", "other/shared-model"}})

	for _, c := range []struct {
		name string
		a, b []string
		want bool
	}{
		{"the same names", []string{"ds/deepseek-chat"}, []string{"ds/deepseek-chat"}, true},
		{"bare id against its namespaced name", []string{"ds/deepseek-chat"}, []string{"deepseek-chat"}, true},
		{"namespaced name against its bare id", []string{"deepseek-chat"}, []string{"ds/deepseek-chat"}, true},
		{"a different model on the same remote", []string{"ds/deepseek-chat"}, []string{"ds/deepseek-reasoner"}, false},
		{"a different remote's model", []string{"ds/deepseek-chat"}, []string{"other/deepseek-chat"}, false},
		{"a different list length", []string{"ds/deepseek-chat"}, []string{"ds/deepseek-chat", "deepseek-chat"}, false},
		{"a name nothing resolves", []string{"ds/deepseek-chat"}, []string{"not-a-model"}, false},
		{"an ambiguous bare id, served by two remotes", []string{"ds/shared-model"}, []string{"shared-model"}, false},
	} {
		if got := sameModelSelection(c.a, c.b); got != c.want {
			t.Errorf("%s: sameModelSelection(%v, %v) = %v, want %v", c.name, c.a, c.b, got, c.want)
		}
	}
}

// The managed-single half of the same rule: the drift test that decides whether
// `oaica launch omp` reconfigures the app on every run.
func TestAManagedAppsLiveConfigIsNotDriftedByTheOtherSpelling(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"ds","base_url":"https://ds.example/v1","api_key":"sk-ds"}]}`)
	stubBareIndex(t, map[string][]string{"deepseek-chat": {"ds/deepseek-chat"}})

	for _, c := range []struct {
		name            string
		current, target string
		want            bool
	}{
		{"nothing configured is not drift", "", "deepseek-chat", false},
		{"the store's namespaced name against the bare selection", "ds/deepseek-chat", "deepseek-chat", false},
		{"the bare name against the namespaced selection", "deepseek-chat", "ds/deepseek-chat", false},
		{"the same name twice", "ds/deepseek-chat", "ds/deepseek-chat", false},
		{"a genuinely different model", "ds/deepseek-chat", "llama3.2", true},
		{"a different remote's model", "other/deepseek-chat", "ds/deepseek-chat", true},
	} {
		if got := managedLiveConfigDrifted(c.current, c.target); got != c.want {
			t.Errorf("%s: managedLiveConfigDrifted(%q, %q) = %v, want %v", c.name, c.current, c.target, got, c.want)
		}
	}
}
