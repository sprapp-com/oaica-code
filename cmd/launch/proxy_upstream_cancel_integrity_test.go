package launch

// proxy_upstream_cancel_integrity_test.go — both local proxies forwarded with
// http.DefaultClient and a request that carried no context (2026-09-26 audit).
//
// Every other upstream call in this package is bounded twice: a dial timeout on
// a package transport and, more importantly, the CALLER's context, so a client
// that gives up (Ctrl-C, Claude Code's own timeout, a closed terminal) closes
// the connection to the model server with it. These two handed the backend a
// request with no context and no transport bounds, so an upstream that accepted
// the connection and then said nothing kept the handler — and both sockets —
// for as long as the process lived. A few of those and the machine is out of
// file descriptors and the user has no idea why.
//
// The test is the observable half of that: a backend that accepts and never
// answers; a client that cancels; the backend's socket has to close.

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"
)

// hangBackend accepts every connection and never writes a byte, reporting
// accepts and closes so a test can tell "nothing reached the backend" (a
// vacuous pass) from "the connection was released".
type hangBackend struct {
	ln       net.Listener
	accepted chan struct{}
	closed   chan struct{}
}

func newHangBackend(t *testing.T) *hangBackend {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	h := &hangBackend{ln: ln, accepted: make(chan struct{}, 16), closed: make(chan struct{}, 16)}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			h.accepted <- struct{}{}
			go func(c net.Conn) {
				defer c.Close()
				_ = c.SetReadDeadline(time.Now().Add(30 * time.Second))
				buf := make([]byte, 4096)
				for {
					if _, err := c.Read(buf); err != nil {
						h.closed <- struct{}{}
						return
					}
				}
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return h
}

func (h *hangBackend) url() string { return "http://" + h.ln.Addr().String() }
func (h *hangBackend) port() int {
	return h.ln.Addr().(*net.TCPAddr).Port
}

func (h *hangBackend) waitAccepted(t *testing.T) {
	t.Helper()
	select {
	case <-h.accepted:
	case <-time.After(5 * time.Second):
		t.Fatal("the proxy never reached the backend, so nothing about cancellation was tested")
	}
}

// upstreamReleaseBudget is how long the backend waits to see its connection
// closed after the client gives up. The property under test is that the socket
// is released AT ALL — the defect this pins held it until the process exited —
// not that a cancellation propagates inside 3 seconds, which is what it used to
// assert. That budget is wall-clock on a loopback round trip plus the proxy's
// own goroutine being scheduled, and under the full suite's load it was missed
// once in ten runs (2026-09-26, 3.01s elapsed — the run that reported
// "still open 3s after the client gave up" with nothing wrong). A leaked
// connection is still caught: it never closes.
const upstreamReleaseBudget = 10 * time.Second

func (h *hangBackend) waitClosed(t *testing.T, within time.Duration) {
	t.Helper()
	started := time.Now()
	select {
	case <-h.closed:
		t.Logf("the upstream was released after %s", time.Since(started).Round(time.Millisecond))
	case <-time.After(within):
		t.Errorf("the backend's connection was still open %s after the client gave up — the proxy forwards without the caller's context, so an upstream that accepted and never answered holds the socket (and this process's file descriptor) until oaica exits", within)
	}
}

// waitForListener blocks until something accepts on the port. RunNormalizingProxyOn
// binds the port itself (that is the signature production uses), so the test
// cannot hand it a listener and must not race it to the connect.
func waitForListener(t *testing.T, host string, port int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", fmt.Sprintf("%s:%d", host, port), 100*time.Millisecond)
		if err == nil {
			conn.Close()
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("the proxy on %s:%d never started listening", host, port)
}

// The logging proxy — the one `oaica launch claude` always routes through.
func TestTheLoggingProxyReleasesAnUpstreamThatNeverAnswers(t *testing.T) {
	backend := newHangBackend(t)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunLocalLoggingProxy(ln, backend.url()) }()
	t.Cleanup(func() { ln.Close() })
	proxyURL := "http://" + ln.Addr().String()

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, proxyURL+"/v1/messages",
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	backend.waitAccepted(t)
	cancel()
	backend.waitClosed(t, upstreamReleaseBudget)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the proxy's own handler did not return after its client gave up")
	}
}

// The normalizing proxy — `oaica serve`'s loopback leg.
func TestTheNormalizingProxyReleasesAnUpstreamThatNeverAnswers(t *testing.T) {
	backend := newHangBackend(t)

	proxyLn, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	proxyPort := proxyLn.Addr().(*net.TCPAddr).Port
	proxyLn.Close() // the proxy binds this port itself
	go func() { _ = RunNormalizingProxyOn("127.0.0.1", proxyPort, backend.port(), "") }()
	waitForListener(t, "127.0.0.1", proxyPort)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, fmt.Sprintf("http://127.0.0.1:%d/v1/chat/completions", proxyPort),
		strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")

	done := make(chan struct{})
	go func() {
		defer close(done)
		if resp, err := http.DefaultClient.Do(req); err == nil {
			resp.Body.Close()
		}
	}()

	backend.waitAccepted(t)
	cancel()
	backend.waitClosed(t, upstreamReleaseBudget)

	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Error("the proxy's own handler did not return after its client gave up")
	}
}
