package middleware

// round83_takeover_account_integrity_test.go — leg 1, two readings the takeover
// did not share with the rest of the leg (2026-09-28 audit, round 83).
//
//  1. F83-L1-1. The account the loop hands the MODEL of the turn that asked to
//     search. The whole-document arm is handed the merged message and gives it
//     the whole turn; the streaming arm started the loop from the ONE chunk
//     that carried the search call, so the model was told it had said only that
//     chunk's prose — or NOTHING AT ALL when the call stood in a chunk of its
//     own, though the client had already been shown every word. Measured on
//     `[P1], [P2 + web_search], [P3], done`: the buffered arm's follow-up
//     assistant turn was "P1P2P3" and the streamed arm's was "P2"; on
//     `[P1], [P2], [web_search], done` they were "P1P2" and "". Two clients of
//     one upstream body then got their continuation from two different accounts
//     of the same turn. Only the streaming arm is wrong, so only it changes: it
//     accumulates the turn's narration as it arrives and waits for the turn to
//     end before the follow-up that needs it, while the search itself still
//     runs against the turn's generation.
//
//  2. F83-L1-2. Which tool types are the web_search tool. This leg's predicate
//     is anthropic.IsWebSearchToolType, exact; the cloud proxy read the same
//     field TRIMMED, so `" web_search_20250305"` was a search turn to the proxy
//     (served by the local surface rather than relayed) and an ordinary turn to
//     the middleware that then installed no takeover — the body was answered
//     with neither arm. One predicate, asked the same way by every reader.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// r83Turn drives one turn through the anthropic middleware and returns the
// client's body together with the assistant turn the loop handed back to the
// MODEL — the follow-up chat's assistant entry carrying the web_search call,
// which is the account the model is asked to continue from.
func r83Turn(t *testing.T, stream bool, tools string, chunks []api.ChatResponse) (string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)

	var mu sync.Mutex
	var accounts []string
	followup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req api.ChatRequest
		body, _ := io.ReadAll(r.Body)
		if json.Unmarshal(body, &req) == nil {
			for i := len(req.Messages) - 1; i >= 0; i-- {
				m := req.Messages[i]
				for _, tc := range m.ToolCalls {
					if tc.Function.Name == "web_search" {
						mu.Lock()
						accounts = append(accounts, "content="+m.Content+" thinking="+m.Thinking)
						mu.Unlock()
						break
					}
				}
			}
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
		write := func(r api.ChatResponse) {
			data, _ := json.Marshal(r)
			_, _ = c.Writer.Write(data)
		}
		if stream {
			for _, chunk := range chunks {
				write(chunk)
			}
			return
		}
		write(r81Merge(chunks))
	})

	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	body := `{"model":"test-model:cloud","max_tokens":100,"stream":` + streamLit + `,` +
		`"messages":[{"role":"user","content":"What is the latest news?"}],` +
		`"tools":` + tools + `}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	mu.Lock()
	defer mu.Unlock()
	if len(accounts) == 0 {
		t.Fatalf("no follow-up assistant turn carrying web_search was sent; the loop never ran\n%s", rec.Body.String())
	}
	return rec.Body.String(), accounts[0]
}

// r83Search is one web_search tool call.
func r83Search(q string) api.ToolCall {
	tc := api.ToolCall{Function: api.ToolCallFunction{Name: "web_search", Arguments: api.NewToolCallFunctionArguments()}}
	tc.Function.Arguments.Set("query", q)
	return tc
}

// r83Msg is one chunk of the turn: its reasoning and prose, then its calls.
func r83Msg(thinking, content string, calls ...api.ToolCall) api.ChatResponse {
	return api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", Thinking: thinking, Content: content, ToolCalls: calls}}
}

// TestTheLoopTellsTheModelTheTurnItSaw is F83-L1-1: the account of the turn
// that asked to search is the turn, on both arms of one upstream body.
func TestTheLoopTellsTheModelTheTurnItSaw(t *testing.T) {
	for _, tc := range []struct {
		note   string
		chunks []api.ChatResponse
	}{
		{"prose before and after the call", []api.ChatResponse{
			r83Msg("", "P1"),
			r83Msg("", "P2", r83Search("news")),
			r83Msg("", "P3"),
			{Model: "test-model", Message: api.Message{Role: "assistant"}, Done: true, DoneReason: "stop"},
		}},
		{"the prose all arrived before the call", []api.ChatResponse{
			r83Msg("", "P1"),
			r83Msg("", "P2"),
			r83Msg("", "", r83Search("news")),
			{Model: "test-model", Message: api.Message{Role: "assistant"}, Done: true, DoneReason: "stop"},
		}},
		{"reasoning either side of the call", []api.ChatResponse{
			r83Msg("T1", "P1"),
			r83Msg("T2", "", r83Search("news")),
			r83Msg("T3", "P2"),
			{Model: "test-model", Message: api.Message{Role: "assistant"}, Done: true, DoneReason: "stop"},
		}},
	} {
		_, doc := r83Turn(t, false, r81SearchTools, tc.chunks)
		_, str := r83Turn(t, true, r81SearchTools, tc.chunks)
		if doc != str {
			t.Errorf("%s: one upstream body asked the model to continue from two different accounts of its own turn:\n   buffered: %s\n   streamed: %s\nThe whole turn is what the client was shown and what the model wrote; the chunk that carried the search call is not the turn (2026-09-28 audit, round 83, F83-L1-1)", tc.note, doc, str)
		}
	}
}

// TestTheLoopKeepsTheWholeTurnWhenTheCallStandsAlone is F83-L1-1's sharpest
// shape: the chunk that carried the call said nothing, so the streamed arm's
// old account was the empty string while the client had been handed every word.
func TestTheLoopKeepsTheWholeTurnWhenTheCallStandsAlone(t *testing.T) {
	chunks := []api.ChatResponse{
		r83Msg("", "P1"),
		r83Msg("", "P2"),
		r83Msg("", "", r83Search("news")),
		{Model: "test-model", Message: api.Message{Role: "assistant"}, Done: true, DoneReason: "stop"},
	}
	_, str := r83Turn(t, true, r81SearchTools, chunks)
	if !strings.Contains(str, "content=P1P2") {
		t.Errorf("the model was asked to continue from %q, want the turn it wrote — P1P2 — which is what the client was streamed and what the whole-document arm hands the loop (2026-09-28 audit, round 83, F83-L1-1)", str)
	}
}

// TestTheWebSearchToolTypeIsAskedOneWay is F83-L1-2, this leg's half: the
// middleware and the converter must read a tool's type exactly as the cloud
// proxy does, or a body is a search turn to one reader and an ordinary turn to
// another.
func TestTheWebSearchToolTypeIsAskedOneWay(t *testing.T) {
	for _, tc := range []struct {
		typ  string
		want bool
	}{
		{"web_search_20250305", true},
		{"web_search", true},
		{" web_search_20250305", false},
		{"web-search", false},
		{"WebSearch", false},
	} {
		if got := hasWebSearchTool([]anthropic.Tool{{Type: tc.typ}}); got != tc.want {
			t.Errorf("hasWebSearchTool(%q) = %v, want %v: the type is asked exactly, and a padded spelling is not the tool the wire specified (2026-09-28 audit, round 83, F83-L1-2)", tc.typ, got, tc.want)
		}
		if got := anthropic.IsWebSearchToolType(tc.typ); got != tc.want {
			t.Errorf("anthropic.IsWebSearchToolType(%q) = %v, want %v — the one predicate every leg asks (2026-09-28 audit, round 83, F83-L1-2)", tc.typ, got, tc.want)
		}
	}
}
