package main

// round33_tools_images_and_negative_counts_integrity_test.go — what the
// prompt-size unit does not include, what the Anthropic bridge does with an
// image it cannot inline, and which upstream counts reach the client and the
// ledger as statements (2026-09-27 audit, round 33, B-F1/F2/F3 + the
// negative-count sub-bar note).
//
//   - B-F1: messagesBytes measured req["messages"]/req["prompt"] and nothing
//     else. A tool schema is not an optional extra: the chat template renders
//     it into the prompt, the replica prefills it, and the upstream reports it
//     in prompt_tokens. Left out of the unit, a tools-borne payload was priced
//     at zero — admitted while the large-context pool was full, exempt from
//     the context-fit clamp, and its real measurement discarded by the
//     calibrator (ratio above calibMaxRatio is bogus).
//   - B-F2: the bridge's image case read only source.media_type/source.data.
//     An Anthropic URL source ({"type":"url","url":...}) has neither, so it
//     was emitted as the literal "data:;base64," — the upstream answered 200
//     about a blank image and the client never learned its picture was gone.
//   - B-F3: the closing message_delta emitted inTok-cacheTok raw. A stream
//     that states a cache hit and then restates a smaller prompt (per-field
//     usage merging keeps the larger hit) emitted input_tokens=-800 with
//     cache_read_input_tokens=900 — two numbers that do not partition the
//     prompt and a negative the client's accounting has no room for.
//   - Sub-bar: a negative count from an upstream reached the ledger as a
//     negative prompt/completion and a negative cost (a credit), on both the
//     non-stream assign and the streaming merge.

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

