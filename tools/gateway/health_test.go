package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// A dead upstream must be labelled, not advertised healthy. UpstreamProbes
// is a package-level cache — reset between tests.
func resetProbes(t *testing.T) {
	t.Helper()
	// Probes the previous test left in flight land FIRST. The landing goroutine
	// creates the results map if it is gone (health.go) and publishes into it,
	// so clearing the map underneath one only moves the problem: the result
	// reappears after the reset, and the next test reads a health answer it did
	// not cause — a wrong verdict the moment httptest reuses an address
	// (2026-09-27 audit, round 30, B2). Waiting for them is what makes the reset
	// the last word.
	if !settleProbes(10 * time.Second) {
		t.Fatal("a probe started by an earlier test never landed, so the probe cache cannot be reset for this one")
	}
	upstreamProbes.Lock()
	upstreamProbes.m = nil
	// The flights too. They are per-test state: a ticket left over from a
	// previous test in the same binary makes the next one JOIN a probe that has
	// already landed instead of starting its own, so the test reads a result it
	// did not cause (2026-09-27 audit, round 29, B3).
	upstreamProbes.flights = nil
	upstreamProbes.Unlock()
}

// settleProbes waits, bounded, for every probe currently in flight to land —
// the landing goroutine publishes its result before it closes the ticket, so
// once this returns, nothing that was already running can write to the cache
// afterwards. Reports false when the deadline passes with a probe still out.
func settleProbes(timeout time.Duration) bool {
	upstreamProbes.Lock()
	flights := make([]*probeFlight, 0, len(upstreamProbes.flights))
	for _, f := range upstreamProbes.flights {
		flights = append(flights, f)
	}
	upstreamProbes.Unlock()

	deadline := time.Now().Add(timeout)
	for _, f := range flights {
		select {
		case <-f.done:
		case <-time.After(time.Until(deadline)):
			return false
		}
	}
	return true
}

func TestModelsHandler_HealthAnnotation(t *testing.T) {
	resetProbes(t)
	var upUp int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if atomic.LoadInt32(&upUp) == 1 {
			w.WriteHeader(http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"ok"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	defer up.Close()
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: t.TempDir() + "/l.jsonl",
		APIKeys: []gwKey{{SHA256: keyHash("sk-test"), Label: "x"}},
		Models:  []gwModel{{ID: "m1", UpstreamID: "m1"}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	req := httptest.NewRequest("GET", "/v1/models", nil)
	w := httptest.NewRecorder()
	g.modelsHandler(w, req)
	var body struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("models body: %v", err)
	}
	if len(body.Data) != 1 || body.Data[0]["status"] != nil {
		t.Fatalf("healthy model should omit status: %v", body.Data)
	}

	atomic.StoreInt32(&upUp, 1)
	resetProbes(t)
	w = httptest.NewRecorder()
	g.modelsHandler(w, req)
	json.Unmarshal(w.Body.Bytes(), &body)
	if len(body.Data) != 1 || body.Data[0]["status"] != "unhealthy" {
		t.Fatalf("dead upstream must be labelled unhealthy, got: %v", body.Data)
	}
}
