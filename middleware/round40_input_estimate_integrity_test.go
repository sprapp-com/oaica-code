package middleware

// round40_input_estimate_integrity_test.go — C40-6. The local leg's client is
// told the prompt size this handler worked out for the request whenever the
// upstream states none, because a session whose context accounting reads 0
// never grows and never auto-compacts. Two shapes missed it: the non-stream
// body (which applied no estimate at all) and the web-search turn's terminal
// message_delta (which wrote input_tokens:0 OVER the estimate message_start had
// already stated).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// TestASilentNonStreamTurnReportsTheEstimate is C40-6's first shape: the
// upstream states no metrics, so the whole turn's prompt count is this
// handler's own reading — reported as 0, the session's meter never grew.
func TestASilentNonStreamTurnReportsTheEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		resp := api.ChatResponse{
			Model:      "test-model",
			Message:    api.Message{Role: "assistant", Content: "Hello there!"},
			Done:       true,
			DoneReason: "stop",
			// No Metrics: nothing stated about the prompt.
		}
		data, _ := json.Marshal(resp)
		c.Writer.WriteHeader(http.StatusOK)
		_, _ = c.Writer.Write(data)
	})

	body := `{"model":"test-model","max_tokens":100,"messages":[{"role":"user","content":"Hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}
	var result anthropic.MessagesResponse
	if err := json.Unmarshal(resp.Body.Bytes(), &result); err != nil {
		t.Fatalf("failed to unmarshal response: %v", err)
	}

	// The independent expectation: the request as the estimator sees it. The
	// count is what this handler seeds the streaming converter with, and the
	// same turn must read the same way whether it was streamed or not.
	want := anthropic.EstimateInputTokens(anthropic.MessagesRequest{
		Model: "test-model",
		Messages: []anthropic.MessageParam{{
			Role:    "user",
			Content: []anthropic.ContentBlock{{Type: "text", Text: localStrPtr("Hi")}},
		}},
	})
	if result.Usage.InputTokens != want {
		t.Errorf("input_tokens = %d for a non-stream turn whose upstream stated no metrics, want the estimate %d: the estimate exists to fill the upstream's silence, and a context meter that reads 0 never fires auto-compaction", result.Usage.InputTokens, want)
	}
}

// TestASilentWebSearchDeltaDoesNotEraseTheSeededEstimate is C40-6's second
// shape: message_start already stated the estimate, and the terminal
// message_delta wrote 0 over it — the client SDK accumulates input_tokens off
// message_delta whenever the field is present, so the turn's size was reset to
// nothing after the client had been told it.
func TestASilentWebSearchDeltaDoesNotEraseTheSeededEstimate(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)

	// The model must actually CALL web_search, or the turn never reaches
	// WebSearchAnthropicWriter.writeTerminalResponse — the plain passthrough
	// converter keeps the estimate on its own (round 38) and the terminal
	// event under test is never written.
	followupServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := api.ChatResponse{
			Model:      "test-model",
			Message:    api.Message{Role: "assistant", Content: "After search."},
			Done:       true,
			DoneReason: "stop",
			// No Metrics: the followup states nothing about its prompt either.
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followupServer.Close()
	t.Setenv("OLLAMA_HOST", followupServer.URL)

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := anthropic.OllamaWebSearchResponse{
			Results: []anthropic.OllamaWebSearchResult{
				{Title: "Result", URL: "https://example.com", Content: "content"},
			},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer searchServer.Close()
	originalEndpoint := anthropic.WebSearchEndpoint
	anthropic.WebSearchEndpoint = searchServer.URL
	defer func() { anthropic.WebSearchEndpoint = originalEndpoint }()

	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		chunks := []api.ChatResponse{
			{Model: "test-model", Message: api.Message{Role: "assistant", Content: "Let me look. "}},
			{
				Model: "test-model",
				Message: api.Message{
					Role: "assistant",
					ToolCalls: []api.ToolCall{{
						ID: "call_ws_silent_usage",
						Function: api.ToolCallFunction{
							Name:      "web_search",
							Arguments: makeArgs("query", "silent usage"),
						},
					}},
				},
				Done: false,
			},
			{
				Model:      "test-model",
				Message:    api.Message{Role: "assistant", Content: ""},
				Done:       true,
				DoneReason: "stop",
				// No Metrics: nothing stated anywhere in the turn.
			},
		}
		c.Writer.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = c.Writer.Write(data)
		}
	})

	body := `{
		"model":"test-model:cloud",
		"max_tokens":100,
		"stream":true,
		"messages":[{"role":"user","content":"Hi"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]
	}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	start, delta := 0, -1
	for _, e := range parseSSEEvents(t, resp.Body.String()) {
		switch e.event {
		case "message_start":
			var ev anthropic.MessageStartEvent
			if err := json.Unmarshal([]byte(e.data), &ev); err == nil {
				start = ev.Message.Usage.InputTokens
			}
		case "message_delta":
			var ev anthropic.MessageDeltaEvent
			if err := json.Unmarshal([]byte(e.data), &ev); err == nil {
				delta = ev.Usage.InputTokens
			}
		}
	}
	if start <= 0 {
		t.Fatalf("message_start stated input_tokens=%d for a turn whose prompt was real: the estimate that fills an upstream's silence did not reach the client at all", start)
	}
	if delta != start {
		t.Errorf("message_delta states input_tokens=%d after message_start stated %d: the terminal event erases the size the client was already told — a turn that grew to %d tokens reads as an empty one, and the session's context meter resets mid-turn", delta, start, start)
	}
}

func localStrPtr(s string) *string { return &s }
