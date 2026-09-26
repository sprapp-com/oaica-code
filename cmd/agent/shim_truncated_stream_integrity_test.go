package agent

// shim_truncated_stream_integrity_test.go — shimClient.Chat looped over the SSE
// frames and returned nil when the body simply ended. The accumulator's `done`
// flag was set by message_stop and never read afterwards, so a connection cut
// after the first text deltas produced a *clean* turn: whatever content had
// arrived was reported as the assistant's complete answer, the round succeeded,
// and nothing anywhere said the rest of the response was missing
// (2026-09-26 audit).
//
// The Anthropic Messages protocol ends a stream with message_stop; a body that
// ends without it is truncated, and the engine has the error path to say so
// (chatRound returns it, and isContextCanceledError only reclassifies an actual
// cancellation).

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

func TestATruncatedStreamIsNotACleanTurn(t *testing.T) {
	// Everything up to the text, then nothing: no content_block_stop, no
	// message_stop, no [DONE].
	truncated := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m"}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"Hello, "}}`,
	}
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w, truncated...)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	var got strings.Builder
	err := shim.Chat(t.Context(), &api.ChatRequest{Model: "m"}, func(r api.ChatResponse) error {
		got.WriteString(r.Message.Content)
		return nil
	})
	if err == nil {
		t.Errorf("Chat returned nil for a stream that ended without message_stop — the partial answer %q was reported as a complete turn", got.String())
	}
	if !strings.Contains(got.String(), "Hello, ") {
		t.Errorf("the deltas that did arrive should still have been delivered, got %q", got.String())
	}
}

// The control: a stream that DOES end with message_stop is still a clean turn.
func TestACompleteStreamIsStillACleanTurn(t *testing.T) {
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w, textStream()...)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	var done bool
	if err := shim.Chat(t.Context(), &api.ChatRequest{Model: "m"}, func(r api.ChatResponse) error {
		if r.Done {
			done = true
		}
		return nil
	}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if !done {
		t.Errorf("the Done frame never reached the callback")
	}
}

// [DONE] is an explicit end-of-stream marker, not a truncation, so a proxy
// that emits it instead of message_stop still yields a clean turn.
func TestAnExplicitDoneMarkerIsNotATruncation(t *testing.T) {
	frames := append(textStream()[:len(textStream())-1], `data: [DONE]`)
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w, frames...)
	}}
	srv := httptest.NewServer(fake.handler())
	defer srv.Close()

	shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
	if err := shim.Chat(t.Context(), &api.ChatRequest{Model: "m"}, func(api.ChatResponse) error {
		return nil
	}); err != nil {
		t.Errorf("an explicit [DONE] terminator was treated as a truncated stream: %v", err)
	}
}
