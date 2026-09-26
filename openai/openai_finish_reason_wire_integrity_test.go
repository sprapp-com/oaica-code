package openai

// openai_finish_reason_wire_integrity_test.go — the same api.ChatResponse must
// not say two different things about why the turn ended (2026-09-27 audit,
// round 19).
//
// finishReason() maps the runner's internal done reasons onto OpenAI's enum, and
// the streaming chunk, the completion and both completion chunks call it. The
// non-streaming /v1/chat/completions builder was left on the verbatim closure it
// replaced, so `POST /v1/chat/completions {"stream":false}` answered
// `"finish_reason":"load"` — a value outside OpenAI's enum, which the OpenAI
// Python SDK types as a Literal and therefore raises on — while the identical
// response with `"stream":true` answered `"stop"`. The helper's own doc comment
// records that the leak "went out on all three wires"; this was the call site
// the conversion missed.
//
// The trigger needs no exotic state: an empty message list reaches
// server/routes.go's `len(req.Messages) == 0` branch, which replies with
// DoneReason "load", and openai.FromChatRequest turns a `"content": []` message
// into no internal message at all.

import (
	"testing"

	"github.com/ollama/ollama/api"
)

// lastFinishReason is the reason the streaming wire ended on — the last chunk
// that states one, which is what a client keys on.
func lastFinishReason(chunks []ChatCompletionChunk) *string {
	var reason *string
	for _, chunk := range chunks {
		if len(chunk.Choices) > 0 && chunk.Choices[0].FinishReason != nil {
			reason = chunk.Choices[0].FinishReason
		}
	}
	return reason
}

func TestFinishReasonAgreesBetweenTheStreamingAndNonStreamingWires(t *testing.T) {
	for _, tc := range []struct {
		name      string
		done      string
		toolCalls bool
	}{
		{name: "a loaded model", done: "load"},
		{name: "an unloaded model", done: "unload"},
		{name: "a normal stop", done: "stop"},
		{name: "a token limit", done: "length"},
		{name: "a filtered turn", done: "content_filter"},
		{name: "a tool call", done: "stop", toolCalls: true},
		{name: "no reason at all", done: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			response := api.ChatResponse{Model: "m", DoneReason: tc.done, Message: api.Message{Role: "assistant"}}
			if tc.toolCalls {
				response.Message.ToolCalls = []api.ToolCall{{Function: api.ToolCallFunction{Name: "Read", Arguments: api.NewToolCallFunctionArguments()}}}
			}

			nonStream := ToChatCompletion("id", response).Choices[0].FinishReason
			stream := lastFinishReason(ToChunks("id", response, false))

			switch {
			case nonStream == nil && stream == nil:
				if tc.done != "" {
					t.Fatalf("both wires reported no finish_reason for done_reason %q — a client reads that as still generating", tc.done)
				}
				return
			case nonStream == nil || stream == nil:
				t.Fatalf("the wires disagree about whether the turn ended: non-streaming %v, streaming %v (done_reason %q)", fmtReason(nonStream), fmtReason(stream), tc.done)
			}
			if *nonStream != *stream {
				t.Errorf("the same response says finish_reason %q with stream:false and %q with stream:true (done_reason %q)", *nonStream, *stream, tc.done)
			}
			if got := *nonStream; !oneOfTheDocumentedFinishReasons(got) {
				t.Errorf("finish_reason %q is not one of OpenAI's documented values — a strict client cannot parse it (done_reason %q)", got, tc.done)
			}
		})
	}
}

func oneOfTheDocumentedFinishReasons(reason string) bool {
	switch reason {
	case "stop", "length", "tool_calls", "function_call", "content_filter":
		return true
	}
	return false
}

func fmtReason(reason *string) string {
	if reason == nil {
		return "null"
	}
	return *reason
}
