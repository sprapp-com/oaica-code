package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// brokenVLLM reproduces the 2026-08-25 outage exactly: the HTTP server is up
// and GET /v1/models is 200, but every chat completion is a 400 (tokenizer
// had no chat_template). The old GET probe reports this backend UP; the
// chat probe must report it DOWN.
func brokenVLLM(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(200)
			io.WriteString(w, `{"object":"list","data":[{"id":"kat-awq"}]}`)
		case "/v1/chat/completions":
			w.WriteHeader(400)
			io.WriteString(w, `{"error":{"message":"default chat template is no longer allowed"}}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

// healthyVLLM answers both probes 200 and records the chat body it got.
func healthyVLLM(t *testing.T, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(200)
		case "/v1/chat/completions":
			b, _ := io.ReadAll(r.Body)
			var m map[string]any
			json.Unmarshal(b, &m)
			*got = m
			w.WriteHeader(200)
			io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
}

func TestProbe_GETMissesChat400_ChatProbeCatchesIt(t *testing.T) {
	srv := brokenVLLM(t)
	defer srv.Close()
	b := newBackend(srv.URL)
	client := &http.Client{Timeout: 2 * time.Second}

	if !b.probeOnce(client, "/v1/models", "") {
		t.Fatal("GET probe should report the broken backend UP (that is the bug being fixed)")
	}
	if b.probeOnce(client, "/v1/models", "kat-awq") {
		t.Fatal("chat probe must report a backend that 400s every completion as DOWN")
	}
}

func TestProbe_ChatSends1TokenForServedModel(t *testing.T) {
	var got map[string]any
	srv := healthyVLLM(t, &got)
	defer srv.Close()
	b := newBackend(srv.URL)
	if !b.probeOnce(&http.Client{Timeout: 2 * time.Second}, "/v1/models", "kat-awq") {
		t.Fatal("chat probe against a healthy backend must be UP")
	}
	if got["model"] != "kat-awq" {
		t.Errorf("probe model = %v, want kat-awq (must match --served-model-name)", got["model"])
	}
	if got["max_tokens"] != float64(1) {
		t.Errorf("probe max_tokens = %v, want 1 (probe must be cheap)", got["max_tokens"])
	}
}

func TestHealthCheck_ChatProbeFlipsBackendDown(t *testing.T) {
	srv := brokenVLLM(t)
	defer srv.Close()
	b := newBackend(srv.URL) // starts healthy=true
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.healthCheck(ctx, probeOpts{healthPath: "/v1/models", probeModel: "kat-awq", timeout: 2 * time.Second})
	// 2 consecutive failures at a 3s cadence -> DOWN by ~4s; allow slack.
	deadline := time.Now().Add(9 * time.Second)
	for time.Now().Before(deadline) {
		if !b.healthy.Load() {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatal("backend that 400s chat stayed UP under the chat probe")
}

func TestLoadConfig_BadJSONReturnsErrorNotExit(t *testing.T) {
	p := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(p, []byte(`{"backends": [`), 0o600)
	if _, err := loadConfig(p); err == nil {
		t.Fatal("malformed config must return an error (previously log.Fatalf'd)")
	}
}

func TestLoadConfig_ProbeDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"backends":["http://127.0.0.1:1"],"probe_model":"kat-awq"}`), 0o600)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.ProbeModel != "kat-awq" || cfg.ProbeTimeoutSec != 10 {
		t.Errorf("probe config = %+v, want model kat-awq timeout 10", cfg)
	}
}

func TestLoadConfig_StallDefaults(t *testing.T) {
	p := filepath.Join(t.TempDir(), "c.json")
	os.WriteFile(p, []byte(`{"backends":["http://127.0.0.1:1"],"probe_model":"kat-awq"}`), 0o600)
	cfg, err := loadConfig(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.StallSec != 120 || cfg.StallMinInflight != 1 || cfg.stallThreshold() != 120*time.Second {
		t.Errorf("stall config absent -> %+v, want stall_sec 120 / stall_min_inflight 1", cfg)
	}

	os.WriteFile(p, []byte(`{"backends":["http://127.0.0.1:1"],"stall_sec":30,"stall_min_inflight":3}`), 0o600)
	if cfg, _ = loadConfig(p); cfg.StallSec != 30 || cfg.StallMinInflight != 3 || cfg.stallThreshold() != 30*time.Second {
		t.Errorf("explicit stall config -> %+v, want 30 / 3", cfg)
	}

	os.WriteFile(p, []byte(`{"backends":["http://127.0.0.1:1"],"stall_sec":-1}`), 0o600)
	if cfg, _ = loadConfig(p); cfg.stallThreshold() != 0 {
		t.Errorf("stall_sec -1 must disable detection, got threshold %s", cfg.stallThreshold())
	}
}

// TestProbe_200WithoutCompletionBodyIsFailure: a replica wedged after
// writing its headers (or a proxy in front of it answering 200 with nothing)
// returns 200 and no completion. That is not UP.
func TestProbe_200WithoutCompletionBodyIsFailure(t *testing.T) {
	var body atomic.Value
	body.Store("")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		io.WriteString(w, body.Load().(string))
	}))
	defer srv.Close()
	b := newBackend(srv.URL)
	client := &http.Client{Timeout: 2 * time.Second}

	for _, tc := range []struct {
		body string
		want bool
	}{
		{"", false},
		{"{}", false},
		{`{"choices":[]}`, false},
		{`{"choices":`, false},
		{`<html>502</html>`, false},
		{`{"choices":[{"message":{"content":""}}]}`, true},
		{`{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"x"}}],"usage":{"total_tokens":8}}`, true},
	} {
		body.Store(tc.body)
		if got := b.probeOnce(client, "/v1/models", "kat-awq"); got != tc.want {
			t.Errorf("chat probe with 200 body %q = %v, want %v", tc.body, got, tc.want)
		}
	}
	// The cheap GET probe is unchanged: 200 with an empty body is still UP.
	body.Store("")
	if !b.probeOnce(client, "/v1/models", "") {
		t.Error("GET probe must still accept a bare 200")
	}
}

