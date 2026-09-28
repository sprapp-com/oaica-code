package middleware

// round89_loop_window_start_integrity_test.go — leg 1, F89-L1-1 (2026-09-29
// audit, round 89).
//
// The web_search loop runs under a window (five minutes in production), and it
// was minted at a different moment on the two arms of one body: the streamed arm
// takes the turn over at the CHUNK that carries the search call, so its window
// starts there, while the whole-document arm starts its loop only once the whole
// turn has been merged and handed over. The streamed arm then spends that window
// waiting for the turn to end (waitForTurnNarration, F83-L1-1) — a wait the other
// arm never has, because it is handed the whole turn at once.
//
// A first turn whose tail after the search call outlives the window therefore
// diverges: the streaming client was handed a second server_tool_use /
// web_search_tool_result pair reporting a search that failed, and no answer, for
// the same upstream body the buffered client was answered from. Measured on the
// round-89 probe (window 200ms, tail 600ms — the production shape is 5 minutes
// and a longer tail):
//
//	document: blocks=text,server_tool_use,web_search_tool_result,text
//	          text="Looking that up. FINAL ANSWER"
//	streamed: blocks=text,server_tool_use,web_search_tool_result,
//	          server_tool_use,web_search_tool_result
//	          text="Looking that up. "
//
// The loop's window is the loop's own budget, so it now starts where the loop's
// own work does on each arm: the tail wait is bounded by the CLIENT's context,
// and the streaming arm re-mints the window once the turn's account is in hand,
// so the follow-up and every later iteration get the window the whole-document
// arm's loop got. Both halves were measured load-bearing: with the re-mint
// removed, the follow-up runs on the expired window and the divergence returns;
// with the wait back on the loop's context, the wait itself times out and the
// divergence returns.
//
// The window is a variable so this pin can hold both arms to one window in
// milliseconds; it is restored before the test returns, and production never
// writes it.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// r89Window is the window this pin runs both arms under, and r89Tail a tail
// long enough to outlive it — the probe's 600ms against 200ms, scaled down from
// production's five minutes.
const (
	r89Window = 200 * time.Millisecond
	r89Tail   = 600 * time.Millisecond
)

// r89Turn drives one turn whose tail after the search-call chunk takes tail, and
// reports the blocks the client met and the text it read.
func r89Turn(t *testing.T, stream bool, tail time.Duration) (string, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)

	followup := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(api.ChatResponse{
			Model: "test-model", Message: api.Message{Role: "assistant", Content: "FINAL ANSWER"},
			Done: true, DoneReason: "stop",
		})
	}))
	defer followup.Close()
	t.Setenv("OLLAMA_HOST", followup.URL)

	search := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(anthropic.OllamaWebSearchResponse{
			Results: []anthropic.OllamaWebSearchResult{{Title: "R", URL: "https://e.com", Content: "c"}},
		})
	}))
	defer search.Close()
	orig := anthropic.WebSearchEndpoint
	anthropic.WebSearchEndpoint = search.URL
	defer func() { anthropic.WebSearchEndpoint = orig }()

	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		c.Writer.WriteHeader(http.StatusOK)
		write := func(ch api.ChatResponse) {
			data, _ := json.Marshal(ch)
			_, _ = c.Writer.Write(data)
			c.Writer.Flush()
		}
		write(api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", Content: "Looking that up. "}})
		write(api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{
			ID: "call_ws_1", Function: api.ToolCallFunction{Name: "web_search", Arguments: makeArgs("query", "latest news")},
		}}}})
		time.Sleep(tail)
		write(api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant"}, Done: true, DoneReason: "stop"})
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

	var order []string
	var text string
	add := func(kind, t2 string) {
		order = append(order, kind)
		if kind == "text" {
			text += t2
		}
	}
	if stream {
		for _, ev := range parseSSEEvents(t, rec.Body.String()) {
			switch ev.event {
			case "content_block_start":
				var e anthropic.ContentBlockStartEvent
				if json.Unmarshal([]byte(ev.data), &e) == nil {
					add(e.ContentBlock.Type, "")
				}
			case "content_block_delta":
				var e struct {
					Delta struct {
						Type string `json:"type"`
						Text string `json:"text"`
					} `json:"delta"`
				}
				if json.Unmarshal([]byte(ev.data), &e) == nil && e.Delta.Type == "text_delta" {
					text += e.Delta.Text
				}
			}
		}
	} else {
		dec := json.NewDecoder(strings.NewReader(rec.Body.String()))
		for {
			var probe struct {
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			}
			if err := dec.Decode(&probe); err != nil {
				break
			}
			for _, b := range probe.Content {
				add(b.Type, b.Text)
			}
		}
	}
	return strings.Join(order, ","), text
}

// TestTheLoopsWindowStartsWhereTheLoopsWorkDoes is the F89-L1-1 pin: one body,
// two arms, one answer, whether the turn's tail is short or long — the tail is
// the turn's work and does not spend the loop's budget on the arm that has to
// wait for it.
func TestTheLoopsWindowStartsWhereTheLoopsWorkDoes(t *testing.T) {
	old := webSearchLoopWindow
	webSearchLoopWindow = r89Window
	t.Cleanup(func() { webSearchLoopWindow = old })

	const want = "text,server_tool_use,web_search_tool_result,text"
	const wantText = "Looking that up. FINAL ANSWER"
	for _, tc := range []struct {
		name string
		tail time.Duration
	}{
		{"a tail inside the window", r89Window / 4},
		{"a tail past the window", r89Tail},
	} {
		t.Run(tc.name, func(t *testing.T) {
			docOrder, docText := r89Turn(t, false, tc.tail)
			strOrder, strText := r89Turn(t, true, tc.tail)

			if docOrder != want || docText != wantText {
				t.Fatalf("premise: the whole-document arm answered blocks=%s text=%q, want blocks=%s text=%q", docOrder, docText, want, wantText)
			}
			if strOrder != docOrder || strText != docText {
				t.Errorf("one upstream body, two arms, two answers: document blocks=%s text=%q, streamed blocks=%s text=%q — the tail after the chunk that carried the search call is the TURN's work, and the loop's window is the loop's own budget, so it starts where the loop's own work does on each arm (2026-09-29 audit, round 89, F89-L1-1)",
					docOrder, docText, strOrder, strText)
			}
		})
	}
}
