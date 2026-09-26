package launch

// cline_narrowing_integrity_test.go — a multi-model selection for Cline recorded
// state the store never held (2026-09-26 audit, fifteenth round).
//
// Cline's provider settings carry one model, so Edit writes models[0].Name and
// Models() answers at most one name. prepareEditorIntegration, though, recorded
// every name the picker returned as the integration's state. The two never
// agreed afterwards, so:
//
//   - savedMatchesModels(saved, models) was false on the next launch;
//   - liveConfigMatches (slices.Equal(editor.Models(), models)) was false too;
//
// each launch therefore rewrote both of Cline's config files and reprinted the
// "configured" block, and selections 2..N were dropped without a word. The
// invariant that has to hold: what the integration state records equals what
// the store holds, so the next launch converges.

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/ollama/ollama/cmd/config"
)

func TestClineStoresOnlyTheModelItRecords(t *testing.T) {
	c := &Cline{}
	tmpDir := t.TempDir()
	setTestHome(t, tmpDir)
	// The integration state lives under the home this test set.
	if err := os.MkdirAll(filepath.Join(tmpDir, ".ollama"), 0o755); err != nil {
		t.Fatal(err)
	}

	selected := []string{"kimi-k2.5:cloud", "glm-5:cloud", "qwen3:cloud"}

	stored, dropped := editorStoredModels(c, selected)
	if len(stored) != 1 || stored[0] != "kimi-k2.5:cloud" {
		t.Fatalf("editorStoredModels kept %v, want just the first model", stored)
	}
	if strings.Join(dropped, ",") != "glm-5:cloud,qwen3:cloud" {
		t.Fatalf("editorStoredModels reported dropped %v, want the other two in order", dropped)
	}

	if err := prepareEditorIntegration("cline", c, launchModelsFromNames(stored)); err != nil {
		t.Fatal(err)
	}

	// What Cline's own files hold.
	held := c.Models()
	if len(held) == 0 {
		t.Fatal("premise: Cline recorded no model at all")
	}

	// The state oaica saved for it.
	saved, err := LoadIntegration("cline")
	if err != nil {
		t.Fatalf("the integration state was not saved: %v", err)
	}
	if !savedMatchesModels(saved, held) {
		t.Errorf("the saved state is %v while Cline's store holds %v — a next launch reads the state, finds it unlike the store, and rewrites both config files again:\n%s", saved.Models, held, readClineFilesForTest(t, tmpDir))
	}
	if !liveConfigMatchesForTest(c, saved) {
		t.Errorf("liveConfigMatches would be false after this write (editor.Models() = %v), so every launch reconfigured the config it had just read", held)
	}
}

// plainEditorStub is an Editor that has asked for no narrowing.
type plainEditorStub struct{}

func (plainEditorStub) Paths() []string          { return nil }
func (plainEditorStub) Edit([]LaunchModel) error { return nil }
func (plainEditorStub) Models() []string         { return nil }

// A plain Editor keeps the whole selection: narrowing is opt-in, and an editor
// that has not asked for it must not lose models (control).
func TestAnEditorWithoutNarrowingKeepsTheWholeSelection(t *testing.T) {
	stored, dropped := editorStoredModels(plainEditorStub{}, []string{"a", "b"})
	if len(dropped) != 0 {
		t.Errorf("editorStoredModels dropped %v for an editor that did not narrow", dropped)
	}
	if len(stored) != 2 {
		t.Errorf("editorStoredModels kept %v of two models for a non-narrowing editor", stored)
	}
}

// readClineFilesForTest dumps both stores so a failure shows what Cline holds.
func readClineFilesForTest(t *testing.T, home string) string {
	t.Helper()
	var b strings.Builder
	for _, p := range []string{clineProvidersPath(home), clineLegacyGlobalStatePath(home)} {
		data, err := os.ReadFile(p)
		if err != nil {
			b.WriteString(p + ": " + err.Error() + "\n")
			continue
		}
		b.WriteString(p + ":\n" + string(data) + "\n")
	}
	return b.String()
}

// liveConfigMatchesForTest is the comparison launchEditorIntegration makes.
func liveConfigMatchesForTest(editor Editor, saved *config.IntegrationConfig) bool {
	return slices.Equal(editor.Models(), saved.Models)
}

// The same defect at the level a user meets it: pick several models once, then
// launch again with nothing changed. The second launch must leave both of
// Cline's files byte-identical — before the fix it rewrote them every time,
// because the state it had saved (three names) never matched the one model the
// store holds, so savedMatchesModels and liveConfigMatches were both false and
// the "configured" block was reprinted on every run.
func TestASecondClineLaunchLeavesTheFilesItJustWrote(t *testing.T) {
	tmpDir := t.TempDir()
	setLaunchTestHome(t, tmpDir)
	withLauncherHooks(t)

	binDir := t.TempDir()
	writeFakeBinary(t, binDir, "cline")
	t.Setenv("PATH", binDir)

	var selections int
	DefaultMultiSelector = func(title string, items []SelectionItem, preChecked []string) ([]string, error) {
		selections++
		return []string{"sample-model", "qwen3:8b", "glm-5:cloud"}, nil
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/experimental/model-recommendations":
			fmt.Fprint(w, `{"recommendations":[]}`)
		case "/api/tags":
			fmt.Fprint(w, `{"models":[{"name":"sample-model"},{"name":"qwen3:8b"}]}`)
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

	if err := LaunchIntegration(context.Background(), IntegrationLaunchRequest{
		Name:           "cline",
		ForceConfigure: true,
	}); err != nil {
		t.Fatalf("LaunchIntegration (first) returned error: %v", err)
	}

	saved, err := config.LoadIntegration("cline")
	if err != nil {
		t.Fatalf("failed to reload saved config: %v", err)
	}
	if diff := compareStrings(saved.Models, []string{"sample-model"}); diff != "" {
		t.Errorf("the saved state and the single model Cline stores disagree (-want +got):\n%s\nCline's files:\n%s", diff, readClineFilesForTest(t, tmpDir))
	}

	providersPath := clineProvidersPath(tmpDir)
	legacyPath := clineLegacyGlobalStatePath(tmpDir)
	firstProviders, err := os.ReadFile(providersPath)
	if err != nil {
		t.Fatal(err)
	}
	firstLegacy, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}

	if err := LaunchIntegration(context.Background(), IntegrationLaunchRequest{Name: "cline"}); err != nil {
		t.Fatalf("LaunchIntegration (second) returned error: %v", err)
	}

	if got, err := os.ReadFile(providersPath); err != nil || !bytes.Equal(got, firstProviders) {
		t.Errorf("the second launch rewrote providers.json although nothing about the selection changed:\n--- before\n%s\n--- after\n%s", firstProviders, got)
	}
	if got, err := os.ReadFile(legacyPath); err != nil || !bytes.Equal(got, firstLegacy) {
		t.Errorf("the second launch rewrote globalState.json although nothing about the selection changed:\n--- before\n%s\n--- after\n%s", firstLegacy, got)
	}
	if selections != 1 {
		t.Errorf("the model picker ran %d times; the second launch had a saved selection and nothing had drifted", selections)
	}
}
