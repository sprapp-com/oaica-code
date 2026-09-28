package middleware

// round70_takeover_arms_integrity_test.go — the web_search takeover, read two
// ways.
//
// The middleware has two arms for one turn: the streaming arm, which sees the
// upstream as CHUNKS, and the whole-document arm, which sees it as one merged
// message. Every decision the takeover makes therefore has to be made from
// different evidence on the two, and round 56 already settled one such question
// in favour of the document's reading (the narration of the chunk that carried
// the call is carried, not dropped).
//
// Two readings were still apart (2026-09-28 audit, round 70):
//
// (1) A client tool call that arrived in an EARLIER chunk than the web_search
// call. The document arm prefers the search and drops the client call — the
// loop takes the turn over — but the streaming arm had already written that
// call to the client, so the same turn reached one client with a Bash tool_use
// and stop_reason end_turn (a call it can never answer) and the other without
// it. The call now waits one chunk, which is the decision it was waiting for.
//
// (2) The model's narration AFTER the chunk that carried the call. The
// document arm keeps it (its message is merged), the streaming arm discarded
// every chunk the takeover superseded, so the same completion reached the
// client with that text on one wire and without it on the other.

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

// r70Turn drives one turn through the anthropic middleware and returns the body
// in the shape the stream flag asks for.
func r70Turn(t *testing.T, stream bool, chunks []api.ChatResponse) string {
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

// r70Blocks renders what the client was handed as "type:id" strings, with the
// server tool ids normalised: those are minted per request from the message id,
// so two arms of one turn never agree on them and they say nothing about this
// reading.
func r70Blocks(t *testing.T, body string, stream bool) []string {
	t.Helper()
	var kinds []string
	normalise := func(s string) string {
		if strings.HasPrefix(s, "srvtoolu_") {
			return "srvtoolu_<MSG>"
		}
		return s
	}
	if stream {
		for _, ev := range parseSSEEvents(t, body) {
			if ev.event != "content_block_start" {
				continue
			}
			var e anthropic.ContentBlockStartEvent
			if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
				t.Fatalf("block start: %v", err)
			}
			b := e.ContentBlock
			kinds = append(kinds, b.Type+":"+normalise(b.ID))
		}
		return kinds
	}
	var e anthropic.MessagesResponse
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body: %v\n%s", err, body)
	}
	for _, b := range e.Content {
		kinds = append(kinds, b.Type+":"+normalise(b.ID))
	}
	return kinds
}

// r70Text is the prose the client was handed, joined in order.
func r70Text(t *testing.T, body string, stream bool) string {
	t.Helper()
	var out strings.Builder
	if stream {
		for _, ev := range parseSSEEvents(t, body) {
			if ev.event != "content_block_delta" {
				continue
			}
			var e anthropic.ContentBlockDeltaEvent
			if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
				t.Fatalf("block delta: %v", err)
			}
			if e.Delta.Type == "text_delta" {
				out.WriteString(e.Delta.Text)
			}
		}
		return out.String()
	}
	var e anthropic.MessagesResponse
	if err := json.Unmarshal([]byte(body), &e); err != nil {
		t.Fatalf("body: %v\n%s", err, body)
	}
	for _, b := range e.Content {
		if b.Type == "text" && b.Text != nil {
			out.WriteString(*b.Text)
		}
	}
	return out.String()
}

func r70Bash() api.ToolCall {
	return api.ToolCall{ID: "call_cli_0", Function: api.ToolCallFunction{Name: "Bash", Arguments: makeArgs("cmd", "ls")}}
}

func r70Search() api.ToolCall {
	return api.ToolCall{ID: "call_ws_1", Function: api.ToolCallFunction{Name: "web_search", Arguments: makeArgs("query", "latest news")}}
}

func r70Stop() api.ChatResponse {
	return api.ChatResponse{
		Model: "test-model", Message: api.Message{Role: "assistant"},
		Done: true, DoneReason: "stop", Metrics: api.Metrics{PromptEvalCount: 10, EvalCount: 2},
	}
}

// TestAStreamedTurnDropsTheClientCallTheDocumentArmDrops is F70-L1-1: the client
// call arrives first, the search call second.
func TestAStreamedTurnDropsTheClientCallTheDocumentArmDrops(t *testing.T) {
	streamed := r70Turn(t, true, []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Bash()}}},
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}}},
		r70Stop(),
	})
	whole := r70Turn(t, false, []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Bash(), r70Search()}},
			Done: true, DoneReason: "stop", Metrics: api.Metrics{PromptEvalCount: 10, EvalCount: 2}},
	})

	got, want := r70Blocks(t, streamed, true), r70Blocks(t, whole, false)
	for _, b := range got {
		if strings.HasPrefix(b, "tool_use:") {
			t.Errorf("the streaming arm served %q and stopped with end_turn: the whole-document arm of the same turn drops that call because the takeover owns the turn, and a tool_use the client is told not to run (and could never answer) is not a block either arm should hand out. streamed=%v whole-document=%v", b, got, want)
		}
	}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("the arms answer one turn differently:\n streamed=%v\n document=%v", got, want)
	}
}

// TestAStreamedTurnCarriesNarrationThatFollowsTheCallChunk is F70-L1-2.
func TestAStreamedTurnCarriesNarrationThatFollowsTheCallChunk(t *testing.T) {
	streamed := r70Turn(t, true, []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: "Let me look that up."}},
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Search()}}},
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: "Here is what I found."}},
		r70Stop(),
	})
	whole := r70Turn(t, false, []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", Content: "Let me look that up.Here is what I found.", ToolCalls: []api.ToolCall{r70Search()}},
			Done: true, DoneReason: "stop", Metrics: api.Metrics{PromptEvalCount: 10, EvalCount: 2}},
	})

	got, want := r70Text(t, streamed, true), r70Text(t, whole, false)
	if got != want {
		t.Errorf("the model's narration after the search call is the same answer the whole-document arm relays:\n streamed=%q\n document=%q", got, want)
	}
	// The arms agree on the narration; the streaming arm writes it in the chunk
	// boundaries it received, so it is two text blocks where the merged document
	// is one. Asserted here so the difference is a recorded shape rather than a
	// surprise.
	if blocks := r70Blocks(t, streamed, true); strings.Count(strings.Join(blocks, ","), "text:") < 2 {
		t.Errorf("the streamed arm answers the turn in the chunks it was given, so the call chunk's narration and the one that followed are separate text blocks, got %v", blocks)
	}
}

// TestAClientCallWithNoSearchStillStreams is the control: the hold must last
// exactly as long as the doubt, so an ordinary tool-calling turn with the
// web_search tool DECLARED but not called still serves its call.
func TestAClientCallWithNoSearchStillStreams(t *testing.T) {
	body := r70Turn(t, true, []api.ChatResponse{
		{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r70Bash()}}},
		r70Stop(),
	})
	blocks := r70Blocks(t, body, true)
	if len(blocks) != 1 || blocks[0] != "tool_use:call_cli_0" {
		t.Fatalf("a turn that never searched must serve its client call as it stands, got %v", blocks)
	}
}
