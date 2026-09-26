package main

// messages_cache_read_integrity_test.go — the gateway's two usage copies lost
// the cache-read handling the client proxy had already been fixed for
// (2026-09-27 audit, round 23).
//
// Anthropic's contract — stated in this repo at anthropic/anthropic.go,
// "input_tokens is the uncached prompt, cache_read_input_tokens the prefix
// served from cache" — is what the /v1/messages bridge translates INTO, and
// the bridge never emitted the cache fields at all: an upstream that served
// 9500 of a 10000-token prompt from its prefix cache was reported to the
// client as input_tokens=10000 with no cache_read, i.e. a 95% hit read as a 0%
// hit, and any consumer that treats input_tokens as fresh input over-counted.
// cmd/launch's client proxy has translated exactly this body correctly since
// 2026-09-26 (anthropic_openai_proxy.go: usage.CacheReadInputTokens =
// cachedTokens(), input = prompt - cached); the server-side bridge did not.
//
// The ledger's own cachedTokens() is the second copy, and it lacked both
// protections the client one carries: the prompt_cache_hit_tokens fallback
// (some builds emit only that spelling) and the negative clamp. In the ledger
// the number is billed, so a build that states its hit under the sibling name
// bills the whole prompt at the fresh rate and a malformed negative hit
// INFLATES the fresh count.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// cachedUsageUpstream answers one Anthropic-facing request with an
// OpenAI-shaped body whose usage states a prompt-cache hit.
func cachedUsageUpstream(t *testing.T, cachedField string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		json.Unmarshal(b, &body)
		stream, _ := body["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"chatcmpl-c","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":10000,"completion_tokens":50,`+cachedField+`}}`)
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		f.Flush()
		io.WriteString(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":10000,\"completion_tokens\":50,"+cachedField+"}}\n\n")
		f.Flush()
		io.WriteString(w, "data: [DONE]\n\n")
		f.Flush()
	}))
	t.Cleanup(srv.Close)
	return srv
}

func messagesGatewayAgainst(t *testing.T, up *httptest.Server) *gateway {
	t.Helper()
	g := &gateway{}
	if err := g.apply(gwConfig{
		UpstreamAddr: up.URL, ListenAddr: ":0",
		LedgerPath: t.TempDir() + "/ledger.jsonl",
		APIKeys:    []gwKey{{SHA256: keyHash("sk-test"), Label: "test"}},
		Models: []gwModel{{
			ID: "oaica-35b-a3b-vision", UpstreamID: "oaica-35b-a3b-vision", OwnedBy: "oaica",
			ContextLength: 262144, MaxCompletionTokens: 32768,
		}},
	}); err != nil {
		t.Fatalf("apply: %v", err)
	}
	return g
}

func TestMessagesBridgeRelaysTheCacheReadSplit(t *testing.T) {
	up := cachedUsageUpstream(t, `"prompt_tokens_details":{"cached_tokens":9500}`)
	g := messagesGatewayAgainst(t, up)

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
	}
	var resp struct {
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			OutputTokens        int `json:"output_tokens"`
			CacheReadInputToken int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not Anthropic JSON: %v\n%s", err, w.Body.String())
	}
	if resp.Usage.InputTokens != 500 {
		t.Errorf("input_tokens = %d, want 500 — Anthropic's contract is that input_tokens is the UNCACHED prompt; the 9500 cached tokens belong in cache_read_input_tokens, and reporting them as fresh input tells the client a 95%% cache hit was a miss", resp.Usage.InputTokens)
	}
	if resp.Usage.CacheReadInputToken != 9500 {
		t.Errorf("cache_read_input_tokens = %d, want 9500 — the upstream stated the hit and the bridge dropped it", resp.Usage.CacheReadInputToken)
	}
	if resp.Usage.OutputTokens != 50 {
		t.Errorf("output_tokens = %d, want 50", resp.Usage.OutputTokens)
	}
	if sum := resp.Usage.InputTokens + resp.Usage.CacheReadInputToken; sum != 10000 {
		t.Errorf("input+cache_read = %d, want the upstream's 10000 — the two fields must partition the prompt, never lose or duplicate it", sum)
	}
}

func TestMessagesStreamBridgeRelaysTheCacheReadSplit(t *testing.T) {
	up := cachedUsageUpstream(t, `"prompt_tokens_details":{"cached_tokens":9500}`)
	g := messagesGatewayAgainst(t, up)

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d", w.Code)
	}
	body := w.Body.String()
	if !strings.Contains(body, `"input_tokens":500`) {
		t.Errorf("streamed message_delta does not carry the uncached count:\n%s", body)
	}
	if !strings.Contains(body, `"cache_read_input_tokens":9500`) {
		t.Errorf("streamed message_delta dropped the cache-read count the upstream stated:\n%s", body)
	}
}

// TestMessagesBridgeKeepsAnUncachedTurnWhole is the control: an upstream that
// states no cache hit keeps the whole prompt as fresh input.
func TestMessagesBridgeKeepsAnUncachedTurnWhole(t *testing.T) {
	up := cachedUsageUpstream(t, `"prompt_tokens_details":{"cached_tokens":0}`)
	g := messagesGatewayAgainst(t, up)

	w := postMessages(t, g, "sk-test", map[string]any{
		"model": "oaica-35b-a3b-vision", "max_tokens": 100,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	var resp struct {
		Usage struct {
			InputTokens         int `json:"input_tokens"`
			CacheReadInputToken int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("response not Anthropic JSON: %v\n%s", err, w.Body.String())
	}
	if resp.Usage.InputTokens != 10000 {
		t.Errorf("input_tokens = %d, want the whole prompt 10000 for a turn with no cache hit", resp.Usage.InputTokens)
	}
	if resp.Usage.CacheReadInputToken != 0 {
		t.Errorf("cache_read_input_tokens = %d, want 0 — an upstream that states no hit must not be given one", resp.Usage.CacheReadInputToken)
	}
}

func TestGatewayCachedTokensHonoursTheSiblingSpelling(t *testing.T) {
	// Some builds state the hit as prompt_cache_hit_tokens (DeepSeek's
	// spelling) rather than under prompt_tokens_details. Reading only the
	// details object bills every one of those tokens at the fresh rate.
	got := usage{PromptTokens: 10000, PromptCacheHitTokens: 9500}.cachedTokens()
	if got != 9500 {
		t.Errorf("cachedTokens() = %d, want 9500 — the sibling spelling prompt_cache_hit_tokens carries exactly this count", got)
	}
}

func TestGatewayCachedTokensClampsANegativeUpstream(t *testing.T) {
	u := usage{PromptTokens: 10000}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: -5000}
	if got := u.cachedTokens(); got != 0 {
		t.Errorf("cachedTokens() = %d, want 0 — a negative hit is not a count, and subtracting it bills prompt - (-5000) = 15000 fresh tokens for a 10000-token prompt", got)
	}
}
