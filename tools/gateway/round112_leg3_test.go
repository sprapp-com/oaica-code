package main

// round112_leg3_test.go — leg 3, round 112 (2026-09-29 audit), F112-L3-2..4.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// F112-L3-2: /health is public and each probe is a real 1-token completion sent with
// the gateway's own upstream credential, outside MaxConcurrent, the large-context pool
// and the ledger. The cache was only stamped when a probe RETURNED, so every request
// that arrived while one was in flight saw a stale cache and ran its own: 200
// concurrent anonymous GETs made 200 upstream completions, worst exactly when a
// starved fleet makes probes slow. One probe is in flight at a time now; the others
// wait for its result.
func TestMine112ConcurrentHealthChecksShareOneProbe(t *testing.T) {
	var hits atomic.Int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		time.Sleep(400 * time.Millisecond) // a probe that is slow under load
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"p"}}]}`))
	}))
	t.Cleanup(up.Close)
	srv, _ := r107Gw(t, up, nil)
	const n = 60
	var wg sync.WaitGroup
	codes := make(chan int, n)
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := http.Get(srv.URL + "/health")
			if err != nil {
				codes <- -1
				return
			}
			resp.Body.Close()
			codes <- resp.StatusCode
		}()
	}
	wg.Wait()
	close(codes)
	if got := hits.Load(); got > 2 {
		t.Errorf("%d concurrent anonymous /health requests made %d real upstream completions — one probe is in flight at a time (2026-09-29 audit, round 112, F112-L3-2)", n, got)
	}
	for c := range codes {
		if c != 200 {
			t.Errorf("a /health request answered %d, want the shared probe's 200 (2026-09-29 audit, round 112, F112-L3-2)", c)
		}
	}
}

// F112-L3-3: every reload builds a fresh transport per upstream and swaps the proxies
// wholesale, and nothing closed the old transports' idle connections, which lingered for
// IdleConnTimeout (90 s) each, up to 64 per retired proxy.
func TestMine112AReloadClosesTheRetiredProxysIdleConnections(t *testing.T) {
	var open atomic.Int32
	up := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"choices":[{"message":{"content":"p"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`))
	}))
	up.Config.ConnState = func(c net.Conn, s http.ConnState) {
		switch s {
		case http.StateNew:
			open.Add(1)
		case http.StateClosed:
			open.Add(-1)
		}
	}
	up.Start()
	t.Cleanup(up.Close)
	cfg := gwConfig{UpstreamAddr: up.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:  []gwModel{{ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768}}}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	t.Cleanup(srv.Close)
	body := `{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`
	for i := 0; i < 20; i++ {
		if code, _, b := r107Post(t, srv, "/v1/chat/completions", body, 0); code != 200 {
			t.Fatalf("premise: request %d answered %d %s", i, code, b)
		}
		if err := g.apply(cfg); err != nil {
			t.Fatal(err)
		}
	}
	// The closes are asynchronous on both ends; give them a moment.
	for i := 0; i < 100 && open.Load() > 2; i++ {
		time.Sleep(20 * time.Millisecond)
	}
	if got := open.Load(); got > 2 {
		t.Errorf("the upstream still holds %d open connections after 20 reloads — a retired proxy's idle connections must be closed (2026-09-29 audit, round 112, F112-L3-3)", got)
	}
}

// TestMine112AStalledBodyHoldsTheKeysSlot RECORDS F112-L3-4 (rank c, not changed).
// completionHandler takes the key's MaxConcurrent slot before it reads the body, and
// the body read has no deadline of its own (the server sets only ReadHeaderTimeout;
// the 15 minute request timeout starts after the read), so a valid key that opens a
// connection, sends headers with a Content-Length and a few bytes of body, and stalls,
// holds its slot until it disconnects. Recorded because the harm falls on the caller
// itself: the slot belongs to the caller's own key, the caller is authenticated, and the
// peer's death releases it through TCP keepalive; moving the acquire after the read
// would let a stalled body cost the gateway a goroutine and a buffer with no cap at all,
// which is worse. What a later round should know: the fix is a read deadline on the body
// (http.ResponseController.SetReadDeadline) with the slot kept where it is; the pin
// states today's reading and goes red when a deadline lands.
func TestMine112AStalledBodyHoldsTheKeysSlot(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(r111Completion))
	}))
	t.Cleanup(up.Close)
	srv, _ := r107Gw(t, up, []gwKey{{SHA256: keyHash("sk"), Label: "k", MaxConcurrent: 1}})
	c, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	c.Write([]byte("POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nAuthorization: Bearer sk\r\nContent-Type: application/json\r\nContent-Length: 500\r\n\r\n{\"model\":\"kat"))
	time.Sleep(300 * time.Millisecond)
	code, _, body := r107Post(t, srv, "/v1/chat/completions", `{"model":"kat-awq","messages":[{"role":"user","content":"go"}]}`, 5*time.Second)
	if code != 429 {
		t.Errorf("an honest request answered %d %s while the key's only slot sat behind a stalled body — this record states 429; if it now answers 200 a body read deadline has landed and this record is spent (2026-09-29 audit, round 112, F112-L3-4)", code, strings.TrimSpace(body))
	}
}
