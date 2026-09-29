package server

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/fs/ggml"
	"github.com/ollama/ollama/llm"
	"github.com/ollama/ollama/middleware"
)

func TestRound119FollowUpThinkingRelax(t *testing.T) {
	t.Setenv("OLLAMA_CONTEXT_LENGTH", "4096")
	t.Setenv("OLLAMA_GO_TEMPLATE", "")
	t.Setenv("OLLAMA_NO_CLOUD", "")
	gin.SetMode(gin.TestMode)
	setTestHome(t, t.TempDir())

	var calls atomic.Int32
	runner := &mockRunner{
		ChatFn: func(_ context.Context, _ llm.ChatRequest, fn func(llm.ChatResponse)) error {
			n := calls.Add(1)
			if n == 1 {
				args := api.NewToolCallFunctionArguments()
				args.Set("query", "latest news")
				fn(llm.ChatResponse{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "call_ws_1", Function: api.ToolCallFunction{Name: "web_search", Arguments: args}}}},
					Done: true, DoneReason: llm.DoneReasonStop})
				return nil
			}
			fn(llm.ChatResponse{Message: api.Message{Role: "assistant", Content: "final answer"}, Done: true, DoneReason: llm.DoneReasonStop})
			return nil
		},
	}
	s := newServerWithMockRunner(t, runner)
	createMinimalGGUFModel(t, s, "p119-nothink", ggml.KV{"tokenizer.chat_template": "{{ messages[0]['content'] }}{# tools tool_calls tool_response #}"}, "", nil)

	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(anthropic.OllamaWebSearchResponse{Results: []anthropic.OllamaWebSearchResult{{Title: "R", URL: "https://ok.example/x", Content: "c"}}})
	}))
	defer search.Close()
	orig := anthropic.WebSearchEndpoint
	anthropic.WebSearchEndpoint = search.URL
	defer func() { anthropic.WebSearchEndpoint = orig }()

	r := gin.New()
	r.POST("/v1/messages", middleware.AnthropicMessagesMiddleware(), s.ChatHandler)
	r.POST("/api/chat", s.ChatHandler)
	middleware.SetFollowUpHandler(r)
	srv := httptest.NewServer(r)
	defer srv.Close()
	t.Setenv("OLLAMA_HOST", srv.URL)

	{
		resp, _ := http.Post(srv.URL+"/api/chat", "application/json", strings.NewReader(`{"model":"p119-nothink","stream":false,"think":true,"messages":[{"role":"user","content":"news"}]}`))
		b, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		t.Logf("follow-up shape on /api/chat (think copied) -> %d %s", resp.StatusCode, b)
	}
	for _, think := range []string{"", `"thinking":{"type":"enabled","budget_tokens":1024},`} {
		for _, stream := range []string{"false", "true"} {
			calls.Store(0)
			body := `{"model":"p119-nothink","max_tokens":64,"stream":` + stream + `,` + think +
				`"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
			resp, err := http.Post(srv.URL+"/v1/messages", "application/json", strings.NewReader(body))
			if err != nil {
				t.Fatal(err)
			}
			b, _ := io.ReadAll(resp.Body)
			resp.Body.Close()
			out := string(b)
			if len(out) > 700 {
				out = out[len(out)-700:]
			}
			t.Logf("think=%v stream=%s -> %d runnerCalls=%d\n%s", think != "", stream, resp.StatusCode, calls.Load(), out)
			if calls.Load() != 2 || !strings.Contains(string(b), "final answer") {
				t.Errorf("think=%v stream=%s: the turn was not answered (runner calls %d)", think != "", stream, calls.Load())
			}
		}
	}
}
