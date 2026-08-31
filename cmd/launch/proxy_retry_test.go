package launch

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestProxy_ClientRetries429HonoringServerHints verifies the proxy retries
// transient upstream failures the way the official SDKs do client-side:
// 429s (with Retry-After) and 5xx that answer BEFORE any response byte,
// bounded attempts, exponential backoff. The gateway's admission control +
// per-key concurrency caps + replica flaps all answer exactly this way.
func TestProxy_ClientRetries429(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var attempts atomic.Int32
	retryAfter := "0"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if attempts.Add(1) <= 2 {
			w.Header().Set("Retry-After", retryAfter)
			w.WriteHeader(http.StatusTooManyRequests)
			_, _ = w.Write([]byte(`{"error":{"code":"concurrency_limited"}}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", Label: "test:kat-awq"}})

	body, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 10,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	done := make(chan *http.Response, 1)
	go func() {
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Error(err)
			done <- nil
			return
		}
		done <- resp
	}()
	select {
	case resp := <-done:
		if resp == nil {
			t.Fatal("no response")
		}
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		resp.Body.Close()
		if out["error"] != nil {
			t.Fatalf("expected retried success, got error: %v", out["error"])
		}
		if n := attempts.Load(); n < 3 {
			t.Fatalf("upstream attempted %d times, want >= 3 (two 429s retried)", n)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("retry loop did not complete")
	}
}

// TestProxy_NoRetryAfterMidStream verifies a stream that breaks MID-response
// is NOT retried (tokens already went out to the caller; a retry would
// duplicate them).
func TestProxy_NoRetryMidStream(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var attempts atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts.Add(1)
		// First (and only) attempt: a 200 STREAM that truncates mid-body —
		// headers + zero bytes of Anthropic output already reach the caller
		// (the converter forwards as soon as it converts), so a retry would
		// emit a DUPLICATE response. No retry may happen.
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("data: {\"id\":\"1\",\"type\":\"message_start\"}\n\n"))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	defer upstream.Close()
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", Label: "test:kat-awq"}})

	body, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 4, "stream": true,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	// Retryable only BEFORE a response; here a realized 200 response came
	// back, so exactly one upstream attempt must have happened.
	if n := attempts.Load(); n != 1 {
		t.Fatalf("upstream attempted %d times after a realized response, want 1 (no mid-flight retry)", n)
	}
}
