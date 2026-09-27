package middleware

// round56_web_search_narration_integrity_test.go — round 56, leg 1, F56-2: the
// narration of the turn that asked for a web search.
//
// The response that carries the web_search call is consumed by the loop, and
// neither arm served it: the passthrough arm relays only the chunks that
// carried no call, and the non-stream arm serves the loop's terminal response
// alone. So the model's text — "Let me look that up." — reached the client on
// the streaming wire and not on the non-streaming one, and the same completion
// answered two different contents. A turn that puts its text and its call in a
// single chunk lost it on both arms, and every further loop iteration lost its
// narration on both. The narration now leads each iteration's server_tool_use
// block, which is the order the model wrote it in (2026-09-28 audit, round 56).

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// r56NarrationEnv points the search endpoint and the follow-up chat at mocks;
// each response in followups answers one /api/chat call, in order.
func r56NarrationEnv(t *testing.T, followups ...api.ChatResponse) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	enableCloudForTest(t)

	var mu sync.Mutex
	next := 0
	followupServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		i := next
		next++
		mu.Unlock()
		resp := api.ChatResponse{
			Model:      "test-model",
			Message:    api.Message{Role: "assistant", Content: "FINAL ANSWER"},
			Done:       true,
			DoneReason: "stop",
			Metrics:    api.Metrics{PromptEvalCount: 40, EvalCount: 15},
		}
		if i < len(followups) {
			resp = followups[i]
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	t.Cleanup(followupServer.Close)
	t.Setenv("OLLAMA_HOST", followupServer.URL)

	searchServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(anthropic.OllamaWebSearchResponse{
			Results: []anthropic.OllamaWebSearchResult{
				{Title: "Test Result", URL: "https://example.com/result", Content: "Some content"},
			},
		})
	}))
	t.Cleanup(searchServer.Close)
	originalEndpoint := anthropic.WebSearchEndpoint
	anthropic.WebSearchEndpoint = searchServer.URL
	t.Cleanup(func() { anthropic.WebSearchEndpoint = originalEndpoint })
}

const (
	r56Narration = "Let me look that up."
	r56Body      = `{"model":"test-model:cloud","max_tokens":100,%STREAM%"messages":[{"role":"user","content":"What is the latest news?"}],"tools":[{"type":"web_search_20250305","name":"web_search"}]}`
)

// r56SearchCall is the call a turn makes to search, on the given id.
func r56SearchCall(id string) api.ToolCall {
	return api.ToolCall{
		ID:       id,
		Function: api.ToolCallFunction{Name: "web_search", Arguments: makeArgs("query", "latest news")},
	}
}

// r56Ask posts one Messages request through the middleware, whose upstream is
// the given chunks written in order.
func r56Ask(t *testing.T, stream bool, chunks ...api.ChatResponse) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	router := gin.New()
	router.Use(AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		c.Writer.WriteHeader(http.StatusOK)
		for _, chunk := range chunks {
			data, _ := json.Marshal(chunk)
			_, _ = c.Writer.Write(data)
		}
	})

	flag := ""
	if stream {
		flag = `"stream":true,`
	}
	body := strings.Replace(r56Body, "%STREAM%", flag, 1)
	req, _ := http.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

// r56PlainBlocks labels the served content of a non-streaming answer.
func r56PlainBlocks(t *testing.T, rec *httptest.ResponseRecorder) []string {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	var resp anthropic.MessagesResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, rec.Body.String())
	}
	out := make([]string, 0, len(resp.Content))
	for _, b := range resp.Content {
		label := b.Type
		if b.Type == "text" && b.Text != nil {
			label += ":" + *b.Text
		}
		if b.Type == "thinking" && b.Thinking != nil {
			label += ":" + *b.Thinking
		}
		out = append(out, label)
	}
	return out
}

// r56StreamBlocks labels the same content as it was streamed: one label per
// block, in the order the blocks were opened, with a text or thinking block's
// deltas joined the way a client accumulates them.
func r56StreamBlocks(t *testing.T, body string) []string {
	t.Helper()
	var order []int
	labels := map[int]string{}
	text := map[int]*strings.Builder{}
	for _, ev := range parseSSEEvents(t, body) {
		var frame struct {
			Index        int `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type     string `json:"type"`
				Text     string `json:"text"`
				Thinking string `json:"thinking"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(ev.data), &frame) != nil {
			continue
		}
		switch ev.event {
		case "content_block_start":
			order = append(order, frame.Index)
			labels[frame.Index] = frame.ContentBlock.Type
			text[frame.Index] = &strings.Builder{}
		case "content_block_delta":
			if b, ok := text[frame.Index]; ok {
				switch frame.Delta.Type {
				case "text_delta":
					b.WriteString(frame.Delta.Text)
				case "thinking_delta":
					b.WriteString(frame.Delta.Thinking)
				}
			}
		}
	}
	out := make([]string, 0, len(order))
	for _, i := range order {
		label := labels[i]
		if b, ok := text[i]; ok && b.Len() > 0 {
			label += ":" + b.String()
		}
		out = append(out, label)
	}
	return out
}

