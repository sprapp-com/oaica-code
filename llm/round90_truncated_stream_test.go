package llm

// round90_truncated_stream_test.go — leg 1, F90-L1-1 (2026-09-29 audit, round
// 90). The streaming readers of this package reported SUCCESS when the
// upstream's body ended at a clean SSE boundary with no final response: the
// `[DONE]` sentinel is skipped, the scanner sees no error, `hasFinalResp` is
// false, and the reader returned nil having handed the caller no response that
// says the turn was over.
//
// One upstream body then had two readings up the stack. The buffered arm merged
// the chunks that HAD arrived and closed the turn with `end_turn` and HTTP 200
// — a truncated answer sold to the client as the model's whole reply, which a
// caller never retries. The streaming arm wrote no terminal event at all: no
// `message_delta`, no `message_stop`, no `error`, so a client that reads the
// stream waits forever for a turn the model already stopped writing.
//
// The neighbouring shape — the transport dying (`unexpected EOF`, `forcibly
// closed`) — is reported as a failure and reaches both arms with its reason.
// This one is the same event from the reader's side (the turn ended without a
// final response) and is reported the same way now, so the runner is the one
// place that has to know it.
//
// The sentence is the family's: "upstream stream ended before the response was
// complete", the same cause the other two translation legs name in the same
// words.

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"golang.org/x/sync/semaphore"
)

const r90TruncatedSentence = "upstream stream ended before the response was complete"

// r90FakeLlamaServer answers /health and one streaming path with `lines`, then
// closes the body at a clean boundary. The port is what a runner's URL field is
// built from in this package's tests.
func r90FakeLlamaServer(t *testing.T, path string, lines []string) *llamaServerRunner {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/health":
			fmt.Fprint(w, `{"status":"ok"}`)
		case path:
			w.Header().Set("Content-Type", "text/event-stream")
			for _, line := range lines {
				fmt.Fprintln(w, line)
			}
		default:
			t.Errorf("unexpected path: %s", r.URL.Path)
		}
	}))
	t.Cleanup(srv.Close)

	parts := strings.Split(srv.URL, ":")
	var portInt int
	fmt.Sscanf(parts[len(parts)-1], "%d", &portInt)
	return &llamaServerRunner{
		port:    portInt,
		cmd:     fakeRunningCmd(),
		sem:     semaphore.NewWeighted(1),
		options: api.Options{Runner: api.Runner{NumCtx: 2048}},
	}
}

func TestAChatStreamThatEndsWithoutAFinishChunkIsAFailure(t *testing.T) {
	runner := r90FakeLlamaServer(t, "/v1/chat/completions", []string{
		`data: {"choices":[{"delta":{"content":"the model wrote this much"}}]}`,
		`data: {"choices":[{"delta":{"content":" and then the runner died"}}]}`,
		`data: [DONE]`,
	})

	var responses []ChatResponse
	opts := api.DefaultOptions()
	err := runner.Chat(t.Context(), ChatRequest{
		Messages: []api.Message{{Role: "user", Content: "test prompt"}},
		Options:  &opts,
	}, func(cr ChatResponse) { responses = append(responses, cr) })

	if err == nil {
		t.Fatalf("a chat stream that ended at a clean boundary with no finish_reason was reported as a SUCCESS: the reader handed the caller %d chunks and no final response, and nothing above it can tell this turn from one the model finished (2026-09-29 audit, round 90, F90-L1-1)", len(responses))
	}
	if !strings.Contains(err.Error(), r90TruncatedSentence) {
		t.Errorf("the truncated turn was reported as %q, want the family's sentence %q — one cause, the same words the other two translation legs name it in (2026-09-29 audit, round 90, F90-L1-1)", err.Error(), r90TruncatedSentence)
	}
	for i, r := range responses {
		if r.Done {
			t.Errorf("response %d was marked Done for a turn whose stream never said it finished:\n%+v", i, r)
		}
	}

	// The control: the same wire WITH a finish_reason is a finished turn and is
	// reported as one — the refusal above must not become "any stream is a
	// failure".
	control := r90FakeLlamaServer(t, "/v1/chat/completions", []string{
		`data: {"choices":[{"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}],"timings":{"prompt_n":5,"predicted_n":2}}`,
		`data: [DONE]`,
	})
	var done []ChatResponse
	if err := control.Chat(t.Context(), ChatRequest{
		Messages: []api.Message{{Role: "user", Content: "test prompt"}},
		Options:  &opts,
	}, func(cr ChatResponse) { done = append(done, cr) }); err != nil {
		t.Fatalf("a finished turn was reported as a failure: %v", err)
	}
	if len(done) == 0 || !done[len(done)-1].Done {
		t.Fatalf("a finished turn delivered %d chunks and no final response:\n%+v", len(done), done)
	}
}

func TestACompletionStreamThatEndsWithoutAStopChunkIsAFailure(t *testing.T) {
	runner := r90FakeLlamaServer(t, "/completion", []string{
		`data: {"content":"the model wrote this much","stop":false}`,
		`data: {"content":" and then the runner died","stop":false}`,
	})

	var responses []CompletionResponse
	opts := api.DefaultOptions()
	err := runner.Completion(t.Context(), CompletionRequest{
		Prompt:  "test prompt",
		Options: &opts,
	}, func(cr CompletionResponse) { responses = append(responses, cr) })

	if err == nil {
		t.Fatalf("a completion stream that ended with no stop chunk was reported as a SUCCESS after %d chunks (2026-09-29 audit, round 90, F90-L1-1)", len(responses))
	}
	if !strings.Contains(err.Error(), r90TruncatedSentence) {
		t.Errorf("the truncated completion was reported as %q, want %q (2026-09-29 audit, round 90, F90-L1-1)", err.Error(), r90TruncatedSentence)
	}
	for i, r := range responses {
		if r.Done {
			t.Errorf("response %d was marked Done for a stream that never said it finished:\n%+v", i, r)
		}
	}
}
