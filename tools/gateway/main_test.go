package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
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

func keyHash(k string) string {
	s := sha256.Sum256([]byte(k))
	return hex.EncodeToString(s[:])
}

// fakeUpstream mimics vLLM: records the body it received and returns either a
// non-streamed completion with usage, or an SSE stream whose LAST chunk
// carries usage (only when stream_options.include_usage was requested).
func fakeUpstream(t *testing.T, got *map[string]any) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "" {
			t.Errorf("caller's Authorization must NOT be forwarded upstream, got %q", r.Header.Get("Authorization"))
		}
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		*got = body
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"4"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
			return
		}
		so, _ := body["stream_options"].(map[string]any)
		inc, _ := so["include_usage"].(bool)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"4\"}}]}\n\n")
		f.Flush()
		if inc {
			io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":11,\"completion_tokens\":5}}\n\n")
			f.Flush()
		}
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
}

func newTestGateway(t *testing.T, upstream string) (*gateway, string) {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream,
		ListenAddr:   ":0",
		LedgerPath:   ledger,
		APIKeys:      []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{
			ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g, ledger
}

func mux(g *gateway) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/health", g.healthHandler)
	m.HandleFunc("/privacy", legalHandler("PRIVACY.md"))
	m.HandleFunc("/terms", legalHandler("TERMS.md"))
	m.HandleFunc("/status", legalHandler("STATUS.md"))
	m.HandleFunc("/models", g.modelsHandler)
	m.HandleFunc("/v1/models", g.modelsHandler)
	m.HandleFunc("/v1/chat/completions", g.completionHandler)
	m.HandleFunc("/v1/completions", g.completionHandler)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) { writeErr(w, 404, "not_found", "unknown route") })
	return m
}

// waitLedger polls until the ledger holds >= n entries. The handler meters
// AFTER it has already streamed the full response, so a client can observe
// the reply before the ledger line lands; production readers tail the file,
// so this ordering is fine, but a test must not assert instantly.
func waitLedger(t *testing.T, path string, n int) []ledgerEntry {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		e := readLedger(t, path)
		if len(e) >= n || time.Now().After(deadline) {
			return e
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func readLedger(t *testing.T, path string) []ledgerEntry {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		t.Fatalf("open ledger: %v", err)
	}
	defer f.Close()
	var out []ledgerEntry
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var e ledgerEntry
		if err := json.Unmarshal(sc.Bytes(), &e); err != nil {
			t.Fatalf("bad ledger line %q: %v", sc.Text(), err)
		}
		out = append(out, e)
	}
	return out
}

func TestAuth_OldKeyRejected_NewKeyAccepted_ConstantTime(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	for _, tc := range []struct {
		key  string
		want int
	}{
		{"sk-oaica-old-rotated-key", 401}, // a retired/guessable key must be rejected
		{"", 401},
		{"sk-new", 200},
	} {
		// completions are the authenticated surface (/models is public)
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "application/json")
		if tc.key != "" {
			req.Header.Set("Authorization", "Bearer "+tc.key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("key %q: got %d want %d", tc.key, resp.StatusCode, tc.want)
		}
	}
}

// /models is intentionally public: served from config, no upstream call,
// nothing in it that the OpenRouter listing does not already publish. A
// poller that sends no key must still get the roster.
func TestModels_PublicWithoutKey(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	for _, p := range []string{"/models", "/v1/models"} {
		resp, err := http.Get(srv.URL + p)
		if err != nil {
			t.Fatal(err)
		}
		var doc struct {
			Data []map[string]any `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&doc)
		resp.Body.Close()
		if resp.StatusCode != 200 || len(doc.Data) != 1 || doc.Data[0]["id"] != "kat-awq" {
			t.Fatalf("%s without key: %d, %v", p, resp.StatusCode, doc.Data)
		}
	}
	if got != nil {
		t.Fatal("/models must never call upstream")
	}
}

func TestModels_ExposesPricingAndContext(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	req, _ := http.NewRequest("GET", srv.URL+"/models", nil)
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&doc)
	if len(doc.Data) != 1 {
		t.Fatalf("want 1 model, got %d", len(doc.Data))
	}
	m := doc.Data[0]
	if m["id"] != "kat-awq" || m["context_length"] != float64(262144) || m["max_completion_tokens"] != float64(32768) {
		t.Errorf("missing context metadata: %v", m)
	}
	p, _ := m["pricing"].(map[string]any)
	if p["prompt"] != "0.00000005" || p["completion"] != "0.00000012" {
		t.Errorf("missing/incorrect pricing: %v", m["pricing"])
	}
	if _, leaked := m["max_model_len"]; leaked {
		t.Errorf("vLLM-ism max_model_len must not be exposed")
	}
}

func TestCompletion_RewritesModel_StripsAuth_MetersNonStream(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, ledger := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	body := `{"model":"oaica/kat-awq","messages":[{"role":"user","content":"2+2"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-new")
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	if resp.Header.Get("X-Request-Id") == "" {
		t.Error("X-Request-Id missing")
	}
	if got["model"] != "kat-awq-served" {
		t.Errorf("upstream model rewrite: got %v want kat-awq-served (caller sent oaica/kat-awq)", got["model"])
	}
	entries := waitLedger(t, ledger, 1)
	if len(entries) != 1 {
		t.Fatalf("want 1 ledger entry, got %d", len(entries))
	}
	e := entries[0]
	if e.KeyLabel != "openrouter" || e.Model != "kat-awq" || e.UpstreamModel != "kat-awq-served" {
		t.Errorf("ledger attribution wrong: %+v", e)
	}
	if !e.UsageSeen || e.PromptTokens != 7 || e.CompletionTokens != 3 {
		t.Errorf("non-stream usage not metered: %+v", e)
	}
	if e.Stream {
		t.Errorf("stream flag wrong: %+v", e)
	}
}

func TestCompletion_Streaming_InjectsIncludeUsage_AndMetersFinalChunk(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, ledger := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	// Caller does NOT ask for usage -- the gateway must add it, otherwise
	// this request meters as 0 output tokens and we get paid nothing.
	body := `{"model":"kat-awq","stream":true,"messages":[{"role":"user","content":"2+2"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if !strings.Contains(string(raw), "data: [DONE]") {
		t.Fatalf("stream did not pass through intact:\n%s", raw)
	}
	so, _ := got["stream_options"].(map[string]any)
	if inc, _ := so["include_usage"].(bool); !inc {
		t.Fatalf("stream_options.include_usage was NOT injected; upstream saw %v", got["stream_options"])
	}
	e := waitLedger(t, ledger, 1)[0]
	if !e.Stream || !e.UsageSeen || e.PromptTokens != 11 || e.CompletionTokens != 5 {
		t.Errorf("streaming usage not metered from final chunk: %+v", e)
	}
}

func TestRouteAllowlist_And_ErrorShapes(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	do := func(method, path, key string) (int, map[string]any) {
		req, _ := http.NewRequest(method, srv.URL+path, strings.NewReader(`{"model":"kat-awq"}`))
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var doc map[string]any
		json.NewDecoder(resp.Body).Decode(&doc)
		return resp.StatusCode, doc
	}
	// vLLM internals must be unreachable even with a valid key.
	for _, p := range []string{"/metrics", "/tokenize", "/docs", "/v1/embeddings", "/reset_prefix_cache"} {
		if code, _ := do("GET", p, "sk-new"); code != 404 {
			t.Errorf("%s: got %d, want 404 (route allowlist)", p, code)
		}
	}
	if code, _ := do("GET", "/v1/chat/completions", "sk-new"); code != 405 {
		t.Errorf("GET on completions: got %d want 405", code)
	}
	code, doc := do("POST", "/v1/chat/completions", "")
	if code != 401 {
		t.Errorf("unauth completion: got %d want 401", code)
	}
	if errObj, _ := doc["error"].(map[string]any); errObj == nil || errObj["message"] == nil {
		t.Errorf("401 must be OpenAI-shaped {error:{message}}, got %v", doc)
	}
	code, doc = do("POST", "/v1/chat/completions", "sk-new")
	_ = code
	// unknown model
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"nope"}`))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 404 {
		t.Errorf("unknown model: got %d want 404", resp.StatusCode)
	}
}

func TestReload_BadConfigKeepsPrevious(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	prev := g.cfg.UpstreamAddr
	bad := filepath.Join(t.TempDir(), "bad.json")
	os.WriteFile(bad, []byte(`{"api_keys":[],"models":[]}`), 0o600)
	g.reload(bad) // must not exit, must not swap
	if g.cfg.UpstreamAddr != prev {
		t.Errorf("bad reload swapped config: %q -> %q", prev, g.cfg.UpstreamAddr)
	}
}

func TestHealth_DownWhenUpstreamDead(t *testing.T) {
	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(503) }))
	g, _ := newTestGateway(t, dead.URL)
	dead.Close()
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/health")
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Errorf("health with dead upstream: got %d want 503", resp.StatusCode)
	}
}

