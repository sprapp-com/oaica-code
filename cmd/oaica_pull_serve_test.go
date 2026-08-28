package cmd

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/ollama/ollama/cmd/launch"
)

// withTempOaicaHome points ~/.oaica-derived paths at a throwaway HOME so
// tests never touch a developer's real manifest — mirrors
// cmd/launch/model_manifest_test.go's helper of the same shape.
func withTempOaicaHome(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("HOME", dir)
	if err := os.MkdirAll(filepath.Join(dir, ".oaica"), 0o700); err != nil {
		t.Fatal(err)
	}
}

func TestAutoPopulateModelManifest_CreatesEntry(t *testing.T) {
	withTempOaicaHome(t)

	autoPopulateModelManifest("my-model", "/home/x/.oaica/models/my-model.gguf")

	e, err := launch.ModelShow("my-model")
	if err != nil {
		t.Fatalf("ModelShow after autoPopulateModelManifest: %v", err)
	}
	if e.Engine != launch.EngineLlamaCPP {
		t.Errorf("Engine = %q, want %q", e.Engine, launch.EngineLlamaCPP)
	}
	if e.ModelPath != "/home/x/.oaica/models/my-model.gguf" {
		t.Errorf("ModelPath = %q", e.ModelPath)
	}
	if e.Notes == "" {
		t.Error("expected a non-empty notes explaining the entry is auto-populated/partial")
	}
}

func TestAutoPopulateModelManifest_NeverOverwritesExisting(t *testing.T) {
	withTempOaicaHome(t)

	// User already hand-configured this model with real detail.
	if _, err := launch.ModelAdd(launch.ModelAddOptions{
		ID: "my-model", Engine: "vllm", ContextWindow: 262144, Quant: "awq-w4a16",
	}); err != nil {
		t.Fatal(err)
	}

	// A pull of the same id must not blow that away with the blanker
	// llama.cpp/auto-populated record.
	autoPopulateModelManifest("my-model", "/some/path.gguf")

	e, err := launch.ModelShow("my-model")
	if err != nil {
		t.Fatal(err)
	}
	if e.Engine != "vllm" || e.ContextWindow != 262144 || e.Quant != "awq-w4a16" {
		t.Fatalf("existing hand-configured entry was overwritten: %+v", e)
	}
}

func TestAutoPopulateModelManifest_NeverPanicsOnManifestError(t *testing.T) {
	// HOME unset/unresolvable: os.UserHomeDir() and modelManifestPath()
	// fail. autoPopulateModelManifest must degrade to a warning, not panic
	// or return an error that could fail the pull itself.
	t.Setenv("HOME", "")
	t.Setenv("USERPROFILE", "") // Windows equivalent, belt and suspenders
	autoPopulateModelManifest("my-model", "/some/path.gguf") // must not panic
}
