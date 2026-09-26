package main

// round29_meter_report_silence_integrity_test.go — the two meter paths that
// could still go quiet (2026-09-27 audit, round 29, B1/B2).
//
// reportUsage's drop line was written with the read lock held: round 28 moved
// the send into the critical section and took the log with it, which made an
// unbounded write to whatever stderr points at hold the lock every completion
// needs. A log write is the one thing in that section whose duration nothing
// here controls.
//
// runMeterReporter gave up after its bounded retries with no line at all: the
// record stayed in the local ledger, meterhub's aggregate was short of it, and
// nothing anywhere said so — the silence round 27 removed from the reload path
// and left in the give-up path.

import (
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockingLogWriter blocks its first Write until release is closed, and reports
// on started that it has been entered.
type blockingLogWriter struct {
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (w *blockingLogWriter) Write(p []byte) (int, error) {
	w.once.Do(func() { close(w.started) })
	<-w.release
	return len(p), nil
}

// TestTheDropLineIsNotWrittenUnderTheLock is B1. A full meter channel is the
// one path that logs from reportUsage; with the sink wedged mid-write, a writer
// must still be able to take the lock.
func TestTheDropLineIsNotWrittenUnderTheLock(t *testing.T) {
	writer := &blockingLogWriter{started: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	unblock := func() { releaseOnce.Do(func() { close(writer.release) }) }
	defer unblock()

	saved := log.Writer()
	log.SetOutput(writer)
	defer log.SetOutput(saved)

	g := &gateway{meterCh: make(chan usageReport, 1)}
	g.meterCh <- usageReport{} // full: the next report is dropped, and said to be

	go g.reportUsage(ledgerEntry{RequestID: "req-wedged"})

	select {
	case <-writer.started:
	case <-time.After(5 * time.Second):
		t.Fatal("reportUsage never wrote its drop line for a full meter channel: this test's premise no longer holds")
	}

	// The sink is mid-write. Take the write lock — exactly what a SIGHUP reload
	// does — and require it to be granted. Held across the log write, as it was,
	// this blocks for as long as the sink blocks, and every completion that
	// needs the read lock waits behind it.
	locked := make(chan struct{})
	go func() {
		g.mu.Lock()
		close(locked)
		g.mu.Unlock()
	}()

	select {
	case <-locked:
	case <-time.After(500 * time.Millisecond):
		unblock()
		<-locked
		t.Error("the write lock could not be taken while reportUsage's drop line was being written: the log write is inside the read-lock section, so a slow log sink stalls every completion on the box")
	}
	unblock()
}

// TestAGivingUpReportIsSaidOutLoud is B2: the reporter must name the record it
// could not deliver.
func TestAGivingUpReportIsSaidOutLoud(t *testing.T) {
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hub.Close()

	ch := make(chan usageReport, 4)
	done := make(chan struct{})
	defer close(done)

	var mu sync.Mutex
	var logged strings.Builder
	saved := log.Writer()
	log.SetOutput(writerFunc(func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return logged.Write(p)
	}))
	defer log.SetOutput(saved)

	ch <- usageReport{ledgerEntry: ledgerEntry{RequestID: "req-refused"}, Region: "test"}
	go runMeterReporter(ch, done, hub.URL, "tok", func(int) time.Duration { return time.Millisecond })

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		out := logged.String()
		mu.Unlock()
		if strings.Contains(out, "req-refused") {
			if !strings.Contains(out, "gave up") {
				t.Fatalf("the report for req-refused was logged, but not as given up: %q", out)
			}
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("a report the hub refused three times was never accounted for: the reporter gave up in silence, so the ledger has it, meterhub's aggregate does not, and nothing said so")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// TestARetiredReporterStopsRetryingAtOnce is the other half of B2: the give-up
// has to be reached promptly, not after the backoff a reload already made
// pointless.
func TestARetiredReporterStopsRetryingAtOnce(t *testing.T) {
	ok := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	defer ok.Close()
	// Control: a hub that accepts the report is a nil error, and the long
	// backoff below is never reached.
	if err := deliverMeterReport(&http.Client{Timeout: time.Second}, make(chan struct{}), ok.URL, "", []byte(`{}`),
		func(int) time.Duration { return 30 * time.Second }); err != nil {
		t.Fatalf("deliverMeterReport against a hub answering 204 = %v; want nil", err)
	}

	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer hub.Close()

	// The backoff is long enough that sleeping it out would be visible: a
	// retired reporter must return on the retirement, not on the timer.
	done := make(chan struct{})
	close(done)
	start := time.Now()
	err := deliverMeterReport(&http.Client{Timeout: time.Second}, done, hub.URL, "", []byte(`{}`),
		func(int) time.Duration { return 30 * time.Second })
	if err == nil {
		t.Fatal("deliverMeterReport reported success against a hub answering 500")
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Errorf("deliverMeterReport took %v to give up after its reporter was retired: it slept out a backoff the retirement had already made pointless", elapsed)
	}
	if !strings.Contains(err.Error(), "reloaded") {
		t.Errorf("the give-up reason does not say the reporter was retired: %v", err)
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }
