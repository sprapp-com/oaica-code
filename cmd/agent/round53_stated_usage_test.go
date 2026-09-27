package agent

// round53_stated_usage_test.go — round 53, L4: the token count the upstream
// STATES for a streamed turn never reached the engine.
//
// anthropicSSEAccumulator dropped message_start and message_delta whole — the
// two frames that carry a usage (input_tokens, cache_read_input_tokens,
// output_tokens) — and the engine keeps only the LAST delta of a turn
// (agent/session.go, *latest = response). So the count the local model stated
// for the prompt was discarded on arrival, and shouldCompact's "prompt_eval"
// trigger (agent/compactor.go, req.Latest.PromptEvalCount) was dead on this leg:
// every session compacted on the character estimate while the model's own count
// of the prompt was in hand. The terminal delta — the message_stop one, which
// carries an empty message and is therefore stored — now carries them, and a
// stream that ends with an explicit [DONE] closes the turn the same way instead
// of emitting no terminal delta at all.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/cmd/launch"
)

// round53UsageStream is one streamed turn, ended either with message_stop or
// with an explicit [DONE].
func round53UsageStream(end string) []string {
	frames := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m","usage":{"input_tokens":1200,"cache_read_input_tokens":1000,"output_tokens":1}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":56}}`,
	}
	if end == "done" {
		return append(frames, `data: [DONE]`)
	}
	return append(frames, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`)
}

func TestTheStatedUsageReachesTheEngineOnTheTerminalDelta(t *testing.T) {
	for _, tc := range []struct{ name, end string }{
		{"message_stop", "stop"},
		{"explicit DONE", "done"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
				writeSSE(w, round53UsageStream(tc.end)...)
			}}
			srv := httptest.NewServer(fake.handler())
			defer srv.Close()

			shim := newShimClient(srv.URL, "test-token", "m", launch.AgentModelMeta{})
			var last api.ChatResponse
			if err := shim.Chat(context.Background(), &api.ChatRequest{Model: "m"}, func(resp api.ChatResponse) error {
				last = resp
				return nil
			}); err != nil {
				t.Fatalf("Chat: %v", err)
			}

			if !last.Done {
				t.Fatalf("the last delta the engine keeps is not the terminal one: %+v", last)
			}
			if last.Metrics.PromptEvalCount != 1200 {
				t.Errorf("the turn's prompt was counted 1200 tokens by the model and %d reached the engine\nthe engine keeps only the last delta of a turn, and the frames that state a usage are the first and the second-to-last, so a count that rides anything else is a count the session never sees: compaction then runs on the character estimate instead of the model's own number",
					last.Metrics.PromptEvalCount)
			}
			if last.Metrics.PromptEvalCachedCount == nil || *last.Metrics.PromptEvalCachedCount != 1000 {
				t.Errorf("the cache-read count the model stated (1000) reached the engine as %v", last.Metrics.PromptEvalCachedCount)
			}
			if last.Metrics.EvalCount != 56 {
				t.Errorf("the turn's output was counted 56 tokens by the model and %d reached the engine", last.Metrics.EvalCount)
			}
		})
	}
}