// /health is unauthenticated and its probe runs on the customer concurrency
// tier: without a cache, a burst of GETs would occupy customer slots for
// free. Repeated calls inside healthCacheTTL must hit upstream once.
func TestHealth_ProbeIsCached(t *testing.T) {
	var probes int32
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&probes, 1)
		w.Write([]byte(`{"choices":[{"message":{"content":"x"}}]}`))
	}))
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	for i := 0; i < 20; i++ {
		resp, err := http.Get(srv.URL + "/health")
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("call %d: %d", i, resp.StatusCode)
		}
	}
	if n := atomic.LoadInt32(&probes); n != 1 {
		t.Fatalf("20 /health calls within %v hit upstream %d times, want 1", healthCacheTTL, n)
	}
	// expiry re-probes
	g.healthMu.Lock()
	g.healthAt = time.Now().Add(-2 * healthCacheTTL)
	g.healthMu.Unlock()
	resp, _ := http.Get(srv.URL + "/health")
	resp.Body.Close()
	if n := atomic.LoadInt32(&probes); n != 2 {
		t.Fatalf("after TTL expiry upstream hit %d times, want 2", n)
	}
}

// The 2026-08-25 audit's #1 blocker: the old probe was an unauthenticated
// GET that gatekeeper answered 401, which the gateway treated as "ok" even
// with every replica dead. The probe must be a real authenticated chat
// completion and must report DOWN when that chat returns anything but 200.
func TestHealth_IsAuthenticatedChatProbe(t *testing.T) {
	t.Setenv("OAICA_GATEWAY_UPSTREAM_KEY", "up-key")
	var sawAuth, sawChat bool
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/v1/chat/completions" && r.Method == "POST" {
			sawChat = true
			sawAuth = r.Header.Get("Authorization") == "Bearer up-key"
			if !sawAuth {
				w.WriteHeader(401) // gatekeeper's real behaviour
				return
			}
			w.WriteHeader(200)
			io.WriteString(w, `{"choices":[{"message":{"content":"x"}}]}`)
			return
		}
		w.WriteHeader(200) // GET /v1/models is 200 even when chat is broken -- must NOT be trusted
	}))
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	resp, _ := http.Get(srv.URL + "/health")
	resp.Body.Close()
	if !sawChat || !sawAuth {
		t.Fatalf("health must POST an authenticated chat probe (chat=%v auth=%v)", sawChat, sawAuth)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("healthy upstream: got %d want 200", resp.StatusCode)
	}
	// Now the "every replica dead but proxies alive" case: chat 503, GET 200.
	t.Setenv("OAICA_GATEWAY_UPSTREAM_KEY", "wrong-key")
	g.healthMu.Lock()
	g.healthAt = time.Time{} // expire the probe cache (see TestHealth_ProbeIsCached)
	g.healthMu.Unlock()
	resp, _ = http.Get(srv.URL + "/health")
	resp.Body.Close()
	if resp.StatusCode != 503 {
		t.Fatalf("chat probe rejected -> health must be 503, got %d", resp.StatusCode)
	}
}

