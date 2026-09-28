package middleware

// round69_stream_estimate_presence_integrity_test.go — the THIRD estimate site.
//
// Round 68 made the "fill in the estimate only where the upstream stated
// nothing" predicate a PRESENCE test at two of the three sites that carry it
// (middleware/anthropic.go's withInputEstimate and anthropic/anthropic.go's
// converter) on the rule that a stated zero is a reading. The third site,
// WebSearchAnthropicWriter.ensureStreamMessageStart, kept asking the cache
// field BY VALUE — and because writeTerminalResponse runs withInputEstimate
// before it, that arm is the only way the site's branch is ever reached. So on
// the wire its own comment calls real — an upstream stating a zero cache read
// and no prompt count — one streamed turn told the client the ESTIMATE in
// message_start (82) and the upstream's stated 0 in the message_delta eight
// lines later, while the whole-document arm of the same handler answered 0
// (2026-09-28 audit, round 69, F69-L1-1).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/anthropic"
)

// TestTheStreamEstimateSiteHonoursAStatedZero drives the web-search streaming
// route with a terminal usage of {input_tokens: 0, cache_read_input_tokens: 0}.
// PromptEvalCount carries omitempty and the cache count is a pointer, so a
// stated zero cache read is the only encoding in which that field arrives
// alone.
func TestTheStreamEstimateSiteHonoursAStatedZero(t *testing.T) {
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)

	zero := 0
	followupServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := api.ChatResponse{
			Model:      "test-model",
			Message:    api.Message{Role: "assistant", Content: "After search."},
			Done:       true,
			DoneReason: "stop",
			Metrics:    api.Metrics{PromptEvalCachedCount: &zero},
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
			// The call arrives on the FIRST chunk, before the stream has been
			// seeded: a leading chunk with nothing in it is passed through and
			// the inner converter opens the stream itself, which is a different
			// site with a different reading of the same estimate.
			{
				Model: "test-model",
				Message: api.Message{
					Role: "assistant",
					ToolCalls: []api.ToolCall{{
						ID: "call_ws_zero",
						Function: api.ToolCallFunction{
							Name:      "web_search",
							Arguments: makeArgs("query", "q"),
						},
					}},
				},
				Done: false,
			},
			{
				Model:      "test-model",
				Message:    api.Message{Role: "assistant"},
				Done:       true,
				DoneReason: "stop",
				Metrics:    api.Metrics{PromptEvalCachedCount: &zero},
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
		"messages":[{"role":"user","content":"What is the latest news?"}],
		"tools":[{"type":"web_search_20250305","name":"web_search"}]
	}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	resp := httptest.NewRecorder()
	router.ServeHTTP(resp, req)

	if resp.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d: %s", resp.Code, resp.Body.String())
	}

	var startInput, deltaInput *int
	for _, ev := range parseSSEEvents(t, resp.Body.String()) {
		switch ev.event {
		case "message_start":
			var e anthropic.MessageStartEvent
			if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
				t.Fatalf("message_start: %v", err)
			}
			v := e.Message.Usage.InputTokens
			startInput = &v
		case "message_delta":
			var e anthropic.MessageDeltaEvent
			if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
				t.Fatalf("message_delta: %v", err)
			}
			v := e.Usage.InputTokens
			deltaInput = &v
		}
	}
	if startInput == nil || deltaInput == nil {
		t.Fatalf("expected message_start and message_delta, got %v", eventNames(parseSSEEvents(t, resp.Body.String())))
	}
	if *deltaInput != 0 {
		t.Fatalf("message_delta input_tokens = %d, want the upstream's stated 0 — the fixture is not exercising the site", *deltaInput)
	}
	if *startInput != 0 {
		t.Errorf("message_start input_tokens = %d, want the upstream's stated 0: a stated zero is a reading, and the estimate must not overwrite it. The same turn's message_delta states 0, so the SSE wire contradicts itself, and the whole-document arm of the same handler answers 0 (2026-09-28 audit, round 69, F69-L1-1)\n%s", *startInput, resp.Body.String())
	}
}
