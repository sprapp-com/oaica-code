package server

// round72_upstream_error_frame_route_integrity_test.go — leg 1, the error frame
// the chat path itself writes into a streaming turn (2026-09-28 audit, round 72,
// F72-L1-1).
//
// The middleware pin (middleware/round72_upstream_error_frame_integrity_test.go)
// drives the writer with a hand-built body. This one drives the REAL path:
// writeChatResponse over a channel that carried a chunk and then the gin.H the
// completion error is reported with (routes.go:2954-2957, written by the
// streaming writer at :2149-2166). A turn that dies partway must reach the
// client with the reason, whether it was asked for buffered or streamed.

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

func r72RouteTurn(t *testing.T, stream bool, vals []any) (int, string) {
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

	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	body := `{"model":"test-model","max_tokens":100,"stream":` + streamLit + `,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	// c.Stream needs a real connection, so serve for real.
	srv := httptest.NewServer(router)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// r72RouteStatedError reads the failure sentence out of a body in either
// medium: the whole-document envelope, or the error event of a stream.
func r72RouteStatedError(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err == nil && envelope.Error.Message != "" {
			return envelope.Error.Message
		}
	}
	return ""
}

func TestATurnThatDiesPartwayStatesTheReasonOnBothArms(t *testing.T) {
	const want = "prediction aborted, token repeat limit reached"
	chunk := api.ChatResponse{Model: "test-model",
		Message: api.Message{Role: "assistant", Content: "hello "}}
	dead := gin.H{"error": want}

	docCode, docBody := r72RouteTurn(t, false, []any{chunk, dead})
	if got := r72RouteStatedError(t, docBody); got != want {
		t.Fatalf("PREMISE: the buffered arm refuses this turn with %q, want %q (status %d, body %q)", got, want, docCode, docBody)
	}

	_, streamBody := r72RouteTurn(t, true, []any{chunk, dead})
	if got := r72RouteStatedError(t, streamBody); got != want {
		t.Fatalf("the same turn that died partway gives the client %q when buffered and %q when streamed: the streaming arm ended the turn without saying why (body %q) (2026-09-28 audit, round 72, F72-L1-1)",
			want, got, streamBody)
	}
}
