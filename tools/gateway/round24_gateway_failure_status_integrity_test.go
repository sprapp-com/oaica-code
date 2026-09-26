package main

// round24_gateway_failure_status_integrity_test.go — three ways the bridge's
// own error handling was defeated by the writer it writes through
// (2026-09-27 audit, round 24).
//
// The gateway calls itself a translator, and a translation has a status. Two
// of these are the same defect: the non-stream success path commits HTTP 200 to
// the client before it knows whether the upstream body can be translated at
// all, so the 502 the code explicitly asks for is discarded as a superfluous
// WriteHeader and the client reads a FAILED turn as a successful one. The
// stream path has the mirror-image version: an upstream that ends before
// emitting anything leaves a bare 200 with an empty body and no message_stop,
// so a client that waits for the terminator waits forever.
//
// The third is the ledger's copy of the same class round 23 fixed in
// cachedTokens(): `scanSSE` replaces the whole usage struct per chunk, so an
// upstream that states its prompt count in one chunk and its completion count
// in another bills only whichever arrived last.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
)

// brokenUpstream answers every /v1/messages-facing request with a chosen
// status, content type and body — the raw shapes the bridge has to survive.
func brokenUpstream(t *testing.T, status int, contentType, body string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		if contentType != "" {
			w.Header().Set("Content-Type", contentType)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// TestGatewayNoChoicesIsABadGateway pins the status the code already asks for:
// an upstream 200 whose body carries no choices cannot be translated into an
// Anthropic message, so the client must see 502 — not a 200 carrying an
// Anthropic error envelope, which reads as a successful turn.
func TestGatewayNoChoicesIsABadGateway(t *testing.T) {
	up := brokenUpstream(t, http.StatusOK, "application/json", `{"id":"chatcmpl-x","choices":[]}`)
	g := messagesGatewayAgainst(t, up)

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("no-choices upstream: status = %d (body %s), want %d",
			w.Code, strings.TrimSpace(w.Body.String()), http.StatusBadGateway)
	}
}

// TestGatewayUnparseableUpstreamBodyIsABadGateway is the same defect one step
// earlier: the body is not JSON at all, so there is no completion to translate.
func TestGatewayUnparseableUpstreamBodyIsABadGateway(t *testing.T) {
	up := brokenUpstream(t, http.StatusOK, "application/json", "<html>gateway timeout</html>")
	g := messagesGatewayAgainst(t, up)

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusBadGateway {
		t.Fatalf("unparseable upstream body: status = %d (body %s), want %d",
			w.Code, strings.TrimSpace(w.Body.String()), http.StatusBadGateway)
	}
}

// TestGatewayEmptyUpstreamStreamIsNeverASilent200 covers the streaming mirror:
// an upstream that ends without a single event. finishStream's own comment
// says "the client must always get a well-formed end" — a 200 with an empty
// body is not one, because a client reading the stream waits for message_stop.
func TestGatewayEmptyUpstreamStreamIsNeverASilent200(t *testing.T) {
	up := brokenUpstream(t, http.StatusOK, "text/event-stream", "")
	g := messagesGatewayAgainst(t, up)

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	body := w.Body.String()
	if w.Code != http.StatusOK {
		// A non-200 is also a well-formed answer: the client is told the turn
		// failed instead of waiting on a terminator that never comes.
		if w.Code < 400 {
			t.Fatalf("empty upstream stream: status = %d with body %q", w.Code, body)
		}
		return
	}
	if !strings.Contains(body, "message_start") || !strings.Contains(body, "message_stop") {
		t.Fatalf("empty upstream stream: status 200 with body %q — no message_start/message_stop", body)
	}
}

// TestUsageRecorderKeepsBothHalvesOfASplitUsageChunk: an upstream that states
// its prompt count in one chunk and its completion count in another (some
// builds emit a running usage object per chunk) must not lose either half.
// Replacing the struct wholesale bills whichever chunk arrived last.
func TestUsageRecorderKeepsBothHalvesOfASplitUsageChunk(t *testing.T) {
	rec := httptest.NewRecorder()
	u := &usageRecorder{ResponseWriter: rec, status: http.StatusOK, stream: true}

	u.scanSSE([]byte("data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"prompt_tokens\":100}}\n\n"))
	u.scanSSE([]byte("data: {\"choices\":[{\"delta\":{}}],\"usage\":{\"completion_tokens\":7}}\n\n"))

	if u.usage.PromptTokens != 100 {
		t.Errorf("prompt_tokens = %d, want 100 (a later chunk zeroed a stated count)", u.usage.PromptTokens)
	}
	if u.usage.CompletionTokens != 7 {
		t.Errorf("completion_tokens = %d, want 7", u.usage.CompletionTokens)
	}
}

// TestUsageRecorderBelievesAFullRestatement is the control: when a chunk
// states a complete usage object, its values win — the merge only protects a
// field the chunk does not speak to, it does not pin the first value seen.
func TestUsageRecorderBelievesAFullRestatement(t *testing.T) {
	rec := httptest.NewRecorder()
	u := &usageRecorder{ResponseWriter: rec, status: http.StatusOK, stream: true}

	u.scanSSE([]byte("data: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":7,\"prompt_cache_hit_tokens\":40}}\n\n"))
	u.scanSSE([]byte("data: {\"usage\":{\"prompt_tokens\":120,\"completion_tokens\":9}}\n\n"))

	var got usage
	got = u.usage
	if got.PromptTokens != 120 || got.CompletionTokens != 9 {
		t.Errorf("usage = %+v, want prompt 120 completion 9", got)
	}
	if got.PromptCacheHitTokens != 40 {
		t.Errorf("prompt_cache_hit_tokens = %d, want the stated 40 to survive a chunk that stays silent about it", got.PromptCacheHitTokens)
	}
}

// TestGatewayKeepsACacheHitALaterChunkOmits is the client-facing half of the
// merge above: the closing chunk states the prompt and completion counts but
// says nothing about the cache, and the earlier chunk's hit must survive into
// message_delta rather than reading as a 0% hit.
func TestGatewayKeepsACacheHitALaterChunkOmits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":10000,\"prompt_tokens_details\":{\"cached_tokens\":9500}}}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":50}}\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	g := messagesGatewayAgainst(t, srv)

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	body := w.Body.String()
	if !strings.Contains(body, `"cache_read_input_tokens":9500`) {
		t.Fatalf("message_delta lost the stated hit: body = %s", body)
	}
	if !strings.Contains(body, `"input_tokens":500`) {
		t.Fatalf("input_tokens should be the uncached 500: body = %s", body)
	}
}

