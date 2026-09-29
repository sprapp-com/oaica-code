package server

// round101_leg1_responses_truncation_verdict_test.go — leg 1, round 101
// (2026-09-29 audit), F101-L1-1.
//
// Every surface of this leg states a turn the model stopped at its cap: the
// native wire `"done_reason":"length"`, OpenAI chat `"finish_reason":"length"`,
// Anthropic `"stop_reason":"max_tokens"` — and the Responses surface said
// `"status":"completed"` with `"incomplete_details":null`, on both of its arms:
//
//	{"message":{"role":"assistant","content":"partial"}}
//	{"message":{"role":"assistant"},"done":true,"done_reason":"length",...}
//	  native     done_reason length
//	  openai     finish_reason length
//	  anthropic  stop_reason max_tokens
//	  responses  status completed, incomplete_details null   <- the one silence
//
// A client that reads the truncation the way this wire defines it
// (`status == "incomplete"`, `incomplete_details.reason == "max_output_tokens"`
// — openai-python types both) was told a capped answer was the whole one, and
// the streaming arm's terminal event was `response.completed` with no
// `response.incomplete` anywhere in the tree. `max_output_tokens` is forwarded
// as `num_predict` and the runner's cap path sets `done_reason:"length"`, so
// this is reachable with a plain client body and an ordinary upstream body.
//
// The call-priority half is deliberate and pinned by the second test: the chat
// arm's `finishReason` and the Anthropic arm's `mapStopReason` both state the
// CALLS when a turn carried them, whatever the cap said, so the Responses arm
// states its cap verdict under the same priority — a truncated turn that
// delivered calls is not called truncated on any of the three translated
// surfaces.

import (
	"context"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/llm"
)

// r101CappedChunks is the turn the model stopped at its cap with no call.
func r101CappedChunks() []llm.ChatResponse {
	return []llm.ChatResponse{
		{Message: api.Message{Role: "assistant", Content: "partial"}},
		{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonLength, PromptEvalCount: 5, EvalCount: 3},
	}
}

// TestMine101EveryArmStatesTheCapsVerdict is F101-L1-1: one upstream body, one
// verdict, in each surface's own word for it.
func TestMine101EveryArmStatesTheCapsVerdict(t *testing.T) {
	for _, stream := range []bool{false, true} {
		for _, surface := range []string{"native", "openai", "responses", "anthropic"} {
			name := surface + "/buffered"
			if stream {
				name = surface + "/streamed"
			}
			t.Run(name, func(t *testing.T) {
				code, body, _ := zzRunChunks(t, r101CappedChunks(), surface, "zz-vw", stream)
				if code != 200 {
					t.Fatalf("status %d\n%s", code, body)
				}
				switch surface {
				case "native":
					if !strings.Contains(body, `"done_reason":"length"`) {
						t.Errorf("the native arm does not state the cap: %s", body)
					}
				case "openai":
					if !strings.Contains(body, `"finish_reason":"length"`) {
						t.Errorf("the chat arm does not state the cap: %s", body)
					}
				case "anthropic":
					if !strings.Contains(body, `"stop_reason":"max_tokens"`) {
						t.Errorf("the Anthropic arm does not state the cap: %s", body)
					}
				case "responses":
					// This wire's own spelling of the same verdict: the status
					// and the reason, and on the streaming arm the terminal
					// event that carries them.
					if !strings.Contains(body, `"status":"incomplete"`) ||
						!strings.Contains(body, `"reason":"max_output_tokens"`) {
						t.Errorf("the Responses arm does not state the cap: %s", body)
					}
					if stream {
						if !strings.Contains(body, "event: response.incomplete") {
							t.Errorf("the streamed Responses arm's terminal event is not the wire's incomplete event: %s", body)
						}
						if strings.Contains(body, "event: response.completed") {
							t.Errorf("the streamed Responses arm states the capped turn as completed: %s", body)
						}
					}
				}
			})
		}
	}
}

// TestMine101AStoppedTurnWithCallsIsTheCallsOnEveryArm pins the priority the fix
// keeps: a capped turn that delivered a call is stated by the calls, as the chat
// and Anthropic arms have always stated it, so the Responses arm states no cap
// verdict for it either.
func TestMine101AStoppedTurnWithCallsIsTheCallsOnEveryArm(t *testing.T) {
	chunks := []llm.ChatResponse{
		{Message: api.Message{Role: "assistant", ToolCalls: []api.ToolCall{{
			Function: api.ToolCallFunction{Name: "Bash", Arguments: api.NewToolCallFunctionArguments()},
		}}}},
		{Message: api.Message{Role: "assistant"}, Done: true, DoneReason: llm.DoneReasonLength},
	}
	chunks[0].Message.ToolCalls[0].Function.Arguments.Set("cmd", "ls")

	for _, stream := range []bool{false, true} {
		for _, surface := range []string{"openai", "responses", "anthropic"} {
			name := surface + "/buffered"
			if stream {
				name = surface + "/streamed"
			}
			t.Run(name, func(t *testing.T) {
				code, body, _ := zzRunChunks(t, chunks, surface, "zz-vw", stream)
				if code != 200 {
					t.Fatalf("status %d\n%s", code, body)
				}
				switch surface {
				case "openai":
					if !strings.Contains(body, `"finish_reason":"tool_calls"`) {
						t.Errorf("the chat arm states the cap over the call: %s", body)
					}
				case "anthropic":
					if !strings.Contains(body, `"stop_reason":"tool_use"`) {
						t.Errorf("the Anthropic arm states the cap over the call: %s", body)
					}
				case "responses":
					if strings.Contains(body, `"status":"incomplete"`) {
						t.Errorf("the Responses arm states a capped turn with a call as incomplete, where its sibling arms state the call: %s", body)
					}
				}
			})
		}
	}
	_ = context.Background
}
