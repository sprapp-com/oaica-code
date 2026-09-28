package server

// round73_undeclared_call_route_integrity_test.go — leg 1, a turn in which the
// model states a tool call the client never declared (2026-09-28 audit, round 73,
// F73-L1-1).
//
// The document arm merges the upstream's chunks and copies `Message.ToolCalls`
// only when the INTERNAL request declared tools (server/routes.go:2435-2437,
// upstream ollama's rule for its own wire). The streaming arm marshals every
// chunk verbatim and has no such gate. So one client request + one upstream body
// reach the client two ways: buffered, a completed empty prose turn; streamed, a
// full tool_use block under stop_reason tool_use. Neither leg 2 nor leg 3 gates a
// relayed upstream call on the declared tool surface.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/middleware"
)

// r73UndeclaredTurn drives the REAL path: writeChatResponse over a channel that
// carried the model's call and then the end of the turn. The internal request
// declares no tools, which is what a client body with no `tools` — or with
// `tool_choice:{"type":"none"}` — converts to.
func r73UndeclaredTurn(t *testing.T, stream bool) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(middleware.AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		req := api.ChatRequest{Stream: &stream}
		args := api.NewToolCallFunctionArguments()
		args.Set("cmd", "ls")
		ch := make(chan any, 2)
		ch <- api.ChatResponse{Model: "test-model", Message: api.Message{
			Role: "assistant",
			ToolCalls: []api.ToolCall{{
				Function: api.ToolCallFunction{Name: "Bash", Arguments: args},
			}},
		}}
		ch <- api.ChatResponse{Model: "test-model", Done: true, DoneReason: "stop",
			Message: api.Message{Role: "assistant"}}
		close(ch)
		writeChatResponse(c, req, ch)
	})

	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	body := `{"model":"test-model","max_tokens":100,"stream":` + streamLit + `,` +
		`"messages":[{"role":"user","content":"list the files"}]}`
	srv := httptest.NewServer(router)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// r73UndeclaredBlocks reads a relayed turn into the tool_use blocks it states
// and the stop_reason.
func r73UndeclaredBlocks(t *testing.T, body string) (calls []string, stop string) {
	t.Helper()
	type open struct {
		id, name string
		parts    strings.Builder
	}
	blocks := map[int]*open{}
	order := []int{}
	add := func(idx int, id, name string) {
		blocks[idx] = &open{id: id, name: name}
		order = append(order, idx)
	}
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(line), "data:"))
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var msg struct {
			Type       string `json:"type"`
			Index      int    `json:"index"`
			StopReason string `json:"stop_reason"`
			Content    []struct {
				Type  string `json:"type"`
				ID    string `json:"id"`
				Name  string `json:"name"`
				Input struct {
					Cmd string `json:"cmd"`
				} `json:"input"`
			} `json:"content"`
			ContentBlock *struct {
				Type string `json:"type"`
				ID   string `json:"id"`
				Name string `json:"name"`
			} `json:"content_block"`
			Delta *struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
				StopReason  string `json:"stop_reason"`
			} `json:"delta"`
		}
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			continue
		}
		// The whole document states the blocks and the verdict in one message.
		if msg.StopReason != "" {
			stop = msg.StopReason
		}
		for i, cb := range msg.Content {
			if cb.Type == "tool_use" {
				add(i, cb.ID, cb.Name)
				blocks[i].parts.WriteString(`{"cmd":"` + cb.Input.Cmd + `"}`)
			}
		}
		// The streamed turn states them as events.
		if msg.ContentBlock != nil && msg.ContentBlock.Type == "tool_use" {
			add(msg.Index, msg.ContentBlock.ID, msg.ContentBlock.Name)
		}
		if msg.Delta != nil {
			if msg.Delta.Type == "input_json_delta" {
				if b := blocks[msg.Index]; b != nil {
					b.parts.WriteString(msg.Delta.PartialJSON)
				}
			}
			if msg.Delta.StopReason != "" {
				stop = msg.Delta.StopReason
			}
		}
	}
	for _, idx := range order {
		b := blocks[idx]
		calls = append(calls, b.id+" "+b.name+" "+b.parts.String())
	}
	return calls, stop
}

