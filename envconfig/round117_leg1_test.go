package envconfig

// F117-L1-3 (2026-09-29 audit, round 117): a number of seconds too large for a Duration means
// "forever" for a request's keep_alive and must mean it in the environment too, not wrap.

import (
	"math"
	"testing"
	"time"
)

func TestRound117KeepAliveEnvSaturates(t *testing.T) {
	for _, s := range []string{"18446744074", "9223372037", "99999999999999999"} {
		t.Setenv("OLLAMA_KEEP_ALIVE", s)
		if got := KeepAlive(); got != time.Duration(math.MaxInt64) {
			t.Errorf("OLLAMA_KEEP_ALIVE=%s read as %v, want forever", s, got)
		}
	}
	t.Setenv("OLLAMA_KEEP_ALIVE", "30")
	if got := KeepAlive(); got != 30*time.Second {
		t.Errorf("30 read as %v", got)
	}
	t.Setenv("OLLAMA_LOAD_TIMEOUT", "18446744074")
	if got := LoadTimeout(); got != time.Duration(math.MaxInt64) {
		t.Errorf("OLLAMA_LOAD_TIMEOUT wrapped to %v", got)
	}
}
