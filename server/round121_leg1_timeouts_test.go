package server

// F121-L1-2 (2026-09-29 audit, round 121): the server drops a client that stalls in its headers.

import (
	"net"
	"strings"
	"testing"
	"time"
)

func TestRound121ServerDropsAStalledHeader(t *testing.T) {
	if newHTTPServer().ReadHeaderTimeout <= 0 {
		t.Fatal("the server has no ReadHeaderTimeout")
	}
	srv := newHTTPServer()
	srv.ReadHeaderTimeout = 300 * time.Millisecond
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	defer srv.Close()
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("POST /api/chat HTTP/1.1\r\nHost: x\r\nX-Slow: "))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := c.Read(make([]byte, 256)); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Errorf("the server still holds a half-sent request after its header timeout (%v)", err)
	}
}
