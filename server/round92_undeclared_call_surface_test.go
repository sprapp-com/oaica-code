package server

// round92_undeclared_call_surface_test.go — leg 1, F92-L1-2 (2026-09-29 audit,
// round 92).
//
// Round 73 taught the buffered writer that the gate hiding a model's tool calls
// from a request that declared none belongs to the NATIVE wire alone, and lifted
// it on the Anthropic surface, whose streaming arm relays the call regardless
// (F73-L1-1). The two OpenAI surfaces were left gated, and the same divergence
// survived there: a model that states a call for a client that declared no tools
// reached the client as prose alone on `/v1/chat/completions` and
// `/v1/responses` when it asked for no stream, and as prose plus an executable
// call when it streamed — one client body, one upstream body, two answers.
// Measured on all eight arms before the fix: native chat buffered/streamed,
// OpenAI chat buffered (no call) / streamed (call), Responses buffered (no
// call) / streamed (call), Anthropic buffered/streamed (call both ways).
//
// The middleware now marks the surface it translates for, and the gate is read
// as what it is — upstream ollama's rule for its own wire.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/middleware"
)

// r92UndeclaredTurn drives the real buffered writer over a channel that carried
// the model's call and then the end of the turn, on the surface the given
// middleware marks (nil for the native wire). The internal request declares no
// tools, which is what a client body with no `tools` converts to.
func r92UndeclaredTurn(t *testing.T, path string, mw gin.HandlerFunc, stream bool) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	if mw != nil {
		router.Use(mw)
	}
	router.POST(path, func(c *gin.Context) {
		req := api.ChatRequest{Stream: &stream}
		args := api.NewToolCallFunctionArguments()
		args.Set("cmd", "ls")
		ch := make(chan any, 2)
		ch <- api.ChatResponse{Model: "test-model", Message: api.Message{
			Role: "assistant",
			ToolCalls: []api.ToolCall{{
				Function: api.ToolCallFunction{Name: "Bash", Arguments: args},
			}},
		}}
		ch <- api.ChatResponse{Model: "test-model", Done: true, DoneReason: "stop",
			Message: api.Message{Role: "assistant"}}
		close(ch)
		writeChatResponse(c, req, ch)
	})

	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	body := `{"model":"test-model","stream":` + streamLit + `,"max_tokens":100,` +
		`"messages":[{"role":"user","content":"list the files"}]}`
	srv := httptest.NewServer(router)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// Every translated surface relays the call on BOTH arms: the client that
// declared no tools is answered the same whether it streamed or not.
func TestATranslatedSurfaceRelaysTheUndeclaredCallOnBothArms(t *testing.T) {
	for _, tc := range []struct {
		name string
		path string
		mw   gin.HandlerFunc
	}{
		{"openai chat", "/v1/chat/completions", middleware.ChatMiddleware()},
		{"responses", "/v1/responses", middleware.ResponsesMiddleware()},
		{"anthropic", "/v1/messages", middleware.AnthropicMessagesMiddleware()},
	} {
		for _, stream := range []bool{false, true} {
			code, body := r92UndeclaredTurn(t, tc.path, tc.mw, stream)
			if code != http.StatusOK {
				t.Errorf("%s (stream=%v): answered %d: %.200s", tc.name, stream, code, body)
				continue
			}
			if !strings.Contains(body, "Bash") {
				t.Errorf("%s (stream=%v): the model's call was dropped — its own other arm relays it (2026-09-29 audit, round 92, F92-L1-2): %.240q",
					tc.name, stream, body)
			}
		}
	}
}

// And the native wire keeps upstream ollama's rule for its own wire, unchanged
// by this: the document arm copies no call the request did not declare. Round 73
// drew that line; this pins that round 92 did not move it.
func TestTheNativeWireKeepsUpstreamsGate(t *testing.T) {
	code, body := r92UndeclaredTurn(t, "/api/chat", nil, false)
	if code != http.StatusOK {
		t.Fatalf("native buffered answered %d: %.200s", code, body)
	}
	if strings.Contains(body, "Bash") {
		t.Errorf("the native buffered arm relayed a call the client never declared: %.240q", body)
	}
}
