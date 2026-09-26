package progress

// spinner_race_integrity_test.go — the spinner's frame index and stop flag were
// plain fields shared with the goroutine NewSpinner starts (2026-09-26 audit).
//
// Spinner.start advances the frame and observes the stop flag every 100 ms
// while String renders from the consumer's goroutine — the pull path does
// exactly this (`oaica pull`, and VSCode.ensureModelsRegistered's pullModel).
// A plain int and a plain time.Time are a data race the moment a render and a
// tick overlap, which is why `go test -race ./cmd/launch/` aborted tests that
// merely pulled a model. Both are atomic now.
//
// The detector is the assertion: run with
// `go test -race -run TestSpinnerIsRaceFreeWhileTicking ./progress/`.
// Reverting the fields to plain int/time.Time makes the detector abort it — a
// behavioural failure, not a compile error.

import (
	"sync"
	"testing"
	"time"
)

func TestSpinnerIsRaceFreeWhileTicking(t *testing.T) {
	s := NewSpinner("pulling manifest")
	defer s.Stop()

	// Several renderers, so a render is in flight across ticks rather than
	// only between them.
	var wg sync.WaitGroup
	deadline := time.Now().Add(700 * time.Millisecond)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for time.Now().Before(deadline) {
				if s.String() == "" {
					t.Error("Spinner.String() rendered empty")
					return
				}
			}
		}()
	}
	wg.Wait()
}
