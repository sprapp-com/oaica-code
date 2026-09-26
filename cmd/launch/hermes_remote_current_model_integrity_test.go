package launch

// hermes_remote_current_model_integrity_test.go — Hermes' CurrentModel could
// not see the config the same launch had just written for a user remote
// (2026-09-26 audit, tenth round).
//
// Configure writes TWO shapes: a daemon-backed model gets model.base_url =
// hermesBaseURL() and model.default = the picker name, while a user-remote
// model gets the remote's own base and the bare upstream id
// (hermesBaseURLFor / hermesModelIDFor, hermes.go:309-318). CurrentModel
// checked only against hermesBaseURL(), so the remote shape — the endpoint it
// had just written — read as an unmanaged config and the launch reported no
// current model, the same writer/health disagreement OMP had.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestHermesCurrentModelUnderstandsARemoteLaunch(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	withHermesPlatform(t, "darwin")
	withHermesOllamaURL(t, "http://127.0.0.1:11434")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET-1234567890","tool_format":"tool_calls"}]}`)

	h := &Hermes{}
	if err := h.Configure("box/big-model"); err != nil {
		t.Fatalf("configure remote model: %v", err)
	}

	// The config really does hold the remote shape this test is about.
	data, err := os.ReadFile(filepath.Join(tmpDir, ".hermes", "config.yaml"))
	if err != nil {
		t.Fatalf("read hermes config: %v", err)
	}
	if !strings.Contains(string(data), "https://box.example/v1") {
		t.Fatalf("the remote endpoint was not written at all:\n%s", data)
	}

	if got := h.CurrentModel(); got != "box/big-model" {
		t.Errorf("CurrentModel() = %q after configuring box/big-model, want the picker name the launch wrote — the remote shape reads as unmanaged", got)
	}

	// A later daemon launch must move the answer back with it.
	if err := h.Configure("gemma4"); err != nil {
		t.Fatalf("configure local model: %v", err)
	}
	data, err = os.ReadFile(filepath.Join(tmpDir, ".hermes", "config.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "http://127.0.0.1:11434/v1") {
		t.Fatalf("the daemon endpoint was not restored:\n%s", data)
	}
	if got := h.CurrentModel(); got != "gemma4" {
		t.Errorf("CurrentModel() = %q after a local launch, want %q", got, "gemma4")
	}
}

// Control: a config naming an endpoint that is neither the daemon nor a
// configured remote is still not ours, so it still reports nothing.
func TestHermesCurrentModelRejectsAForeignEndpoint(t *testing.T) {
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	withHermesPlatform(t, "darwin")
	withHermesOllamaURL(t, "http://127.0.0.1:11434")
	withTempRemotesFile(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"sk-box-SECRET-1234567890","tool_format":"tool_calls"}]}`)

	configPath := filepath.Join(tmpDir, ".hermes", "config.yaml")
	if err := os.MkdirAll(filepath.Dir(configPath), 0o755); err != nil {
		t.Fatal(err)
	}
	cfg := "" +
		"model:\n" +
		"  provider: ollama-launch\n" +
		"  default: gemma4\n" +
		"  base_url: https://someone-else.example/v1\n" +
		"providers:\n" +
		"  ollama-launch:\n" +
		"    api: https://someone-else.example/v1\n" +
		"    default_model: gemma4\n"
	if err := os.WriteFile(configPath, []byte(cfg), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := (&Hermes{}).CurrentModel(); got != "" {
		t.Errorf("CurrentModel() = %q for a config oaica never wrote, want empty", got)
	}
}
