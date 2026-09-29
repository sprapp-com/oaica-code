package manifest

import (
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"
)

func TestRound116PruneRacesAtomicWrite(t *testing.T) {
	t.Setenv("OLLAMA_MODELS", t.TempDir())
	root, _ := Path()
	fp := filepath.Join(root, "registry.ollama.ai", "library", "m", "latest")
	os.MkdirAll(filepath.Dir(fp), 0o755)
	os.WriteFile(fp, []byte("{}\n"), 0o644)
	var stop atomic.Bool
	go func() {
		for !stop.Load() {
			PruneDirectory(root)
		}
	}()
	var fails int
	var first error
	deadline := time.Now().Add(3 * time.Second)
	n := 0
	for time.Now().Before(deadline) {
		n++
		if err := WriteFileAtomic(fp, []byte("{}\n"), 0o644); err != nil {
			fails++
			if first == nil {
				first = err
			}
		}
	}
	stop.Store(true)
	t.Logf("%d writes, %d failed; first: %v", n, fails, first)
	if fails > 0 {
		t.Fatalf("RED: a concurrent prune (model delete) failed a re-pull's manifest write of an existing model")
	}
}
