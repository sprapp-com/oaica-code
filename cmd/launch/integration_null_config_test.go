package launch

// integration_null_config_test.go — a present-but-null integration entry is
// not "configured" (round-4 audit, 2026-09-26).
//
// config.LoadIntegration maps ABSENT to os.ErrNotExist but returned
// (nil, nil) for a present-but-null map value, so every caller that checked
// only `err == nil` dereferenced a nil pointer: `oaica launch vscode`,
// IntegrationSelectionItems and config.IntegrationModel all panicked instead
// of reporting a bad config file. A hand-edited or half-written
// ~/.ollama/config.json is enough to produce one.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime/debug"
	"strings"
	"testing"

	"github.com/ollama/ollama/cmd/config"
)

// invIntegrationConfig writes a config file with the given integrations map.
func invIntegrationConfig(t *testing.T, home string, integrations map[string]any) {
	t.Helper()
	p := filepath.Join(home, ".ollama", "config.json")
	if err := os.MkdirAll(filepath.Dir(p), 0o755); err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(map[string]any{"integrations": integrations})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}

// invNoPanic runs fn, returning the panic's top frames ("" when it did not
// panic).
func invNoPanic(fn func()) (msg string) {
	defer func() {
		if r := recover(); r != nil {
			lines := strings.Split(string(debug.Stack()), "\n")
			frames := make([]string, 0, 3)
			for _, l := range lines {
				s := strings.TrimSpace(l)
				if !strings.HasPrefix(s, "github.com/ollama/ollama/") || strings.Contains(s, "integration_null_config_test.go") {
					continue
				}
				frames = append(frames, "    at "+s)
				if len(frames) == 3 {
					break
				}
			}
			msg = fmt.Sprintf("%v\n%s", r, strings.Join(frames, "\n"))
		}
	}()
	fn()
	return ""
}

func TestNullIntegrationEntryIsReportedNotDereferenced(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	// Every integration the UI can reach set to null: vscode is Hidden, so a
	// vscode-only config would never exercise IntegrationSelectionItems' loop.
	entries := map[string]any{"vscode": nil}
	for _, spec := range ListVisibleIntegrationSpecs() {
		entries[spec.Name] = nil
	}
	invIntegrationConfig(t, home, entries)

	// The readers must treat it as "not configured". A nil pointer with a nil
	// error is indistinguishable from "found" for every caller.
	if cfg, err := config.LoadIntegration("vscode"); cfg != nil || err == nil {
		t.Errorf("LoadIntegration on a null entry = (%v, %v), want (nil, non-nil error)", cfg, err)
	} else if !strings.Contains(err.Error(), "vscode") {
		t.Errorf("the error should name the integration: %v", err)
	}
	if got := config.IntegrationModel("vscode"); got != "" {
		t.Errorf("IntegrationModel on a null entry = %q, want \"\"", got)
	}
	if got := config.IntegrationModels("vscode"); got != nil {
		t.Errorf("IntegrationModels on a null entry = %v, want nil", got)
	}

	// The three paths that panicked.
	if msg := invNoPanic(func() {
		prev := userRemoteLaunchModels
		userRemoteLaunchModels = func() ([]LaunchModel, []error) { return nil, nil }
		defer func() { userRemoteLaunchModels = prev }()
		_ = new(VSCode).Run("some-model", nil, nil)
	}); msg != "" {
		t.Errorf("`oaica launch vscode` panicked on a null integration entry:\n%s", msg)
	}
	if msg := invNoPanic(func() { _, _ = IntegrationSelectionItems() }); msg != "" {
		t.Errorf("IntegrationSelectionItems panicked on a null integration entry:\n%s", msg)
	}
	if msg := invNoPanic(func() { _ = config.IntegrationModel("vscode") }); msg != "" {
		t.Errorf("config.IntegrationModel panicked on a null integration entry:\n%s", msg)
	}
}

// The shape the writers actually produce, so the fix above cannot have been
// achieved by refusing every entry.
func TestWellFormedIntegrationEntryStillReads(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	invIntegrationConfig(t, home, map[string]any{"vscode": map[string]any{"models": []string{"m1"}}})

	cfg, err := config.LoadIntegration("vscode")
	if err != nil || cfg == nil {
		t.Fatalf("LoadIntegration = (%v, %v), want a config and no error", cfg, err)
	}
	if got := config.IntegrationModel("vscode"); got != "m1" {
		t.Errorf("IntegrationModel = %q, want m1", got)
	}
	if _, err := config.LoadIntegration("no-such-integration"); err == nil {
		t.Error("an absent integration must still report not-exist")
	}
}
