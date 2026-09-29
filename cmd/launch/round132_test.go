package launch

import (
	"bufio"
	"fmt"
	"net"
	"net/http"
	"testing"
	"time"
)

// F132-L2-3 (2026-09-29 audit, round 132): a peer that sends headers with a Content-Length and never the body is
// answered 401 within the body deadline, not held for ever.
func TestRound132UnauthenticatedStalledBodyIsAnswered(t *testing.T) {
	old := normalizingProxyBodyTimeout
	normalizingProxyBodyTimeout = 500 * time.Millisecond
	defer func() { normalizingProxyBodyTimeout = old }()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	go func() { _ = RunNormalizingProxyOnKeyed("127.0.0.1", port, 1, "K", "") }()
	waitForListener(t, "127.0.0.1", port)
	c, err := net.Dial("tcp", fmt.Sprintf("127.0.0.1:%d", port))
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	fmt.Fprintf(c, "POST /v1/messages HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 1\r\n\r\n")
	c.SetReadDeadline(time.Now().Add(4 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(c), nil)
	if err != nil {
		t.Fatalf("no answer within the deadline: %v", err)
	}
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status %d, want 401", resp.StatusCode)
	}
}