// r56IndexOf is the position of the first label carrying the given prefix.
func r56IndexOf(blocks []string, want string) int {
	for i, b := range blocks {
		if b == want {
			return i
		}
	}
	return -1
}

// TestAThoughtInTheCallChunkIsStreamedAsADelta: the narration the loop serves
// includes the model's thinking, and a thinking block written WHOLE inside
// content_block_start is one the client never accumulates — cmd/agent/sse.go
// reads thinking from thinking_delta alone, as the SDKs do, so the thought is
// bytes the engine never sees. The converter emits the delta framing for the
// same block.
func TestAThoughtInTheCallChunkIsStreamedAsADelta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)

	writer := &WebSearchAnthropicWriter{
		BaseWriter: BaseWriter{ResponseWriter: ginCtx.Writer},
		stream:     true,
	}

	const thought = "the user wants the latest news"
	thoughtCopy := thought
	block := anthropic.ContentBlock{Type: "thinking", Thinking: &thoughtCopy}
	if err := writer.writeStreamContentBlocks([]anthropic.ContentBlock{block}); err != nil {
		t.Fatalf("writeStreamContentBlocks: %v", err)
	}

	events := parseSSEEvents(t, rec.Body.String())
	want := []string{"content_block_start", "content_block_delta", "content_block_stop"}
	if len(events) != len(want) {
		t.Fatalf("the thought streamed as %v, want %v — a thinking block that carries its text in the start event and no delta between is a block whose thought the client's accumulator leaves empty",
			eventNames(events), want)
	}
	var start anthropic.ContentBlockStartEvent
	if err := json.Unmarshal([]byte(events[0].data), &start); err != nil {
		t.Fatalf("parse content_block_start: %v", err)
	}
	if start.ContentBlock.Type != "thinking" {
		t.Fatalf("the start event announced %q, want thinking", start.ContentBlock.Type)
	}
	if start.ContentBlock.Thinking != nil && *start.ContentBlock.Thinking != "" {
		t.Errorf("the start event carried %q as the thought; it is accumulated from the deltas that follow", *start.ContentBlock.Thinking)
	}
	var delta anthropic.ContentBlockDeltaEvent
	if err := json.Unmarshal([]byte(events[1].data), &delta); err != nil {
		t.Fatalf("parse content_block_delta: %v", err)
	}
	if delta.Delta.Type != "thinking_delta" || delta.Delta.Thinking != thought {
		t.Errorf("the delta is %q carrying %q, want thinking_delta carrying %q", delta.Delta.Type, delta.Delta.Thinking, thought)
	}
}

