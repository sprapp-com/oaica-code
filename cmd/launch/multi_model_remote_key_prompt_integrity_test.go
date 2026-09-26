package launch

// multi_model_remote_key_prompt_integrity_test.go — a multi-model selection
// prompted for the FIRST model's credential only, so a remote further down the
// list was written into the store with no token (2026-09-27 audit, round 20).
//
// The rule round 19 added is that the credential is on file before the store is
// written, and the hook that enforces it was applied to models[0]. `oaica launch
// cline --model llama3.2 --model box/big-model` writes every selected row into
// cline's providers.json, taking the token from the endpoint each row resolves
// to — for box/big-model that is the remote's, and the prompt that would have
// put it in ~/.oaica/remotes.json never ran. The entry is written with an empty
// credential, and the launch that uses that config goes out unauthenticated:
// the same failure the round-19 fix was about, one row over.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEverySelectedRemotesCredentialIsPromptedBeforeTheStoreIsWritten(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	withTempRemotesFile(t)
	// The remote declares where its key comes from, and nothing has set it: the
	// state that makes ensureRemoteAPIKeyForModel prompt.
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key_env":"BOX_KEY_UNSET","tool_format":"tool_calls"}]}`)
	t.Setenv("BOX_KEY_UNSET", "")

	models := []LaunchModel{fallbackLaunchModel("llama3.2"), fallbackLaunchModel("box/big-model")}
	ep, ok := resolveRemoteEndpoint(models[1].Name)
	if !ok {
		t.Fatal("premise: the box/big-model row does not resolve, so no store would ask for its credential")
	}
	if ep.Token != "" {
		t.Fatalf("premise: the remote already resolves a token (%q), so there is nothing to prompt for", ep.Token)
	}

	var asked []string
	oldPrompt := remoteAPIKeyPrompt
	remoteAPIKeyPrompt = func(modelName string) error {
		asked = append(asked, modelName)
		// What the real prompt does when the user types a key.
		if strings.HasPrefix(modelName, "box/") {
			return savePromptedRemoteKey("box", "sk-box-prompted")
		}
		return nil
	}
	t.Cleanup(func() { remoteAPIKeyPrompt = oldPrompt })

	// Droid, because it really does store several models: cline and the other
	// single-model editors narrow the selection before the write, so the hole
	// this test is about cannot show there.
	if err := prepareEditorIntegration("droid", &Droid{}, models); err != nil {
		t.Fatalf("prepareEditorIntegration: %v", err)
	}

	if !strings.Contains(strings.Join(asked, " "), "box/big-model") {
		t.Errorf("the credential prompt ran for %v only: the remote row is written into the store taking its token from the endpoint it resolves to, which is empty", asked)
	}

	data, err := os.ReadFile(filepath.Join(home, ".factory", "settings.json"))
	if err != nil {
		t.Fatalf("read droid settings: %v", err)
	}
	if !strings.Contains(string(data), "sk-box-prompted") {
		t.Errorf("the remote's entry was written without the credential the prompt had just collected:\n%s", data)
	}
}
