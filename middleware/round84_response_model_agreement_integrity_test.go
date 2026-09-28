package middleware

// round84_response_model_agreement_integrity_test.go — leg 1, R84-L1-1
// (2026-09-28 audit, round 84).
//
// One /v1/messages body, two `model` strings. The client asks for a suffixed
// name (`kimi-k2.5:cloud`) and the server rewrites it to the bare upstream name
// before the request leaves, so the ChatResponse that comes back states a model
// the client never asked for. Every site in this middleware that writes a model
// into a reply states the CLIENT's string (w.req.Model) — the streamed arm, the
// web_search loop, the error frames — except the buffered pass-through arm,
// which took whatever the converter read off the upstream response. The same
// body therefore answered with two different models depending on `stream`.
//
// The fix states the client's model on that arm too — the converter already
// carries it, being what the streamed arm's message_start writes.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
)

// r84ModelArm drives one request through the middleware with a client model and
// an upstream model of its own, and reports the model the client is handed.
func r84ModelArm(t *testing.T, clientModel, upstreamModel string, stream bool) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		resp := api.ChatResponse{
			Model:      upstreamModel,
			Message:    api.Message{Role: "assistant", Content: "hello "},
			Done:       true,
			DoneReason: "stop",
			Metrics:    api.Metrics{PromptEvalCount: 12, EvalCount: 5},
		}
		data, _ := json.Marshal(resp)
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write(data)
	})
	body := `{"model":"` + clientModel + `","max_tokens":100,"stream":` +
		map[bool]string{true: "true", false: "false"}[stream] +
		`,"messages":[{"role":"user","content":"hello"}]}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)

	if !stream {
		var msg struct {
			Model string `json:"model"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &msg); err != nil {
			t.Fatalf("the buffered arm is not a message: %v\n%s", err, rec.Body.String())
		}
		return msg.Model
	}
	for _, ev := range parseSSEEvents(t, rec.Body.String()) {
		if ev.event != "message_start" {
			continue
		}
		var msg struct {
			Message struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal([]byte(ev.data), &msg); err != nil {
			t.Fatalf("message_start is not JSON: %v\n%s", err, ev.data)
		}
		return msg.Message.Model
	}
	return "<no message_start>"
}

// TestTheMessageStatesTheClientsModelOnBothArms is R84-L1-1. A suffixed client
// model that the server strips upstream is the ordinary case, and both spellings
// of one body must name the model the client asked for.
func TestTheMessageStatesTheClientsModelOnBothArms(t *testing.T) {
	for _, tc := range []struct {
		note     string
		client   string
		upstream string
	}{
		{"the server strips the suffix upstream", "kimi-k2.5:cloud", "kimi-k2.5"},
		{"the names already agree", "kimi-k2.5", "kimi-k2.5"},
		{"the upstream answers another model entirely", "glm-5.3", "glm-5.3-20260101"},
	} {
		buffered := r84ModelArm(t, tc.client, tc.upstream, false)
		streamed := r84ModelArm(t, tc.client, tc.upstream, true)
		if buffered != tc.client || streamed != tc.client {
			t.Errorf("%s: the client asked for %q and the upstream answered %q — the buffered arm states %q and the streamed arm %q, want %q on both: the message's model is what the client asked for, not what the upstream reported back, and one body may not answer with two models depending on `stream` (2026-09-28 audit, round 84, R84-L1-1)",
				tc.note, tc.client, tc.upstream, buffered, streamed, tc.client)
		}
	}
}

// A second case was written and dropped: the guard against an empty client
// model cannot be reached through this middleware, because a request with no
// model is rejected as invalid before the upstream is called (measured: 400
// invalid_request_error). The guard on the converter being present is likewise
// unreachable from HTTP — the converter is built in the same struct literal as
// the writer — and stays only for writers the web_search tests build by hand.
