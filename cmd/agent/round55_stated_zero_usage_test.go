package agent

// round55_stated_zero_usage_test.go — round 55, L1: a stated input_tokens of 0
// is a STATEMENT, not silence.
//
// The Anthropic convention (anthropic.go) makes input_tokens the part of the
// prompt that was NOT served from cache, so a fully cached prompt states
// input_tokens:0 and puts the whole prompt in cache_read_input_tokens — the
// shape both local legs write (llm/llama_server.go's promptEvalCount is
// CacheN+PromptN with PromptN=0 on a full hit; x/mlxrunner states the same
// split). Round 54 taught the accumulator to read the closing frame at all, but
// it treated only a POSITIVE count as a statement — so that zero read as silence
// and the turn fell back to message_start's ESTIMATE: a stream that stated 1000
// on the way in and "0 fresh, 900 cached" on the way out reported a prompt of
// 1900 for a prompt of 900, and shouldCompact's "prompt_eval" trigger
// (agent/compactor.go) fired on a roughly doubled count.
//
// The frames below are the ones the two real legs write, not invented ones: the
// opening frame carries an estimate (middleware/anthropic.go,
// ensureStreamMessageStart) and the closing frame the observed usage.

import (
	"strings"
	"testing"
)

// round55FullCacheHitStream is one turn whose prompt was served entirely from
// cache: message_start estimates 1000 tokens, and message_delta states the real
// split — input_tokens 0, cache_read_input_tokens 900, output 56.
func round55FullCacheHitStream(closeWithInput bool) []string {
	delta := `{"input_tokens":0,"cache_read_input_tokens":900,"output_tokens":56}`
	if !closeWithInput {
		// A leg that is silent about the prompt on the closing frame — the
		// control: the opening frame's estimate is the only count there is.
		delta = `{"output_tokens":56}`
	}
	return []string{
		`event: message_start` + "\n" + `data: {"type":"message_start","message":{"id":"msg_1","type":"message","role":"assistant","content":[],"model":"m","usage":{"input_tokens":1000,"output_tokens":1}}}`,
		`event: content_block_start` + "\n" + `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`,
		`event: content_block_delta` + "\n" + `data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"hi"}}`,
		`event: content_block_stop` + "\n" + `data: {"type":"content_block_stop","index":0}`,
		`event: message_delta` + "\n" + `data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":` + delta + `}`,
		`event: message_stop` + "\n" + `data: {"type":"message_stop"}`,
	}
}

// TestAStatedZeroInputIsNotSilence: the closing frame said the prompt was 0
// fresh and 900 cached, which is a 900-token prompt — not a reason to fall back
// to the opening estimate.
func TestAStatedZeroInputIsNotSilence(t *testing.T) {
	last := lastDeltaOfTurn(t, round55FullCacheHitStream(true))
	if got := last.Metrics.PromptEvalCount; got != 900 {
		t.Errorf("a stream that stated input_tokens=0 with cache_read_input_tokens=900 reported a prompt of %d, want 900: input_tokens EXCLUDES the cache read and the two are summed, so this turn's prompt was 900 — reading the zero as silence fell back to message_start's 1000-token ESTIMATE and reported %d for it, a count the session never had (2026-09-28 audit, round 55)\n",
			got, got)
	}
	if last.Metrics.PromptEvalCachedCount == nil || *last.Metrics.PromptEvalCachedCount != 900 {
		t.Errorf("the cache-read count the model stated (900) reached the engine as %v", last.Metrics.PromptEvalCachedCount)
	}
}

// The control: a closing frame that says NOTHING about the prompt still falls
// back to the opening frame's count, which is the reason the fallback exists —
// the gateway leg states zeros in message_start and everything here, the local
// leg the other way round, and a leg that states it only on the way in must not
// lose it.
func TestAClosingFrameSilentAboutThePromptKeepsTheOpeningCount(t *testing.T) {
	last := lastDeltaOfTurn(t, round55FullCacheHitStream(false))
	if got := last.Metrics.PromptEvalCount; got != 1000 {
		t.Errorf("no frame but message_start stated a prompt, and the turn reports %d rather than its 1000: a closing frame that says nothing about input_tokens is silence, and only a STATED count may overwrite the estimate (2026-09-28 audit, round 55)", got)
	}
	if last.Metrics.EvalCount != 56 {
		t.Errorf("the output count 56 that a frame did state reached the engine as %d", last.Metrics.EvalCount)
	}
}

// The frame shapes above are only meaningful if they are what the accumulator
// reads: this pins the parse, so a typo in the fixtures cannot make both tests
// pass for the wrong reason.
func TestTheFixtureFramesAreTheOnesTheAccumulatorReads(t *testing.T) {
	a := newAnthropicSSEAccumulator()
	for i, frame := range round55FullCacheHitStream(true) {
		event, rest, ok := strings.Cut(frame, "\ndata: ")
		if !ok {
			t.Fatalf("fixture frame %d is not an SSE frame: %q", i, frame)
		}
		event, ok = strings.CutPrefix(event, "event: ")
		if !ok {
			t.Fatalf("fixture frame %d has no event line: %q", i, frame)
		}
		data := rest
		if _, _, err := a.Feed(event, []byte(data)); err != nil {
			t.Fatalf("fixture frame %d (%s) failed to parse: %v", i, event, err)
		}
	}
	if !a.done {
		t.Fatalf("the fixture stream never closed the turn: %q", round55FullCacheHitStream(true))
	}
	if !a.inputStated {
		t.Errorf("the closing frame stated input_tokens and the accumulator did not record the statement — see the fixture: %s", round55FullCacheHitStream(true)[4])
	}
}