// TestACallTheClientDidNotDeclareReachesBothArmsTheSame is the F73-L1-1 pin.
func TestACallTheClientDidNotDeclareReachesBothArmsTheSame(t *testing.T) {
	docCode, docBody := r73UndeclaredTurn(t, false)
	streamCode, streamBody := r73UndeclaredTurn(t, true)

	docCalls, docStop := r73UndeclaredBlocks(t, docBody)
	streamCalls, streamStop := r73UndeclaredBlocks(t, streamBody)

	// Premise: the streaming arm relays the call the model stated.
	if len(streamCalls) == 0 {
		t.Fatalf("PREMISE: the streaming arm relays no call for this turn (status %d, body %q)", streamCode, streamBody)
	}
	if docCode != streamCode {
		t.Errorf("the same turn is answered %d buffered and %d streamed", docCode, streamCode)
	}
	if len(docCalls) != len(streamCalls) {
		t.Errorf("the model's call reaches the client as %v when the turn is buffered and %v when it is streamed — one client request, one upstream body, two answers (body %q) (2026-09-28 audit, round 73, F73-L1-1)",
			docCalls, streamCalls, docBody)
	}
	for i := range docCalls {
		if i < len(streamCalls) && docCalls[i] != streamCalls[i] {
			t.Errorf("call %d is %q buffered and %q streamed", i, docCalls[i], streamCalls[i])
		}
	}
	if docStop != streamStop {
		t.Errorf("the same turn ends with stop_reason %q buffered and %q streamed (2026-09-28 audit, round 73, F73-L1-1)", docStop, streamStop)
	}
}

// r73EmptyAnswerTurn drives a turn whose chunks carry NO content: one chunk that
// states nothing, then the end of the turn.
func r73EmptyAnswerTurn(t *testing.T, stream bool) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(middleware.AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		req := api.ChatRequest{Stream: &stream}
		ch := make(chan any, 2)
		ch <- api.ChatResponse{Model: "test-model", Message: api.Message{Role: "assistant"}}
		ch <- api.ChatResponse{Model: "test-model", Done: true, DoneReason: "stop",
			Message: api.Message{Role: "assistant"}}
		close(ch)
		writeChatResponse(c, req, ch)
	})

	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	body := `{"model":"test-model","max_tokens":100,"stream":` + streamLit + `,` +
		`"messages":[{"role":"user","content":"say nothing"}]}`
	srv := httptest.NewServer(router)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestAnEmptyAnswerStillStatesTheTurn records what each arm writes for a turn
// that carries no content: it is the CONTROL for the empty-channel observation
// (F73-L1-2) — the shape a live client can reach must keep its answer.
func TestAnEmptyAnswerStillStatesTheTurn(t *testing.T) {
	docCode, docBody := r73EmptyAnswerTurn(t, false)
	streamCode, streamBody := r73EmptyAnswerTurn(t, true)
	t.Logf("empty answer buffered: %d %q", docCode, docBody)
	t.Logf("empty answer streamed: %d %q", streamCode, streamBody)
	if docCode != 200 {
		t.Fatalf("a turn that ends with no content is answered %d buffered, want 200", docCode)
	}
	if !strings.Contains(streamBody, "message_stop") {
		t.Errorf("a turn that ends with no content states no terminal event streamed (body %q): the arms must agree about a turn no client can avoid", streamBody)
	}
}

// r73EmptyChannelTurn drives a turn whose upstream channel closes without a
// single chunk.
func r73EmptyChannelTurn(t *testing.T, stream bool) (int, string) {
	t.Helper()
	gin.SetMode(gin.TestMode)

	router := gin.New()
	router.Use(middleware.AnthropicMessagesMiddleware())
	router.POST("/v1/messages", func(c *gin.Context) {
		req := api.ChatRequest{Stream: &stream}
		ch := make(chan any)
		close(ch)
		writeChatResponse(c, req, ch)
	})

	streamLit := "false"
	if stream {
		streamLit = "true"
	}
	body := `{"model":"test-model","max_tokens":100,"stream":` + streamLit + `,` +
		`"messages":[{"role":"user","content":"hi"}]}`
	srv := httptest.NewServer(router)
	defer srv.Close()
	req, _ := http.NewRequest(http.MethodPost, srv.URL+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	defer resp.Body.Close()
	out, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(out)
}

// TestAnUpstreamChannelThatStatesNothingIsRefusedTheSameWay is the F73-L1-2 pin:
// one client request, one upstream shape, and the two arms of this handler must
// answer it the same way — and the way the other two legs answer it, with a 502
// that states why.
func TestAnUpstreamChannelThatStatesNothingIsRefusedTheSameWay(t *testing.T) {
	const want = "upstream returned an empty stream"

	docCode, docBody := r73EmptyChannelTurn(t, false)
	streamCode, streamBody := r73EmptyChannelTurn(t, true)

	if !strings.Contains(docBody, want) || docCode != http.StatusBadGateway {
		t.Fatalf("PREMISE: the buffered arm answers an empty upstream %d %q, want 502 with %q", docCode, docBody, want)
	}
	if streamCode != docCode {
		t.Errorf("an upstream channel that states nothing is answered %d buffered and %d streamed (body %q) (2026-09-28 audit, round 73, F73-L1-2)",
			docCode, streamCode, streamBody)
	}
	if !strings.Contains(streamBody, want) {
		t.Errorf("the streamed arm gives the client %q for an upstream that said nothing, want the same reason the buffered arm states (%q) (2026-09-28 audit, round 73, F73-L1-2)",
			streamBody, want)
	}
}
