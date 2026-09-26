package launch

// omp_cloud_row_id_integrity_test.go — the OMP writer named the LOCAL model for
// an ollama-cloud catalogue row (2026-09-27 audit, round 22).
//
// The same defect deepSeekHarnessModelIDFor fixed in round 21, in the file
// beside it: a catalogue row is displayed as "gpt-oss" by the time a writer sees
// it (findLaunchModel strips the "ollama/" picker prefix) with its daemon-side
// id in LaunchModel.Upstream ("gpt-oss:cloud"), and ompModelID took only the
// NAME. models.yml therefore carried a row whose id is the LOCAL model of that
// name — OMP loads it, offers a multi-GB pull, or silently runs a different
// model — while the Run half (ompModelName) already asked for
// "ollama/gpt-oss:cloud". File and run disagreed about what was being launched.

import (
	"os"
	"strings"
	"testing"
)

func TestOMPWritesTheCloudRowsUpstreamName(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	cloud := LaunchModel{Name: "ollama/gpt-oss", Remote: true, Upstream: "gpt-oss:cloud"}
	if err := (&OMP{}).ConfigureWithModels("ollama/gpt-oss", []LaunchModel{cloud}); err != nil {
		t.Fatalf("ConfigureWithModels of a daemon-routed cloud row = %v, want success: the daemon serves and proxies that row", err)
	}

	path, err := ompModelsPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read models.yml: %v", err)
	}
	text := string(data)
	if !strings.Contains(text, "gpt-oss:cloud") {
		t.Errorf("models.yml never names the cloud alias the row documents, so OMP loads the local model of that name:\n%s", text)
	}
	if strings.Contains(text, "ollama/gpt-oss") {
		t.Errorf("the picker's display name reached models.yml:\n%s", text)
	}
}

// Control: a row with no upstream name is written exactly as before, so the fix
// is scoped to display-only ids.
func TestOMPKeepsBareDaemonNames(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	if err := (&OMP{}).ConfigureWithModels("qwen3.5", launchModelsFromNames([]string{"qwen3.5"})); err != nil {
		t.Fatalf("ConfigureWithModels of a bare daemon model = %v, want success", err)
	}
	path, err := ompModelsPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read models.yml: %v", err)
	}
	if !strings.Contains(string(data), "qwen3.5") {
		t.Errorf("the bare daemon model is missing from models.yml:\n%s", string(data))
	}
}
