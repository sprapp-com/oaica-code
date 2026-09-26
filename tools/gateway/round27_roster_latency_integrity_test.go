package main

// round27_roster_latency_integrity_test.go — /v1/models must answer, not wait
// (2026-09-27 audit, round 27, B-A).
//
// The health annotation ran its probes INLINE, one upstream after another: a
// roster whose upstreams are unreachable held the caller for probeTimeout
// (5s) per distinct upstream, so three dead backends took 15s and the launch
// picker's 800ms client gave up and silently dropped every model. The header
// comment claimed probes were "NOT on the request path" the whole time.
//
// A request now spends at most healthBudget on probes whose answers it does
// not have, and serves the rest without a status key.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// decodeModelsBody returns the "data" array of a /v1/models response.
func decodeModelsBody(t *testing.T, body []byte) []map[string]any {
	t.Helper()
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatalf("models body: %v", err)
	}
	return doc.Data
}

// TestRosterDoesNotWaitOnUnreachableUpstreams is the timing half: three models
// whose upstreams accept a connection and never answer, one probe each, and
// the roster has to come back well inside probeTimeout rather than
// 3 × probeTimeout.
func TestRosterDoesNotWaitOnUnreachableUpstreams(t *testing.T) {
	resetProbes(t)

	// Three distinct upstreams, each one a server that accepts and never
	// replies. Closing them unblocks the probes the handler left running.
	release := make(chan struct{})
	hang := func() *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			<-release
		}))
	}
	a, b, c := hang(), hang(), hang()
	// Cleanups run last-registered-first, and httptest's Close waits for the
	// requests in flight: the probes this test leaves running are released
	// BEFORE the servers are closed, in one cleanup, or the close waits on a
	// probe that waits on the close.
	t.Cleanup(func() {
		close(release)
		a.Close()
		b.Close()
		c.Close()
	})

	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: a.URL, ListenAddr: ":0", LedgerPath: t.TempDir() + "/l.jsonl",
		APIKeys: []gwKey{{SHA256: keyHash("sk-test"), Label: "x"}},
		Models: []gwModel{
			{ID: "m-a", UpstreamID: "m-a", UpstreamAddr: a.URL},
			{ID: "m-b", UpstreamID: "m-b", UpstreamAddr: b.URL},
			{ID: "m-c", UpstreamID: "m-c", UpstreamAddr: c.URL},
		},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	start := time.Now()
	rec := httptest.NewRecorder()
	g.modelsHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	elapsed := time.Since(start)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if elapsed > 2*probeTimeout/3 {
		t.Errorf("/v1/models took %v with three unreachable upstreams: the roster is waiting on probes it does not have (probeTimeout is %v each, and this is what made the picker drop every row)", elapsed, probeTimeout)
	}
	body := decodeModelsBody(t, rec.Body.Bytes())
	if len(body) != 3 {
		t.Fatalf("roster = %d models, want 3 — every model must be listed whether or not its probe has landed", len(body))
	}
	for _, m := range body {
		if m["status"] != nil {
			t.Errorf("model %v carries status %v while its probe cannot have finished: an answer served before the probe landed is not an answer", m["id"], m["status"])
		}
	}
}

// TestARosterDoesNotProbeTheSameUpstreamOncePerModel: the budget is one
// deadline for the whole roster, so a slow upstream shared by many models
// cannot be paid for many times either.
func TestARosterDoesNotProbeTheSameUpstreamOncePerModel(t *testing.T) {
	resetProbes(t)

	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
	}))
	defer up.Close()
	defer close(release)

	models := make([]gwModel, 0, 20)
	for i := 0; i < 20; i++ {
		models = append(models, gwModel{ID: string(rune('a' + i)), UpstreamID: "same", UpstreamAddr: up.URL})
	}
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: t.TempDir() + "/l.jsonl",
		APIKeys: []gwKey{{SHA256: keyHash("sk-test"), Label: "x"}},
		Models:  models,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	start := time.Now()
	rec := httptest.NewRecorder()
	g.modelsHandler(rec, httptest.NewRequest(http.MethodGet, "/v1/models", nil))
	elapsed := time.Since(start)
	if elapsed > 2*probeTimeout/3 {
		t.Errorf("/v1/models took %v for 20 models on one unreachable upstream: the wait is per request, not per model", elapsed)
	}
}
