package launch

// proxy_sse_error_frame_integrity_test.go — a mid-stream failure delivered as
// an ordinary SSE error frame was swallowed, and the client was told only
// "stream ended before the response was complete" (2026-09-26 audit, seventh
// round).
//
// The recognition site exists for exactly this frame — vLLM and this fleet's
// own gateway report a mid-stream failure as `data: {"error": {...}}` — but it
// sat in the branch that runs when json.Unmarshal FAILS. An error object like
// that parses perfectly into openAIStreamChunk: the struct has no error field,
// Choices comes back empty, the loop body does nothing, and the scan reached
// the end of the stream with an empty upstreamErr. The user then got the
// generic incomplete-stream message, with the upstream's actual reason — rate
// limit, context overflow, model not found — discarded.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// sseUpstream serves one SSE frame, then a JSON error frame, then EOF — a
// stream that opens, then fails, with no [DONE].
func sseUpstream(t *testing.T, errorFrame string) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\"}}]}\n\n")
		if flusher != nil {
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: "+errorFrame+"\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestAMidStreamSSEErrorFrameReachesTheClient(t *testing.T) {
	const detail = "the upstream rate limited this request (429 too many requests)"
	frame, err := json.Marshal(map[string]any{
		"error": map[string]any{"type": "rate_limit_error", "message": detail},
	})
	if err != nil {
		t.Fatal(err)
	}
	srv := sseUpstream(t, string(frame))
	proxy := startCalibProxy(t, srv.URL, "sess-sse-error-frame")

	status, body := proxyClientErrorText(t, proxy, true)
	if !strings.Contains(body, "rate limited this request") {
		t.Errorf("the upstream's mid-stream error frame was discarded (HTTP %d):\n%s\n— the frame parsed into a chunk with no choices, so the error branch below it never ran and the user is told only that the stream ended early, with the actual reason (rate limit, context overflow, model not found) lost", status, body)
	}
}

// The control: a mid-stream error that arrives as a frame that does NOT parse
// as JSON is still reported — that path worked and must keep working.
func TestAMalformedSSEFrameIsStillReported(t *testing.T) {
	srv := sseUpstream(t, `{"error":{"message":"upstream exploded`+"\x00"+`"}}not-json`)
	proxy := startCalibProxy(t, srv.URL, "sess-sse-bad-frame")

	_, body := proxyClientErrorText(t, proxy, true)
	if !strings.Contains(body, "error") && !strings.Contains(body, "complete") {
		t.Errorf("a mid-stream failure produced no error text at all:\n%s", body)
	}
}

// And a stream that ends normally must still be reported as normal: the error
// probe must not fire on ordinary frames.
func TestAnOrdinaryStreamStillCompletesCleanly(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher, _ := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello there\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		if flusher != nil {
			flusher.Flush()
		}
	}))
	t.Cleanup(srv.Close)
	proxy := startCalibProxy(t, srv.URL, "sess-sse-clean")

	status, body := proxyClientErrorText(t, proxy, true)
	if status != http.StatusOK {
		t.Errorf("a clean stream answered HTTP %d:\n%s", status, body)
	}
	if strings.Contains(body, "stream ended before the response was complete") {
		t.Errorf("a stream with a finish_reason and a [DONE] was reported as incomplete:\n%s", body)
	}
}
