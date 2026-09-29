package middleware

// round108_leg1_web_search_limits_test.go — leg 1, round 108 (2026-09-29 audit),
// F108-L1-3.
//
// An Anthropic web_search tool states its own limits: `max_uses` (the most
// searches this request may run — a cost and rate cap) and `allowed_domains` /
// `blocked_domains` (which sites may reach the model). The :cloud loop ran a fixed
// three searches whatever the tool said, and decoded no domain list at all, so the
// client's cap was exceeded and a blocked site reached the model and the client.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// r108SearchTurn runs one cloud turn whose model keeps asking for web_search and
// whose search backend answers the given results. It returns the searches run
// and the client's body.
func r108SearchTurn(t *testing.T, tool string, results []anthropic.OllamaWebSearchResult) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)
	followup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ChatResponse{Model: "test-model",
			Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}},
			Done:    true, DoneReason: "stop"})
	}))
	t.Cleanup(followup.Close)
	t.Setenv("OLLAMA_HOST", followup.URL)
	var hits int32
	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_ = json.NewEncoder(w).Encode(anthropic.OllamaWebSearchResponse{Results: results})
	}))
	t.Cleanup(search.Close)
	orig := anthropic.WebSearchEndpoint
	anthropic.WebSearchEndpoint = search.URL
	t.Cleanup(func() { anthropic.WebSearchEndpoint = orig })

	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		data, _ := json.Marshal(api.ChatResponse{Model: "test-model",
			Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}},
			Done:    true, DoneReason: "stop"})
		c.Writer.Write(data)
	})
	body := `{"model":"test-model:cloud","max_tokens":100,"messages":[{"role":"user","content":"news"}],"tools":[` + tool + `]}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return int(atomic.LoadInt32(&hits)), rec.Body.String()
}

// TestMine108AWebSearchToolsMaxUsesIsTheLoopsBound is F108-L1-3's pin for
// max_uses: fewer than the loop's own bound, and never more.
func TestMine108AWebSearchToolsMaxUsesIsTheLoopsBound(t *testing.T) {
	ok := []anthropic.OllamaWebSearchResult{{Title: "R", URL: "https://ok.example/x", Content: "c"}}
	for _, c := range []struct {
		name, tool string
		want       int
	}{
		{"max_uses 1", `{"type":"web_search_20250305","name":"web_search","max_uses":1}`, 1},
		{"max_uses 2", `{"type":"web_search_20250305","name":"web_search","max_uses":2}`, 2},
		{"unstated", `{"type":"web_search_20250305","name":"web_search"}`, maxWebSearchLoops},
		{"above the bound", `{"type":"web_search_20250305","name":"web_search","max_uses":99}`, maxWebSearchLoops},
	} {
		hits, body := r108SearchTurn(t, c.tool, ok)
		if hits != c.want {
			t.Errorf("%s: %d searches ran, want %d (2026-09-29 audit, round 108, F108-L1-3)", c.name, hits, c.want)
		}
		if !strings.Contains(body, "max_uses_exceeded") {
			t.Errorf("%s: the client was not told the search budget ran out: %s", c.name, body)
		}
	}
}

// TestMine108AWebSearchToolsDomainListsFilterItsResults is F108-L1-3's pin for the
// domain lists.
func TestMine108AWebSearchToolsDomainListsFilterItsResults(t *testing.T) {
	res := []anthropic.OllamaWebSearchResult{
		{Title: "B", URL: "https://blocked.example/x", Content: "c"},
		{Title: "S", URL: "https://sub.blocked.example/y", Content: "c"},
		{Title: "K", URL: "https://keep.example/z", Content: "c"},
	}
	_, body := r108SearchTurn(t, `{"type":"web_search_20250305","name":"web_search","max_uses":1,"blocked_domains":["blocked.example"]}`, res)
	if strings.Contains(body, "blocked.example") || !strings.Contains(body, "keep.example") {
		t.Errorf("blocked_domains: the client's body is %s, want keep.example and neither blocked.example nor its subdomain (2026-09-29 audit, round 108, F108-L1-3)", body)
	}
	_, body = r108SearchTurn(t, `{"type":"web_search_20250305","name":"web_search","max_uses":1,"allowed_domains":["keep.example"]}`, res)
	if strings.Contains(body, "blocked.example") || !strings.Contains(body, "keep.example") {
		t.Errorf("allowed_domains: the client's body is %s, want only keep.example (2026-09-29 audit, round 108, F108-L1-3)", body)
	}
}
