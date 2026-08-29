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

// fakeMeterHub is a minimal /ingest recorder for the metering tests below.
func fakeMeterHub(t *testing.T) (*httptest.Server, *[]usageRecord) {
	t.Helper()
	var mu sync.Mutex
	var records []usageRecord
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var rec usageRecord
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &rec)
		mu.Lock()
		records = append(records, rec)
		mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, &records
}

// vLLMWithUsage is a fake backend that returns a real usage object, so the
// metering tests can verify token counts actually get parsed and reported.
func vLLMWithUsage(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(200)
		case "/v1/chat/completions":
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"choices":[{"message":{"content":"x"}}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`)
		default:
			w.WriteHeader(404)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func waitForRecords(t *testing.T, records *[]usageRecord, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if len(*records) >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("meterhub never received %d record(s), got %d", n, len(*records))
}

func TestMeter_DisabledByDefault(t *testing.T) {
	metered = nil // explicit: no test before this one may have left it set
	srv := vLLMWithUsage(t)
	b := newBackend(srv.URL)
	h := serveWith(newStaticPool([]*backend{b}), func(bs []*backend, _ int) *backend { return bs[0] })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (metering disabled must never break a request)", w.Code)
	}
}

func TestMeter_ReportsRequestWithoutXOaicaMeteredHeader(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })

	srv := vLLMWithUsage(t)
	b := newBackend(srv.URL)
	h := serveWith(newStaticPool([]*backend{b}), func(bs []*backend, _ int) *backend { return bs[0] })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`))
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	waitForRecords(t, records, 1)
	rec := (*records)[0]
	if rec.PromptTokens != 11 || rec.CompletionTokens != 5 {
		t.Errorf("tokens = (%d,%d), want (11,5)", rec.PromptTokens, rec.CompletionTokens)
	}
	if rec.KeyLabel != "direct" {
		t.Errorf("key_label = %q, want \"direct\" (no Authorization header on the request)", rec.KeyLabel)
	}
	if rec.Region != "test-region" || rec.Model != "kat-awq" || rec.Status != 200 {
		t.Errorf("record = %+v, unexpected region/model/status", rec)
	}
}

// TestMeter_SkipsRequestAlreadyMeteredByGateway is the double-counting
// regression: a request the gateway already billed (and forwards through
// gatekeeper to oaicalb, carrying X-Oaica-Metered) must NOT be reported a
// second time here, or every gateway-routed completion would be counted
// twice between gateway's own ledger/meterhub report and this one.
func TestMeter_SkipsRequestAlreadyMeteredByGateway(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })

	srv := vLLMWithUsage(t)
	b := newBackend(srv.URL)
	h := serveWith(newStaticPool([]*backend{b}), func(bs []*backend, _ int) *backend { return bs[0] })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("X-Oaica-Metered", "1")
	w := httptest.NewRecorder()
	h(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	time.Sleep(150 * time.Millisecond) // give an (incorrect) report a chance to land
	if len(*records) != 0 {
		t.Fatalf("meterhub got %d record(s) for an already-metered request, want 0 (double-counting)", len(*records))
	}
}

func TestMeter_AuthenticatedRequestLabelledDifferently(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })

	srv := vLLMWithUsage(t)
	b := newBackend(srv.URL)
	h := serveWith(newStaticPool([]*backend{b}), func(bs []*backend, _ int) *backend { return bs[0] })

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-something")
	w := httptest.NewRecorder()
	h(w, req)

	waitForRecords(t, records, 1)
	if (*records)[0].KeyLabel != "direct:authenticated" {
		t.Errorf("key_label = %q, want \"direct:authenticated\"", (*records)[0].KeyLabel)
	}
}

