package middleware

// round81_takeover_run_and_join_integrity_test.go — leg 1, the web_search
// takeover read two ways (2026-09-28 audit, round 81).
//
// The takeover stands between two arms of one upstream body: the STREAMING arm,
// which sees the completion as chunks and relays the ones that carry no
// web_search call, and the BUFFERED arm, which sees the one message the
// server's document lane merged those chunks into (server/routes.go,
// writeChatResponse) — carrying Content, Thinking, ToolCalls and the ordered
// OutputRuns that lane builds while merging. Three readings were apart:
//
//  1. F81-L1-1. The order the turn's own output arrived in. The buffered arm
//     read the merged message as a fixed [thinking, text] pair and the
//     streaming arm relayed each chunk where it stood, so a model that wrote
//     its prose and then its reasoning reached one client as
//     [text, thinking] and the other as [thinking, text] — one upstream turn,
//     two block structures, on the same request body.
//
//  2. F81-L1-2. One run, two blocks. A run is a contiguous stretch of one kind
//     and it is one block on both arms. Where the chunk boundary the takeover
//     swallowed fell INSIDE a run — the model's reasoning written across the
//     chunk that carried the search call — the streaming arm closed the open
//     block and opened a second one for the rest, and the buffered arm's single
//     merged run reached the client as one block.
//
//  3. F81-L1-3. A nameless entry's bytes. An entry the upstream never named is
//     not a call: its arguments are the model's own output and reach the client
//     as TEXT, which is what both other legs and this leg's own two arms do for
//     the shape (round 77, F77-L1-1). The takeover released the chunks it had
//     held without their calls — and dropped the nameless entry's bytes with
//     them, while the buffered arm never read them at all.

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

// r81Merge spells the buffered arm of a chunk list: content and thinking
// joined, calls in order, and OutputRuns the way the server's document lane
// builds them — a run per contiguous stretch of one kind, in the order the
// streaming converter asks for them inside one chunk (reasoning, then prose,
// then the calls it carries).
func r81Merge(chunks []api.ChatResponse) api.ChatResponse {
	out := chunks[len(chunks)-1]
	var content, thinking strings.Builder
	var calls []api.ToolCall
	var runs []api.OutputRun
	add := func(kind, s string) {
		if s == "" {
			return
		}
		if kind == "text" {
			content.WriteString(s)
		} else {
			thinking.WriteString(s)
		}
		if n := len(runs); n > 0 && runs[n-1].Kind == kind {
			runs[n-1].Text += s
			return
		}
		runs = append(runs, api.OutputRun{Kind: kind, Text: s})
	}
	for _, c := range chunks {
		add("thinking", c.Message.Thinking)
		add("text", c.Message.Content)
		for _, tc := range c.Message.ToolCalls {
			calls = append(calls, tc)
			runs = append(runs, api.OutputRun{Kind: "call"})
		}
	}
	out.Message = api.Message{Role: "assistant", Content: content.String(), Thinking: thinking.String(), ToolCalls: calls, OutputRuns: runs}
	if !out.Message.OutputRunsAccountFor() {
		out.Message.OutputRuns = nil
	}
	return out
}

// r81Turn drives one turn through the anthropic middleware and returns the body
// in the shape the stream flag asks for. stream=true writes the chunks (the
// streaming arm); stream=false writes the ONE message the document lane merges
// them into, which is what the buffered arm is handed in the product.
func r81Turn(t *testing.T, stream bool, tools string, chunks []api.ChatResponse) string {
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
	return rec.Body.String()
}

