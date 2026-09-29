package main

// round114_leg3_shutdown_test.go — leg 3, round 114 (2026-09-29 audit), F114-L3-1.
//
// main() ended with log.Fatal(srv.ListenAndServe()) and trapped only SIGHUP: a SIGTERM (a
// systemctl restart, a deploy, `docker stop`) ended the process at once. Every in-flight
// stream was cut mid-frame with no terminal event, and none of them got a ledger row, because
// the row is written after proxy.ServeHTTP returns and signal death runs nothing: a turn the
// upstream generated and billed left no trace, and the overage accounting never saw it.
// serveUntilDone drains instead: on the signal it stops accepting, lets streams finish for a
// grace period, then cancels the requests' context so the remainder take the existing aborted-row
// path, and returns only when the handlers are done.

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// r114Stream starts a gateway on a real listener, its upstream an SSE endpoint that sends
// `frames` frames `gap` apart (or trickles until the request ends when frames < 0), and
// returns the base URL, the ledger path, a cancel that plays the signal, and a channel that
// receives serveUntilDone's result.
func r114Stream(t *testing.T, frames int, gap, grace time.Duration) (string, string, context.CancelFunc, chan error) {
	t.Helper()
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		for i := 0; frames < 0 || i < frames; i++ {
			select {
			case <-r.Context().Done():
				return
			default:
			}
			io.WriteString(w, `data: {"choices":[{"index":0,"delta":{"content":"x"}}]}`+"\n\n")
			fl.Flush()
			time.Sleep(gap)
		}
		io.WriteString(w, `data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}],"usage":{"prompt_tokens":5,"completion_tokens":4}}`+"\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	t.Cleanup(up.Close)
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:  []gwModel{{ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768, Pricing: gwPricing{Prompt: "0", Completion: "0"}}}}); err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	srv := &http.Server{Handler: mux(g), ReadHeaderTimeout: 10 * time.Second}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	done := make(chan error, 1)
	go func() { done <- serveUntilDone(ctx, srv, ln, grace) }()
	return "http://" + ln.Addr().String(), ledger, cancel, done
}

func r114Post(t *testing.T, base string) *http.Response {
	t.Helper()
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","stream":true,"messages":[{"role":"user","content":"go"}]}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestMine114ShutdownLetsAStreamFinishAndLedgersIt(t *testing.T) {
	base, ledger, signalStop, done := r114Stream(t, 6, 150*time.Millisecond, 10*time.Second)
	resp := r114Post(t, base)
	defer resp.Body.Close()
	sc := bufio.NewScanner(resp.Body)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	got := 0
	for sc.Scan() {
		if got == 1 {
			signalStop() // the signal arrives mid-stream
		}
		if strings.HasPrefix(sc.Text(), "data:") {
			got++
		}
	}
	if err := sc.Err(); err != nil {
		t.Errorf("the stream was cut after the signal: %v (2026-09-29 audit, round 114, F114-L3-1)", err)
	}
	if got < 8 { // 6 content frames + the finish frame + [DONE]
		t.Errorf("the client received %d of 8 frames — a signal must let an in-flight stream finish (2026-09-29 audit, round 114, F114-L3-1)", got)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntilDone returned %v after a clean drain, want nil — http.ErrServerClosed is the normal end (2026-09-29 audit, round 114, F114-L3-1)", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveUntilDone did not return after the streams finished")
	}
	b, _ := os.ReadFile(ledger)
	if !strings.Contains(string(b), `"status":200`) {
		t.Errorf("no ledger row for the served turn after shutdown: %q (2026-09-29 audit, round 114, F114-L3-1)", b)
	}
	// And the listener is closed: a new connection is refused.
	if c, err := net.DialTimeout("tcp", strings.TrimPrefix(base, "http://"), time.Second); err == nil {
		c.Close()
		t.Errorf("the gateway still accepts connections after shutdown (2026-09-29 audit, round 114, F114-L3-1)")
	}
}

func TestMine114ShutdownGraceExpiryAbortsTheStreamButLedgersIt(t *testing.T) {
	base, ledger, signalStop, done := r114Stream(t, -1, 50*time.Millisecond, 300*time.Millisecond)
	resp := r114Post(t, base)
	defer resp.Body.Close()
	go io.Copy(io.Discard, resp.Body)
	time.Sleep(200 * time.Millisecond)
	start := time.Now()
	signalStop()
	select {
	case err := <-done:
		if err != nil {
			t.Errorf("serveUntilDone returned %v, want nil (2026-09-29 audit, round 114, F114-L3-1)", err)
		}
		// Grace 300ms: cancelling the requests' context ends the stream at once. Without it the
		// second wait runs its whole 10s before the connection is closed under the handler.
		if took := time.Since(start); took > 4*time.Second {
			t.Errorf("serveUntilDone took %v — the streams still running at the end of the grace period must be aborted through their context, not waited out (2026-09-29 audit, round 114, F114-L3-1)", took)
		}
	case <-time.After(15 * time.Second):
		t.Fatalf("serveUntilDone never returned for a stream that outlasts the grace period (2026-09-29 audit, round 114, F114-L3-1)")
	}
	// Read the ledger the instant serveUntilDone returns: it returns only once the handlers have
	// written their rows, and a process that exits on its return must not lose one.
	b, _ := os.ReadFile(ledger)
	if !strings.Contains(string(b), `"aborted":true`) {
		t.Errorf("no aborted ledger row for the stream cut at the end of the grace period: %q (2026-09-29 audit, round 114, F114-L3-1)", b)
	}
}
