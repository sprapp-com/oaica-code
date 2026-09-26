package main

// round30_reload_log_and_probe_reset_integrity_test.go — two more instances of
// shapes the previous two rounds fixed elsewhere (2026-09-27 audit, round 30).
//
// round 29's B1 was that a log write is an unbounded write to whatever stderr
// points at, and holding a lock across it lets a slow sink stall every
// completion on the box. That fix hoisted reportUsage's drop line out of its
// READ-lock section; one function away, apply() writes the reload's drop line
// inside its WRITE-lock section, which is strictly worse — a pending writer
// blocks new readers unconditionally.
//
// round 29's B3 cleared upstreamProbes.flights between tests; the landing
// goroutine creates the results map if it is gone, so a probe the previous test
// left in flight republishes its result into the map the reset had just
// cleared, and the next test reads a health answer it did not cause.

import (
	"bytes"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// gatedLogWriter blocks the first write that carries marker, and passes every
// other write through — so a test can wedge exactly one log line without
// freezing whatever else the code under test logs.
type gatedLogWriter struct {
	marker  string
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *gatedLogWriter) Write(p []byte) (int, error) {
	if !bytes.Contains(p, []byte(w.marker)) {
		return len(p), nil
	}
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

// TestAReloadsDropLineIsNotWrittenUnderTheWriteLock: the reload that cannot
// hand its queued reports over says so — but not while holding the lock.
func TestAReloadsDropLineIsNotWrittenUnderTheWriteLock(t *testing.T) {
	oldBackoff := meterReporterBackoff
	meterReporterBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { meterReporterBackoff = oldBackoff })

	gate := make(chan struct{})
	var gateOnce sync.Once
	letHubGo := func() { gateOnce.Do(func() { close(gate) }) }
	defer letHubGo()

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Hold the first POST open, so the reports behind it stay queued.
		<-gate
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	writer := &gatedLogWriter{marker: "reload dropped", started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(writer.release) }) }
	defer unblock()

	saved := log.Writer()
	log.SetOutput(writer)
	defer log.SetOutput(saved)

	withHub := gwConfig{
		UpstreamAddr: "http://127.0.0.1:1", ListenAddr: ":0", LedgerPath: t.TempDir() + "/l.jsonl",
		MeterHubAddr: hub.URL,
		APIKeys:      []gwKey{{SHA256: keyHash("sk-test"), Label: "x"}},
		Models:       []gwModel{{ID: "m", UpstreamID: "m"}},
	}
	g := &gateway{}
	if err := g.apply(withHub); err != nil {
		t.Fatalf("apply: %v", err)
	}
	for i := 0; i < 5; i++ {
		g.reportUsage(ledgerEntry{RequestID: "r", Model: "m", Status: 200})
	}
	// The reporter is holding one POST open against the hub, so the rest are
	// still on the channel when the reload comes.
	time.Sleep(50 * time.Millisecond)

	// The reload switches metering off: there is no reporter to hand the queue
	// to, so the drop line is written.
	off := withHub
	off.MeterHubAddr = ""
	applied := make(chan error, 1)
	go func() { applied <- g.apply(off) }()

	select {
	case <-writer.started:
	case err := <-applied:
		t.Fatalf("apply returned (%v) without writing the drop line: this test's premise about the reload path no longer holds", err)
	case <-time.After(5 * time.Second):
		t.Fatal("apply never wrote its drop line for a reload that could not hand its queued reports over: this test's premise no longer holds")
	}

	// The sink is mid-write. A reader — every completion needs one — must be
	// able to enter. Held across the log write, as it is, this blocks for as
	// long as the sink blocks, and a pending writer blocks readers outright.
	locked := make(chan struct{})
	go func() {
		g.mu.RLock()
		close(locked)
		g.mu.RUnlock()
	}()

	select {
	case <-locked:
	case <-time.After(500 * time.Millisecond):
		unblock()
		<-locked
		t.Error("the read lock could not be taken while apply() was writing its reload drop line: the log write is inside apply's WRITE-lock section, so a slow log sink freezes every completion on the box")
	}
	unblock()
	letHubGo()
	<-applied
}

// TestAProbeLeftInFlightDoesNotSurviveTheNextReset: resetProbes must be the
// last word on what is cached when a test begins.
func TestAProbeLeftInFlightDoesNotSurviveTheNextReset(t *testing.T) {
	resetProbes(t)

	gate := make(chan struct{})
	var gateOnce sync.Once
	letGo := func() { gateOnce.Do(func() { close(gate) }) }
	defer letGo()

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.WriteHeader(http.StatusOK)
	}))
	defer up.Close()

	// A probe the PREVIOUS test started and did not wait for.
	g := &gateway{}
	g.startUpstreamProbe(up.URL, "m")
	go func() {
		time.Sleep(100 * time.Millisecond)
		letGo()
	}()

	// The next test begins with its own reset.
	resetProbes(t)

	// Long enough for a probe that was NOT waited for to land and republish.
	time.Sleep(300 * time.Millisecond)

	if health, ok := cachedUpstreamHealth(up.URL); ok {
		t.Errorf("a probe started before the reset published a result into the cache the reset had cleared (health=%v): the next test reads a health answer it did not cause, and a reused httptest address turns that into a wrong verdict", health)
	}
}
