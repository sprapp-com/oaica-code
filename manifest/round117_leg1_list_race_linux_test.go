package manifest_test

import (
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
)

func TestRound117DeleteRacesListing(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	keep := model.ParseName("keep:latest")
	manifest.WriteManifest(keep, manifest.Layer{}, nil)
	gone := model.ParseName("gone:latest")
	var stop atomic.Bool
	go func() {
		for !stop.Load() {
			manifest.WriteManifest(gone, manifest.Layer{}, nil)
			if m, err := manifest.ParseNamedManifest(gone); err == nil {
				m.Remove()
			}
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
		t.Errorf("RED: a listing (continueOnError) failed because another model was deleted mid-walk")
	}
}
