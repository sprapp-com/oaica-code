package launch

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamResponse_ReportsRealUsageWithCacheRead pins the 2026-08-30 fix:
// a streamed /v1/messages response must carry the upstream's real usage on
// its message_delta event -- input_tokens = uncached prompt tokens,
// cache_read_input_tokens = prefix-cache hits, output_tokens = completion.
// Before the fix the done event was emitted without Metrics, so every
// streamed response said input_tokens=0/output_tokens=0; Claude Code (which
// always streams) therefore never saw its context grow and never
// auto-compacted -- a real .46 session reached 253,958 of 262,144 tokens
// that way and died at the wall.
func TestStreamResponse_ReportsRealUsageWithCacheRead(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const prompt, cached, completion = 12000, 9000, 37
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{"role":"assistant","content":"hello"}}]}`)
		fmt.Fprintf(w, "data: %s\n\n", `{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`)
		fmt.Fprintf(w, "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":%d,\"completion_tokens\":%d,\"total_tokens\":%d,\"prompt_tokens_details\":{\"cached_tokens\":%d}}}\n\n",
			prompt, completion, prompt+completion, cached)
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	defer upstream.Close()

	proxyURL := startCalibProxy(t, upstream.URL, "sess-usage")
	body := calibMessagesBody(t, 2000, 64, true)
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected 200, got %d: %s", resp.StatusCode, raw)
	}

	var delta struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	found := false
	for _, line := range strings.Split(string(raw), "\n") {
		if !strings.HasPrefix(line, "data: ") || !strings.Contains(line, `"message_delta"`) {
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &delta); err != nil {
			t.Fatalf("decode message_delta: %v", err)
		}
		found = true
	}
	if !found {
		t.Fatalf("no message_delta event in stream:\n%s", raw)
	}
	if delta.Usage.InputTokens != prompt-cached || delta.Usage.CacheReadInputTokens != cached || delta.Usage.OutputTokens != completion {
		t.Fatalf("message_delta usage = %+v, want input=%d cache_read=%d output=%d",
			delta.Usage, prompt-cached, cached, completion)
	}
}

// TestNonStreamResponse_SplitsCachedTokens mirrors the stream case for the
// JSON path: input_tokens excludes the cached prefix and
// cache_read_input_tokens carries it, so input+cache_read == prompt.
func TestNonStreamResponse_SplitsCachedTokens(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const prompt, cached, completion = 5000, 4096, 11
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprintf(w, `{"choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":%d,"total_tokens":%d,"prompt_tokens_details":{"cached_tokens":%d}}}`,
			prompt, completion, prompt+completion, cached)
	}))
	defer upstream.Close()

	proxyURL := startCalibProxy(t, upstream.URL, "sess-usage-json")
	body := calibMessagesBody(t, 2000, 64, false)
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out struct {
		Usage struct {
			InputTokens          int `json:"input_tokens"`
			OutputTokens         int `json:"output_tokens"`
			CacheReadInputTokens int `json:"cache_read_input_tokens"`
		} `json:"usage"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Usage.InputTokens != prompt-cached || out.Usage.CacheReadInputTokens != cached || out.Usage.OutputTokens != completion {
		t.Fatalf("usage = %+v, want input=%d cache_read=%d output=%d", out.Usage, prompt-cached, cached, completion)
	}
}
