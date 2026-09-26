package launch

// deepseek_harness_cloud_row_integrity_test.go — the DeepSeek Harness writer
// named the LOCAL model for an ollama-cloud catalogue row (2026-09-27 audit,
// round 21).
//
// A catalogue row is displayed as "ollama/gpt-oss" and documents the
// daemon-side name "gpt-oss:cloud" in LaunchModel.Upstream. findLaunchModel
// strips the "ollama/" display prefix before a writer sees the row, so
// ConfigureWithModels wrote the BARE id "gpt-oss" into agent-default-model and
// into every models[] entry — the name of the local model. The harness then
// asked the daemon for a model that is not pulled (or, worse, ran a local model
// that merely shares the name) while the cloud alias the row carries would have
// launched fine. The row decides its id, as it does for every other writer.

import (
	"os"
	"strings"
	"testing"
)

func TestDeepSeekHarnessWritesTheCloudRowsUpstreamName(t *testing.T) {
	deepSeekHarnessRemoteEnv(t)

	cloud := LaunchModel{Name: "ollama/gpt-oss", Remote: true, Upstream: "gpt-oss:cloud"}
	if err := (&DeepSeekHarness{}).ConfigureWithModels("ollama/gpt-oss", []LaunchModel{cloud}); err != nil {
		t.Fatalf("ConfigureWithModels of a daemon-routed cloud row = %v, want success: the daemon serves and proxies that row", err)
	}

	settingsPath, err := deepSeekHarnessSettingsPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(settingsPath)
	if err != nil {
		t.Fatalf("read settings: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "gpt-oss:cloud") {
		t.Errorf("the settings never name the cloud alias the row documents, so the harness asks the daemon for a model it does not serve:\n%s", text)
	}
	if strings.Contains(text, "ollama/gpt-oss") {
		t.Errorf("the picker's display name reached the settings:\n%s", text)
	}
	if got := (&DeepSeekHarness{}).CurrentModel(); got != "gpt-oss:cloud" {
		t.Errorf("CurrentModel = %q, want the row's upstream name, so a saved-config re-launch reads back as the model that was written", got)
	}
}

// TestDeepSeekHarnessKeepsBareDaemonNames is the control: a row with no upstream
// name is written exactly as before, so the fix is scoped to display-only ids.
func TestDeepSeekHarnessKeepsBareDaemonNames(t *testing.T) {
	deepSeekHarnessRemoteEnv(t)

	if err := (&DeepSeekHarness{}).ConfigureWithModels("qwen3.5", launchModelsFromNames([]string{"qwen3.5"})); err != nil {
		t.Fatalf("ConfigureWithModels of a bare daemon model = %v, want success", err)
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
		t.Errorf("the bare daemon model is missing from the settings:\n%s", string(data))
	}
}