func TestMeter_NonCompletionPathNeverReported(t *testing.T) {
	meterSrv, records := fakeMeterHub(t)
	metered = newMeterHub(meterSrv.URL, "tok", "test-region")
	t.Cleanup(func() { metered = nil })

	srv := vLLMWithUsage(t)
	b := newBackend(srv.URL)
	h := serveWith(newStaticPool([]*backend{b}), func(bs []*backend, _ int) *backend { return bs[0] })

	req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
	w := httptest.NewRecorder()
	h(w, req)

	time.Sleep(150 * time.Millisecond)
	if len(*records) != 0 {
		t.Fatalf("meterhub got %d record(s) for GET /v1/models, want 0", len(*records))
	}
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
		if hashPick(bs, k, 0, 0) == want {
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

	lb := httptest.NewServer(sessionHandler(newStaticPool(bs), 0))
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

// setInflight forces a backend's inflight counter to n, for overflow tests
// that need a specific load skew without spinning up n real requests.
func setInflight(b *backend, n int64) {
	atomic.StoreInt64(&b.inflight, n)
}

func TestAverageHealthyLoad_IgnoresUnhealthyAndFloorsAtOne(t *testing.T) {
	a := newBackend("http://127.0.0.1:1")
	b := newBackend("http://127.0.0.1:2")
	c := newBackend("http://127.0.0.1:3")
	c.healthy.Store(false) // excluded from the average entirely
	setInflight(a, 0)
	setInflight(b, 0)
	setInflight(c, 100) // must not affect the average — c is unhealthy

	if got := averageHealthyLoad([]*backend{a, b, c}); got != 1 {
		t.Errorf("averageHealthyLoad = %v, want 1 (floor, both healthy backends at 0 load)", got)
	}

	setInflight(a, 2)
	setInflight(b, 6)
	if got := averageHealthyLoad([]*backend{a, b, c}); got != 4 {
		t.Errorf("averageHealthyLoad = %v, want 4 ((2+6)/2, c excluded)", got)
	}
}

func TestAverageHealthyLoad_AllUnhealthyReturnsFloor(t *testing.T) {
	a := newBackend("http://127.0.0.1:1")
	a.healthy.Store(false)
	if got := averageHealthyLoad([]*backend{a}); got != 1 {
		t.Errorf("averageHealthyLoad with no healthy backends = %v, want 1 (floor)", got)
	}
}

func TestHashPick_OverflowDisabledStaysSticky(t *testing.T) {
	a := newBackend("http://127.0.0.1:1")
	b := newBackend("http://127.0.0.1:2")
	bs := []*backend{a, b}
	key := sessionKeyFor(t, bs, a)
	setInflight(a, 1000) // wildly overloaded
	setInflight(b, 0)

	// overflowFactor=0 (disabled): must stay pinned regardless of load skew
	// — byte-identical to pre-2026-08-29 behavior.
	if got := hashPick(bs, key, 0, 0); got != a {
		t.Error("overflowFactor=0 must never reroute away from the hashed backend")
	}
}

func TestHashPick_OverflowReroutesWhenHashedBackendIsHot(t *testing.T) {
	a := newBackend("http://127.0.0.1:1")
	b := newBackend("http://127.0.0.1:2")
	bs := []*backend{a, b}
	key := sessionKeyFor(t, bs, a)

	// a: 10 inflight, b: 0 inflight -> average of healthy backends is 5;
	// a's load (10) is 2x the average, so overflowFactor=1.5 must trip.
	setInflight(a, 10)
	setInflight(b, 0)
	if got := hashPick(bs, key, 1.5, 0); got != b {
		t.Errorf("expected reroute to the least-loaded backend (b) when the hashed one (a) is 2x average load with overflowFactor=1.5")
	}
}

func TestHashPick_OverflowStaysPinnedUnderThreshold(t *testing.T) {
	a := newBackend("http://127.0.0.1:1")
	b := newBackend("http://127.0.0.1:2")
	bs := []*backend{a, b}
	key := sessionKeyFor(t, bs, a)

	// a: 3 inflight, b: 1 inflight -> average 2; a's load (3) is only
	// 1.5x average, under a 2.0 threshold -> must stay pinned.
	setInflight(a, 3)
	setInflight(b, 1)
	if got := hashPick(bs, key, 2.0, 0); got != a {
		t.Error("expected the session to stay pinned to a when its load is under the overflow threshold")
	}
}

func TestHashPick_OverflowNextRequestRejoinsAfterDrain(t *testing.T) {
	a := newBackend("http://127.0.0.1:1")
	b := newBackend("http://127.0.0.1:2")
	bs := []*backend{a, b}
	key := sessionKeyFor(t, bs, a)

	setInflight(a, 10)
	setInflight(b, 0)
	if got := hashPick(bs, key, 1.5, 0); got != b {
		t.Fatal("expected reroute while a is hot")
	}

	// a drains back to a normal load — the SAME session's next request
	// (no separate "un-reroute" state) must rejoin its hashed backend.
	setInflight(a, 1)
	setInflight(b, 1)
	if got := hashPick(bs, key, 1.5, 0); got != a {
		t.Error("expected the session to rejoin its hashed backend once load drained — overflow reroute must be per-request, not sticky")
	}
}

func TestHashPick_OverflowNeverPicksAnUnhealthyAlternate(t *testing.T) {
	a := newBackend("http://127.0.0.1:1")
	b := newBackend("http://127.0.0.1:2")
	bs := []*backend{a, b}
	key := sessionKeyFor(t, bs, a)

	b.healthy.Store(false) // the only "alternate" is unhealthy
	setInflight(a, 10)
	if got := hashPick(bs, key, 1.5, 0); got != a {
		t.Error("expected to stay on the hashed backend (a) when the only alternate (b) is unhealthy, even if a is hot — better a slow answer than none")
	}
}

// -- context-size tiering (2026-08-29) --

func TestContextEligible_UnboundedBackendsUnaffectedByEstimate(t *testing.T) {
	a := newBackend("http://127.0.0.1:1")
	b := newBackend("http://127.0.0.1:2")
	bs := []*backend{a, b}
	got := contextEligible(bs, 999999)
	if len(got) != 2 {
		t.Errorf("expected both unbounded backends eligible regardless of estimate, got %d", len(got))
	}
}

func TestContextEligible_FiltersOutTooSmallBackend(t *testing.T) {
	small := newBackendWithContext("http://127.0.0.1:1", 10000)
	big := newBackendWithContext("http://127.0.0.1:2", 0) // unbounded
	bs := []*backend{small, big}
	got := contextEligible(bs, 50000)
	if len(got) != 1 || got[0] != big {
		t.Errorf("expected only the unbounded backend eligible for a 50k-token request, got %v", got)
	}
}

func TestContextEligible_ZeroEstimateSkipsFilteringEntirely(t *testing.T) {
	small := newBackendWithContext("http://127.0.0.1:1", 10000)
	bs := []*backend{small}
	got := contextEligible(bs, 0)
	if len(got) != 1 {
		t.Error("estTokens=0 (unknown / GET request) must never filter anything out")
	}
}

func TestContextEligible_NeverReturnsEmptyEvenWhenNothingFits(t *testing.T) {
	small := newBackendWithContext("http://127.0.0.1:1", 1000)
	bs := []*backend{small}
	got := contextEligible(bs, 999999)
	if len(got) != 1 {
		t.Error("no backend can fit the request, but oaicalb cannot reject outright -- must fall back to the full set, not an empty one")
	}
}

func TestLeastConnPick_RoutesAroundTooSmallBackend(t *testing.T) {
	small := newBackendWithContext("http://127.0.0.1:1", 10000)
	big := newBackendWithContext("http://127.0.0.1:2", 0)
	bs := []*backend{small, big}
	for i := 0; i < 10; i++ {
		if got := leastConnPick(bs, 50000); got != big {
			t.Fatal("a 50k-token request must never land on a backend declaring a 10k max_context")
		}
	}
}

func TestHashPick_RatchetsUpWhenHashedBackendTooSmall(t *testing.T) {
	small := newBackendWithContext("http://127.0.0.1:1", 10000)
	big := newBackendWithContext("http://127.0.0.1:2", 0)
	bs := []*backend{small, big}
	key := sessionKeyFor(t, bs, small)

	// small request: stays on the hashed (small) backend
	if got := hashPick(bs, key, 0, 5000); got != small {
		t.Error("a request that fits should stay on its hashed backend")
	}
	// same session grows past small's max_context: ratchets to big
	if got := hashPick(bs, key, 0, 50000); got != big {
		t.Error("a request too big for the hashed backend must degrade to a backend that fits, not stay pinned")
	}
	// shrinks back down: deliberately does NOT ratchet back (see hashPick doc)
	if got := hashPick(bs, key, 0, 5000); got != small {
		t.Error("a subsequent small request from the same session should rejoin its original hashed backend")
	}
}

func TestServeWith_NoContextLimitsNeverReadsBodyTwice(t *testing.T) {
	// Regression guard: when no backend declares max_context, serveWith must
	// not touch the body at all, so a handler downstream sees it exactly
	// once, unmodified -- proving the zero-config path is untouched.
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	b := newBackend(srv.URL) // maxContext=0, unbounded
	h := serveWith(newStaticPool([]*backend{b}), func(bs []*backend, _ int) *backend { return bs[0] })

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"probe":"body"}`))
	w := httptest.NewRecorder()
	h(w, req)

	if gotBody != `{"probe":"body"}` {
		t.Errorf("expected body to reach the backend unmodified, got %q", gotBody)
	}
}

func TestServeWith_ContextLimitsRestoreBodyForProxying(t *testing.T) {
	// With context limits active, serveWith reads the body to estimate size
	// -- it must still restore it so the real proxied request carries the
	// full original body, not an empty one.
	var gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		w.WriteHeader(200)
	}))
	defer srv.Close()
	b := newBackendWithContext(srv.URL, 999999) // declares a max_context -> triggers body read
	h := serveWith(newStaticPool([]*backend{b}), func(bs []*backend, _ int) *backend { return bs[0] })

	body := `{"model":"x","messages":[{"role":"user","content":"hello"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(body))
	w := httptest.NewRecorder()
	h(w, req)

	if gotBody != body {
		t.Errorf("expected the full original body to reach the backend after the estimate read, got %q", gotBody)
	}
}

// -- backend hot reload (SIGHUP) --

func reloadTestOpts() probeOpts {
	return probeOpts{interval: 10 * time.Millisecond, timeout: time.Second}
}

func TestPoolReload_KeepsUnchangedAddsNewDropsRemoved(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer a.Close()
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer b.Close()
	c := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer c.Close()

	p := newBackendPool(reloadTestOpts())
	if added, removed, kept := p.reload(lbConfig{Backends: []string{a.URL, b.URL}}); added != 2 || removed != 0 || kept != 0 {
		t.Fatalf("initial reload = (%d,%d,%d), want (2,0,0)", added, removed, kept)
	}
	oldB := p.current().bs[1]
	oldA := p.current().bs[0]
	// mark A healthy so we can observe the drop flipping it
	oldA.healthy.Store(true)

	added, removed, kept := p.reload(lbConfig{Backends: []string{b.URL, c.URL}})
	if added != 1 || removed != 1 || kept != 1 {
		t.Fatalf("reload = (%d,%d,%d), want (1,1,1)", added, removed, kept)
	}
	snap := p.current()
	if len(snap.bs) != 2 || snap.bs[0] != oldB || snap.bs[1].url.String() != c.URL {
		t.Fatalf("snapshot after reload = %v, want [same B object, new C]", snap.bs)
	}
	if oldA.healthy.Load() {
		t.Error("removed backend must be marked unhealthy so no stale reference can pick it")
	}
	if snap.bs[0] != oldB {
		t.Error("unchanged backend must keep its object (health/in-flight state preserved)")
	}
	if snap.hasContextLimits {
		t.Error("plain backends must not enable context tiering")
	}
}

func TestPoolReload_ChangedMaxContextReplacesBackend(t *testing.T) {
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer a.Close()
	p := newBackendPool(reloadTestOpts())
	p.reload(lbConfig{BackendConfigs: []backendConfigEntry{{URL: a.URL, MaxContext: 1000}}})
	first := p.current().bs[0]
	added, removed, kept := p.reload(lbConfig{BackendConfigs: []backendConfigEntry{{URL: a.URL, MaxContext: 5000}}})
	if added != 1 || removed != 1 || kept != 0 {
		t.Fatalf("reload with changed max_context = (%d,%d,%d), want (1,1,0)", added, removed, kept)
	}
	if got := p.current().bs[0]; got == first || got.maxContext != 5000 {
		t.Fatalf("expected a fresh backend with max_context 5000, got same=%v max=%d", got == first, got.maxContext)
	}
	if !p.current().hasContextLimits {
		t.Error("backend_configs with max_context must enable context tiering")
	}
}

func TestPoolReload_RemovedBackendHealthLoopStops(t *testing.T) {
	var probes atomic.Int64
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { probes.Add(1); w.WriteHeader(200) }))
	defer a.Close()
	p := newBackendPool(reloadTestOpts())
	p.reload(lbConfig{Backends: []string{a.URL}})
	deadline := time.Now().Add(time.Second)
	for probes.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if probes.Load() < 2 {
		t.Fatal("health loop never probed the backend")
	}
	p.reload(lbConfig{Backends: []string{}}) // drop it
	time.Sleep(50 * time.Millisecond)        // let an in-flight probe finish
	n := probes.Load()
	time.Sleep(100 * time.Millisecond) // ten intervals: a live loop would add ~10 probes
	if probes.Load() != n {
		t.Fatalf("health loop kept probing after the backend was removed (%d -> %d)", n, probes.Load())
	}
}

func TestServeWith_RoutesToBackendAddedByReload(t *testing.T) {
	// Count only requests routed by the handler under test (marked with
	// X-Test); the pool's health probes hit the same servers and must not
	// be counted.
	var hitA, hitB atomic.Int64
	counted := func(c *atomic.Int64) http.HandlerFunc {
		return func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("X-Test") == "1" {
				c.Add(1)
			}
			w.WriteHeader(200)
		}
	}
	a := httptest.NewServer(counted(&hitA))
	defer a.Close()
	b := httptest.NewServer(counted(&hitB))
	defer b.Close()
	routed := func() *http.Request {
		r := httptest.NewRequest("GET", "/v1/models", nil)
		r.Header.Set("X-Test", "1")
		return r
	}

	p := newBackendPool(reloadTestOpts())
	p.reload(lbConfig{Backends: []string{a.URL}})
	h := serveWith(p, leastConnPick)
	// Requests are proxied to whichever backend the live snapshot holds;
	// force "healthy" so the test does not depend on probe timing.
	p.current().bs[0].healthy.Store(true)
	h(httptest.NewRecorder(), routed())
	if hitA.Load() != 1 {
		t.Fatalf("expected A to serve the first request, hits A=%d B=%d", hitA.Load(), hitB.Load())
	}
	p.reload(lbConfig{Backends: []string{b.URL}}) // A out, B in -- no restart, same handler
	p.current().bs[0].healthy.Store(true)
	h(httptest.NewRecorder(), routed())
	if hitB.Load() != 1 || hitA.Load() != 1 {
		t.Fatalf("expected B to serve after reload, hits A=%d B=%d", hitA.Load(), hitB.Load())
	}
}

func TestLoadConfig_BadFileRejected(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "oaicalb.json")
	os.WriteFile(path, []byte(`{"backends": [`), 0o644)
	if _, err := loadConfig(path); err == nil {
		t.Fatal("expected an error for a truncated config -- a SIGHUP reload must keep the old backends on such input")
	}
}