// TestAToolSchemaIsChargedAsPrompt is B-F1.
func TestAToolSchemaIsChargedAsPrompt(t *testing.T) {
	release := make(chan struct{})
	holding := make(chan struct{}, 10)
	var n atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Only the control request is held: a request that (before the fix)
		// slips past the pool must still be answered, or the test hangs on the
		// very admission it is there to demonstrate.
		if n.Add(1) == 1 {
			holding <- struct{}{}
			<-release
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	g := newTestGatewayWithAdmission(t, upstream.URL, 1000, 1) // pool of 1

	// A 1 MB tool schema — the same payload the messages-borne control carries,
	// spelled where the OpenAI tool wire puts it.
	toolsBody := func(toolBytes int) string {
		schema := strings.Repeat("x", toolBytes)
		b, _ := json.Marshal(map[string]any{
			"model": "kat-awq", "max_tokens": 10,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
			"tools": []map[string]any{{
				"type": "function",
				"function": map[string]any{
					"name":        "lookup",
					"description": schema,
					"parameters":  map[string]any{"type": "object"},
				},
			}},
		})
		return string(b)
	}

	// The unit itself: a tool schema is prompt the upstream prefills.
	var withTools map[string]any
	if err := json.Unmarshal([]byte(toolsBody(1<<20)), &withTools); err != nil {
		t.Fatal(err)
	}
	var noTools map[string]any
	if err := json.Unmarshal([]byte(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`), &noTools); err != nil {
		t.Fatal(err)
	}
	if got, want := messagesBytes(withTools), messagesBytes(noTools); got <= want {
		t.Errorf("messagesBytes(1MB tool schema) = %d, messagesBytes(same request without tools) = %d: the tool schema is not in the prompt-size unit, so a tools-borne payload is priced at zero", got, want)
	}

	post := func(body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk-new")
		w := httptest.NewRecorder()
		mux(g).ServeHTTP(w, req)
		return w
	}

	// Hold the only pool slot with a messages-borne large request.
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- postCompletionWithMessages(t, g, "sk-new", largeMessages(5000)) }()
	select {
	case <-holding:
	case <-time.After(5 * time.Second):
		t.Fatal("the control request never reached the upstream: the pool slot was never held")
	}

	// The same payload spelled as a tool schema must be admitted the same way:
	// a full pool refuses it, not prices it at zero.
	w := post(toolsBody(1 << 20))
	if w.Code != http.StatusTooManyRequests {
		t.Errorf("a 1MB tool schema while the large-context pool is full: status = %d, body = %s; want 429 large_context_admission_limited — the schema is prompt the replica prefills, so a request carrying one is a large-context request", w.Code, strings.TrimSpace(w.Body.String()))
	}

	close(release)
	<-done
}

// TestAURLSourceImageReachesTheUpstreamAsAURL is B-F2.
func TestAURLSourceImageReachesTheUpstreamAsAURL(t *testing.T) {
	var mu map[string]any
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		mu = body
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-abc","choices":[{"finish_reason":"stop","message":{"content":"a cat"}}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`)
	}))
	defer upstream.Close()

	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{{
			ID: "kat-vision", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768,
			InputModalities: []string{"text", "image"},
			Pricing:         gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	post := func(body string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Authorization", "Bearer sk")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()
		g.messagesHandler(w, req)
		return w.Code, strings.TrimSpace(w.Body.String())
	}

	// Anthropic's URL source: the documented way a client sends an image host
	// side (and what the Claude wire uses when it does not inline).
	code, body := post(`{"model":"kat-vision","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"text","text":"what is this?"},` +
		`{"type":"image","source":{"type":"url","url":"https://example.com/cat.png"}}]}]}`)
	if code != http.StatusOK {
		t.Fatalf("url-source image: status = %d, body = %s; want 200", code, body)
	}
	msgs, _ := mu["messages"].([]any)
	if len(msgs) == 0 {
		t.Fatal("upstream saw no messages")
	}
	first, _ := msgs[0].(map[string]any)
	parts, _ := first["content"].([]any)
	var url string
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		if pm["type"] != "image_url" {
			continue
		}
		iu, _ := pm["image_url"].(map[string]any)
		url, _ = iu["url"].(string)
	}
	if url == "" {
		t.Errorf("the upstream request carries no image part at all: the URL-source image was dropped, so the model answers about a prompt it never received")
	} else if strings.HasPrefix(url, "data:") {
		t.Errorf("the upstream received the data URI %q for an Anthropic url source: the type was ignored and the blank data:;base64, built from two empty strings was sent in its place", url)
	} else if url != "https://example.com/cat.png" {
		t.Errorf("upstream image url = %q, want the client's https://example.com/cat.png", url)
	}

	// A source the OpenAI wire cannot express is refused, not blanked: the
	// client must not read a 200 about an image the model never saw.
	code, body = post(`{"model":"kat-vision","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"file","file_id":"file_123"}}]}]}`)
	if code == http.StatusOK {
		t.Errorf("an image source of an unrepresentable type was answered 200 (body = %s): the client is told its image was seen", body)
	}
}

