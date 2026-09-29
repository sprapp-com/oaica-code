package manifest_test

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
)

func TestRound117XdevListing(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	root, _ := manifest.Path()
	other, err := os.MkdirTemp("/dev/shm", "probe1-")
	if err != nil {
		t.Skip(err)
	}
	defer os.RemoveAll(other)
	os.MkdirAll(filepath.Join(other, "library", "m"), 0o755)
	if err := os.Symlink(other, filepath.Join(root, "registry.ollama.ai")); err != nil {
		t.Fatal(err)
	}
	name := model.ParseName("m:latest")
	if err := manifest.WriteManifest(name, manifest.Layer{}, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("tmp name valid? %v", model.ParseNameFromFilepath("registry.ollama.ai/library/m/.manifest-123.tmp").IsValid())
	var stop atomic.Bool
	go func() {
		for !stop.Load() {
			manifest.WriteManifest(name, manifest.Layer{}, nil)
		}
	}()
	var fails, n int
	var first error
	for d := time.Now().Add(3 * time.Second); time.Now().Before(d); {
		n++
		if _, err := manifest.Manifests(true); err != nil {
			fails++
			if first == nil {
				first = err
			}
		}
	}
	stop.Store(true)
	t.Logf("%d listings, %d failed; first: %v", n, fails, first)
	if fails > 0 {
		t.Errorf("RED: a listing failed while a cross-fs manifest write ran")
	}
}

// A temp file the fallback writer left behind (a crash between create and rename) is not a model:
// it must not fail a strict listing at every startup.
func TestRound117LeftoverFallbackTempIsNotAModel(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	name := model.ParseName("m:latest")
	if err := manifest.WriteManifest(name, manifest.Layer{}, nil); err != nil {
		t.Fatal(err)
	}
	root, _ := manifest.Path()
	dir := filepath.Join(root, "registry.ollama.ai", "library", "m")
	if err := os.WriteFile(filepath.Join(dir, ".manifest-42.tmp"), []byte("{"), 0o644); err != nil {
		t.Fatal(err)
	}
	ms, err := manifest.Manifests(false)
	if err != nil || len(ms) != 1 {
		t.Fatalf("strict listing with a leftover temp file: %d models, err %v", len(ms), err)
	}
}