// TestAPreCallNarrationReachesTheClientOnBothArms is F56-2: the non-streaming
// answer of a web-search turn carried no text at all where the streaming one
// carried the model's own words.
func TestAPreCallNarrationReachesTheClientOnBothArms(t *testing.T) {
	r56NarrationEnv(t)

	initial := api.ChatResponse{
		Model:      "test-model",
		Message:    api.Message{Role: "assistant", ToolCalls: []api.ToolCall{r56SearchCall("call_ws_1")}},
		Done:       true,
		DoneReason: "stop",
		Metrics:    api.Metrics{PromptEvalCount: 10, EvalCount: 2},
	}

	// The streaming wire: the narration in a chunk of its own, then the chunk
	// that carries the call.
	narrationChunk := api.ChatResponse{
		Model:   "test-model",
		Message: api.Message{Role: "assistant", Content: r56Narration},
	}
	srec := r56Ask(t, true, narrationChunk, initial)
	streamed := r56StreamBlocks(t, srec.Body.String())

	// The non-streaming wire: one response, text and call together.
	one := initial
	one.Message.Content = r56Narration
	prec := r56Ask(t, false, one)
	plain := r56PlainBlocks(t, prec)

	want := "text:" + r56Narration
	sIdx, pIdx := r56IndexOf(streamed, want), r56IndexOf(plain, want)
	if pIdx < 0 {
		t.Errorf("the non-streaming answer of a web-search turn served %v: the model's own words %q are missing, while the same completion streamed reaches the client with them", plain, r56Narration)
	}
	if sIdx < 0 {
		t.Errorf("the streaming answer served %v: the narration %q is missing", streamed, r56Narration)
	}
	// Exactly once: the passthrough already relayed it where it arrived in a
	// chunk of its own, and the served content must not repeat it.
	for name, blocks := range map[string][]string{"stream": streamed, "non-stream": plain} {
		n := 0
		for _, b := range blocks {
			if b == want {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the %s answer carries the narration %d times (%v): the model said it once", name, n, blocks)
		}
	}

	// Order: the words lead the search they asked for, as the model wrote them.
	for name, found := range map[string]int{"stream": sIdx, "non-stream": pIdx} {
		blocks := streamed
		if name == "non-stream" {
			blocks = plain
		}
		search := r56IndexOf(blocks, "server_tool_use")
		if search < 0 {
			t.Fatalf("the %s answer served no server_tool_use block: %v", name, blocks)
		}
		if found >= 0 && found > search {
			t.Errorf("the %s answer served %v: the narration follows the search it introduced", name, blocks)
		}
	}
}

// TestANarrationInTheCallChunkReachesTheClient: a turn that writes its text and
// its call in ONE chunk is carried by neither passthrough, so both arms owe it.
func TestANarrationInTheCallChunkReachesTheClient(t *testing.T) {
	r56NarrationEnv(t)

	together := api.ChatResponse{
		Model: "test-model",
		Message: api.Message{Role: "assistant", Content: r56Narration,
			ToolCalls: []api.ToolCall{r56SearchCall("call_ws_1")}},
		Done:       true,
		DoneReason: "stop",
		Metrics:    api.Metrics{PromptEvalCount: 10, EvalCount: 2},
	}

	want := "text:" + r56Narration
	streamed := r56StreamBlocks(t, r56Ask(t, true, together).Body.String())
	plain := r56PlainBlocks(t, r56Ask(t, false, together))

	if r56IndexOf(streamed, want) < 0 {
		t.Errorf("the streaming answer of a turn whose text sat in the call's own chunk served %v, want the text %q before the search: the passthrough relays only the chunks that carried no call", streamed, r56Narration)
	}
	if r56IndexOf(plain, want) < 0 {
		t.Errorf("the non-streaming answer served %v, want the text %q before the search", plain, r56Narration)
	}
	for name, blocks := range map[string][]string{"stream": streamed, "non-stream": plain} {
		n := 0
		for _, b := range blocks {
			if b == want {
				n++
			}
		}
		if n != 1 {
			t.Errorf("the %s answer carries the narration %d times (%v)", name, n, blocks)
		}
	}
}

// TestALaterLoopsNarrationReachesTheClient: the loop's further iterations carry
// a call too, and their narration was dropped by the same rule. Each turn's
// words lead its own search.
func TestALaterLoopsNarrationReachesTheClient(t *testing.T) {
	const second = "One more search."
	r56NarrationEnv(t, api.ChatResponse{
		Model:      "test-model",
		Message:    api.Message{Role: "assistant", Content: second, ToolCalls: []api.ToolCall{r56SearchCall("call_ws_2")}},
		Done:       true,
		DoneReason: "stop",
		Metrics:    api.Metrics{PromptEvalCount: 20, EvalCount: 5},
	})

	first := api.ChatResponse{
		Model:      "test-model",
		Message:    api.Message{Role: "assistant", Content: r56Narration, ToolCalls: []api.ToolCall{r56SearchCall("call_ws_1")}},
		Done:       true,
		DoneReason: "stop",
		Metrics:    api.Metrics{PromptEvalCount: 10, EvalCount: 2},
	}

	plain := r56PlainBlocks(t, r56Ask(t, false, first))
	i1, i2 := r56IndexOf(plain, "text:"+r56Narration), r56IndexOf(plain, "text:"+second)
	if i1 < 0 || i2 < 0 {
		t.Fatalf("the answer of a two-iteration search served %v: both turns' words must reach the client", plain)
	}
	searches := 0
	for i, b := range plain {
		if b != "server_tool_use" {
			continue
		}
		searches++
		if (searches == 1 && i1 > i) || (searches == 2 && i2 > i) {
			t.Errorf("the answer served %v: search %d precedes the words that asked for it", plain, searches)
		}
	}
	if searches != 2 {
		t.Errorf("the answer served %d searches, want 2: %v", searches, plain)
	}
}
