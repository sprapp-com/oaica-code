package middleware

// round71_takeover_residuals_integrity_test.go — two residuals of the round-70
// takeover fix, both of them the same shape: a decision the streaming arm makes
// from the chunks it has, where the whole-document arm makes it from the whole
// message (2026-09-28 audit, round 71, leg 1).
//
// (1) The round-70 hold lasts exactly ONE chunk. The wire it was written
// against has the client call and the search call adjacent, so the hold never
// had to survive anything — but a model that calls Bash, writes a line of
// prose, and only then searches produces `[call], [text], [call]`, and the
// intervening text chunk (which carries no tool call) released the held call
// with keepCalls=true. The streaming arm then served a tool_use the
// whole-document arm of the same turn drops, which is precisely the F70-L1-1
// symptom arriving through a wire round 70's own test could not spell.
//
// (2) The takeover's ERROR terminal is the one terminal that does not carry the
// turn's narration. Success (writeLoopResult) and max-uses both prepend it; the
// error terminal builds its content list from the search blocks alone, so a
// turn whose search endpoint answered 500 serves the model's prose to streaming
// clients and drops it for everyone else.

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

// r71Turn is r70Turn with the search endpoint's status as a parameter — the
// error terminal only exists on a search failure, and the round-70 harness
// always answered 200.
func r71Turn(t *testing.T, stream bool, chunks []api.ChatResponse, searchStatus int) string {
	t.Helper()
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)

	followup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		resp := api.ChatResponse{
			Model:      "test-model",
			Message:    api.Message{Role: "assistant", Content: "FINAL ANSWER"},
			Done:       true,
			DoneReason: "stop",
			Metrics:    api.Metrics{PromptEvalCount: 50, EvalCount: 17},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer followup.Close()
	t.Setenv("OLLAMA_HOST", followup.URL)

	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if searchStatus != http.StatusOK {
			w.WriteHeader(searchStatus)
			_, _ = w.Write([]byte(`{"error":"boom"}`))
			return
		}
		resp := anthropic.OllamaWebSearchResponse{
			Results: []anthropic.OllamaWebSearchResult{{Title: "R", URL: "https://e.com", Content: "c"}},
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer search.Close()
	orig := anthropic.WebSearchEndpoint
	anthropic.WebSearchEndpoint = search.URL
	defer func() { anthropic.WebSearchEndpoint = orig }()

	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		c.Writer.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = c.Writer.Write(data)
		}
	})

	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	body := `{"model":"test-model:cloud","max_tokens":100,"stream":` + streamLit + `,` +
		`"messages":[{"role":"user","content":"What is the latest news?"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	return rec.Body.String()
}

// TestAHeldCallIsNotReleasedByAnInterveningTextChunk is F71-L1-1.
func TestAHeldCallIsNotReleasedByAnInterveningTextChunk(t *testing.T) {
	streamed := r71Turn(t, true, []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Bash()}}},
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: "Let me check the directory first."}},
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}}},
		r70Stop(),
	}, http.StatusOK)
	whole := r71Turn(t, false, []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: "Let me check the directory first.",
			ToolCalls: []api.ToolCall{r70Bash(), r70Search()}},
			Done: true, DoneReason: "stop", Metrics: api.Metrics{PromptEvalCount: 10, EvalCount: 2}},
	}, http.StatusOK)

	got, want := r70Blocks(t, streamed, true), r70Blocks(t, whole, false)
	for _, b := range got {
		if strings.HasPrefix(b, "tool_use:") {
			t.Errorf("the streaming arm served %q: a text chunk between the client call and the search call released the hold, so the same turn reaches one client with a tool_use the other arm drops (2026-09-28 audit, round 71, F71-L1-1). streamed=%v whole-document=%v", b, got, want)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the arms answer one turn differently:\n streamed=%v\n document=%v", got, want)
	}
}

// TestASearchFailureStillCarriesTheTurnsNarration is F71-L1-2.
func TestASearchFailureStillCarriesTheTurnsNarration(t *testing.T) {
	chunks := []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: "Let me look that up."}},
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}}},
		r70Stop(),
	}
	streamed := r71Turn(t, true, chunks, http.StatusInternalServerError)
	whole := r71Turn(t, false, []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: "Let me look that up.", ToolCalls: []api.ToolCall{r70Search()}},
			Done: true, DoneReason: "stop", Metrics: api.Metrics{PromptEvalCount: 10, EvalCount: 2}},
	}, http.StatusInternalServerError)

	got, want := r70Text(t, streamed, true), r70Text(t, whole, false)
	// The narration the model produced reached the streaming client before the
	// search even ran; the error terminal is the only terminal that builds its
	// content list without it, so the same failed turn is one thing to a
	// streaming client and another to everyone else.
	if !strings.Contains(got, "Let me look that up.") {
		t.Errorf("the streaming arm lost the narration it had already streamed: %q", got)
	}
	if !strings.Contains(want, "Let me look that up.") {
		t.Errorf("the error terminal drops the turn's narration, which both its siblings (success and max-uses) carry: %q", want)
	}
	if got != want {
		t.Errorf("a search failure reaches the two arms with different prose:\n streamed=%q\n document=%q", got, want)
	}
}