// hungVLLM is a replica that LISTENS but hangs: every real chat request
// blocks until release is closed. Its probe behaviour is switchable: with
// flaky set, every second probe hangs (so the prober times out) and the
// others answer a valid completion -- the pattern the plain 2-consecutive-
// failures rule can never catch.
type hungVLLM struct {
	srv         *httptest.Server
	release     chan struct{}
	releaseOnce sync.Once
	probes      atomic.Int64
	flaky       atomic.Bool
}

func newHungVLLM(t *testing.T) *hungVLLM {
	t.Helper()
	h := &hungVLLM{release: make(chan struct{})}
	h.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/chat/completions" {
			w.WriteHeader(404)
			return
		}
		body, _ := io.ReadAll(r.Body)
		if bytes.Contains(body, []byte(`"content":"ping"`)) { // oaicalb's probe
			if n := h.probes.Add(1); h.flaky.Load() && n%2 == 0 {
				<-r.Context().Done() // hang until the prober gives up
				return
			}
			w.WriteHeader(200)
			io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
			return
		}
		select {
		case <-h.release:
			w.WriteHeader(200)
			io.WriteString(w, `{"choices":[{"message":{"content":"late"}}]}`)
		case <-r.Context().Done():
		}
	}))
	t.Cleanup(func() { h.unhang(); h.srv.Close() })
	return h
}

func (h *hungVLLM) unhang() { h.releaseOnce.Do(func() { close(h.release) }) }

// sessionKeyFor finds an X-Session-Id that hashes onto want (all healthy).
func sessionKeyFor(t *testing.T, bs []*backend, want *backend) string {
	t.Helper()
	for i := 0; i < 1000; i++ {
		k := "session-" + itoa(int64(i))
		if hashPick(bs, k) == want {
			return k
		}
	}
	t.Fatal("no session key hashes onto the wanted backend")
	return ""
}

func chatVia(t *testing.T, lb, key string, timeout time.Duration) (*http.Response, error) {
	t.Helper()
	req, _ := http.NewRequest("POST", lb+"/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"real work"}],"max_tokens":64}`))
	req.Header.Set("X-Session-Id", key)
	return (&http.Client{Timeout: timeout}).Do(req)
}

func waitFor(t *testing.T, d time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(5 * time.Millisecond)
	}
	return cond()
}

