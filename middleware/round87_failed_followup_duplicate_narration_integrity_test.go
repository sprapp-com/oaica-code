package middleware

// round87_failed_followup_duplicate_narration_integrity_test.go — leg 1,
// F87-L1-1 (2026-09-29 audit, round 87).
//
// A search that succeeds and a follow-up that FAILS is the one terminal that
// ends the loop after the search has already been accounted for. By then
// serverContent holds the turn's narration (it is prepended to the search block
// so each iteration's prose sits before its own search) — and the failure
// carried `append(slices.Clone(serverContent), carriedNarrationBlocks(...))`,
// a SECOND copy of the same prose. The whole-document arm therefore reached the
// client with the model's sentence twice, in its own block, while the streaming
// arm — whose narration had already gone out on the wire when the turn was
// streamed, and which therefore appends nothing there — carried it once: the
// same failed turn, one thing to each arm.
//
// Measured on 2026-09-29 before the fix, follow-up answering 500 after a
// successful search: streamed 5 blocks
// [text, server_tool_use, web_search_tool_result, server_tool_use,
// web_search_tool_result] with the prose "Let me look. ", document 6 blocks
// with the prose "Let me look. Let me look. " — the four terminals that end the
// loop BEFORE the search block is appended (:506 query-less, :523 search
// failure) are unchanged: there serverContent does not hold the narration yet.

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

// r87Turn drives one turn through the anthropic middleware with BOTH statuses
// under the test's control — the search's and the follow-up's. The follow-up
// failing after the search succeeded is the state F87-L1-1 is about.
func r87Turn(t *testing.T, stream bool, chunks []api.ChatResponse, searchStatus, followupStatus int) (string, int) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)

	followup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if followupStatus != http.StatusOK {
			w.WriteHeader(followupStatus)
			_, _ = w.Write([]byte(`{"error":"too long"}`))
			return
		}
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
			_, _ = w.Write([]byte(`{"error":"search down"}`))
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
	return rec.Body.String(), rec.Code
}

// TestAFailedFollowUpCarriesTheTurnsProseOnce is the F87-L1-1 pin: the terminal
// that ends the loop after the search has been accounted for hands each arm the
// turn's prose exactly once, in the same blocks, in the same order.
func TestAFailedFollowUpCarriesTheTurnsProseOnce(t *testing.T) {
	const narration = "Let me look. "
	streamChunks := []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: narration}},
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}}},
		r70Stop(),
	}
	docChunks := []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: narration, ToolCalls: []api.ToolCall{r70Search()}},
			Done: true, DoneReason: "stop", Metrics: api.Metrics{PromptEvalCount: 10, EvalCount: 2}},
	}

	streamed, sCode := r87Turn(t, true, streamChunks, http.StatusOK, http.StatusInternalServerError)
	whole, wCode := r87Turn(t, false, docChunks, http.StatusOK, http.StatusInternalServerError)
	if sCode != http.StatusOK || wCode != http.StatusOK {
		t.Fatalf("premise: the failed follow-up itself is a 200 turn (streamed %d, document %d)", sCode, wCode)
	}

	got, want := r70Text(t, streamed, true), r70Text(t, whole, false)
	for _, arm := range []struct {
		name, text string
	}{{"streamed", got}, {"document", want}} {
		if n := strings.Count(arm.text, narration); n != 1 {
			t.Errorf("the %s arm handed the client the turn's prose %d times: %q — the narration is already in what the terminal carries; the failed follow-up is not a reason to say it twice (2026-09-29 audit, round 87, F87-L1-1)",
				arm.name, n, arm.text)
		}
	}
	if got != want {
		t.Errorf("one failed follow-up reaches the two arms with different prose:\n streamed=%q\n document=%q", got, want)
	}
	sb, wb := r70Blocks(t, streamed, true), r70Blocks(t, whole, false)
	if len(sb) != len(wb) {
		t.Fatalf("the same turn is %d blocks to the streaming client and %d to everyone else (2026-09-29 audit, round 87, F87-L1-1):\n streamed=%v\n document=%v",
			len(sb), len(wb), sb, wb)
	}
	for i := range sb {
		if sb[i] != wb[i] {
			t.Errorf("block %d of the failed turn differs: %q streamed, %q document (2026-09-29 audit, round 87, F87-L1-1)", i, sb[i], wb[i])
		}
	}
}
