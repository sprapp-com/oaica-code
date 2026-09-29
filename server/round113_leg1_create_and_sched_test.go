package server

// round113_leg1_create_and_sched_test.go — leg 1, round 113 (2026-09-29 audit),
// F113-L1-1 and F113-L1-2: two more places a producer or a caller was left waiting on a
// channel nobody would ever write, or read.

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/manifest"
	"github.com/ollama/ollama/types/model"
)

// F113-L1-1: in CreateHandler's producer, a failed pull of the base model sent its error
// frame and did not return, so it ran on into createModel with no base layers. With
// stream:false the consumer (waitForStream) answers on the first error frame and nothing
// drains the channel, so the producer blocked for ever in createModel: one leaked
// goroutine per failed request. In the default stream it wrote a MANIFEST for a model with
// zero layers after telling the client the create had failed: a model that shows in
// /api/tags, cannot run, and makes a retry hit an existing name.
func r113CreateServer(t *testing.T) *httptest.Server {
	t.Helper()
	gin.SetMode(gin.TestMode)
	setTestHome(t, t.TempDir())
	s := &Server{}
	r := gin.New()
	r.POST("/api/create", s.CreateHandler)
	srv := httptest.NewServer(r)
	t.Cleanup(srv.Close)
	return srv
}

func TestMine113AFailedBasePullDoesNotLeakTheProducer(t *testing.T) {
	srv := r113CreateServer(t)
	time.Sleep(200 * time.Millisecond)
	before := runtime.NumGoroutine()
	f := false
	for i := 0; i < 10; i++ {
		b, _ := json.Marshal(api.CreateRequest{Model: "leak" + string(rune('a'+i)), From: "127.0.0.1:1/lib/base:latest", Stream: &f})
		resp, err := http.Post(srv.URL+"/api/create", "application/json", strings.NewReader(string(b)))
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode < 400 {
			t.Fatalf("premise: a create from an unreachable base answered %d", resp.StatusCode)
		}
	}
	if after := r112Settle(before); after > before+2 {
		t.Errorf("goroutines before=%d after=%d — a failed create must not leave its producer blocked on a send nobody reads (2026-09-29 audit, round 113, F113-L1-1)", before, after)
	}
}

func TestMine113AFailedBasePullWritesNoManifest(t *testing.T) {
	srv := r113CreateServer(t)
	b, _ := json.Marshal(api.CreateRequest{Model: "ghost", From: "127.0.0.1:1/lib/base:latest"})
	resp, err := http.Post(srv.URL+"/api/create", "application/json", strings.NewReader(string(b)))
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(body), `"error"`) {
		t.Fatalf("premise: the create did not report an error: %s", body)
	}
	time.Sleep(500 * time.Millisecond)
	if _, err := manifest.ParseNamedManifest(model.ParseName("ghost")); err == nil {
		t.Errorf("a create whose base could not be pulled wrote a manifest for it — the client was told it failed and a layerless model was left in /api/tags (2026-09-29 audit, round 113, F113-L1-1)")
	}
}

// F113-L1-2: a request cancelled while it waits in the scheduler's queue was dropped by
// processPending without an answer, and scheduleRunner waits on success and error
// channels only, so the handler goroutine (with its request body and gin context) blocked
// for the life of the process: one per impatient client behind a slow model load. The
// dropped request is now answered with its context's error, and the wait also selects on
// the context.
func TestMine113ACancelledQueuedRequestIsAnswered(t *testing.T) {
	ctx, done := context.WithTimeout(t.Context(), 10*time.Second)
	defer done()
	s := InitScheduler(ctx)
	s.waitForRecovery = 10 * time.Millisecond
	s.getGpuFn = getGpuFn
	s.getSystemInfoFn = getSystemInfoFn
	a := newScenarioRequest(t, ctx, "ollama-model-1", 10, &api.Duration{Duration: time.Minute}, nil)
	s.newServerFn = a.newServer

	reqCtx, cancel := context.WithCancel(ctx)
	runnerCh, errCh := s.getRunner(reqCtx, a.req.model, api.DefaultOptions(), nil, false, false, nil)
	returned := make(chan string, 1)
	go func() {
		select {
		case <-runnerCh:
			returned <- "runner"
		case err := <-errCh:
			returned <- "err " + err.Error()
		}
	}()
	cancel() // the client leaves while queued
	s.Run(ctx)
	select {
	case r := <-returned:
		if !strings.HasPrefix(r, "err") {
			t.Errorf("the caller of a cancelled queued request was answered %q, want its context's error (2026-09-29 audit, round 113, F113-L1-2)", r)
		}
	case <-time.After(3 * time.Second):
		t.Errorf("the caller of a cancelled queued request was never answered (2026-09-29 audit, round 113, F113-L1-2)")
	}
}

// TestMine113ScheduleRunnerStopsWaitingWhenItsContextIsDone pins the other half: the wait
// in scheduleRunner itself selects on the context, so a request whose answer never comes
// (a scheduler that is not running) does not block its handler for ever.
func TestMine113ScheduleRunnerStopsWaitingWhenItsContextIsDone(t *testing.T) {
	ctx, done := context.WithTimeout(t.Context(), 10*time.Second)
	defer done()
	s := &Server{sched: InitScheduler(ctx)} // never Run: nothing will ever answer
	reqCtx, cancel := context.WithTimeout(ctx, 300*time.Millisecond)
	defer cancel()
	m := &Model{Name: "m", ModelPath: "/tmp/none"}
	finished := make(chan error, 1)
	go func() {
		_, _, _, err := s.scheduleRunner(reqCtx, m, nil, nil, nil, nil)
		finished <- err
	}()
	select {
	case err := <-finished:
		if err == nil {
			t.Errorf("scheduleRunner returned no error for a request nothing answered")
		}
	case <-time.After(3 * time.Second):
		t.Errorf("scheduleRunner is still waiting 3s after its request context ended (2026-09-29 audit, round 113, F113-L1-2)")
	}
}