// r81Blocks renders what the client was handed as the sequence of its blocks:
// "<type id>" where a block opens, and the block's text for the text and
// thinking ones — a block's deltas joined, because the two arms deliver one
// block with different delta granularity and that is not what is compared here.
// Server tool ids are minted per request and normalised: two arms of one turn
// never agree on them and they say nothing about this reading.
func r81Blocks(t *testing.T, body string, stream bool) string {
	t.Helper()
	norm := func(s string) string {
		if strings.HasPrefix(s, "srvtoolu_") {
			return "srvtoolu_<MSG>"
		}
		return s
	}
	out := ""
	if !stream {
		var msg anthropic.MessagesResponse
		if err := json.Unmarshal([]byte(body), &msg); err != nil {
			t.Fatalf("the buffered arm's body is not one message: %v\n%s", err, body)
		}
		for _, b := range msg.Content {
			switch b.Type {
			case "text":
				v := ""
				if b.Text != nil {
					v = *b.Text
				}
				out += `<text >{text "` + v + `"}`
			case "thinking":
				v := ""
				if b.Thinking != nil {
					v = *b.Thinking
				}
				out += `<thinking >{thinking "` + v + `"}`
			default:
				out += "<" + b.Type + " " + norm(b.ID) + ">"
			}
		}
		return out
	}

	openKind := ""
	acc := ""
	flush := func() {
		if openKind == "" {
			return
		}
		out += "<" + openKind + " >{" + openKind + ` "` + acc + `"}`
		openKind = ""
		acc = ""
	}
	for _, ev := range parseSSEEvents(t, body) {
		switch ev.event {
		case "content_block_start":
			var e anthropic.ContentBlockStartEvent
			if err := json.Unmarshal([]byte(ev.data), &e); err != nil {
				t.Fatalf("block start: %v", err)
			}
			flush()
			switch e.ContentBlock.Type {
			case "text", "thinking":
				openKind = e.ContentBlock.Type
			default:
				out += "<" + e.ContentBlock.Type + " " + norm(e.ContentBlock.ID) + ">"
			}
		case "content_block_stop":
			flush()
		case "content_block_delta":
			var d anthropic.ContentBlockDeltaEvent
			if err := json.Unmarshal([]byte(ev.data), &d); err != nil {
				t.Fatalf("delta: %v", err)
			}
			switch d.Delta.Type {
			case "text_delta":
				acc += d.Delta.Text
			case "thinking_delta":
				acc += d.Delta.Thinking
			}
		}
	}
	flush()
	return out
}

const r81PlainTools = `[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}}]`
const r81SearchTools = `[{"name":"Bash","description":"run","input_schema":{"type":"object","properties":{"cmd":{"type":"string"}}}},{"type":"web_search_20250305","name":"web_search"}]`

func r81Call(name, key, val string) api.ToolCall {
	tc := api.ToolCall{Function: api.ToolCallFunction{Name: name, Arguments: api.NewToolCallFunctionArguments()}}
	tc.Function.Arguments.Set(key, val)
	return tc
}

// TestNarrationKeepsTheOrderItArrivedIn is F81-L1-1. The model wrote its prose
// and then its reasoning; that is the order the streaming arm relays and the
// order the buffered arm must write, on the plain request and on the one that
// takes the turn over alike.
func TestNarrationKeepsTheOrderItArrivedIn(t *testing.T) {
	for _, tc := range []struct {
		note  string
		tools string
		call  api.ToolCall
	}{
		{"plain", r81PlainTools, r81Call("Bash", "cmd", "ls")},
		{"search", r81SearchTools, r81Call("web_search", "query", "news")},
	} {
		chunks := []api.ChatResponse{
			{Model: "test-model", Message: api.Message{Role: "assistant", Content: "prose"}},
			{Model: "test-model", Message: api.Message{Role: "assistant", Thinking: "T"}},
			{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{tc.call}}},
			{Model: "test-model", Message: api.Message{Role: "assistant"}, Done: true, DoneReason: "stop"},
		}
		doc := r81Blocks(t, r81Turn(t, false, tc.tools, chunks), false)
		str := r81Blocks(t, r81Turn(t, true, tc.tools, chunks), true)
		if doc != str {
			t.Errorf("the %s request reached a buffered client as %s and a streaming one as %s — the prose arrived before the reasoning, and both arms of one upstream turn have to say so (2026-09-28 audit, round 81, F81-L1-1)\n%s",
				tc.note, doc, str, doc)
		}
		want := `<text >{text "prose"}<thinking >{thinking "T"}`
		if !strings.HasPrefix(doc, want) {
			t.Errorf("the %s request answered %s, want it to open with %s — the blocks are the order the turn's output arrived in (2026-09-28 audit, round 81, F81-L1-1)",
				tc.note, doc, want)
		}
	}
}