// TestGatewayLedgerKeepsACacheHitALaterChunkOmits is the billed half of the
// same merge. The ledger's cached_tokens is what the cost math subtracts from
// the prompt, so losing it bills the whole prompt at the fresh rate — the
// outcome round 23's commit claimed to have stopped.
func TestGatewayLedgerKeepsACacheHitALaterChunkOmits(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}],\"usage\":{\"prompt_tokens\":10000,\"prompt_tokens_details\":{\"cached_tokens\":9500}}}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":50}}\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)

	ledger := t.TempDir() + "/ledger.jsonl"
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: srv.URL, ListenAddr: ":0", LedgerPath: ledger,
		APIKeys: []gwKey{{SHA256: keyHash("sk-test"), Label: "test"}},
		Models: []gwModel{{
			ID: "oaica-35b-a3b-vision", UpstreamID: "oaica-35b-a3b-vision", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	raw, err := os.ReadFile(ledger)
	if err != nil {
		t.Fatalf("ledger: %v", err)
	}
	if !strings.Contains(string(raw), `"cached_tokens":9500`) {
		t.Fatalf("ledger lost the stated hit: %s", strings.TrimSpace(string(raw)))
	}
}

// TestGatewayCachedTokensANegativeDetailsFallsBackToTheSibling: a malformed
// negative details count must not suppress the sibling spelling that states
// the hit — the clamp used to run after the fallback, so the pair ended at 0
// and the whole prompt billed fresh.
func TestGatewayCachedTokensANegativeDetailsFallsBackToTheSibling(t *testing.T) {
	u := usage{
		PromptTokens:         10000,
		PromptCacheHitTokens: 9500,
		PromptTokensDetails: &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: -5000},
	}
	if got := u.cachedTokens(); got != 9500 {
		t.Fatalf("cachedTokens = %d, want the sibling's 9500", got)
	}
}

var _ = json.Marshal
