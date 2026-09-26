package main

// round25_gateway_concurrency_integrity_test.go — three defects around the
// config reload, all of them "the reload path and the request path disagree
// about what is shared" (2026-09-27 audit, round 25).
//
// The gateway reloads its config on SIGHUP: apply() swaps g.cfg, g.entitlement,
// g.meterCh and the admission pool while requests are being served. Round 25's
// auditor proved two unlocked reads against that swap with -race and a
// goroutine leak that fires on the swap itself.
//
// Run this file under -race: the two race tests cannot fail without it (that is
// what a race is), and the leak test fails either way. Both are part of the
// round's verification:
//
//	cd tools/gateway && go test -race -run 'Round25|MeterReporter|CfgRace|Entitlement' .
//
// The race tests drive the READER the fix introduced rather than the whole
// handler: the defect was the read sitting outside every lock scope, and the
// helper is where the lock now is. Reverting the helper's RLock makes them RED
// under -race (verified for this round).

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"runtime"
	"sync"
	"testing"
	"time"
)

// TestReloadDoesNotLeakAMeterReporter: every apply() with MeterHubAddr set
// starts a reporter goroutine, and the reporter of the config being replaced
// must stop. runMeterReporter ranges over its channel, so it exits when that
// channel closes — and nothing closed it, so each reload parked one more
// goroutine on a receive that could never return.
func TestReloadDoesNotLeakAMeterReporter(t *testing.T) {
	cfg := gwConfig{
		UpstreamAddr: "http://127.0.0.1:9", ListenAddr: ":0",
		LedgerPath:   t.TempDir() + "/ledger.jsonl",
		MeterHubAddr: "http://127.0.0.1:9", // nothing listens: the reporter only queues
		APIKeys:      []gwKey{{SHA256: keyHash("sk-test"), Label: "test"}},
		Models:       []gwModel{{ID: "m", UpstreamID: "m", OwnedBy: "oaica"}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Let the first reporter reach its select before measuring.
	time.Sleep(50 * time.Millisecond)
	before := runtime.NumGoroutine()

	const reloads = 8
	for i := 0; i < reloads; i++ {
		cfg.MeterHubToken = string(rune('a' + i)) // a reload that changes something
		if err := g.apply(cfg); err != nil {
			t.Fatalf("apply %d: %v", i, err)
		}
	}

	// The retired reporters exit promptly once their done channel closes; poll
	// briefly instead of sleeping a fixed amount, so a pass is not a race with
	// the scheduler.
	deadline := time.Now().Add(2 * time.Second)
	for {
		n := runtime.NumGoroutine()
		if n <= before+1 {
			return // one live reporter for the current config, and no leak
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d reloads left %d goroutines (was %d before them): each apply() starts a reporter and the retired one never stops", reloads, n, before)
		}
		runtime.Gosched()
		time.Sleep(5 * time.Millisecond)
	}
}

// TestTheReporterTakesItsBackoffAtStartup is the fourth defect, and it is a
// test-side race that the round's `-race` run caught: meterReporterBackoff is a
// package var (a test hook — the deliberately-unreachable-meterhub test shrinks
// it and restores it in t.Cleanup), and the reporter goroutine re-read it on
// every retry. main_test.go's t.Cleanup therefore wrote a package var while a
// live goroutine was reading it.
//
// The fix is that the value is read ONCE, by the goroutine that starts the
// reporter, and passed in. This test pins that behaviourally: with the backoff
// captured at startup, changing the package var afterwards cannot slow a
// report already in flight. Reverting to a read inside the retry loop makes the
// second report's retries wait on the swapped-in value and the deadline below
// expires.
func TestTheReporterTakesItsBackoffAtStartup(t *testing.T) {
	attempts := make(chan string, 32)
	hub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rep usageReport
		_ = json.NewDecoder(r.Body).Decode(&rep)
		attempts <- rep.RequestID
		w.WriteHeader(http.StatusInternalServerError) // never delivered: every report retries maxAttempts times
	}))
	t.Cleanup(hub.Close)

	oldBackoff := meterReporterBackoff
	meterReporterBackoff = func(int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { meterReporterBackoff = oldBackoff })

	cfg := gwConfig{
		UpstreamAddr: "http://127.0.0.1:9", ListenAddr: ":0",
		LedgerPath:   t.TempDir() + "/ledger.jsonl",
		MeterHubAddr: hub.URL, MeterHubToken: "tok",
		APIKeys: []gwKey{{SHA256: keyHash("sk-test"), Label: "test"}},
		Models:  []gwModel{{ID: "m", UpstreamID: "m", OwnedBy: "oaica"}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}

	// Wait for the first report's attempts, then swap the hook for a value that
	// would stall a reporter which read it per retry.
	g.reportUsage(ledgerEntry{RequestID: "first"})
	if err := waitForAttempts(attempts, "first", 2*time.Second); err != nil {
		t.Fatalf("premise: the reporter never retried: %v", err)
	}
	meterReporterBackoff = func(int) time.Duration { return 10 * time.Second }

	g.reportUsage(ledgerEntry{RequestID: "second"})
	if err := waitForAttempts(attempts, "second", 2*time.Second); err != nil {
		t.Errorf("a report queued after the backoff hook changed did not finish retrying: %v — the reporter is reading the package var on every retry, so a test's cleanup (or any other writer) races the live goroutine", err)
	}
}

