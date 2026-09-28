package server

// round86_midstream_error_status_integrity_test.go — leg 1, F86-L1-1 and
// F86-L1-2 (2026-09-28 audit, round 86).
//
// A turn that dies partway pushes a frame the middleware's Anthropic writer
// reads as the upstream's failure — `upstreamErrorFrame` turns it into an
// Anthropic error envelope, and the TYPE of that envelope is chosen from the
// `status` the frame states. The buffered arm always got that field, because a
// request whose turn never reached the wire is answered with
// `c.JSON(status, …)`. The already-written arm encoded only `{"error": …}`, so
// the status defaulted to 500 and the streamed arm could state `api_error` and
// nothing else — one mid-stream 429 reaching a non-streaming client as
// `rate_limit_error` with HTTP 429 and a streaming one as `api_error`, with the
// upstream's request to back off never signalled (F86-L1-1).
//
// F86-L1-2 is the same mid-stream failure's other axis and is RECORDED, not
// fixed: once the streamed arm has put a 200 and content blocks on the wire it
// cannot un-say them, and an Anthropic error document cannot carry content, so
// no change makes the two arms byte-agree there. Measured on 2026-09-28: for
// `[content chunk][error frame]` the buffered arm answers HTTP 500 with the
// sentence and no content, while the streamed arm answers HTTP 200 with the
// content blocks and then the same error event. The two media differ; the types
// the events state do not (this file's pin).

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/middleware"
)

// r86ErrorTurn drives one chunk list through the real chat lane, both arms, and
// reports the HTTP status and the error type the client is handed.
func r86ErrorTurn(t *testing.T, stream bool, vals []any) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(middleware.AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		req := api.ChatRequest{Stream: &stream}
		ch := make(chan any, len(vals))
		for _, v := range vals {
			ch <- v
		}
		close(ch)
		writeChatResponse(c, req, ch)
	})
	lit := "false"
	if stream {
		lit = "true"
	}
	body := `{"model":"test-model","max_tokens":100,"stream":` + lit + `,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	srv := httptest.NewServer(router)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	return resp.StatusCode, string(out)
}

// r86ErrorType reads the Anthropic error type out of either medium: the
// whole-document envelope, or the `error` event of a stream.
func r86ErrorType(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var env struct {
			Error struct {
				Type string `json:"type"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &env); err == nil && env.Error.Type != "" {
			return env.Error.Type
		}
	}
	return "<none>"
}

// TestAMidStreamFailureIsTypedTheSameOnBothArms is the F86-L1-1 pin: the status
// the producer stated decides the error type, and it decides it the same way
// whether or not the turn had already reached the client.
func TestAMidStreamFailureIsTypedTheSameOnBothArms(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		want   string
	}{
		{"a rate limit", http.StatusTooManyRequests, "rate_limit_error"},
		{"an overloaded upstream", http.StatusServiceUnavailable, "overloaded_error"},
		{"the vendor's own overload code", 529, "overloaded_error"},
		{"a bad request", http.StatusBadRequest, "invalid_request_error"},
		{"a missing model", http.StatusNotFound, "not_found_error"},
		{"a prompt too large", http.StatusRequestEntityTooLarge, "request_too_large"},
		{"an untyped failure", http.StatusInternalServerError, "api_error"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			frame := gin.H{"error": "runner died", "status": tc.status}
			// Nothing written yet: the two arms have always agreed here, and the
			// pin keeps it that way.
			_, firstPlain := r86ErrorTurn(t, false, []any{frame})
			_, firstStream := r86ErrorTurn(t, true, []any{frame})
			// Content already relayed: this is the arm the field was dropped on.
			_, afterPlain := r86ErrorTurn(t, false, []any{
				api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", Content: "P1"}}, frame})
			_, afterStream := r86ErrorTurn(t, true, []any{
				api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", Content: "P1"}}, frame})

			for _, arm := range []struct {
				when string
				got  string
			}{
				{"before any content", r86ErrorType(t, firstPlain)},
				{"before any content, streamed", r86ErrorType(t, firstStream)},
				{"after content", r86ErrorType(t, afterPlain)},
				{"after content, streamed", r86ErrorType(t, afterStream)},
			} {
				if arm.got != tc.want {
					t.Errorf("%s: the client was handed %q, want %q — the type of the failure is the status the producer stated, on both arms (2026-09-28 audit, round 86, F86-L1-1)",
						arm.when, arm.got, tc.want)
				}
			}
		})
	}
}
