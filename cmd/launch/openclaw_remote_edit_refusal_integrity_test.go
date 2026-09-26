package launch

// openclaw_remote_edit_refusal_integrity_test.go — OpenClaw's writer accepted a
// selection its runner refuses, and rewrote the config anyway (2026-09-26
// audit, round 16).
//
// Openclaw.Run refuses a user remote with a clear message: OpenClaw drives the
// Ollama native API through the daemon, and that API is not served for user
// remotes. But Edit runs first — the launcher calls Edit to write the config,
// then Run to launch — and Edit wrote the whole document regardless:
// models.providers.ollama.models was replaced with the selection (the remote
// model among them) under a baseUrl that is the local daemon, and
// agents.defaults.model.primary was set to the namespaced picker name. Run
// then refused, so the launch never happened, but OpenClaw was left configured
// for a model the daemon cannot serve with its primary pointing nowhere.
//
// The refusal has to be inside Edit, before the write (museRejectNonDaemonModels).

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestOpenclawRefusesARemoteBeforeWritingItsConfig(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://third-party.invalid/v1","api_key":"KEY_BOX","tool_format":"tool_calls"}]}`)

	models := append([]LaunchModel{{Name: "box/big-model"}}, launchModelsFromNames([]string{"llama3.2"})...)
	err := (&Openclaw{}).Edit(models)
	if err == nil {
		t.Error("Openclaw.Edit accepted a user remote that Openclaw.Run refuses: the config is rewritten for a launch that cannot happen")
	} else if !strings.Contains(err.Error(), "box/big-model") {
		t.Errorf("the refusal does not name the model that cannot be used: %v", err)
	}

	// The write is the harm: the user's working OpenClaw config is replaced for
	// a launch whose Run returns an error.
	configPath := filepath.Join(home, ".openclaw", "openclaw.json")
	if b, rerr := os.ReadFile(configPath); rerr == nil && strings.Contains(string(b), "big-model") {
		t.Errorf("the refused model was written into %s anyway:\n%s", configPath, b)
	}
}

// A daemon-only selection is unaffected.
func TestOpenclawStillConfiguresDaemonModels(t *testing.T) {
	home := t.TempDir()
	setTestHome(t, home)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://third-party.invalid/v1","api_key":"KEY_BOX","tool_format":"tool_calls"}]}`)

	if err := (&Openclaw{}).Edit(launchModelsFromNames([]string{"llama3.2", "qwen3:8b"})); err != nil {
		t.Fatalf("a daemon-backed selection was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(home, ".openclaw", "openclaw.json")); err != nil {
		t.Errorf("a daemon-backed selection wrote no config: %v", err)
	}
}