// include_usage:false from the client was a metering bypass (200, 0 tokens).
func TestCompletion_Streaming_ClientCannotDisableUsage(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, ledger := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	body := `{"model":"kat-awq","stream":true,"stream_options":{"include_usage":false},"messages":[{"role":"user","content":"x"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	so, _ := got["stream_options"].(map[string]any)
	if inc, _ := so["include_usage"].(bool); !inc {
		t.Fatalf("client include_usage:false must be overridden to true upstream, got %v", got["stream_options"])
	}
	e := waitLedger(t, ledger, 1)[0]
	if !e.UsageSeen || e.CompletionTokens == 0 {
		t.Fatalf("stream with client-disabled usage must still meter: %+v", e)
	}
}

// Upstream 429/503 (gatekeeper / katlb) must reach the client as OpenAI
// error objects with Retry-After, and internal topology headers must be
// stripped.
func TestProxy_NormalizesUpstreamErrors_StripsTopology(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Katlb-Backend", "http://127.0.0.1:30105")
		w.Header().Set("X-Gatekeeper-Tier", "openrouter")
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(429)
		io.WriteString(w, `{"error":"concurrency limit reached for your tier","tier":"openrouter","limit":32}`)
	}))
	defer up.Close()
	g, ledger := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"kat-awq","messages":[]}`))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 429 {
		t.Fatalf("status %d want 429", resp.StatusCode)
	}
	if resp.Header.Get("X-Katlb-Backend") != "" || resp.Header.Get("X-Gatekeeper-Tier") != "" {
		t.Errorf("topology headers leaked: %v", resp.Header)
	}
	if resp.Header.Get("Retry-After") == "" {
		t.Errorf("429 must carry Retry-After")
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("content-type %q want application/json", ct)
	}
	var doc struct {
		Error map[string]any `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&doc)
	if doc.Error == nil || doc.Error["code"] != "rate_limit_exceeded" || doc.Error["message"] == nil {
		t.Errorf("not OpenAI-shaped: %v", doc)
	}
	e := waitLedger(t, ledger, 1)[0]
	if e.Status != 429 {
		t.Errorf("429 must be ledgered for reconciliation, got status %d", e.Status)
	}
}

// A client that disconnects mid-stream still produces a ledger row (marked
// aborted) -- previously the GPU work was unmetered.
func TestCompletion_ClientAbortIsLedgered(t *testing.T) {
	release := make(chan struct{})
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		f.Flush()
		<-release // hold the stream open until the client is gone
	}))
	defer up.Close()
	g, ledger := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	body := `{"model":"kat-awq","stream":true,"messages":[{"role":"user","content":"x"}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 64)
	resp.Body.Read(buf) // got first chunk
	resp.Body.Close()   // client aborts
	close(release)
	e := waitLedger(t, ledger, 1)
	if len(e) != 1 {
		t.Fatalf("aborted stream must still be ledgered, got %d rows", len(e))
	}
	if !e[0].Aborted {
		t.Errorf("row must be flagged aborted: %+v", e[0])
	}
}

// OpenRouter's provider form requires public Privacy/Terms URLs. They must
// serve WITHOUT a key and must not be a placeholder.
func TestLegalPages_PublicAndComplete(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	for path, must := range map[string]string{
		"/privacy": "We do not use your prompts",
		"/terms":   "NO SLA",
		"/status":  "/health",
	} {
		resp, err := http.Get(srv.URL + path) // no Authorization header
		if err != nil {
			t.Fatal(err)
		}
		body, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Errorf("%s: got %d without auth, want 200 (must be public)", path, resp.StatusCode)
		}
		if !strings.Contains(string(body), must) {
			t.Errorf("%s: missing expected text %q", path, must)
		}
		if strings.Contains(string(body), "[LEGAL ENTITY NAME]") || strings.Contains(string(body), " DOMAIN]") {
			t.Errorf("%s: still contains a placeholder", path)
		}
	}
}

func TestModels_CreatedIsStableAcrossPolls(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	poll := func() float64 {
		req, _ := http.NewRequest("GET", srv.URL+"/models", nil)
		req.Header.Set("Authorization", "Bearer sk-new")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var doc struct {
			Data []map[string]any `json:"data"`
		}
		json.NewDecoder(resp.Body).Decode(&doc)
		return doc.Data[0]["created"].(float64)
	}
	a := poll()
	time.Sleep(1100 * time.Millisecond)
	if b := poll(); a != b {
		t.Fatalf("created changed between polls (%v -> %v); OpenRouter would see a new model each time", a, b)
	}
}

// max_tokens is clamped to the published max_completion_tokens, and further
// to nonStreamMaxTokens for non-streaming requests (Cloudflare 100 s TTFB).
func TestCompletion_ClampsMaxTokens(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL) // model has max_completion_tokens 32768
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	send := func(body string) float64 {
		req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-new")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		v, _ := got["max_tokens"].(float64)
		return v
	}
	if v := send(`{"model":"kat-awq","max_tokens":200000,"messages":[]}`); v != float64(nonStreamMaxTokens) {
		t.Errorf("non-stream 200000 -> upstream %v, want %d (nonStreamMaxTokens)", v, nonStreamMaxTokens)
	}
	if nonStreamMaxTokens > 4096 {
		// ~80 tok/s per stream under the 32-way cap; the proxy's
		// ResponseHeaderTimeout is 90 s. 8192 produced real 504s.
		t.Errorf("nonStreamMaxTokens = %d cannot finish inside the 90 s upstream timeout", nonStreamMaxTokens)
	}
	if v := send(`{"model":"kat-awq","stream":true,"max_tokens":200000,"messages":[]}`); v != 32768 {
		t.Errorf("stream 200000 -> upstream %v, want 32768 (published max_completion_tokens)", v)
	}
	if v := send(`{"model":"kat-awq","max_tokens":500,"messages":[]}`); v != 500 {
		t.Errorf("within-limit 500 must pass through unchanged, got %v", v)
	}
}