// TestAStreamThatRestatesASmallerPromptNeverEmitsNegativeInputTokens is B-F3.
func TestAStreamThatRestatesASmallerPromptNeverEmitsNegativeInputTokens(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		// A cache hit stated first (900 of 900 prompt tokens), then a later
		// chunk that restates a smaller prompt — per-field usage merging keeps
		// the larger hit, which used to be subtracted whole.
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":900,\"prompt_tokens_details\":{\"cached_tokens\":900}}}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":100}}\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	defer upstream.Close()

	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: filepath.Join(t.TempDir(), "l.jsonl"),
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{{
			ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	req := httptest.NewRequest(http.MethodPost, "/v1/messages",
		strings.NewReader(`{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hi"}]}`))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.messagesHandler(w, req)

	var in, cached int
	found := false
	for _, line := range strings.Split(w.Body.String(), "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Usage *struct {
				InputTokens          int `json:"input_tokens"`
				CacheReadInputTokens int `json:"cache_read_input_tokens"`
				OutputTokens         int `json:"output_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil || ev.Usage == nil {
			continue
		}
		in, cached, found = ev.Usage.InputTokens, ev.Usage.CacheReadInputTokens, true
	}
	if !found {
		t.Fatalf("no usage event in the stream:\n%s", w.Body.String())
	}
	if in < 0 {
		t.Errorf("streamed input_tokens = %d (cache_read_input_tokens = %d): the cache counter was subtracted raw, so a stream that restates a smaller prompt emits a prompt the client's accounting cannot hold", in, cached)
	}
	if cached > in+cached {
		t.Errorf("cache_read_input_tokens (%d) exceeds the prompt it is a part of (input_tokens %d + cache %d)", cached, in, cached)
	}
}

// TestANegativeUpstreamCountIsNotABillableStatement is the sub-bar note.
func TestANegativeUpstreamCountIsNotABillableStatement(t *testing.T) {
	var body string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, body)
	}))
	defer upstream.Close()

	g := &gateway{}
	ledgerPath := filepath.Join(t.TempDir(), "l.jsonl")
	if err := g.apply(gwConfig{
		UpstreamAddr: upstream.URL, ListenAddr: ":0", LedgerPath: ledgerPath,
		APIKeys: []gwKey{{SHA256: keyHash("sk"), Label: "k"}},
		Models: []gwModel{{
			ID: "kat-awq", OwnedBy: "oaica", ContextLength: 262144, MaxCompletionTokens: 32768,
			Pricing: gwPricing{Prompt: "0.00000005", Completion: "0.00000012"},
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	post := func(b string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(b))
		req.Header.Set("Authorization", "Bearer sk")
		w := httptest.NewRecorder()
		mux(g).ServeHTTP(w, req)
	}

	// A malformed upstream: negative counts, on the non-stream shape...
	body = `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":-400,"completion_tokens":-5}}`
	post(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)
	// ... and stated mid-stream over an earlier good count.
	body = `{"id":"x","choices":[{"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":700,"completion_tokens":20}}`
	post(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}],"max_tokens":10}`)

	entries := waitLedger(t, ledgerPath, 2)
	for _, e := range entries {
		if e.PromptTokens < 0 {
			t.Errorf("ledger row records prompt_tokens = %d from an upstream that stated a negative count: the row is a billable statement the gateway cannot make", e.PromptTokens)
		}
		if e.CompletionTokens < 0 {
			t.Errorf("ledger row records completion_tokens = %d", e.CompletionTokens)
		}
		if e.CostUSD < 0 {
			t.Errorf("ledger row records cost_usd = %v for a request whose upstream stated negative counts: a negative count credits the customer for tokens nobody served", e.CostUSD)
		}
	}

	// Control: the sane row is still billed as itself (no blanket zeroing).
	var sawSane bool
	for _, e := range entries {
		if e.PromptTokens == 700 && e.CompletionTokens == 20 && e.CostUSD > 0 {
			sawSane = true
		}
	}
	if !sawSane {
		t.Errorf("control row (prompt 700 / completion 20) missing from the ledger: %+v", entries)
	}

	// And the streaming shape, where the negative arrives as a later chunk.
	body = ""
	upstream.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"hi\"}}],\"usage\":{\"prompt_tokens\":800,\"completion_tokens\":30}}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":-800,\"completion_tokens\":-30}}\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	})
	post(`{"model":"kat-awq","messages":[{"role":"user","content":"hi"}],"max_tokens":10,"stream":true}`)
	entries = waitLedger(t, ledgerPath, 3)
	last := entries[len(entries)-1]
	if last.PromptTokens < 0 || last.CompletionTokens < 0 || last.CostUSD < 0 {
		t.Errorf("streamed row after a negative restatement: prompt=%d completion=%d cost=%v; a negative count is not a statement and must not overwrite the good counts the same stream already gave", last.PromptTokens, last.CompletionTokens, last.CostUSD)
	}
	if last.PromptTokens != 800 {
		t.Errorf("streamed row prompt_tokens = %d, want the 800 the stream stated: the later negative erased a stated count instead of being ignored", last.PromptTokens)
	}
}
