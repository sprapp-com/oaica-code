package main

// bench_failed_run_exit_integrity_test.go — a bench run in which every epoch
// failed still exited 0 (2026-09-26 audit). BenchmarkModel printed its
// per-epoch errors to stderr and `continue`d, then returned nil; main called
// it and ignored the result entirely, so `bench -model x > results.csv` over a
// refused connection or a wrong model name produced an empty report, no rows,
// and a success exit code — indistinguishable from a run that was simply
// missing its rows, for any script or CI step that checks $?.
//
// The three bench_test.go tests that recorded this as the contract
// (ServerError, Timeout, NoMetrics) asserted "Expected error to be handled
// internally" and were rewritten with this change.

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

func benchOptionsFor(t *testing.T, models string, epochs int) flagOptions {
	t.Helper()
	fOpt := createTestFlagOptions()
	fOpt.models = &models
	fOpt.epochs = &epochs
	warmup := 0
	fOpt.warmup = &warmup
	return fOpt
}

func TestABenchRunThatProducedNoResultsFails(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "boom", http.StatusInternalServerError)
	}))
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)

	var out bytes.Buffer
	fOpt := benchOptionsFor(t, "test-model", 2)
	code := runBench(fOpt, &out, os.Stdout)
	if code == 0 {
		t.Errorf("a run whose every epoch failed exited 0 — the report has no rows and nothing distinguishes it from success (stderr: %s)", out.String())
	}
}

// The control: a run that produced rows still exits 0.
func TestABenchRunWithResultsSucceeds(t *testing.T) {
	server := createMockOllamaServer(t, mockServerOptions{generateResponses: defaultGenerateResponses()})
	defer server.Close()
	t.Setenv("OLLAMA_HOST", server.URL)

	var out bytes.Buffer
	fOpt := benchOptionsFor(t, "test-model", 1)
	if code := runBench(fOpt, &out, os.Stdout); code != 0 {
		t.Errorf("a successful run exited %d (stderr: %s)", code, out.String())
	}
}

// And a run that asked for no models is a usage error, not a success.
func TestABenchRunWithNoModelsIsAUsageError(t *testing.T) {
	var out bytes.Buffer
	fOpt := benchOptionsFor(t, "", 1)
	if code := runBench(fOpt, &out, os.Stdout); code == 0 {
		t.Errorf("`bench` with no -model exited 0 although it ran nothing")
	}
	if !strings.Contains(out.String(), "No model") {
		t.Errorf("the usage error was not reported: %s", out.String())
	}
}