// TestANarrationRunCrossingTheTakeoverIsOneBlock is F81-L1-2. One run is one
// block: reasoning the model wrote across the chunk boundary the takeover
// swallowed is not two blocks, and neither is prose, on the plain request and
// on the search request alike.
func TestANarrationRunCrossingTheTakeoverIsOneBlock(t *testing.T) {
	for _, tc := range []struct {
		note    string
		tools   string
		call    api.ToolCall
		first   api.Message
		second  api.Message
		wantOne string
	}{
		{"plain/reasoning", r81PlainTools, r81Call("Bash", "cmd", "ls"),
			api.Message{Role: "assistant", Thinking: "T1"}, api.Message{Role: "assistant", Thinking: "T2"},
			`<thinking >{thinking "T1T2"}`},
		{"search/reasoning", r81SearchTools, r81Call("web_search", "query", "news"),
			api.Message{Role: "assistant", Thinking: "T1"}, api.Message{Role: "assistant", Thinking: "T2"},
			`<thinking >{thinking "T1T2"}`},
		{"plain/prose", r81PlainTools, r81Call("Bash", "cmd", "ls"),
			api.Message{Role: "assistant", Content: "one"}, api.Message{Role: "assistant", Content: "two"},
			`<text >{text "onetwo"}`},
		{"search/prose", r81SearchTools, r81Call("web_search", "query", "news"),
			api.Message{Role: "assistant", Content: "one"}, api.Message{Role: "assistant", Content: "two"},
			`<text >{text "onetwo"}`},
	} {
		second := tc.second
		second.ToolCalls = []api.ToolCall{tc.call}
		chunks := []api.ChatResponse{
			{Model: "test-model", Message: tc.first},
			{Model: "test-model", Message: second},
			{Model: "test-model", Message: api.Message{Role: "assistant"}, Done: true, DoneReason: "stop"},
		}
		doc := r81Blocks(t, r81Turn(t, false, tc.tools, chunks), false)
		str := r81Blocks(t, r81Turn(t, true, tc.tools, chunks), true)
		if doc != str {
			t.Errorf("the %s request reached a buffered client as %s and a streaming one as %s — one run of one kind is one block on both arms (2026-09-28 audit, round 81, F81-L1-2)\n%s",
				tc.note, doc, str, doc)
		}
		if !strings.HasPrefix(doc, tc.wantOne) {
			t.Errorf("the %s request answered %s, want it to open with %s — the run that crossed the chunk boundary is ONE block, not two (2026-09-28 audit, round 81, F81-L1-2)",
				tc.note, doc, tc.wantOne)
		}
	}
}

// TestANamelessEntrysBytesSurviveTheTakeover is F81-L1-3. An entry the upstream
// never named is the model's own output relayed as text; taking the turn over
// for a web_search call must not drop it on either arm.
func TestANamelessEntrysBytesSurviveTheTakeover(t *testing.T) {
	nameless := api.ToolCall{Function: api.ToolCallFunction{Arguments: api.NewToolCallFunctionArguments()}}
	nameless.Function.Arguments.Set("k", "v")
	for _, tc := range []struct {
		note  string
		tools string
		call  api.ToolCall
	}{
		{"plain", r81PlainTools, r81Call("Bash", "cmd", "ls")},
		{"search", r81SearchTools, r81Call("web_search", "query", "news")},
	} {
		chunks := []api.ChatResponse{
			{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{nameless}}},
			{Model: "test-model", Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{tc.call}}},
			{Model: "test-model", Message: api.Message{Role: "assistant"}, Done: true, DoneReason: "stop"},
		}
		doc := r81Blocks(t, r81Turn(t, false, tc.tools, chunks), false)
		str := r81Blocks(t, r81Turn(t, true, tc.tools, chunks), true)
		want := `<text >{text "{"k":"v"}"}`
		for _, arm := range []struct {
			name string
			body string
		}{{"buffered", doc}, {"streaming", str}} {
			if !strings.HasPrefix(arm.body, want) {
				t.Errorf("the %s arm of the %s request answered %s, want it to open with %s — an entry the upstream never named is the model's own output, relayed as text, and the takeover may not drop it (2026-09-28 audit, round 81, F81-L1-3)",
					arm.name, tc.note, arm.body, want)
			}
		}
		if doc != str {
			t.Errorf("the %s request reached a buffered client as %s and a streaming one as %s — both arms of one upstream turn have to say the same thing (2026-09-28 audit, round 81, F81-L1-3)",
				tc.note, doc, str)
		}
	}
}
