package launch

// proxy_stream_flush_integrity_test.go — the two loopback proxies relayed
// with a bare io.Copy and never flushed (2026-09-26 audit).
//
// Go's HTTP server buffers a handler's writes in a 2048-byte bufio.Writer and
// drains it only when a buffer fills, when the handler returns, or when the
// handler calls Flush. A streaming relay that never flushes therefore delivers
// whatever the backend sends in 2 KiB gulps and holds the tail until the turn
// ends — the client sees nothing, then a burst, then nothing again. A short
// answer plus a kept-alive connection arrives only when the model stops
// talking, which for the client's own SSE parsing is the same as not
// streaming at all.
//
// The proxy the two paths do not use is the one that gets this right:
// anthropic_openai_proxy's passthrough flushes after every write, and says why
// ("Claude Code's own SSE parsing depends on timely chunk delivery"). These
// two are the legs `oaica serve` and the agent shims actually run on.
//
// The test is the visible half of that: a backend that sends one frame, holds
// the connection open, then sends another. The first frame has to reach the
// client while the backend is still holding it.

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

// slowStreamBackend answers with one SSE frame immediately and a second after
// hold, giving a test a window in which a correctly-flushing relay has already
// delivered the first frame and a buffering one has delivered nothing.
func slowStreamBackend(t *testing.T, hold time.Duration) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"delta\":\"first\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		time.Sleep(hold)
		_, _ = w.Write([]byte("data: {\"delta\":\"second\"}\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv
}

// timeToFirstByte posts through the proxy and reports how long the first body
// byte took to arrive.
func timeToFirstByte(t *testing.T, proxyURL string) time.Duration {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")

	type result struct {
		d   time.Duration
		err error
	}
	done := make(chan result, 1)
	start := time.Now()
	go func() {
		client := &http.Client{Timeout: 10 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			done <- result{0, err}
			return
		}
		defer resp.Body.Close()
		buf := make([]byte, 1)
		_, err = io.ReadFull(resp.Body, buf)
		done <- result{time.Since(start), err}
	}()

	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("reading the first byte through the proxy: %v", r.err)
		}
		return r.d
	case <-time.After(5 * time.Second):
		t.Fatal("no body byte reached the client within 5s")
		return 0
	}
}

// portOfURL is the port behind an httptest server's URL.
func portOfURL(t *testing.T, raw string) int {
	t.Helper()
	u, err := url.Parse(raw)
	if err != nil {
		t.Fatalf("parse %q: %v", raw, err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("port in %q: %v", raw, err)
	}
	return p
}

const streamHold = 1200 * time.Millisecond

// A frame the backend has already written and flushed must not wait for the
// backend to finish the turn.
func TestTheLoggingProxyFlushesEachFrameAsItArrives(t *testing.T) {
	backend := slowStreamBackend(t, streamHold)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunLocalLoggingProxy(ln, backend.URL) }()
	t.Cleanup(func() { ln.Close() })

	ttfb := timeToFirstByte(t, "http://"+ln.Addr().String())
	if ttfb > streamHold/2 {
		t.Errorf("the first streamed frame took %s to reach the client while the backend held the connection for %s — the relay never flushes, so Go's 2 KiB response buffer holds frames until it fills or the handler returns, and streaming is off", ttfb, streamHold)
	}
}

func TestTheNormalizingProxyFlushesEachFrameAsItArrives(t *testing.T) {
	backend := slowStreamBackend(t, streamHold)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyPort := proxyLn.Addr().(*net.TCPAddr).Port
	proxyLn.Close() // the proxy binds this port itself
	go func() { _ = RunNormalizingProxyOn("127.0.0.1", proxyPort, portOfURL(t, backend.URL), "") }()
	waitForListener(t, "127.0.0.1", proxyPort)

	ttfb := timeToFirstByte(t, fmt.Sprintf("http://127.0.0.1:%d", proxyPort))
	if ttfb > streamHold/2 {
		t.Errorf("the first streamed frame took %s to reach the client while the backend held the connection for %s — the relay never flushes, so Go's 2 KiB response buffer holds frames until it fills or the handler returns, and streaming is off", ttfb, streamHold)
	}
}
