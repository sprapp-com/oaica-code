package main

// round34_bridge_body_and_prompt_unit_integrity_test.go — the translated body
// under the upstream's Content-Length, a block the bridge drops without a word,
// and two prompt guards that a shape can still walk past (2026-09-27 audit,
// round 34, B-F1/F2/F3/F4/F5).
//
//   - B-F1: the /v1/messages bridge buffers the upstream's answer and writes a
//     TRANSLATED body — a different length — while the reverse proxy has already
//     copied the upstream's Content-Length onto the response. net/http enforces
//     a declared Content-Length on handler writes, so the client read a
//     truncated or empty body with `unexpected EOF`. Every test that drove the
//     handler through httptest.NewRecorder bypasses that enforcement, which is
//     why the suite never saw it.
//   - B-F2: the block switch has no default and no `document` case, so an
//     Anthropic document block (and any unknown type) fell out silently and the
//     model was asked about a document it never received — the same class the
//     image case two cases above refuses in words.
//   - B-F3: the non-stream path emitted resp.Usage.PromptTokens-cached raw, so a
//     negative upstream count reached the client as a negative input_tokens,
//     while the ledger row for the same turn records 0 (round 33 clamped the
//     stream path; this is the other one).
//   - B-F4: the context-fit clamp compares int(v) > fitBudget on a float64, so
//     max_tokens 1e19 wraps negative and the clamp is skipped — reachable
//     whenever the model publishes no max_completion_tokens, because the
//     output-budget clamp above it only runs when one is published.
//   - B-F5: messagesBytes tests `req["messages"]` by PRESENCE, so a body carrying
//     an empty messages array beside a real legacy `prompt` measured ~0 tokens
//     and skipped both guards — the round-33 fix covered "messages absent".

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// round34Gateway is the config the five probes share: one model with a window,
// a published output budget and a price.
func round34Gateway(t *testing.T, g *gateway, upstream string, model gwModel, threshold, maxConcurrent int) {
	t.Helper()
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l.jsonl"),
		APIKeys:                    []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models:                     []gwModel{model},
		LargeContextTokenThreshold: threshold, MaxConcurrentLargeContext: maxConcurrent,
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
}

func round34Model() gwModel {
	return gwModel{
		ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768,
		Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
	}
}

