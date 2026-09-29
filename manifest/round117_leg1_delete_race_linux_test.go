package manifest_test

import (
	"fmt"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
)

// Real deletes (Manifest.Remove -> PruneDirectory), one every 5ms, while fresh model names are written.
func round117RealRun(t *testing.T, write func(i int) error) (int, int, error) {
	var stop atomic.Bool
	go func() {
		for i := 0; !stop.Load(); i++ {
			n := model.ParseName(fmt.Sprintf("victim%d:latest", i))
			manifest.WriteManifest(n, manifest.Layer{}, nil)
			if m, err := manifest.ParseNamedManifest(n); err == nil {
				m.Remove()
			}
			time.Sleep(5 * time.Millisecond)
		}
	}()
	var fails int
	var first error
	const N = 500
	for i := 0; i < N; i++ {
		if err := write(i); err != nil {
			fails++
			if first == nil {
				first = err
			}
		}
	}
	stop.Store(true)
	return N, fails, first
}

func TestRound117RealDeleteNew(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	n, f, e := round117RealRun(t, func(i int) error {
		return manifest.WriteManifest(model.ParseName(fmt.Sprintf("fresh%d:latest", i)), manifest.Layer{}, nil)
	})
	t.Logf("WriteManifest: %d fresh writes, %d failed; first: %v", n, f, e)
	if f > 0 {
		t.Errorf("RED")
	}
}
