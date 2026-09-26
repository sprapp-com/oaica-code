package launch

// deepseek_harness_remote_refusal_integrity_test.go — the DeepSeek Harness
// writer accepted a user-remote selection and wrote it as a daemon model
// (2026-09-27 audit, round 18).
//
// The harness settings carry ONE provider block: a single baseURL and a single
// apiKeyEnv pointing at the local daemon, whose credential oaica hands the
// child as OLLAMA_LAUNCH_DSH_API_KEY="ollama". ConfigureWithModels wrote the
// picker row's Name — the namespaced "box/big-model" — as the model id in both
// agent-default-model and every llm-pi-ai.providers.ollama.models[] entry, so
// every request went to the daemon, which does not resolve namespaced remotes,
// and failed model-not-found after the launch had already validated a key. The
// web-search block above it names the same remote-hostile endpoint, so the
// same selection could not be rescued by translating the model rows alone.
//
// Muse and the ChatGPT app answer this the same way and for the same reason:
// a single endpoint for every row plus no credential field oaica can write is
// unrepresentable, so the write is refused before the store is touched.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func deepSeekHarnessRemoteEnv(t *testing.T) {
	t.Helper()
	setTestHome(t, t.TempDir())
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:12345")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET","tool_format":"tool_calls"}]}`)
	if _, ok := resolveRemoteEndpoint("box/big-model"); !ok {
		t.Fatal("premise: the box/big-model row does not resolve, so it would never be treated as a remote at all")
	}
}

func TestDeepSeekHarnessRefusesARemoteModel(t *testing.T) {
	deepSeekHarnessRemoteEnv(t)

	models := append([]LaunchModel{{Name: "box/big-model", Remote: true}}, launchModelsFromNames([]string{"qwen3.5"})...)
	err := (&DeepSeekHarness{}).ConfigureWithModels("box/big-model", models)
	if err == nil {
		t.Fatalf("ConfigureWithModels accepted a user-remote model: the settings name one daemon endpoint and one daemon credential, so the picker name %q would be sent to the local daemon, which does not resolve namespaced remotes", "box/big-model")
	}
	if !strings.Contains(err.Error(), "box/big-model") {
		t.Errorf("refusal %q does not name the model it refused", err)
	}

	settingsPath, perr := deepSeekHarnessSettingsPath()
	if perr != nil {
		t.Fatal(perr)
	}
	if data, rerr := os.ReadFile(settingsPath); rerr == nil && strings.Contains(string(data), "box/big-model") {
		t.Errorf("the refused model reached the settings file anyway:\n%s", string(data))
	}
	patchPath, perr := deepSeekHarnessPatchPath()
	if perr != nil {
		t.Fatal(perr)
	}
	if _, rerr := os.Stat(patchPath); rerr == nil {
		t.Errorf("the refused launch still wrote the patch file %s", filepath.Base(patchPath))
	}
}

// Control: a daemon-backed selection still writes, so the refusal is scoped to
// remote rows and not to the harness store.
func TestDeepSeekHarnessStillWritesDaemonModels(t *testing.T) {
	deepSeekHarnessRemoteEnv(t)

	if err := (&DeepSeekHarness{}).ConfigureWithModels("qwen3.5", launchModelsFromNames([]string{"qwen3.5"})); err != nil {
		t.Fatalf("ConfigureWithModels of a daemon model = %v, want success", err)
	}
	settingsPath, err := deepSeekHarnessSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	if !strings.Contains(string(data), "qwen3.5") {
		t.Errorf("the daemon model is missing from the settings file:\n%s", string(data))
	}
	if (&DeepSeekHarness{}).CurrentModel() != "qwen3.5" {
		t.Errorf("CurrentModel = %q, want qwen3.5 — a written daemon selection must still read back as healthy", (&DeepSeekHarness{}).CurrentModel())
	}
}