// waitForAttempts drains attempt records until count of the report's own
// attempts reach runMeterReporter's maxAttempts, or the deadline passes.
func waitForAttempts(attempts <-chan string, requestID string, within time.Duration) error {
	const maxAttempts = 3
	deadline := time.Now().Add(within)
	seen := 0
	for seen < maxAttempts {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			return fmt.Errorf("saw %d of %d attempts for %q within %v", seen, maxAttempts, requestID, within)
		}
		select {
		case got := <-attempts:
			if got == requestID {
				seen++
			}
		case <-time.After(remaining):
			return fmt.Errorf("saw %d of %d attempts for %q within %v", seen, maxAttempts, requestID, within)
		}
	}
	return nil
}

// TestAModelsRequestDoesNotRaceAConfigReload is the /v1/models half: the// handler snapshots g.cfg.Models under RLock, releases it, and then called
// annotateHealth — which read g.cfg.UpstreamAddr with no lock at all, against
// apply()'s write under the write lock. Under -race this is RED before the fix.
func TestAModelsRequestDoesNotRaceAConfigReload(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	t.Cleanup(up.Close)

	cfg := gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0",
		LedgerPath: t.TempDir() + "/ledger.jsonl",
		APIKeys:    []gwKey{{SHA256: keyHash("sk-test"), Label: "test"}},
		Models:     []gwModel{{ID: "m", UpstreamID: "m", OwnedBy: "oaica"}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}

	other := cfg
	other.UpstreamAddr = up.URL + "/reloaded"
	other.LedgerPath = t.TempDir() + "/ledger2.jsonl"

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := g.apply(other); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 300; i++ {
		g.modelsHandler(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	}
	close(stop)
	wg.Wait()
}

// TestEntitlementIsReadUnderTheLock is the completion path's half: apply()
// replaces g.entitlement on every reload and the handler read the field outside
// every lock scope. It drives the REAL handler (mux → completionHandler), not
// the snapshot helper, because a test of the helper would pass with the handler
// still reading the field directly — which is the defect.
//
// Entitlement stays disabled in both configurations on purpose: the race is the
// field read beside a field write, and apply() writes g.entitlement (to nil)
// whether or not entitlement is on. Keeping it off also keeps every request
// from calling a meterhub that is not there.
func TestEntitlementIsReadUnderTheLock(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	t.Cleanup(up.Close)

	ledger := t.TempDir() + "/ledger.jsonl"
	cfg := gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	other := cfg
	other.LedgerPath = t.TempDir() + "/ledger2.jsonl"
	other.MeterHubToken = "reloaded"

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if err := g.apply(other); err != nil {
				return
			}
		}
	}()
	for i := 0; i < 60; i++ {
		postCompletion(t, g, "sk-new")
	}
	close(stop)
	wg.Wait()
}
