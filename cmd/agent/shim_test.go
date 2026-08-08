package agent

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/ollama/ollama/agent"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

// fakeAnthropic serves Anthropic /v1/messages with a pluggable stream writer
// and records the requests it receives.
type fakeAnthropic struct {
	mu        sync.Mutex
	requests  []anthropic.MessagesRequest
	stream    func(w http.ResponseWriter, req anthropic.MessagesRequest)
	status    int
	errorBody string
}

func (f *fakeAnthropic) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer test-token" {
			http.Error(w, `{"type":"authentication_error","message":"bad token"}`, http.StatusUnauthorized)
			return
		}
		var m anthropic.MessagesRequest
		if err := json.NewDecoder(r.Body).Decode(&m); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		f.mu.Lock()
		f.requests = append(f.requests, m)
		f.mu.Unlock()
		if f.status != 0 {
			w.WriteHeader(f.status)
			_, _ = w.Write([]byte(f.errorBody))
			return
		}
		if f.stream != nil {
			f.stream(w, m)
		}
	})
	return mux
}

func writeSSE(w http.ResponseWriter, frames ...string) {
	w.Header().Set("Content-Type", "text/event-stream")
	for _, frame := range frames {
		_, _ = w.Write([]byte(frame))
		_, _ = w.Write([]byte("\n\n"))
	}
}

func textStream() []string {
	return []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m"}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello, "}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"world!"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
}

// TestShimClientTextStream: deltas stream to the callback, Done frame arrives
// at message_stop, Chat returns nil.
func TestShimClientTextStream(t *testing.T) {
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w, textStream()...)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	var got []string
	var done bool
	err := shim.Chat(context.Background(), &api.ChatRequest{Model: "m"}, func(resp api.ChatResponse) error {
		if resp.Message.Content != "" {
			got = append(got, resp.Message.Content)
		}
		if resp.Done {
			done = true
		}
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if strings.Join(got, "") != "Hello, world!" {
		t.Errorf("streamed content = %q", strings.Join(got, ""))
	}
	if !done {
		t.Error("expected a Done frame after message_stop")
	}
}

// TestShimClientToolUse: a tool_use block streams in and is emitted as a
// ToolCall once at content_block_stop.
func TestShimClientToolUse(t *testing.T) {
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/a.txt\"}"}}`,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
		)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	var calls []api.ToolCall
	err := shim.Chat(context.Background(), &api.ChatRequest{Model: "m"}, func(resp api.ChatResponse) error {
		calls = append(calls, resp.Message.ToolCalls...)
		return nil
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(calls) != 1 || calls[0].Function.Name != "read_file" {
		t.Fatalf("calls = %#v", calls)
	}
	if calls[0].Function.Arguments.ToMap()["path"] != "/tmp/a.txt" {
		t.Errorf("args = %#v", calls[0].Function.Arguments.ToMap())
	}
}

// TestShimClientErrorEvent: an SSE error event surfaces as a non-nil error.
func TestShimClientErrorEvent(t *testing.T) {
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w, `event: error`+"\n"+`data: {"type":"error","error":{"type":"overloaded_error","message":"try later"}}`)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	err := shim.Chat(context.Background(), &api.ChatRequest{Model: "m"}, func(api.ChatResponse) error { return nil })
	if err == nil || !strings.Contains(err.Error(), "try later") {
		t.Fatalf("err = %v, want upstream message", err)
	}
}

// TestShimClientHTTPError: a non-2xx response yields a descriptive error for
// every error body shape the endpoints emit: the nested Anthropic
// {"type":"error","error":{...}} shape, a flat {"type":...,"message":...}
// shape, and a raw body in neither shape (e.g. {"error":"model not found"}).
func TestShimClientHTTPError(t *testing.T) {
	cases := []struct {
		name      string
		status    int
		errorBody string
		want      string
	}{
		{
			name:      "nested anthropic error",
			status:    http.StatusTooManyRequests,
			errorBody: `{"type":"error","error":{"type":"rate_limit_error","message":"slow down"}}`,
			want:      "slow down",
		},
		{
			name:      "flat anthropic error",
			status:    http.StatusUnauthorized,
			errorBody: `{"type":"authentication_error","message":"bad key"}`,
			want:      "bad key",
		},
		{
			name:      "raw body fallback",
			status:    http.StatusNotFound,
			errorBody: `{"error":"model not found"}`,
			want:      `{"error":"model not found"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAnthropic{status: tc.status, errorBody: tc.errorBody}
			srv := httptest.NewServer(fake.handler())
			defer srv.Close()

			shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
			err := shim.Chat(context.Background(), &api.ChatRequest{Model: "m"}, func(api.ChatResponse) error { return nil })
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err = %v, want error containing %q", err, tc.want)
			}
		})
	}
}

// stubTool is a minimal agent.Tool for the integration test.
type stubTool struct{}

func (stubTool) Name() string        { return "read_file" }
func (stubTool) Description() string { return "stub read_file" }
func (stubTool) Schema() api.ToolFunction {
	return api.ToolFunction{Name: "read_file", Parameters: api.ToolFunctionParameters{Type: "object"}}
}
func (stubTool) Execute(ctx context.Context, tc agent.ToolContext, args map[string]any) (agent.ToolResult, error) {
	return agent.ToolResult{Content: "stub file contents"}, nil
}

// TestSessionRunToolLoop: an end-to-end agent.Session.Run over the shim. The
// fake streams a read_file tool_use on the first call, then a plain text
// answer once it sees the tool_result fed back — proving the shim's round
// trip (tool call → execute → feed result → next model call).
func TestSessionRunToolLoop(t *testing.T) {
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, req anthropic.MessagesRequest) {
		hasToolResult := false
		for _, m := range req.Messages {
			for _, b := range m.Content {
				if b.Type == "tool_result" {
					hasToolResult = true
				}
			}
		}
		if !hasToolResult {
			writeSSE(w,
				`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`,
				`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":"{\"path\":\"/tmp/a.txt\"}"}}`,
				`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
				`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
			)
			return
		}
		writeSSE(w,
			`event: content_block_start`+"\n"+`data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
			`event: content_block_delta`+"\n"+`data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Done reading."}}`,
			`event: content_block_stop`+"\n"+`data: {"type":"content_block_stop","index":0}`,
			`event: message_stop`+"\n"+`data: {"type":"message_stop"}`,
		)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{ToolCapable: true})
	sess := &agent.Session{
		Client: shim,
		Tools:  &agent.Registry{},
	}
	sess.Tools.Register(stubTool{})

	result, err := sess.Run(context.Background(), agent.RunOptions{
		Model:    "m",
		Messages: []api.Message{{Role: "user", Content: "read /tmp/a.txt"}},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if len(result.Messages) == 0 {
		t.Fatal("no messages in result")
	}
	last := result.Messages[len(result.Messages)-1]
	if !strings.Contains(last.Content, "Done reading.") {
		t.Errorf("final assistant content = %q, want the tool-fed answer", last.Content)
	}

	// The second request must have carried the tool_result back to the model.
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if len(fake.requests) < 2 {
		t.Fatalf("requests = %d, want >= 2 (model → tool result → model)", len(fake.requests))
	}
	lastReq := fake.requests[len(fake.requests)-1]
	found := false
	for _, m := range lastReq.Messages {
		for _, b := range m.Content {
			if b.Type == "tool_result" && b.ToolUseID == "toolu_1" && b.Content == "stub file contents" {
				found = true
			}
		}
	}
	if !found {
		t.Error("tool_result was not fed back to the model in the final request")
	}
}
