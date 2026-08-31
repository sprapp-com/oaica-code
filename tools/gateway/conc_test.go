package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

// per-key concurrency cap: gwKey.MaxConcurrent
func newTestGatewayWithKeyConc(t *testing.T, upstream string, maxConcurrent int) *gateway {
	t.Helper()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-conc"), Label: "conc", MaxConcurrent: maxConcurrent}},
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
	return g
}

func TestKeyConcurrent_SecondRequest429WhileFirstInFlight(t *testing.T) {
	release := make(chan struct{})
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-release
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"usage": map[string]any{"prompt_tokens": 3, "completion_tokens": 2, "prompt_tokens_details": nil},
		})
	}))
	defer upstream.Close()
	var once sync.Once
	defer once.Do(func() { close(release) })
	g := newTestGatewayWithKeyConc(t, upstream.URL, 1)

	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		body, _ := json.Marshal(map[string]any{"model": "kat-awq", "max_tokens": 4, "messages": []map[string]any{{"role": "user", "content": "hi"}}})
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-conc")
		w := httptest.NewRecorder()
		mux(g).ServeHTTP(w, req)
		done <- w
	}()

	// first request occupies the single slot until released
	time.Sleep(100 * time.Millisecond)
	w := postCompletion(t, g, "sk-conc")
	if w.Code != http.StatusTooManyRequests {
		t.Fatalf("second request: status = %d, want 429", w.Code)
	}
	if got := w.Header().Get("Retry-After"); got == "" {
		t.Errorf("429 missing Retry-After")
	}
	once.Do(func() { close(release) })
	if w := <-done; w.Code != http.StatusOK {
		t.Fatalf("first request: status = %d, want 200", w.Code)
	}
}

func TestKeyConcurrent_ZeroIsUnlimited(t *testing.T) {
	var got map[string]any
	upstream := fakeUpstream(t, &got)
	g := newTestGatewayWithKeyConc(t, upstream.URL, 0)
	for i := 0; i < 3; i++ {
		w := postCompletion(t, g, "sk-conc")
		if w.Code != http.StatusOK {
			t.Fatalf("request %d: status = %d, want 200 (max_concurrent unset = no cap)", i+1, w.Code)
		}
	}
}

// request wall-clock cap: gwConfig.RequestTimeoutSec
func TestRequestTimeout_SluggishUpstreamAborted(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		<-r.Context().Done() // hang until the gateway's deadline kills it
	}))
	defer upstream.Close()
	ledger := filepath.Join(t.TempDir(), "ledger.jsonl")
	cfg := gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-to"), Label: "to"}},
		Models: []gwModel{{
			ID: "kat-awq", UpstreamID: "kat-awq-served", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
		RequestTimeoutSec: 1,
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		t.Fatalf("apply: %v", err)
	}
	start := time.Now()
	w := postCompletion(t, g, "sk-to")
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("request took %v, want it bounded by the 1s cap", elapsed)
	}
	if w.Code == http.StatusOK && len(w.Body.String()) > 0 && g.requestTimeout == 0 {
		t.Fatalf("requestTimeout not applied")
	}
}