// TestAMessagesBodySurvivesTheUpstreamsContentLength is B-F1: the bridge's
// output must reach the client whole, which only a real server can show —
// httptest.NewRecorder does not enforce a declared Content-Length.
func TestAMessagesBodySurvivesTheUpstreamsContentLength(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		var req map[string]any
		_ = req
		w.Header().Set("Content-Type", "application/json")
		// A non-stream OpenAI answer whose serialized length differs from the
		// Anthropic envelope the bridge writes.
		io.WriteString(w, `{"id":"chatcmpl-abc","choices":[{"finish_reason":"stop","message":{"content":"Hello from the model"}}],"usage":{"prompt_tokens":11,"completion_tokens":5}}`)
	}))
	defer upstream.Close()

	g := &gateway{}
	round34Gateway(t, g, upstream.URL, round34Model(), -1, 0)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()

	client := &http.Client{Timeout: 10 * time.Second}
	post := func(path, body string) (int, []byte, error) {
		t.Helper()
		req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer sk")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		b, readErr := io.ReadAll(resp.Body)
		return resp.StatusCode, b, readErr
	}

	const msgBody = `{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"hi"}]}`

	// A streamed /v1/chat/completions request's non-SSE answer is forwarded
	// verbatim, so its body length matches the declared one: the control.
	code, nativeBody, nativeErr := post("/v1/chat/completions", msgBody)
	if code != http.StatusOK || nativeErr != nil || !json.Valid(nativeBody) {
		t.Fatalf("control (/v1/chat/completions): status=%d err=%v body=%q", code, nativeErr, nativeBody)
	}

	code, body, readErr := post("/v1/messages", msgBody)
	if readErr != nil {
		t.Errorf("/v1/messages non-stream: status=%d read error: %v (body so far %q); the bridge writes a body of its own length under the upstream's Content-Length, so net/http cuts the response short and the client cannot read the answer it was billed for", code, readErr, body)
	}
	if code != http.StatusOK {
		t.Fatalf("/v1/messages non-stream: status=%d body=%q", code, body)
	}
	if !json.Valid(body) {
		t.Errorf("/v1/messages non-stream body is not valid JSON: %q", body)
	}
	if !strings.Contains(string(body), "Hello from the model") {
		t.Errorf("/v1/messages body lost the model's answer: %q", body)
	}

	// The error path, where ModifyResponse re-writes the body AND its
	// Content-Length to the OpenAI-shaped error it builds: the bridge then
	// answers with Anthropic's envelope, whose wording is what Claude Code
	// pattern-matches to its compaction recovery path.
	errUpstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		io.WriteString(w, `{"error":{"message":"This model's maximum context length is 262144 tokens. However, you requested 230145 tokens (230145 in the messages, 0 in the completion). Please reduce the length of the messages or completion."}}`)
	}))
	defer errUpstream.Close()
	g2 := &gateway{}
	round34Gateway(t, g2, errUpstream.URL, round34Model(), -1, 0)
	srv2 := httptest.NewServer(mux(g2))
	defer srv2.Close()

	req, err := http.NewRequest(http.MethodPost, srv2.URL+"/v1/messages", strings.NewReader(msgBody))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	errBody, errRead := io.ReadAll(resp.Body)
	if errRead != nil {
		t.Errorf("/v1/messages upstream error: status=%d read error: %v (body so far %q)", resp.StatusCode, errRead, errBody)
	}
	if !strings.Contains(string(errBody), "prompt is too long") {
		t.Errorf("/v1/messages upstream 400 body = %q; want Anthropic's \"prompt is too long: 230145 tokens > 262144 maximum\" wording — that exact string is what the client matches to its context-recovery path, and without it it retries the identical doomed request", errBody)
	}
}

// TestADocumentBlockIsNotSilentlyDropped is B-F2.
func TestADocumentBlockIsNotSilentlyDropped(t *testing.T) {
	var mu atomic.Pointer[map[string]any]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Store(&body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-abc","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
	}))
	defer upstream.Close()

	g := &gateway{}
	round34Gateway(t, g, upstream.URL, round34Model(), -1, 0)

	post := func(body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		g.messagesHandler(w, req)
		return w.Code, strings.TrimSpace(w.Body.String())
	}

	code, resp := post(`{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"JVBERi0xLjQK"}},` +
		`{"type":"text","text":"Summarise this PDF."}]}]}`)
	if code == http.StatusOK {
		t.Errorf("a document block was answered 200 (body = %s): the block matched no case in the block switch and was dropped, so the model was asked about a document it never received — a silent wrong answer, and the attached payload is never charged to the prompt either", resp)
	} else if !strings.Contains(resp, "document") {
		t.Errorf("the refusal does not name the block it could not represent: %s", resp)
	}

	// An unknown block type is the same class: refused in words, not dropped.
	code, resp = post(`{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"some_future_block","data":"x"}]}]}`)
	if code == http.StatusOK {
		t.Errorf("an unknown content block was answered 200 (body = %s): any block type this bridge cannot represent must be named, not dropped", resp)
	}
}

// TestANegativeUpstreamCountNeverReachesTheAnthropicClient is B-F3.
func TestANegativeUpstreamCountNeverReachesTheAnthropicClient(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-x","choices":[{"finish_reason":"stop","message":{"content":"hi"}}],"usage":{"prompt_tokens":-400,"completion_tokens":3}}`)
	}))
	defer upstream.Close()

	g := &gateway{}
	round34Gateway(t, g, upstream.URL, round34Model(), -1, 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"kat-awq","max_tokens":64,"messages":[{"role":"user","content":"hello"}]}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.messagesHandler(w, req)

	var resp struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
			OutputTokens         int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("status=%d body=%s: %v", w.Code, w.Body.String(), err)
	}
	if resp.Usage.InputTokens < 0 {
		t.Errorf("the non-stream /v1/messages body states input_tokens=%d (cache_read=%d): the upstream's negative count was passed through as a statement about the prompt, and it contradicts the ledger row, which records 0 for the same turn", resp.Usage.InputTokens, resp.Usage.CacheReadInputTokens)
	}
	if resp.Usage.OutputTokens < 0 {
		t.Errorf("the body states output_tokens=%d", resp.Usage.OutputTokens)
	}
}

