package launch

// remote_key_before_config_integrity_test.go — the credential prompt ran
// AFTER the integration had already written its config (2026-09-27 audit,
// round 19).
//
// Every managed writer takes a remote's credential from the endpoint it
// resolves at configure time: opencode embeds it in the JSON it caches for
// Run, pi writes it into its single provider slot, the rest into their own
// settings. ensureRemoteAPIKeyForModel was hooked once, in
// launchAfterConfiguration — which the managed path reaches only after
// configure — so a remote with no key on file was written with an empty
// credential, and the key the user then typed was saved to
// ~/.oaica/remotes.json while the config kept the empty string. The launch
// that followed used the config, not the file: it went out unauthenticated,
// and every later launch of that remote had to be reconfigured by hand.

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
)

// remoteKeyRecordingRunner is a managed single-model integration that records
// the credential its endpoint resolution sees while it is configuring — the
// same value the real writers persist.
type remoteKeyRecordingRunner struct {
	launcherManagedRunner
	configuredToken string
}

func (r *remoteKeyRecordingRunner) ConfigureWithModels(primary string, models []LaunchModel) error {
	for _, model := range models {
		if ep, ok := resolveRemoteEndpoint(model.Name); ok {
			r.configuredToken = ep.Token
			break
		}
	}
	return r.Configure(primary)
}

// remoteKeyRecordingEditor is the same for the editor write path
// (prepareEditorIntegration), which every Editor integration goes through.
type remoteKeyRecordingEditor struct {
	launcherEditorRunner
	configuredToken string
}

func (r *remoteKeyRecordingEditor) Edit(models []LaunchModel) error {
	for _, model := range models {
		if ep, ok := resolveRemoteEndpoint(model.Name); ok {
			r.configuredToken = ep.Token
			break
		}
	}
	return r.launcherEditorRunner.Edit(models)
}

// stubKeyPrompt records the model it was asked about and saves a key the way
// the real prompt does.
func stubKeyPrompt(t *testing.T, want string) {
	t.Helper()
	oldPrompt := remoteAPIKeyPrompt
	remoteAPIKeyPrompt = func(modelName string) error {
		if modelName != want {
			t.Errorf("the key was prompted for %q, want %q", modelName, want)
		}
		return savePromptedRemoteKey("box", "sk-typed-key")
	}
	t.Cleanup(func() { remoteAPIKeyPrompt = oldPrompt })
}

func remoteKeyLaunchEnv(t *testing.T) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	withInteractiveSession(t, true)
	withLauncherHooks(t)
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key_env":"BOX_API_KEY","tool_format":"tool_calls"}]}`)
	t.Setenv("BOX_API_KEY", "")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/experimental/model-recommendations":
			fmt.Fprint(w, `{"recommendations":[]}`)
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"gemma4"}]}`)
		case "/api/show":
			fmt.Fprint(w, `{"model_info":{"general.context_length":131072}}`)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	t.Setenv("OLLAMA_HOST", srv.URL)

	DefaultConfirmPrompt = func(prompt string, options ConfirmOptions) (bool, error) {
		return true, nil
	}
}

func TestManagedLaunchWritesTheRemoteKeyItJustPromptedFor(t *testing.T) {
	remoteKeyLaunchEnv(t)

	runner := &remoteKeyRecordingRunner{}
	withIntegrationOverride(t, "stubmanaged", runner)
	DefaultSingleSelector = func(title string, items []SelectionItem, current string) (string, error) {
		return "box/big-model", nil
	}
	stubKeyPrompt(t, "box/big-model")

	if err := LaunchIntegration(context.Background(), IntegrationLaunchRequest{
		Name:          "stubmanaged",
		ModelOverride: "box/big-model",
	}); err != nil {
		t.Fatalf("LaunchIntegration returned error: %v", err)
	}

	if runner.configuredToken == "" {
		t.Errorf("the config was written with an EMPTY credential: the key prompt for a remote with no key on file runs after the integration has already configured itself, so the file it writes cannot carry the key the user just typed — the launch then goes out unauthenticated even though the key is on disk")
	}
	if runner.configuredToken != "sk-typed-key" {
		t.Errorf("configured credential = %q, want the key the prompt just saved", runner.configuredToken)
	}
}

func TestEditorLaunchWritesTheRemoteKeyItJustPromptedFor(t *testing.T) {
	remoteKeyLaunchEnv(t)

	editor := &remoteKeyRecordingEditor{launcherEditorRunner: launcherEditorRunner{models: []string{"box/big-model"}}}
	withIntegrationOverride(t, "stubeditor", editor)
	stubKeyPrompt(t, "box/big-model")

	if err := LaunchIntegration(context.Background(), IntegrationLaunchRequest{
		Name:          "stubeditor",
		ModelOverride: "box/big-model",
	}); err != nil {
		t.Fatalf("LaunchIntegration returned error: %v", err)
	}

	if editor.configuredToken != "sk-typed-key" {
		t.Errorf("the editor's store got credential %q, want the key the prompt just saved", editor.configuredToken)
	}
}
