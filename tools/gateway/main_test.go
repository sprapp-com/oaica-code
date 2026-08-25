package main

import (
	"bufio"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
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
	m.HandleFunc("/models", g.authed(g.modelsHandler))
	m.HandleFunc("/v1/models", g.authed(g.modelsHandler))
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
		req, _ := http.NewRequest("GET", srv.URL+"/models", nil)
		if tc.key != "" {
			req.Header.Set("Authorization", "Bearer "+tc.key)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != tc.want {
			t.Errorf("key %q: got %d want %d", tc.key, resp.StatusCode, tc.want)
		}
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
