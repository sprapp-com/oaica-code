package server

// round114_leg1_drain_test.go — leg 1, round 114 (2026-09-29 audit), F114-L1-1.
//
// Serve()'s signal goroutine called srvr.Close() on SIGTERM or SIGINT, so a restart, a deploy or a
// Ctrl-C cut every request in flight at once: each stream ended in a bare TCP EOF with no terminal
// frame (no message_stop, [DONE], response.completed or error event), and a non-stream request lost
// its whole answer, though a generation seconds from finishing was in progress. drainServer stops
// accepting, gives the requests in flight a grace period to finish, and only then closes.

import (
	"bufio"
	"io"
	"net"
	"net/http"
	"os"
	"strings"
	"syscall"
	"testing"
	"time"
)

func r114Server(t *testing.T, h http.HandlerFunc) (*http.Server, string, chan error) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srvr := &http.Server{Handler: h}
	done := make(chan error, 1)
	go func() { done <- srvr.Serve(ln) }()
	t.Cleanup(func() { _ = srvr.Close() })
	return srvr, "http://" + ln.Addr().String(), done
}

func TestMine114AnInFlightStreamFinishesWhenTheServerIsStopped(t *testing.T) {
	entered := make(chan struct{})
	srvr, base, _ := r114Server(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		close(entered)
		for i := 0; i < 5; i++ {
			io.WriteString(w, "frame\n")
			fl.Flush()
			time.Sleep(100 * time.Millisecond)
		}
		io.WriteString(w, "[DONE]\n")
	})
	resp, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
	drained := make(chan struct{})
	go func() { drainServer(srvr, 10*time.Second); close(drained) }()
	sc := bufio.NewScanner(resp.Body)
	var lines []string
	for sc.Scan() {
		lines = append(lines, sc.Text())
	}
	if err := sc.Err(); err != nil || len(lines) != 6 || lines[5] != "[DONE]" {
		t.Errorf("the stream ended after %d lines with %v (last %q) — a stop must let an in-flight stream finish (2026-09-29 audit, round 114, F114-L1-1)", len(lines), err, strings.Join(lines[max(0, len(lines)-1):], ""))
	}
	select {
	case <-drained:
	case <-time.After(10 * time.Second):
		t.Fatalf("drainServer did not return once the stream had finished")
	}
	if c, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), time.Second); err == nil {
		c.Close()
		t.Errorf("the server still accepts connections after being stopped (2026-09-29 audit, round 114, F114-L1-1)")
	}
}

func TestMine114AStreamOutlastingTheGraceIsCutNotWaitedForever(t *testing.T) {
	entered := make(chan struct{})
	ctxDone := make(chan struct{})
	srvr, base, _ := r114Server(t, func(w http.ResponseWriter, r *http.Request) {
		fl := w.(http.Flusher)
		io.WriteString(w, "frame\n")
		fl.Flush()
		close(entered)
		<-r.Context().Done()
		close(ctxDone)
	})
	resp, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
	start := time.Now()
	drainServer(srvr, 300*time.Millisecond)
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("drainServer took %v with a 300ms grace (2026-09-29 audit, round 114, F114-L1-1)", took)
	}
	select {
	case <-ctxDone:
	case <-time.After(3 * time.Second):
		t.Errorf("the handler's context was never cancelled — a stream past the grace period must be cut (2026-09-29 audit, round 114, F114-L1-1)")
	}
}

// A second signal while the drain is under way cuts what is left at once: an operator is never stuck
// behind the grace period.
func TestMine114ASecondSignalForcesTheStop(t *testing.T) {
	entered := make(chan struct{})
	srvr, base, _ := r114Server(t, func(w http.ResponseWriter, r *http.Request) {
		w.(http.Flusher).Flush()
		close(entered)
		<-r.Context().Done()
	})
	resp, err := http.Get(base)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	<-entered
	signals := make(chan os.Signal, 2)
	finished := make(chan struct{})
	go handleShutdownSignals(signals, srvr, time.Hour, func() { close(finished) })
	signals <- syscall.SIGTERM
	time.Sleep(200 * time.Millisecond)
	select {
	case <-finished:
		t.Fatalf("the first signal cut a stream that had an hour of grace (2026-09-29 audit, round 114, F114-L1-1)")
	default:
	}
	signals <- syscall.SIGTERM
	select {
	case <-finished:
	case <-time.After(5 * time.Second):
		t.Errorf("a second signal did not force the stop while a drain with an hour of grace was running (2026-09-29 audit, round 114, F114-L1-1)")
	}
}
