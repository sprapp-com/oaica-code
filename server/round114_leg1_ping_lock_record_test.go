package server

// round114_leg1_ping_lock_record_test.go — leg 1, round 114 (2026-09-29 audit), F114-L1-3.
// RECORDED, not changed.
//
// runnerRef.needsReload takes runner.refMu and holds it across runner.llama.Ping, which has a
// ten-second timeout (two minutes while the runner is still loading). Every getRunner fast-path
// call and every processPending reload check goes through it, so against a runner that HANGS
// without dying (a wedged GPU, a SIGSTOPped process: /health never answers) the pings serialise:
// four waiting clients learn the runner is dead after about forty seconds, and processCompleted,
// which needs the same refMu to book that runner's finish and expiry events, blocks behind them
// and with it the bookkeeping of every other model. Probed with a real Scheduler: an unrelated
// model's request end was booked 39.7 s late.
//
// Recorded rather than changed: needsReload is upstream ollama's scheduler and a wide change to
// its locking is the kind that makes an upstream merge hard and can deadlock in ways a unit test
// does not show, for a fault (a runner that is neither alive nor dead) that the runner's own
// exit handling already covers when the process does die. The fix a later round should make is
// small in shape: snapshot the fields the comparison reads under refMu, Ping OUTSIDE the lock,
// and share one health verdict per runner for a short TTL. The pin states today's reading and goes
// red when the lock is released before the Ping.

import (
	"context"
	"testing"
	"time"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/llm"
)

// r114Pinger is a runner whose only behaviour is its Ping.
type r114Pinger struct {
	llm.LlamaServer
	fn func(context.Context) error
}

func (p r114Pinger) Ping(ctx context.Context) error { return p.fn(ctx) }

func TestMine114NeedsReloadHoldsTheRunnerLockAcrossThePing(t *testing.T) {
	release := make(chan struct{})
	pinging := make(chan struct{})
	opts := api.DefaultOptions()
	runner := &runnerRef{
		model:   &Model{},
		Options: &opts,
	}
	runner.llama = r114Pinger{fn: func(ctx context.Context) error {
		close(pinging)
		select {
		case <-release:
		case <-ctx.Done():
		}
		return nil
	}}
	done := make(chan struct{})
	go func() {
		runner.needsReload(context.Background(), &LlmRequest{model: &Model{}, opts: opts})
		close(done)
	}()
	select {
	case <-pinging:
	case <-time.After(5 * time.Second):
		t.Skip("the ping was not reached; the comparison returned first")
	}
	locked := !runner.refMu.TryLock()
	if !locked {
		runner.refMu.Unlock()
	}
	close(release)
	<-done
	if !locked {
		t.Errorf("refMu was free while the Ping was in flight, so this record is spent: needsReload no longer holds the runner lock across a ten-second health check (2026-09-29 audit, round 114, F114-L1-3)")
	}
}
