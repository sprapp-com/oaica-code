package middleware

// round72_upstream_error_frame_integrity_test.go — leg 1, the upstream's error
// frame arriving AFTER content has already streamed (2026-09-28 audit, round 72,
// F72-L1-1).
//
// The chat path hands the middleware a 200 whose body is a stream of chunks, and
// a generation that dies partway — runner OOM/exit, "prediction aborted, token
// repeat limit reached", a parser failure mid-turn — reaches that stream as one
// more line: {"error": "…"} (server/routes.go:2947,2954 and its streaming writer
// at :2149-2166). Buffered, the same frame is read by writeChatResponse and
// answered 500 with the sentence. Streamed, it was unmarshalled into a zero
// api.ChatResponse — the field name is not one of the struct's — so the arm
// emitted nothing, ended the turn with no terminal event at all, and the reason
// the model stopped was never given to the client. One turn, two answers: the
// document arm refuses it, the streaming arm quietly truncates.

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

// r72ErrorFrameTurn drives the real middleware over a handler that writes the
// upstream's own body: one content chunk, then the error frame the chat path
// puts into the stream.
func r72ErrorFrameTurn(t *testing.T, stream bool, withContent bool) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	const sentence = "prediction aborted, token repeat limit reached"
	chunk := api.ChatResponse{
		Model:   "test-model",
		Message: api.Message{Role: "assistant", Content: "hello "},
	}

	router.POST("/v1/messages", func(c *gin.Context) {
		if !stream {
			// The buffered path (writeChatResponse): a turn that hits the error
			// answers it INSTEAD of the content, and answers it 500.
			_ = withContent
			c.JSON(http.StatusInternalServerError, gin.H{"error": sentence})
			return
		}
		// The streaming path (streamResponse): the 200 and its content are
		// already gone, and the error arrives as one more line of the body.
		c.Writer.WriteHeader(http.StatusOK)
		if withContent {
			data, _ := json.Marshal(chunk)
			_, _ = c.Writer.Write(append(data, '\n'))
		}
		_, _ = c.Writer.Write([]byte(`{"error":"` + sentence + `"}` + "\n"))
	})

	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	body := `{"model":"test-model","max_tokens":100,"stream":` + streamLit + `,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")

	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	return w.Code, w.Body.String()
}

// r72StatedError reads the failure sentence a body gives the client: the
// Anthropic error envelope both arms and the gateway use.
func r72StatedError(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var envelope struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &envelope); err == nil && envelope.Error.Message != "" {
			return envelope.Error.Message
		}
	}
	return ""
}

// r72TakeoverTurn drives the web-search takeover with the upstream's body: a
// chunk that carries the search call (the loop takes the turn over), then the
// error frame. Returns what the client saw.
func r72TakeoverTurn(t *testing.T, chunks []api.ChatResponse, errorFrame string) string {
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
		_, _ = c.Writer.Write([]byte(errorFrame))
	})

	body := `{"model":"test-model:cloud","max_tokens":100,"stream":true,` +
		`"messages":[{"role":"user","content":"What is the latest news?"}],` +
		`"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec.Body.String()
}

// TestAMidStreamErrorFrameEndsTheSearchTakeoverToo is the same pin on the route
// where the model asked to search: the takeover's streaming arm absorbed the
// error frame as narration and answered 200 with an empty body — neither the
// model's prose nor the reason the turn stopped (2026-09-28 audit, round 72,
// F72-L1-1).
func TestAMidStreamErrorFrameEndsTheSearchTakeoverToo(t *testing.T) {
	const want = "prediction aborted, token repeat limit reached"

	searchCall := api.ChatResponse{Model: "test-model",
		Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{
			Function: api.ToolCallFunction{Name: "web_search", Arguments: makeArgs("query", "news")},
		}}},
		Done: false,
	}

	body := r72TakeoverTurn(t, []api.ChatResponse{searchCall},
		`{"error":"`+want+`"}`)
	if got := r72StatedError(t, body); got != want {
		t.Fatalf("a search turn whose generation died mid-stream gives the client %q, want the upstream's own sentence %q (body %q) (2026-09-28 audit, round 72, F72-L1-1)",
			got, want, body)
	}
}

// TestAMidStreamErrorFrameReachesTheClientOnBothArms is the F72-L1-1 pin: the
// upstream's reason for ending the turn is one reading, and the two arms must
// give it the same way — the document arm already refuses the turn with it, so
// the streaming arm must state it too.
func TestAMidStreamErrorFrameReachesTheClientOnBothArms(t *testing.T) {
	const want = "prediction aborted, token repeat limit reached"

	for _, withContent := range []bool{false, true} {
		name := "the error frame is the first thing on the wire"
		if withContent {
			name = "the error frame arrives after content has streamed"
		}
		t.Run(name, func(t *testing.T) {
			docCode, docBody := r72ErrorFrameTurn(t, false, withContent)
			if got := r72StatedError(t, docBody); got != want {
				t.Fatalf("PREMISE: the document arm refuses this turn with %q, want %q (status %d)", got, want, docCode)
			}
			if docCode == http.StatusOK {
				t.Fatalf("PREMISE: the document arm answered the error frame %d", docCode)
			}

			_, streamBody := r72ErrorFrameTurn(t, true, withContent)
			if got := r72StatedError(t, streamBody); got != want {
				t.Fatalf("the same upstream error frame gives the client %q when the turn is buffered and %q when it is streamed — the streaming arm ended the turn without stating why (body %q) (2026-09-28 audit, round 72, F72-L1-1)",
					want, got, streamBody)
			}
		})
	}
}