// TestStall_HungBackendIsDrainedThenReadmitted is the hung-replica scenario
// end to end: two backends behind the session-hash listener, one healthy,
// one that accepts and hangs while its probe flaps. A session pinned to the
// hung one must move to the healthy one within the stall threshold (plus a
// probe cycle), and come back once the hung replica answers again.
func TestStall_HungBackendIsDrainedThenReadmitted(t *testing.T) {
	hung := newHungVLLM(t)
	hung.flaky.Store(true)
	var got map[string]any
	healthy := healthyVLLM(t, &got)
	defer healthy.Close()

	hb, ok := newBackend(hung.srv.URL), newBackend(healthy.URL)
	bs := []*backend{hb, ok}
	opts := probeOpts{
		healthPath: "/v1/models", probeModel: "kat-awq",
		timeout: 200 * time.Millisecond, interval: 50 * time.Millisecond,
		stall: 400 * time.Millisecond, stallMin: 1,
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go hb.healthCheck(ctx, opts)
	go ok.healthCheck(ctx, opts)

	lb := httptest.NewServer(sessionHandler(bs))
	defer lb.Close()
	key := sessionKeyFor(t, bs, hb)

	// 1. A flapping probe with nothing in flight is NOT enough: the existing
	//    2-consecutive-failures rule stays byte-compatible.
	if !waitFor(t, 5*time.Second, func() bool { return hung.probes.Load() >= 4 }) {
		t.Fatal("prober never reached the hung backend")
	}
	if !hb.healthy.Load() {
		t.Fatal("flapping probe with no stalled requests must not mark the backend DOWN")
	}

	// 2. A real request pinned to it goes in and hangs.
	fired := time.Now()
	type result struct {
		resp *http.Response
		err  error
	}
	done := make(chan result, 1)
	go func() {
		resp, err := chatVia(t, lb.URL, key, 20*time.Second)
		done <- result{resp, err}
	}()
	if !waitFor(t, 2*time.Second, func() bool { return atomic.LoadInt64(&hb.inflight) == 1 }) {
		t.Fatal("pinned request did not land on the hung backend")
	}
	if !hb.healthy.Load() {
		t.Fatal("backend marked DOWN before the request was old enough to count as stalled")
	}

	// 3. Within stall + a probe cycle (timeout + interval) the hung backend
	//    is DOWN, and the same session is now served by the healthy one.
	if !waitFor(t, opts.stall+3*time.Second, func() bool { return !hb.healthy.Load() }) {
		t.Fatalf("hung backend never marked DOWN (inflight=%d)", atomic.LoadInt64(&hb.inflight))
	}
	downAfter := time.Since(fired)
	if downAfter > opts.stall+opts.timeout+opts.interval+time.Second {
		t.Errorf("DOWN after %s, want within stall %s + one probe cycle", downAfter, opts.stall)
	}
	t.Logf("DOWN after %s (stall threshold %s)", downAfter.Round(time.Millisecond), opts.stall)
	resp, err := chatVia(t, lb.URL, key, 2*time.Second)
	if err != nil {
		t.Fatalf("request for the pinned session after failover: %v", err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if got := resp.Header.Get("X-Katlb-Backend"); got != ok.url.String() {
		t.Fatalf("session still routed to %s after failover, want healthy %s", got, ok.url)
	}

	// 4. Replica recovers: the stuck generation completes and probes pass.
	//    It is re-admitted and the session goes back to it.
	hung.flaky.Store(false)
	hung.unhang()
	r := <-done
	if r.err != nil || r.resp.StatusCode != 200 || r.resp.Header.Get("X-Katlb-Backend") != hb.url.String() {
		t.Fatalf("released request: err=%v resp=%+v", r.err, r.resp)
	}
	r.resp.Body.Close()
	if !waitFor(t, 3*time.Second, func() bool {
		if !hb.healthy.Load() {
			return false
		}
		resp, err := chatVia(t, lb.URL, key, 2*time.Second)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.Header.Get("X-Katlb-Backend") == hb.url.String()
	}) {
		t.Fatalf("recovered backend not re-admitted (healthy=%v)", hb.healthy.Load())
	}
}

// TestStall_OldInflightWithPassingProbeStaysUp: a long legitimate
// generation on a replica whose probe passes is not a hang.
func TestStall_OldInflightWithPassingProbeStaysUp(t *testing.T) {
	var got map[string]any
	srv := healthyVLLM(t, &got)
	defer srv.Close()
	b := newBackend(srv.URL)
	id := b.begin() // a request that has been running "forever"
	defer b.end(id)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.healthCheck(ctx, probeOpts{
		healthPath: "/v1/models", probeModel: "kat-awq",
		timeout: time.Second, interval: 20 * time.Millisecond, stall: 10 * time.Millisecond,
	})
	time.Sleep(300 * time.Millisecond) // many probe cycles, all with a stalled request
	if !b.healthy.Load() {
		t.Fatal("stalled request with a passing probe must not mark the backend DOWN")
	}
	if b.oldestInflight() < 300*time.Millisecond {
		t.Errorf("oldest in-flight = %s, want >= 300ms", b.oldestInflight())
	}
}

// TestStall_DisabledFallsBackToTwoFailures: stall 0 (stall_sec -1) keeps the
// old rule exactly -- one failure with stalled requests is not DOWN.
func TestStall_DisabledFallsBackToTwoFailures(t *testing.T) {
	hung := newHungVLLM(t)
	hung.flaky.Store(true)
	b := newBackend(hung.srv.URL)
	id := b.begin()
	defer b.end(id)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go b.healthCheck(ctx, probeOpts{
		healthPath: "/v1/models", probeModel: "kat-awq",
		timeout: 100 * time.Millisecond, interval: 20 * time.Millisecond, stall: 0,
	})
	waitFor(t, 3*time.Second, func() bool { return hung.probes.Load() >= 6 })
	if !b.healthy.Load() {
		t.Fatal("with stall detection disabled a flapping probe must not mark the backend DOWN")
	}
}
