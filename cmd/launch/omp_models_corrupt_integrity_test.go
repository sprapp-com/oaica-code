package launch

// omp_models_corrupt_integrity_test.go — writeOMPModelsConfig threw away the
// result of reading ~/.omp/agent/models.yml (`if existing, err := ...; err ==
// nil`), so a file it could not parse was replaced by one containing nothing
// but the ollama provider. Every other provider the user had configured — and
// every top-level key — was deleted. Its sibling writeOMPAgentConfig returns
// the same parse error rather than writing over the document
// (2026-09-26 audit).
//
// The trigger is not exotic: a hand-edited file, a interrupted write, or a
// shape a newer omp writes.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func ompModelsFixturePath(t *testing.T, home string) string {
	t.Helper()
	dir := filepath.Join(home, ".omp", "agent")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	return filepath.Join(dir, "models.yml")
}

func TestACorruptOMPModelsFileIsRefusedNotReplacedByOllamaAlone(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	path := ompModelsFixturePath(t, home)
	corrupt := "providers:\n  anthropic:\n    baseUrl: https://api.anthropic.com\n    models:\n      - id: claude\n   bad-indent: [\n"
	if err := os.WriteFile(path, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	err := writeOMPModelsConfig("model-a", testLaunchModels("model-a"))
	if err == nil {
		t.Errorf("writeOMPModelsConfig returned nil for a models.yml it could not parse — it then wrote a document holding only the ollama provider, deleting the user's other providers and top-level keys")
	}
	got, rerr := os.ReadFile(path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if string(got) != corrupt {
		t.Errorf("the unparseable models.yml was overwritten:\n%s\nwant it left byte-identical at:\n%s", got, corrupt)
	}
}

// The control: a readable file keeps everything oaica does not model.
func TestAReadableOMPModelsFileKeepsOtherProviders(t *testing.T) {
	home := t.TempDir()
	setOMPTestHome(t, home)
	path := ompModelsFixturePath(t, home)
	fixture := "providers:\n  anthropic:\n    baseUrl: https://api.anthropic.com\n    apiKey: sk-ant-keep-me\ntopLevelNote: keep\n"
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := writeOMPModelsConfig("model-a", testLaunchModels("model-a")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"api.anthropic.com", "sk-ant-keep-me", "topLevelNote", "topLevelNote: keep"} {
		if !strings.Contains(string(got), want) {
			t.Errorf("%s did not survive the update:\n%s", want, got)
		}
	}
}
