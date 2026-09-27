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
//
// Round 54 corrected the shape of this pin. It had hand-written a message_start
// stating 1200 with a 1000-token cache read and asserted the turn's prompt was
// 1200 — a frame no leg of this product emits, and a reading of input_tokens
// this product does not use (see anthropic.go: input_tokens EXCLUDES what
// cache_read_input_tokens counts, and a client's context arithmetic is their
// sum). Both frames the two real legs actually write are pinned here, plus the
// convention itself.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

// round53UsageStream is one streamed turn ended either with message_stop or with
// an explicit [DONE], carrying the usage frames the named leg writes:
//
//   - "gateway": tools/gateway/messages.go states zeros in message_start (it has
//     nothing to say about the prompt until the turn closes) and the prompt, the
//     cache split and the output count together in message_delta (finishStream).
//   - "local": middleware/anthropic.go seeds message_start with an estimate and
//     states the observed usage in message_delta.
//   - "startonly": a leg that states everything on the opening frame and is
//     silent about the prompt on the closing one.
//   - "silent": a leg that never states a prompt count at all.
func round53UsageStream(leg, end string) []string {
	var start, delta string
	switch leg {
	case "gateway":
		start = `{"input_tokens":0,"output_tokens":0}`
		delta = `{"input_tokens":4600,"cache_read_input_tokens":400,"output_tokens":56}`
	case "local":
		start = `{"input_tokens":1000,"output_tokens":1}`
		delta = `{"input_tokens":4600,"cache_read_input_tokens":400,"output_tokens":56}`
	case "startonly":
		start = `{"input_tokens":4600,"cache_read_input_tokens":400,"output_tokens":1}`
		delta = `{"output_tokens":56}`
	case "silent":
		start = `{"input_tokens":0,"output_tokens":0}`
		delta = `{"output_tokens":56}`
	}
	frames := []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m","usage":` + start + `}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + delta + `}`,
	}
	if end == "done" {
		return append(frames, `data: [DONE]`)
	}
	return append(frames, `event: message_stop`+"\n"+`data: {"type":"message_stop"}`)
}

// lastDeltaOfTurn runs one streamed turn and returns the last delta the engine
// keeps, which is the turn's stored response (agent/session.go).
func lastDeltaOfTurn(t *testing.T, frames []string) api.ChatResponse {
	t.Helper()
	fake := &fakeAnthropic{stream: func(w http.ResponseWriter, _ anthropic.MessagesRequest) {
		writeSSE(w, frames...)
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
	return last
}

func TestTheStatedUsageReachesTheEngineOnTheTerminalDelta(t *testing.T) {
	for _, leg := range []string{"gateway", "local", "startonly"} {
		for _, end := range []string{"stop", "done"} {
			t.Run(leg+"/"+end, func(t *testing.T) {
				last := lastDeltaOfTurn(t, round53UsageStream(leg, end))

				// The whole prompt, in this product's convention: input_tokens
				// excludes the cache read, so the two are summed (anthropic.go,
				// UsageFromMetrics converts back the same way). A leg that states
				// 4600 fresh and 400 cached stated a 5000-token prompt.
				if last.Metrics.PromptEvalCount != 5000 {
					t.Errorf("the turn's prompt is 4600 fresh + 400 cached = 5000 tokens and %d reached the engine\nthe engine keeps only the last delta of a turn, and the frames that state a usage are the first and the second-to-last, so a count that rides anything else is a count the session never sees: compaction then runs on the character estimate instead of the model's own number",
						last.Metrics.PromptEvalCount)
				}
				if last.Metrics.PromptEvalCachedCount == nil || *last.Metrics.PromptEvalCachedCount != 400 {
					t.Errorf("the cache-read count the model stated (400) reached the engine as %v", last.Metrics.PromptEvalCachedCount)
				}
				if last.Metrics.EvalCount != 56 {
					t.Errorf("the turn's output was counted 56 tokens by the model and %d reached the engine", last.Metrics.EvalCount)
				}
			})
		}
	}
}

// TestAPromptNoFrameStatedIsNotReportedAsZero: only a positive count is a
// statement, the rule both sibling legs keep. A turn whose upstream said nothing
// about its prompt reports nothing, rather than a zero that would read as "this
// prompt was empty" to the meter and to compaction.
func TestAPromptNoFrameStatedIsNotReportedAsZero(t *testing.T) {
	last := lastDeltaOfTurn(t, round53UsageStream("silent", "stop"))
	if last.Metrics.PromptEvalCount != 0 || last.Metrics.PromptEvalCachedCount != nil {
		t.Errorf("no frame stated a prompt count, yet the terminal delta reports prompt=%d cached=%v; a silent upstream must leave the field unset, not state a zero",
			last.Metrics.PromptEvalCount, last.Metrics.PromptEvalCachedCount)
	}
	if last.Metrics.EvalCount != 56 {
		t.Errorf("the output count 56 that a frame did state reached the engine as %d", last.Metrics.EvalCount)
	}
}
