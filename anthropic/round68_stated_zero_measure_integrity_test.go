package anthropic

// round68_stated_zero_measure_integrity_test.go — one predicate, three sites.
//
// Round 67 settled the rule for this wire: a stated zero is a READING. The
// terminal message_delta already asked the cache field for PRESENCE
// (CacheReadInputTokens != nil) for exactly that reason, while message_start —
// and the middleware's whole-document arm — asked it by VALUE and substituted
// the estimate whenever the upstream stated 0. One turn whose upstream counted
// an empty prompt beside a stated cache read therefore told the client the
// estimate in message_start and 0 in message_delta, erasing the count it had
// just given, and the whole-document arm of the same middleware told a
// non-streaming client the estimate for the same body (2026-09-28 audit,
// round 68, F68-L1-2).

import (
	"testing"

	"github.com/ollama/ollama/api"
)

// TestAStatedZeroPromptIsNotAnAbsentMeasurement is the streaming half: the two
// events of ONE turn must state the same number, and it is the upstream's.
func TestAStatedZeroPromptIsNotAnAbsentMeasurement(t *testing.T) {
	const estimate = 4096
	metrics := api.Metrics{PromptEvalCount: 0, PromptEvalCachedCount: testIntPtr(0)}
	conv := NewStreamConverter("msg_r68a", "test-model", estimate)

	start := conv.Process(api.ChatResponse{
		Message: api.Message{Role: "assistant"},
		Metrics: metrics,
	})
	su, ok := startUsage(start)
	if !ok {
		t.Fatalf("no message_start in %#v", start)
	}
	if su.InputTokens != 0 {
		t.Errorf("message_start input_tokens = %d, want the upstream's stated 0: the cache field is stated, so the estimate does not own this turn (2026-09-28 audit, round 68, F68-L1-2)", su.InputTokens)
	}

	done := conv.Process(api.ChatResponse{
		Done:       true,
		DoneReason: "stop",
		Metrics:    metrics,
	})
	du, ok := deltaUsage(done)
	if !ok {
		t.Fatalf("no message_delta in %#v", done)
	}
	if du.InputTokens != su.InputTokens {
		t.Errorf("one turn reports two prompt sizes: message_start said %d and message_delta says %d — the terminal event must not erase what the start stated (2026-09-28 audit, round 68, F68-L1-2)",
			su.InputTokens, du.InputTokens)
	}
}

// TestASilentMeasurementStillGetsTheEstimate is the control: with BOTH fields
// unstated the estimate is still what the client is told, on both events. The
// fix must not read "stated cache" into an upstream that said nothing.
func TestASilentMeasurementStillGetsTheEstimate(t *testing.T) {
	const estimate = 4096
	silent := api.Metrics{}
	conv := NewStreamConverter("msg_r68b", "test-model", estimate)

	start := conv.Process(api.ChatResponse{Message: api.Message{Role: "assistant"}, Metrics: silent})
	su, ok := startUsage(start)
	if !ok {
		t.Fatalf("no message_start in %#v", start)
	}
	if su.InputTokens != estimate {
		t.Errorf("message_start input_tokens = %d, want the estimate %d for an upstream that stated nothing", su.InputTokens, estimate)
	}

	done := conv.Process(api.ChatResponse{Done: true, DoneReason: "stop", Metrics: silent})
	du, ok := deltaUsage(done)
	if !ok {
		t.Fatalf("no message_delta in %#v", done)
	}
	if du.InputTokens != estimate {
		t.Errorf("message_delta input_tokens = %d, want the estimate %d: a metrics-less done must not erase the prompt size the client was already told (round 38)",
			du.InputTokens, estimate)
	}
}