// TestAnAbsurdOutputBudgetIsClampedEvenWithNoPublishedBudget is B-F4.
func TestAnAbsurdOutputBudgetIsClampedEvenWithNoPublishedBudget(t *testing.T) {
	var mu atomic.Pointer[map[string]any]
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu.Store(&body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	// A model that publishes no max_completion_tokens: the output-budget clamp
	// above the fit clamp runs only when one is published, so the fit clamp is
	// the only guard left.
	m := round34Model()
	m.MaxCompletionTokens = 0
	g := &gateway{}
	round34Gateway(t, g, upstream.URL, m, -1, 0)

	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions",
		strings.NewReader(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}],"max_tokens":1e19}`))
	req.Header.Set("Authorization", "Bearer sk")
	w := httptest.NewRecorder()
	mux(g).ServeHTTP(w, req)

	got := mu.Load()
	if got == nil {
		t.Fatal("the upstream was never called")
	}
	if v, _ := (*got)["max_tokens"].(float64); v > float64(262144) {
		t.Errorf("upstream received max_tokens = %v for a 1e19 ask (status %d): int(1e19) wraps negative, so the fit clamp's comparison is false and the request the clamp exists to prevent goes upstream as the hard 400 it was meant to become a smaller ask", v, w.Code)
	}
}

// TestAPromptBesideAnEmptyMessagesArrayIsStillMeasured is B-F5.
func TestAPromptBesideAnEmptyMessagesArrayIsStillMeasured(t *testing.T) {
	release := make(chan struct{})
	holding := make(chan struct{}, 10)
	var n atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if n.Add(1) == 1 {
			holding <- struct{}{}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	g := &gateway{}
	round34Gateway(t, g, upstream.URL, round34Model(), 1000, 1)

	// Under the model's window (so the control is admitted and reaches the
	// upstream, holding the pool's only slot) but far over the 1000-token
	// admission threshold this test sets.
	huge := strings.Repeat("x", 200<<10)
	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk")
		w := httptest.NewRecorder()
		mux(g).ServeHTTP(w, req)
		return w
	}

	// The measurement itself: a real prompt beside an empty messages array is
	// still a prompt.
	both := map[string]any{"model": "kat-awq", "messages": []any{}, "prompt": huge}
	if b, err := json.Marshal(both); err == nil {
		var decoded map[string]any
		if err := json.Unmarshal(b, &decoded); err != nil {
			t.Fatal(err)
		}
		if got := messagesBytes(decoded) / 4; got < 1000 {
			t.Errorf("messagesBytes({\"messages\":[], \"prompt\": 1MB})/4 = %d estimated tokens: the presence of an empty messages array short-circuited the legacy prompt branch", got)
		}
	}

	// Behaviourally: hold the only large-context slot, then the same payload
	// behind messages:[] — the pool must treat it like the prompt-only shape.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		done <- post(`{"model":"kat-awq","messages":[{"role":"user","content":"` + huge + `"}],"max_tokens":2048}`)
	}()
	select {
	case <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("the control request never reached the upstream: the pool slot was never held")
	}

	w := post(`{"model":"kat-awq","messages":[],"prompt":"` + huge + `","max_tokens":2048}`)
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("a 1MB prompt behind an empty messages array while the large-context pool is full: status = %d, body = %s; want 429 — the guard reads whichever field is present and stops, so the twin spelling of the same payload walks past it", w.Code, strings.TrimSpace(w.Body.String()))
	}

	close(release)
	<-done
}
