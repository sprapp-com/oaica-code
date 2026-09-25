package launch

// proxy_usage_test.go — what the proxy reports as usage when the upstream
// reports none, and when its two cache-hit field spellings disagree.
//
// Both matter for the same reason: the client (Claude Code, via the
// Anthropic SDK) sizes its context accounting and its auto-compaction on the
// usage the proxy hands it. A hard zero is read as fact — a session that
// never appears to grow, so it never compacts, until the request hits the
// 262k wall (the 2026-08-30 incident this file's neighbours describe). And a
// zeroed cache count tells the user their prompt was 100% uncached when the
// upstream said otherwise.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// jsonUpstream answers every request with one complete non-streaming body.
func jsonUpstream(t *testing.T, body string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
	}))
}

// deltaUsage returns message_delta.usage from an SSE body.
func deltaUsage(t *testing.T, body string) map[string]any {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev map[string]any
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev); err != nil {
			continue
		}
		if ev["type"] == "message_delta" {
			u, _ := ev["usage"].(map[string]any)
			return u
		}
	}
	return nil
}

func usageInt(u map[string]any, key string) int {
	if u == nil {
		return -1
	}
	f, ok := u[key].(float64)
	if !ok {
		return -1
	}
	return int(f)
}

// An upstream that never sends the usage chunk (it ignores
// stream_options.include_usage, or a gateway strips it) must not be reported
// as a zero-token turn: the client believes that number, so a session that
// really did grow looks flat and auto-compaction never fires.
func TestProxyStream_NoUsageChunkStillReportsAUsableInputCount(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"), true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-no-usage")
	body, status := postMessagesStream(t, proxy)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}
	in := usageInt(deltaUsage(t, body), "input_tokens")
	if in <= 0 {
		t.Errorf("input_tokens = %d for a turn whose prompt was real; the client reads that as an empty context\n%s", in, body)
	}
}

// The same for a non-streaming upstream that sends no usage object at all.
func TestProxyNonStream_NoUsageObjectStillReportsAUsableInputCount(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}]}`)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-nostream-no-usage")
	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var got struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	if got.Usage.InputTokens <= 0 {
		t.Errorf("usage.input_tokens = %d for a turn whose prompt was real\n%s", got.Usage.InputTokens, b)
	}
}

// The two spellings of the cache-hit count: an upstream that sends BOTH keys
// with the details object present but zeroed has still told us 4096 tokens
// were cached. Keying the fallback on the details object's presence read that
// as 0% cached.
func TestCachedTokens_PrefersTheNonZeroSpelling(t *testing.T) {
	u := &openAIUsage{
		PromptTokens:         5000,
		PromptCacheHitTokens: 4096,
	}
	u.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 0}
	if got := u.cachedTokens(); got != 4096 {
		t.Errorf("cachedTokens = %d, want 4096 (the sibling field the upstream did populate)", got)
	}

	// The populated details object still wins — it is the more specific
	// spelling, and the one the fleet's own gateway emits.
	u2 := &openAIUsage{PromptTokens: 5000, PromptCacheHitTokens: 4096}
	u2.PromptTokensDetails = &struct {
		CachedTokens int `json:"cached_tokens"`
	}{CachedTokens: 1000}
	if got := u2.cachedTokens(); got != 1000 {
		t.Errorf("cachedTokens = %d, want the details object's 1000", got)
	}
}
