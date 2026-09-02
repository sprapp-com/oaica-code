package anthropic

import (
	"testing"

	"github.com/ollama/ollama/api"
)

// deltaUsage returns the usage a message_delta carried, and whether one was
// emitted at all.
func deltaUsage(events []StreamEvent) (DeltaUsage, bool) {
	for _, e := range events {
		if e.Event != "message_delta" {
			continue
		}
		if d, ok := e.Data.(MessageDeltaEvent); ok {
			return d.Usage, true
		}
	}
	return DeltaUsage{}, false
}

// startUsage returns the usage a message_start carried, and whether one was
// emitted at all.
func startUsage(events []StreamEvent) (Usage, bool) {
	for _, e := range events {
		if e.Event != "message_start" {
			continue
		}
		if d, ok := e.Data.(MessageStartEvent); ok {
			return d.Message.Usage, true
		}
	}
	return Usage{}, false
}

// TestAMetricslessDoneDoesNotEraseThePromptSize is the round-38 contract under
// the cache-read field's new pointer type: the Anthropic SDKs accumulate
// input_tokens (and cache_read_input_tokens) off message_delta whenever the
// field is present, so a done event that states NOTHING must leave both alone.
// Writing a hard 0 there overwrites the count message_start already reported —
// or the estimate seeded for a prompt the upstream never counted — and the
// session's context accounting silently resets to empty, which is exactly the
// "session never appears to grow" failure the estimate exists to prevent
// (2026-09-26 audit, round 38).
func TestAMetricslessDoneDoesNotEraseThePromptSize(t *testing.T) {
	const estimate = 4096
	conv := NewStreamConverter("msg_1", "test-model", estimate)

	start := conv.Process(api.ChatResponse{Message: api.Message{Role: "assistant"}, Done: false})
	usage, ok := startUsage(start)
	if !ok {
		t.Fatalf("no message_start in %#v", start)
	}
	if usage.InputTokens != estimate {
		t.Fatalf("message_start input_tokens = %d, want the seeded estimate %d", usage.InputTokens, estimate)
	}

	// A done event that carries no metrics at all.
	done := conv.Process(api.ChatResponse{Done: true, DoneReason: "stop", Metrics: api.Metrics{}})
	delta, ok := deltaUsage(done)
	if !ok {
		t.Fatalf("no message_delta in %#v", done)
	}
	if delta.InputTokens != estimate {
		t.Errorf("message_delta input_tokens = %d, want %d: a done event with no metrics must not erase the prompt size the client was already told", delta.InputTokens, estimate)
	}
	if delta.CacheReadInputTokens != nil {
		t.Errorf("message_delta cache_read_input_tokens = %d, want it unstated: nothing measured a cache read", *delta.CacheReadInputTokens)
	}
}

// TestAStatedUsageSplitsThePromptIntoUncachedAndCached pins the other half: an
// upstream that DID state usage owns both fields, and input_tokens is the
// UNCACHED prompt — Anthropic's own semantics, and the sum a client computes
// for its context meter (input_tokens + cache_read_input_tokens) is the real
// prompt length rather than twice it.
func TestAStatedUsageSplitsThePromptIntoUncachedAndCached(t *testing.T) {
	conv := NewStreamConverter("msg_2", "test-model", 4096)

	conv.Process(api.ChatResponse{Message: api.Message{Role: "assistant"}, Done: false})
	done := conv.Process(api.ChatResponse{
		Done:       true,
		DoneReason: "stop",
		Metrics: api.Metrics{
			PromptEvalCount:       100,
			PromptEvalCachedCount: testIntPtr(80),
			EvalCount:             7,
		},
	})
	delta, ok := deltaUsage(done)
	if !ok {
		t.Fatalf("no message_delta in %#v", done)
	}
	if delta.InputTokens != 20 {
		t.Errorf("input_tokens = %d, want 20 (100 stated prompt - 80 cached)", delta.InputTokens)
	}
	if delta.CacheReadInputTokens == nil || *delta.CacheReadInputTokens != 80 {
		t.Errorf("cache_read_input_tokens = %v, want 80", delta.CacheReadInputTokens)
	}
	if delta.OutputTokens != 7 {
		t.Errorf("output_tokens = %d, want 7", delta.OutputTokens)
	}
}

// TestAFullyCachedPromptReportsZeroInputWithTheCacheStated is the corner the
// "only overwrite when input_tokens > 0" reading would get wrong: a prompt
// served entirely from cache has a real uncached count of 0, and the cache
// field is what shows the read happened. Erasing it would tell the client a
// 100-token prompt was empty.
func TestAFullyCachedPromptReportsZeroInputWithTheCacheStated(t *testing.T) {
	conv := NewStreamConverter("msg_3", "test-model", 4096)

	conv.Process(api.ChatResponse{Message: api.Message{Role: "assistant"}, Done: false})
	done := conv.Process(api.ChatResponse{
		Done:       true,
		DoneReason: "stop",
		Metrics: api.Metrics{
			PromptEvalCount:       100,
			PromptEvalCachedCount: testIntPtr(100),
			EvalCount:             1,
		},
	})
	delta, ok := deltaUsage(done)
	if !ok {
		t.Fatalf("no message_delta in %#v", done)
	}
	if delta.InputTokens != 0 {
		t.Errorf("input_tokens = %d, want 0 for a fully cached prompt", delta.InputTokens)
	}
	if delta.CacheReadInputTokens == nil || *delta.CacheReadInputTokens != 100 {
		t.Fatalf("cache_read_input_tokens = %v, want 100: a fully-cached prompt is not an empty one", delta.CacheReadInputTokens)
	}
}
