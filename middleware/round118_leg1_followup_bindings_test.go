// round118_leg1_followup_bindings_test.go — F118-L1-2 (2026-09-29 audit, round 118).

package middleware

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// The first leg and the loop's follow-up leg of ONE web_search turn: what each
// hands the backend.
func TestRound118FollowUpKeepsTheRequestsBindings(t *testing.T) {
	for _, stream := range []bool{false, true} {
		gin.SetMode(gin.TestMode)
		enableCloudForTest(t)

		var followUp api.ChatRequest
		backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &followUp)
			_ = json.NewEncoder(w).Encode(api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", Content: `{"a":1}`}, Done: true, DoneReason: "stop"})
		}))
		t.Setenv("OLLAMA_HOST", backend.URL)
		search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			_ = json.NewEncoder(w).Encode(anthropic.OllamaWebSearchResponse{Results: []anthropic.OllamaWebSearchResult{{Title: "R", URL: "https://ok.example/x", Content: "c"}}})
		}))
		orig := anthropic.WebSearchEndpoint
		anthropic.WebSearchEndpoint = search.URL

		var first api.ChatRequest
		router := gin.New()
		router.Use(AnthropicMessagesMiddleware())
		router.POST("/v1/messages", func(c *gin.Context) {
			b, _ := io.ReadAll(c.Request.Body)
			_ = json.Unmarshal(b, &first)
			data, _ := json.Marshal(api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}}, Done: true, DoneReason: "stop"})
			c.Writer.Write(data)
		})
		s := "false"
		if stream {
			s = "true"
		}
		body := `{"model":"test-model","max_tokens":100,"stream":` + s + `,"thinking":{"type":"disabled"},"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"integer"}},"required":["a"]}}},"messages":[{"role":"user","content":"news"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
		req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)

		fj := func(r api.ChatRequest) string {
			th := "<nil>"
			if r.Think != nil {
				b, _ := json.Marshal(r.Think)
				th = string(b)
			}
			return "think=" + th + " format=" + string(r.Format)
		}
		t.Logf("stream=%v first leg:     %s", stream, fj(first))
		t.Logf("stream=%v follow-up leg: %s", stream, fj(followUp))
		if string(first.Format) != string(followUp.Format) || (first.Think == nil) != (followUp.Think == nil) {
			t.Errorf("RED stream=%v: one request, two legs, two different sets of bindings", stream)
		}
		anthropic.WebSearchEndpoint = orig
		search.Close()
		backend.Close()
	}
}