// kat-awq's AWQ quant emits garbage for image input ("!!!!!!!!", verified
// live). A text-only model must refuse images with a 400 BEFORE the request
// reaches vLLM, and /models must advertise input_modalities honestly.
func TestCompletion_RejectsImagesForTextOnlyModel(t *testing.T) {
	var got map[string]any
	reached := false
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		reached = true
		w.WriteHeader(200)
		io.WriteString(w, `{"choices":[{"message":{"content":"!!!!"}}],"usage":{"prompt_tokens":1,"completion_tokens":1}}`)
	}))
	defer up.Close()
	g, ledger := newTestGateway(t, up.URL) // model has no input_modalities -> text only
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	body := `{"model":"kat-awq","messages":[{"role":"user","content":[{"type":"text","text":"what is this"},{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("image to text-only model: got %d want 400", resp.StatusCode)
	}
	if reached {
		t.Fatal("request must be refused at the gateway, never forwarded to vLLM")
	}
	var doc struct {
		Error map[string]any `json:"error"`
	}
	json.NewDecoder(resp.Body).Decode(&doc)
	if doc.Error == nil || doc.Error["code"] != "invalid_request_error" {
		t.Errorf("not OpenAI-shaped: %v", doc)
	}
	if rows := readLedger(t, ledger); len(rows) != 0 {
		t.Errorf("a refused request must not be billed, got %d ledger rows", len(rows))
	}
	_ = got
}

func TestCompletion_AllowsImagesForVisionModel(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	// promote the test model to vision-capable
	cfg := g.cfg
	cfg.Models[0].InputModalities = []string{"text", "image"}
	if err := g.apply(cfg); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	body := `{"model":"kat-awq","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AAAA"}}]}]}`
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("image to vision model: got %d want 200", resp.StatusCode)
	}
}

func TestModels_AdvertisesInputModalities(t *testing.T) {
	var got map[string]any
	up := fakeUpstream(t, &got)
	defer up.Close()
	g, _ := newTestGateway(t, up.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	req, _ := http.NewRequest("GET", srv.URL+"/models", nil)
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var doc struct {
		Data []map[string]any `json:"data"`
	}
	json.NewDecoder(resp.Body).Decode(&doc)
	arch, _ := doc.Data[0]["architecture"].(map[string]any)
	in, _ := arch["input_modalities"].([]any)
	if len(in) != 1 || in[0] != "text" {
		t.Fatalf("text-only model must advertise input_modalities [text], got %v", arch)
	}
}

// fakeMeterHub records every /ingest POST it receives, thread-safely
// (the reporter runs on its own goroutine).
type fakeMeterHub struct {
	mu      sync.Mutex
	reports []usageReport
	authHdr []string
}

func newFakeMeterHub(t *testing.T) (*httptest.Server, *fakeMeterHub) {
	t.Helper()
	fh := &fakeMeterHub{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ingest" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		var rep usageReport
		json.NewDecoder(r.Body).Decode(&rep)
		fh.mu.Lock()
		fh.reports = append(fh.reports, rep)
		fh.authHdr = append(fh.authHdr, r.Header.Get("Authorization"))
		fh.mu.Unlock()
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)
	return srv, fh
}

func (fh *fakeMeterHub) count() int {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return len(fh.reports)
}

func (fh *fakeMeterHub) last() usageReport {
	fh.mu.Lock()
	defer fh.mu.Unlock()
	return fh.reports[len(fh.reports)-1]
}

func waitForCount(t *testing.T, fh *fakeMeterHub, n int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if fh.count() >= n {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("meterhub never received %d report(s), got %d", n, fh.count())
}

// TestMeterHub_DisabledByDefault proves MeterHubAddr="" (the zero value,
// what every existing config has) never starts a reporter goroutine and
// never touches meterCh — byte-identical to before meterhub existed.
func TestMeterHub_DisabledByDefault(t *testing.T) {
	upstream := fakeUpstream(t, nil)
	g, _ := newTestGateway(t, upstream.URL)
	if g.meterCh != nil {
		t.Fatal("meterCh must be nil when MeterHubAddr is unset")
	}
	// writeLedger must not panic/block with a nil meterCh.
	g.writeLedger(ledgerEntry{RequestID: "req_x"})
}

// TestMeterHub_ReceivesReportWithRegionAndAuth verifies a real end-to-end
// completion gets reported to meterhub, with the region tag and bearer
// auth header meterhub's own authed() checks for.
func TestMeterHub_ReceivesReportWithRegionAndAuth(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	meterSrv, fh := newFakeMeterHub(t)

	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}}},
		MeterHubAddr: meterSrv.URL, MeterHubToken: "hub-secret", Region: "a100b",
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-new")
	w := httptest.NewRecorder()
	mux(g).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("completion status = %d, body=%s", w.Code, w.Body.String())
	}

	waitForCount(t, fh, 1)
	rep := fh.last()
	if rep.Region != "a100b" {
		t.Errorf("reported Region = %q, want a100b", rep.Region)
	}
	if rep.KeyLabel != "openrouter" {
		t.Errorf("reported KeyLabel = %q, want openrouter", rep.KeyLabel)
	}
	if rep.PromptTokens != 7 || rep.CompletionTokens != 3 {
		t.Errorf("reported tokens = (%d, %d), want (7, 3)", rep.PromptTokens, rep.CompletionTokens)
	}
	if got := fh.authHdr[0]; got != "Bearer hub-secret" {
		t.Errorf("Authorization header sent to meterhub = %q, want %q", got, "Bearer hub-secret")
	}
}

// TestMeterHub_UnreachableNeverBlocksOrFailsTheRequest is the core safety
// property: a real chat completion must succeed and be written to the
// LOCAL ledger even when meterhub is completely unreachable.
func TestMeterHub_UnreachableNeverBlocksOrFailsTheRequest(t *testing.T) {
	// Shrink the reporter's retry backoff for this test only: the
	// production default (1s, 2s) is correct for a real meterhub outage
	// but left this test's background goroutine retrying for seconds
	// after the test function returned, competing for scheduler time with
	// every test that ran after it under `go test ./...`.
	oldBackoff := meterReporterBackoff
	meterReporterBackoff = func(attempt int) time.Duration { return time.Millisecond }
	t.Cleanup(func() { meterReporterBackoff = oldBackoff })

	var got map[string]any
	upstream := fakeUpstream(t, &got)

	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}}},
		// Deliberately unreachable: nothing listens on this port.
		MeterHubAddr: "http://127.0.0.1:1", Region: "a100b",
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}

	start := time.Now()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-new")
	w := httptest.NewRecorder()
	mux(g).ServeHTTP(w, req)
	elapsed := time.Since(start)

	if w.Code != http.StatusOK {
		t.Fatalf("completion status = %d, want 200 (an unreachable meterhub must never fail the request)", w.Code)
	}
	if elapsed > time.Second {
		t.Errorf("request took %v — an unreachable meterhub must not add request latency (reportUsage is a non-blocking channel send)", elapsed)
	}

	b, err := os.ReadFile(ledger)
	if err != nil || len(b) == 0 {
		t.Fatal("local JSONL ledger must still be written even when meterhub is unreachable")
	}

	// Bound the background reporter goroutine's lifetime: with the
	// shrunk backoff above its 3-attempt retry cycle is now ~3ms instead
	// of ~3s, so a short sleep here reliably lets it finish and exit its
	// `for range ch` loop (via the close below) before the test returns,
	// instead of leaking it to run concurrently with later tests.
	time.Sleep(50 * time.Millisecond)
	close(g.meterCh)
}

// fakeSubscriberService serves /subscribers/get with a fixed, configurable
// status per key label — stands in for meterhub in entitlement tests.
func fakeSubscriberService(t *testing.T, statuses map[string]string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/subscribers/get":
			key := r.URL.Query().Get("key")
			status, ok := statuses[key]
			if !ok {
				status = "unknown"
			}
			json.NewEncoder(w).Encode(map[string]string{"key_label": key, "status": status})
		case "/subscribers/usage":
			// Not-over-cap by default: tests that only care about
			// subscription status shouldn't also need to stub usage.
			// See fakeSubscriberServiceOverCap for the rate-limit path.
			key := r.URL.Query().Get("key")
			json.NewEncoder(w).Encode(map[string]any{
				"key_label": key, "plan": "",
				"window_5h": map[string]any{"tokens": 0, "over": false},
				"window_7d": map[string]any{"tokens": 0, "over": false},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func newTestGatewayWithEntitlement(t *testing.T, upstream, meterhubAddr string, failOpen bool) *gateway {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{
			{SHA256: keyHash("sk-active"), Label: "alice"},
			{SHA256: keyHash("sk-canceled"), Label: "bob"},
			{SHA256: keyHash("sk-unknown"), Label: "carol"},
		},
		Models: []gwModel{{ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}}},
		MeterHubAddr: meterhubAddr, Region: "a100b",
		EntitlementEnabled: true, EntitlementFailOpen: failOpen, EntitlementCacheTTLSec: 60,
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g
}

func newTestGatewayWithOverageBilling(t *testing.T, upstream, meterhubAddr string) (*gateway, string) {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-active"), Label: "alice"}},
		Models: []gwModel{{ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}}},
		MeterHubAddr: meterhubAddr, Region: "a100b",
		EntitlementEnabled: true, EntitlementFailOpen: false, EntitlementCacheTTLSec: 60,
		EntitlementOverageBilling: true,
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g, ledger
}

func postCompletion(t *testing.T, g *gateway, apiKey string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	w := httptest.NewRecorder()
	mux(g).ServeHTTP(w, req)
	return w
}

func TestEntitlement_DisabledByDefault(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	g, _ := newTestGateway(t, upstream.URL) // no MeterHubAddr, no EntitlementEnabled
	if g.entitlement != nil {
		t.Fatal("entitlement cache must be nil when EntitlementEnabled is unset")
	}
	w := postCompletion(t, g, "sk-new")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (entitlement disabled must never block a valid key)", w.Code)
	}
}

func TestEntitlement_ActiveKeyAllowed(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	meterhub := fakeSubscriberService(t, map[string]string{"alice": "active"})
	g := newTestGatewayWithEntitlement(t, upstream.URL, meterhub.URL, false)

	w := postCompletion(t, g, "sk-active")
	if w.Code != http.StatusOK {
		t.Fatalf("active subscriber: status = %d, body=%s, want 200", w.Code, w.Body.String())
	}
}

func TestEntitlement_PastDueKeyStillAllowed(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	meterhub := fakeSubscriberService(t, map[string]string{"alice": "past_due"})
	g := newTestGatewayWithEntitlement(t, upstream.URL, meterhub.URL, false)

	w := postCompletion(t, g, "sk-active")
	if w.Code != http.StatusOK {
		t.Fatalf("past_due subscriber: status = %d, want 200 (grace period, matches Stripe's own semantics)", w.Code)
	}
}

func TestEntitlement_CanceledKeyBlocked(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	meterhub := fakeSubscriberService(t, map[string]string{"bob": "canceled"})
	g := newTestGatewayWithEntitlement(t, upstream.URL, meterhub.URL, false)

	w := postCompletion(t, g, "sk-canceled")
	if w.Code != http.StatusForbidden {
		t.Fatalf("canceled subscriber: status = %d, body=%s, want 403", w.Code, w.Body.String())
	}
}

func TestEntitlement_UnknownKeyBlockedWhenFailClosed(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	meterhub := fakeSubscriberService(t, map[string]string{}) // carol has no record
	g := newTestGatewayWithEntitlement(t, upstream.URL, meterhub.URL, false)

	w := postCompletion(t, g, "sk-unknown")
	if w.Code != http.StatusForbidden {
		t.Fatalf("unknown key, fail-closed: status = %d, want 403 (this is literally \"block unsubscribed users\")", w.Code)
	}
}

func TestEntitlement_UnreachableMeterHubFailsOpenWhenConfigured(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	g := newTestGatewayWithEntitlement(t, upstream.URL, "http://127.0.0.1:1", true) // unreachable, failOpen=true

	w := postCompletion(t, g, "sk-active")
	if w.Code != http.StatusOK {
		t.Fatalf("unreachable meterhub, fail-open: status = %d, want 200 (an aggregation-layer outage must never block real traffic when fail-open is chosen)", w.Code)
	}
}

func TestEntitlement_UnreachableMeterHubFailsClosedWhenConfigured(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	g := newTestGatewayWithEntitlement(t, upstream.URL, "http://127.0.0.1:1", false) // unreachable, failOpen=false

	w := postCompletion(t, g, "sk-active")
	if w.Code != http.StatusForbidden {
		t.Fatalf("unreachable meterhub, fail-closed: status = %d, want 403 (explicit operator choice: block when the check itself fails)", w.Code)
	}
}

// fakeSubscriberServiceOverCap: active status, but /subscribers/usage
// reports the given window as over -- the rate-limit path, distinct from
// canceled/suspended.
func fakeSubscriberServiceOverCap(t *testing.T, over5h, over7d bool) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/subscribers/get":
			json.NewEncoder(w).Encode(map[string]string{"key_label": "alice", "status": "active"})
		case "/subscribers/usage":
			json.NewEncoder(w).Encode(map[string]any{
				"key_label": "alice", "plan": "starter",
				"window_5h": map[string]any{"tokens": 9_000_000, "cap": 8_000_000, "over": over5h},
				"window_7d": map[string]any{"tokens": 9_000_000, "cap": 40_000_000, "over": over7d},
			})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestEntitlement_OverWindowCapBlockedWith429(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	meterhub := fakeSubscriberServiceOverCap(t, true, false)
	g := newTestGatewayWithEntitlement(t, upstream.URL, meterhub.URL, false)

	w := postCompletion(t, g, "sk-active")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("over 5h cap: status = %d, want 429", w.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(w.Body.Bytes(), &body)
	if body.Error.Code != "rate_limited" {
		t.Errorf("error code = %q, want rate_limited", body.Error.Code)
	}
}

func TestEntitlement_UnderWindowCapAllowed(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	meterhub := fakeSubscriberServiceOverCap(t, false, false)
	g := newTestGatewayWithEntitlement(t, upstream.URL, meterhub.URL, false)

	w := postCompletion(t, g, "sk-active")
	if w.Code != http.StatusOK {
		t.Fatalf("under cap: status = %d, want 200", w.Code)
	}
}

func TestEntitlement_CacheAvoidsRepeatedMeterHubCalls(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	var hits int32
	meterhub := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/subscribers/get" {
			atomic.AddInt32(&hits, 1)
		}
		json.NewEncoder(w).Encode(map[string]string{"key_label": "alice", "status": "active"})
	}))
	defer meterhub.Close()
	g := newTestGatewayWithEntitlement(t, upstream.URL, meterhub.URL, false)

	for range 5 {
		w := postCompletion(t, g, "sk-active")
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	}
	if got := atomic.LoadInt32(&hits); got != 1 {
		t.Errorf("meterhub hit %d times for 5 requests within the cache TTL, want 1 (cache should avoid repeated calls)", got)
	}
}

// largeMessages builds a "messages" array whose marshaled size divided by 4
// clears the given estimated-token threshold — see estimateMessageTokens.
func largeMessages(estimatedTokens int) []map[string]any {
	content := strings.Repeat("x", estimatedTokens*4)
	return []map[string]any{{"role": "user", "content": content}}
}

func newTestGatewayWithAdmission(t *testing.T, upstream string, threshold, maxConcurrent int) *gateway {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{
			ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
		LargeContextTokenThreshold: threshold,
		MaxConcurrentLargeContext:  maxConcurrent,
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g
}

func postCompletionWithMessages(t *testing.T, g *gateway, apiKey string, messages []map[string]any) *httptest.ResponseRecorder {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"model": "kat-awq", "max_tokens": 10, "messages": messages})
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+apiKey)
	w := httptest.NewRecorder()
	mux(g).ServeHTTP(w, req)
	return w
}

func TestAdmission_SmallRequestNeverGated(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	// threshold 1 token would classify almost anything as "large" -- use a
	// generous threshold and a tiny message to prove small requests never
	// touch the pool at all, even with maxConcurrent effectively 0.
	g := newTestGatewayWithAdmission(t, upstream.URL, 50_000, 0)
	w := postCompletionWithMessages(t, g, "sk-new", []map[string]any{{"role": "user", "content": "hi"}})
	if w.Code != http.StatusOK {
		t.Fatalf("small request: status = %d, want 200", w.Code)
	}
}

func TestAdmission_DisabledWhenThresholdNegative(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	g := newTestGatewayWithAdmission(t, upstream.URL, -1, 1)
	w := postCompletionWithMessages(t, g, "sk-new", largeMessages(100_000))
	if w.Code != http.StatusOK {
		t.Fatalf("admission disabled: status = %d, want 200 even for a huge prompt", w.Code)
	}
}

func TestAdmission_LargeContextBlockedWhenPoolFull(t *testing.T) {
	release := make(chan struct{})
	// holding is buffered so every call into this handler (not just the
	// first, held one) can send without a receiver waiting -- only the
	// first send is ever actually read via <-holding below; release is
	// closed (not sent-once) so every subsequent <-release also returns
	// immediately, which is what lets the 3rd request finish normally.
	holding := make(chan struct{}, 10)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		holding <- struct{}{}
		<-release
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"4"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer upstream.Close()
	g := newTestGatewayWithAdmission(t, upstream.URL, 1000, 1) // pool of 1

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- postCompletionWithMessages(t, g, "sk-new", largeMessages(5000))
	}()
	<-holding // first request now holds the only pool slot, blocked in the handler

	// Second large request must be rejected: the pool is full.
	w2 := postCompletionWithMessages(t, g, "sk-new", largeMessages(5000))
	if w2.Code != http.StatusTooManyRequests {
		t.Fatalf("second large request while pool full: status = %d, want 429", w2.Code)
	}
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	json.Unmarshal(w2.Body.Bytes(), &body)
	if body.Error.Code != "large_context_admission_limited" {
		t.Errorf("error code = %q, want large_context_admission_limited", body.Error.Code)
	}

	close(release)
	w1 := <-done
	if w1.Code != http.StatusOK {
		t.Fatalf("first (held) large request: status = %d, want 200", w1.Code)
	}

	// Pool slot released after the first request finished -- a third large
	// request must now succeed.
	w3 := postCompletionWithMessages(t, g, "sk-new", largeMessages(5000))
	if w3.Code != http.StatusOK {
		t.Fatalf("third large request after pool drained: status = %d, want 200", w3.Code)
	}
}

func TestLedger_CapturesBackendAndStripsHeaderFromClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Katlb-Backend", "http://127.0.0.1:30106")
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"4"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer upstream.Close()
	g, ledgerPath := newTestGateway(t, upstream.URL)

	w := postCompletionWithMessages(t, g, "sk-new", []map[string]any{{"role": "user", "content": "hi"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	if h := w.Header().Get("X-Katlb-Backend"); h != "" {
		t.Errorf("X-Katlb-Backend leaked to the client: %q", h)
	}

	b, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var entry ledgerEntry
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.Backend != "http://127.0.0.1:30106" {
		t.Errorf("ledger backend = %q, want http://127.0.0.1:30106", entry.Backend)
	}
}

func TestLedger_CapturesSessionID(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	g, ledgerPath := newTestGateway(t, upstream.URL)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"kat-awq","max_tokens":10,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-new")
	req.Header.Set("X-Session-Id", "oaica-session-abc123")
	w := httptest.NewRecorder()
	mux(g).ServeHTTP(w, req)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}

	b, err := os.ReadFile(ledgerPath)
	if err != nil {
		t.Fatal(err)
	}
	var entry ledgerEntry
	lines := strings.Split(strings.TrimSpace(string(b)), "\n")
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &entry); err != nil {
		t.Fatal(err)
	}
	if entry.SessionID != "oaica-session-abc123" {
		t.Errorf("ledger session_id = %q, want oaica-session-abc123", entry.SessionID)
	}
}

func TestComputeCostUSD_FreshTokensOnlyWithNoCachedPrice(t *testing.T) {
	p := gwPricing{Prompt: "0.00000005", Completion: "0.00000012"}
	got := computeCostUSD(p, 1000, 0, 100)
	want := 1000*0.00000005 + 100*0.00000012
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %v, want %v", got, want)
	}
}

func TestComputeCostUSD_CachedTokensBillAtDiscountedRate(t *testing.T) {
	p := gwPricing{Prompt: "0.00000005", Completion: "0.00000012", CachedPrompt: "0.00000001"}
	// 1000 prompt tokens, 600 of them cache hits: 400 fresh @ 0.00000005 +
	// 600 cached @ 0.00000001 + 100 completion @ 0.00000012.
	got := computeCostUSD(p, 1000, 600, 100)
	want := 400*0.00000005 + 600*0.00000001 + 100*0.00000012
	if diff := got - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("cost = %v, want %v (cached discount not applied correctly)", got, want)
	}
	// Sanity: must be cheaper than if none of it were a cache hit.
	uncached := computeCostUSD(gwPricing{Prompt: p.Prompt, Completion: p.Completion}, 1000, 0, 100)
	if got >= uncached {
		t.Errorf("cached-discounted cost %v should be less than uncached cost %v", got, uncached)
	}
}

func TestComputeCostUSD_MalformedPriceReturnsZeroNotError(t *testing.T) {
	p := gwPricing{Prompt: "not-a-number", Completion: ""}
	got := computeCostUSD(p, 1000, 0, 100)
	if got != 0 {
		t.Errorf("cost with malformed pricing = %v, want 0 (pricing is informational, must never break the response)", got)
	}
}

func TestLedger_CachedPricingAppliedToRealCompletion(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"4"}}],"usage":{"prompt_tokens":1000,"completion_tokens":50,"prompt_tokens_details":{"cached_tokens":800}}}`)
	}))
	defer upstream.Close()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012", CachedPrompt: "0.00000001"}}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}

	w := postCompletionWithMessages(t, g, "sk-new", []map[string]any{{"role": "user", "content": "hi"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	entries := waitLedger(t, ledger, 1)
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	e := entries[0]
	if e.CachedTokens != 800 {
		t.Fatalf("cached_tokens = %d, want 800", e.CachedTokens)
	}
	want := 200*0.00000005 + 800*0.00000001 + 50*0.00000012
	if diff := e.CostUSD - want; diff > 1e-12 || diff < -1e-12 {
		t.Errorf("ledger cost_usd = %v, want %v", e.CostUSD, want)
	}
}

func TestEntitlement_OverCapBlockedByDefault_OverageBillingOff(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	meterhub := fakeSubscriberServiceOverCap(t, true, false)
	g := newTestGatewayWithEntitlement(t, upstream.URL, meterhub.URL, false) // overage billing NOT enabled

	w := postCompletion(t, g, "sk-active")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("overage billing off, over cap: status = %d, want 429 (unchanged default behavior)", w.Code)
	}
}

func TestEntitlement_OverCapAllowedAndFlaggedAsOverageWhenBillingEnabled(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"4"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer upstream.Close()
	meterhub := fakeSubscriberServiceOverCap(t, true, false) // alice active, over 5h cap
	g, ledgerPath := newTestGatewayWithOverageBilling(t, upstream.URL, meterhub.URL)

	w := postCompletionWithMessages(t, g, "sk-active", []map[string]any{{"role": "user", "content": "hi"}})
	if w.Code != http.StatusOK {
		t.Fatalf("overage billing on, over cap: status = %d, want 200 (allowed, billed as overage)", w.Code)
	}

	entries := waitLedger(t, ledgerPath, 1)
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	if !entries[0].Overage {
		t.Error("ledger entry.Overage = false, want true (request was over the 5h cap and only let through via overage billing)")
	}
}

func TestEntitlement_UnderCapNeverFlaggedAsOverage(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"4"}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`)
	}))
	defer upstream.Close()
	meterhub := fakeSubscriberServiceOverCap(t, false, false) // under cap
	g, ledgerPath := newTestGatewayWithOverageBilling(t, upstream.URL, meterhub.URL)

	w := postCompletionWithMessages(t, g, "sk-active", []map[string]any{{"role": "user", "content": "hi"}})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", w.Code)
	}
	entries := waitLedger(t, ledgerPath, 1)
	if len(entries) != 1 {
		t.Fatalf("ledger entries = %d, want 1", len(entries))
	}
	if entries[0].Overage {
		t.Error("ledger entry.Overage = true for a request under cap, want false")
	}
}

// -- upstream error logging (2026-08-29) --

func newTestGatewayWithErrorLog(t *testing.T, upstream string) (*gateway, string) {
	t.Helper()
	errLog := filepath.Join(t.TempDir(), "upstream-errors.jsonl")
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr:         upstream,
		ListenAddr:           ":0",
		LedgerPath:           ledger,
		UpstreamErrorLogPath: errLog,
		APIKeys:              []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{
			ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g, errLog
}

func TestUpstreamErrorLog_CapturesRealErrorMessageAndCorrelationInfo(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"This model's maximum context length is 262144 tokens. However, you requested 270000 tokens (238000 in the messages, 32000 in the completion).","type":"invalid_request_error"}}`)
	}))
	defer upstream.Close()

	g, errLogPath := newTestGatewayWithErrorLog(t, upstream.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}],"max_tokens":32000}`))
	req.Header.Set("Authorization", "Bearer sk-new")
	req.Header.Set("X-Session-Id", "sess-overflow-test")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 400 {
		t.Fatalf("expected 400 from the gateway, got %d", resp.StatusCode)
	}

	deadline := time.Now().Add(2 * time.Second)
	var lines []string
	for {
		b, _ := os.ReadFile(errLogPath)
		lines = strings.Split(strings.TrimSpace(string(b)), "\n")
		if len(lines) >= 1 && lines[0] != "" || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(lines) == 0 || lines[0] == "" {
		t.Fatal("expected at least one line in the upstream error log")
	}
	var got upstreamErrorLogLine
	if err := json.Unmarshal([]byte(lines[len(lines)-1]), &got); err != nil {
		t.Fatalf("bad JSON line: %v (%s)", err, lines[len(lines)-1])
	}
	if !strings.Contains(got.Message, "maximum context length is 262144") {
		t.Errorf("expected the real upstream error message preserved, got %q", got.Message)
	}
	if got.SessionID != "sess-overflow-test" {
		t.Errorf("expected session_id correlation, got %q", got.SessionID)
	}
	// The request asked for max_tokens=32000, but this is a non-streaming
	// completion, so the gateway's own clamp (nonStreamMaxTokens=4096)
	// rewrites it before the request ever reaches upstream -- the captured
	// value should reflect what was ACTUALLY sent, not what the client
	// originally asked for.
	if got.MaxTokens != 4096 {
		t.Errorf("expected the clamped max_tokens=4096 (non-stream cap) captured, got %d", got.MaxTokens)
	}
	if got.Status != 400 {
		t.Errorf("expected status=400, got %d", got.Status)
	}
}

func TestUpstreamErrorLog_DisabledWhenPathEmpty(t *testing.T) {
	// newTestGateway (the default helper used by every other test) leaves
	// UpstreamErrorLogPath empty -- must not create a file or panic on a
	// nil g.errLog.
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(400)
		io.WriteString(w, `{"error":{"message":"bad request"}}`)
	}))
	defer upstream.Close()
	g, _ := newTestGateway(t, upstream.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions",
		strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if g.errLog != nil {
		t.Error("expected errLog to stay nil when UpstreamErrorLogPath is empty")
	}
}

// -- context-length-fit clamp (2026-08-29) --

func TestContextFitClamp_ReproducesRealIncident_PromptPlusMaxTokensOverContextLength(t *testing.T) {
	// Real 2026-08-29 incident: a Claude Code auto-compaction call itself
	// failed with "maximum context length is 262144 tokens... requested
	// 230145 input + 32000 output = 262145" -- one token over. This test
	// builds a request whose messages estimate to roughly that many
	// tokens (estimateMessageTokens is chars/4) with max_tokens=32000,
	// against a model with ContextLength=262144, and asserts the gateway
	// clamps max_tokens down so the forwarded request actually fits,
	// instead of forwarding one guaranteed to 400 upstream.
	var gotMaxTokens float64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		gotMaxTokens, _ = req["max_tokens"].(float64)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-new"), Label: "openrouter"}},
		Models: []gwModel{{
			ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	// ~230145 tokens at chars/4 -> ~920580 chars of message content.
	bigContent := strings.Repeat("x", 920580)
	body, _ := json.Marshal(map[string]any{
		"model":      "kat-awq",
		"messages":   []map[string]any{{"role": "user", "content": bigContent}},
		"max_tokens": 32000,
		"stream":     true,
	})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected the gateway to clamp and forward successfully, got %d", resp.StatusCode)
	}
	if gotMaxTokens >= 32000 {
		t.Fatalf("expected max_tokens to be clamped below the original 32000 (prompt was near context_length), got %v", gotMaxTokens)
	}
	if gotMaxTokens <= 0 {
		t.Fatalf("expected a positive, still-useful max_tokens after clamping, got %v", gotMaxTokens)
	}
}

func TestContextFitClamp_SmallPromptUnaffected(t *testing.T) {
	// A normal small request must not have its max_tokens touched by the
	// context-fit clamp -- only large-context requests near the ceiling
	// should be affected.
	var gotMaxTokens float64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var req map[string]any
		json.Unmarshal(b, &req)
		gotMaxTokens, _ = req["max_tokens"].(float64)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\ndata: [DONE]\n\n")
	}))
	defer upstream.Close()

	g, _ := newTestGateway(t, upstream.URL)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	body, _ := json.Marshal(map[string]any{
		"model":      "kat-awq",
		"messages":   []map[string]any{{"role": "user", "content": "hello"}},
		"max_tokens": 2000,
		"stream":     true,
	})
	req, _ := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if gotMaxTokens != 2000 {
		t.Errorf("expected max_tokens to pass through unclamped for a small request, got %v", gotMaxTokens)
	}
}
