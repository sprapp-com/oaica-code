package main

// round110_leg3_pull_stall_test.go — leg 3, round 110 (2026-09-29 audit), F110-L3-3.
//
// /v1/pull streams a model file behind a four-slot semaphore, and the server has
// no write timeout. Four anonymous sockets that request a public file-source model
// and never read held all four slots for as long as they stayed open, so every
// legitimate pull got "retry shortly" that never became true. A stream whose client
// has accepted nothing for pullStallTimeout is now dropped and its slot released.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestMine110AStalledDownloadReleasesItsPullSlot(t *testing.T) {
	old := pullStallTimeout
	pullStallTimeout = 300 * time.Millisecond
	t.Cleanup(func() { pullStallTimeout = old })

	blob := filepath.Join(t.TempDir(), "big.gguf")
	f, err := os.Create(blob)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(256 << 20); err != nil {
		t.Fatal(err)
	}
	f.Close()

	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: "http://127.0.0.1:1", ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "ledger.jsonl"),
		APIKeys:     []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:      []gwModel{{ID: "kat-awq", OwnedBy: "oaica", Pricing: gwPricing{Prompt: "0", Completion: "0"}}},
		PullCatalog: []gwPullEntry{{Model: "big", Source: "file", FilePath: blob, SizeBytes: 256 << 20}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	t.Cleanup(srv.Close)
	addr := strings.TrimPrefix(srv.URL, "http://")

	var socks []net.Conn
	for i := 0; i < cap(pullStreamSem); i++ {
		c, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		socks = append(socks, c)
		c.Write([]byte("GET /v1/pull/big HTTP/1.1\r\nHost: x\r\n\r\n"))
	}
	t.Cleanup(func() {
		for _, c := range socks {
			c.Close()
		}
	})

	// The stallers hold every slot until the timeout drops them; a legitimate pull
	// then gets one. Poll rather than sleep a fixed time.
	var code int
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); time.Sleep(100 * time.Millisecond) {
		resp, err := http.Get(srv.URL + "/v1/pull/big")
		if err != nil {
			t.Fatal(err)
		}
		code = resp.StatusCode
		resp.Body.Close()
		if code == http.StatusOK {
			return
		}
	}
	t.Errorf("a legitimate pull still answers %d after every staller was past the stall timeout — stalled anonymous downloads must not hold the pull slots (2026-09-29 audit, round 110, F110-L3-3)", code)
}

// TestMine110ASlowButReadingDownloadIsNotCutOff is the control: the deadline is per
// write, so a client that keeps reading finishes however long the transfer takes.
func TestMine110ASlowButReadingDownloadIsNotCutOff(t *testing.T) {
	// 3 s, not tighter: when the client's window closes, TCP's persist timer can
	// hold the sender for several hundred milliseconds although the client is
	// reading, and this test is about the deadline being per write, not about TCP.
	old := pullStallTimeout
	pullStallTimeout = 3 * time.Second
	t.Cleanup(func() { pullStallTimeout = old })
	const size = 48 << 20
	blob := filepath.Join(t.TempDir(), "slow.gguf")
	f, _ := os.Create(blob)
	f.Truncate(size)
	f.Close()
	g := &gateway{}
	if err := g.apply(gwConfig{UpstreamAddr: "http://127.0.0.1:1", ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "ledger.jsonl"),
		APIKeys:     []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:      []gwModel{{ID: "kat-awq", OwnedBy: "oaica", Pricing: gwPricing{Prompt: "0", Completion: "0"}}},
		PullCatalog: []gwPullEntry{{Model: "slow", Source: "file", FilePath: blob, SizeBytes: size}}}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	t.Cleanup(srv.Close)
	resp, err := http.Get(srv.URL + "/v1/pull/slow")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	buf := make([]byte, 1<<20)
	total, start := 0, time.Now()
	for {
		n, err := resp.Body.Read(buf)
		total += n
		if err != nil {
			break
		}
		time.Sleep(150 * time.Millisecond)
	}
	if time.Since(start) < pullStallTimeout {
		t.Fatalf("premise: the transfer took %v, under the %v stall timeout, so it cannot show a per-write deadline", time.Since(start), pullStallTimeout)
	}
	if total != size {
		t.Errorf("a slow client received %d of %d bytes in %v — a stream that keeps being read must not be cut off, however long it takes (2026-09-29 audit, round 110, F110-L3-3)", total, size, time.Since(start))
	}
}
