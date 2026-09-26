package main

// round27_reload_meter_drain_integrity_test.go — a reload must not silently
// drop the reports its reporter was holding (2026-09-27 audit, round 27, B-C).
//
// apply() retires the meter reporter by closing meterDone, and runMeterReporter
// returns on that signal — so every report still queued on the channel it was
// draining became unreachable: nobody sends on it any more and nobody closes
// it. The local ledger still has those records, so no billing data is lost, but
// the aggregated view at meterhub is missing them and nothing said so.
//
// The reports the retired reporter had not delivered are now taken out of its
// channel and handed to the reporter that replaces it.

import (
	"bytes"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// captureGatewayLog runs fn with the standard logger pointed at a buffer and
// returns what it wrote.
func captureGatewayLog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	saved := log.Writer()
	flags := log.Flags()
	log.SetOutput(&buf)
	log.SetFlags(0)
	defer func() {
		log.SetOutput(saved)
		log.SetFlags(flags)
	}()
	fn()
	return buf.String()
}

// TestAReloadHandsQueuedReportsToTheNewReporter: five reports with the hub
// holding the first request open, so four are still queued when the reload
// happens. Every one of the five must reach the hub.
func TestAReloadHandsQueuedReportsToTheNewReporter(t *testing.T) {
	oldBackoff := meterReporterBackoff
	meterReporterBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { meterReporterBackoff = oldBackoff })

	var mu sync.Mutex
	seen := map[string]bool{}
	gate := make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-gate:
		case <-time.After(10 * time.Second):
		}
		body, _ := io.ReadAll(r.Body)
		var rep struct {
			RequestID string `json:"request_id"`
		}
		json.Unmarshal(body, &rep)
		mu.Lock()
		seen[rep.RequestID] = true
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()

	ledger := t.TempDir() + "/l.jsonl"
	cfg := gwConfig{
		UpstreamAddr: "http://127.0.0.1:1", ListenAddr: ":0", LedgerPath: ledger,
		MeterHubAddr: hub.URL,
		APIKeys:      []gwKey{{SHA256: keyHash("sk-test"), Label: "x"}},
		Models:       []gwModel{{ID: "m", UpstreamID: "m"}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}

	want := []string{"r1", "r2", "r3", "r4", "r5"}
	for _, id := range want {
		g.reportUsage(ledgerEntry{RequestID: id, Model: "m", Status: 200})
	}
	// The reporter is holding r1's POST open against the hub, so r2..r5 are
	// still on the channel.
	time.Sleep(50 * time.Millisecond)

	// The reload: same hub address, new reporter, and the queue of the one it
	// replaces.
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply (reload): %v", err)
	}
	close(gate)

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		missing := 0
		for _, id := range want {
			if !seen[id] {
				missing++
			}
		}
		got := len(seen)
		mu.Unlock()
		if missing == 0 {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("meterhub saw %d of %d reports after the reload: the reload dropped the ones its reporter was holding, and the aggregated view is missing records the local ledger has", got, len(want))
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestAReloadWithMeteringSwitchedOffSaysWhatItCouldNotHandOver covers the other
// half of the same fix: with no reporter to hand the queue to, the loss is
// announced (the local ledger still has the records, so this is a delay in the
// aggregated view and the operator is the one who can act on it) — what must
// not happen is the silence.
func TestAReloadWithMeteringSwitchedOffSaysWhatItCouldNotHandOver(t *testing.T) {
	oldBackoff := meterReporterBackoff
	meterReporterBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { meterReporterBackoff = oldBackoff })

	gate := make(chan struct{})
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-gate
		w.WriteHeader(http.StatusOK)
	}))
	defer hub.Close()
	defer close(gate)

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
	for i := 0; i < 3; i++ {
		g.reportUsage(ledgerEntry{RequestID: string(rune('a' + i)), Model: "m", Status: 200})
	}
	time.Sleep(50 * time.Millisecond)

	off := cfg
	off.MeterHubAddr = ""
	got := captureGatewayLog(t, func() {
		if err := g.apply(off); err != nil {
			t.Fatalf("apply (metering off): %v", err)
		}
	})
	if got == "" {
		t.Error("a reload that switched metering off dropped the queued reports without saying so: the aggregated view is quietly missing records the local ledger has")
	}
}
