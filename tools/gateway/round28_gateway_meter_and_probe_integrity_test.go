package main

// round28_gateway_meter_and_probe_integrity_test.go — two things a reload or a
// discarded map must not do (2026-09-27 audit, round 28, B-F1/B-F2).

import (
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// TestAReportCannotBeSentOutsideTheSwapWindow is B-F1: reportUsage's read of
// g.meterCh and its send must be one critical section, because apply() drains
// the channel it is retiring while holding the write lock.
//
// The send is a non-blocking select, so the seam below is what makes the window
// observable: the hook runs inside the critical section, and the write lock
// must therefore not be obtainable while it blocks. If the read lock is dropped
// before the send — the pre-fix shape — the reload walks straight through and
// the report it did not drain is never delivered and never counted as dropped.
func TestAReportCannotBeSentOutsideTheSwapWindow(t *testing.T) {
	oldBackoff := meterReporterBackoff
	meterReporterBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { meterReporterBackoff = oldBackoff })

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer hub.Close()

	cfg := gwConfig{
		UpstreamAddr: "http://127.0.0.1:1", ListenAddr: ":0", LedgerPath: t.TempDir() + "/l.jsonl",
		MeterHubAddr: hub.URL,
		APIKeys:      []gwKey{{SHA256: keyHash("sk-test"), Label: "x"}},
		Models:       []gwModel{{ID: "m", UpstreamID: "m"}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}

	entered := make(chan struct{})
	release := make(chan struct{})
	meterReportSendHook = func() {
		close(entered)
		<-release
	}
	t.Cleanup(func() { meterReportSendHook = nil })

	go g.reportUsage(ledgerEntry{RequestID: "held", Model: "m", Status: 200})
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("reportUsage never reached its send")
	}

	// A reload, while the report sits between its read of the channel and its
	// send. It must wait for the critical section.
	reloaded := make(chan struct{})
	go func() {
		if err := g.apply(cfg); err != nil {
			t.Errorf("apply (reload): %v", err)
		}
		close(reloaded)
	}()
	select {
	case <-reloaded:
		t.Fatal("a reload completed while a report was between its read of g.meterCh and its send: the drain can miss that report, and the sender never learns it was not delivered or dropped")
	case <-time.After(200 * time.Millisecond):
	}

	close(release)
	select {
	case <-reloaded:
	case <-time.After(5 * time.Second):
		t.Fatal("the reload never completed after the report was released")
	}
}

// TestAProbeLandingAfterItsMapWasDiscardedDoesNotPanic is B-F2: the probe
// goroutine outlives the request that started it, so it must not assume the
// probe map still exists. Reaching the end of this test without a panic IS the
// assertion — a panic takes the whole test binary with it, which is how the
// defect presented: every test passing, the package failing.
func TestAProbeLandingAfterItsMapWasDiscardedDoesNotPanic(t *testing.T) {
	release := make(chan struct{})
	var releaseOnce sync.Once
	releaseProbe := func() { releaseOnce.Do(func() { close(release) }) }
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()
	defer releaseProbe()

	g := &gateway{}
	flight := g.startUpstreamProbe(srv.URL, "m")

	// The map the landing probe is about to write into is discarded, the way a
	// test helper resets it between two tests in one binary.
	upstreamProbes.Lock()
	upstreamProbes.m = nil
	upstreamProbes.Unlock()

	releaseProbe()
	select {
	case <-flight.done:
	case <-time.After(10 * time.Second):
		t.Fatal("the probe never finished")
	}
}
