package openai

// openai_integrity_test.go — the OpenAI-compatible wire's own contract, four
// places where oaica told a client something other than what happened
// (2026-09-26 audit, fourth round):
//
//   - finish_reason carried the runner's internal done reason ("load",
//     "unload", anything unrecognised) straight through, and omitted the
//     field entirely for a turn that had ended;
//   - max_completion_tokens — the modern spelling of the output cap — was
//     unmarshalled away, so a client that sent only it got no num_predict at
//     all;
//   - tool_choice "none" was ignored, so a client that forbade tool calls
//     could still be answered with one;
//   - a stop array with a non-string element was half-accepted here while the
//     completions route rejected the same payload, and an all-non-string
//     array left an EMPTY stop list behind, which disables stopping.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// F12: only the four documented finish reasons are ever reported.
func TestFinishReasonIsAlwaysOneOfTheDocumentedFour(t *testing.T) {
	allowed := map[string]bool{"stop": true, "length": true, "tool_calls": true, "content_filter": true}

	for _, dr := range []string{"stop", "length", "tool_calls", "content_filter", "load", "unload", "wat"} {
		for _, hasToolCalls := range []bool{false, true} {
			got := finishReason(dr, hasToolCalls)
			// The one deliberate nil: a turn that ended with no reason at
			// all. Claiming "stop" there would assert a normal completion
			// the runner never reported.
			if got == nil {
				t.Errorf("finishReason(%q, %v) = nil — a turn that ended MUST say why; a nil finish_reason is read by clients as \"still generating\"", dr, hasToolCalls)
				continue
			}
			if !allowed[*got] {
				t.Errorf("finishReason(%q, %v) = %q — the runner's internal reason leaked onto the wire", dr, hasToolCalls, *got)
			}
			if hasToolCalls && *got != "tool_calls" {
				t.Errorf("finishReason(%q, hasToolCalls=true) = %q, want tool_calls", dr, *got)
			}
		}
	}
	if got := finishReason("length", false); got == nil || *got != "length" {
		t.Errorf("finishReason(\"length\", false) = %v, want length", got)
	}
	if got := finishReason("load", false); got == nil || *got != "stop" {
		t.Errorf("finishReason(\"load\", false) = %v, want stop (a loaded model ended its turn normally)", got)
	}
	// No reason at all: reported as absent rather than as a normal stop.
	if got := finishReason("", false); got != nil {
		t.Errorf("finishReason(\"\", false) = %q, want nil — the runner reported no reason, and claiming one invents a completion", *got)
	}
}

// F13: max_completion_tokens is the output cap too.
func TestMaxCompletionTokensSetsNumPredict(t *testing.T) {
	for _, tc := range []struct {
		body string
		want int
	}{
		{`{"model":"m","max_tokens":11,"messages":[{"role":"user","content":"hi"}]}`, 11},
		{`{"model":"m","max_completion_tokens":222,"messages":[{"role":"user","content":"hi"}]}`, 222},
		// Both sent: the modern field wins, which is what OpenAI documents.
		{`{"model":"m","max_tokens":11,"max_completion_tokens":222,"messages":[{"role":"user","content":"hi"}]}`, 222},
	} {
		var req ChatCompletionRequest
		if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
			t.Fatal(err)
		}
		cr, err := FromChatRequest(req)
		if err != nil {
			t.Fatalf("%s: %v", tc.body, err)
		}
		got, ok := cr.Options["num_predict"]
		if !ok {
			t.Errorf("%s: no num_predict reached the runner — the client's output cap was dropped", tc.body)
			continue
		}
		if n, ok := got.(int); !ok || n != tc.want {
			t.Errorf("%s: num_predict = %#v, want %d", tc.body, got, tc.want)
		}
	}
}

// F13b: tool_choice "none" means no tools.
func TestToolChoiceNoneSendsNoTools(t *testing.T) {
	tools := []api.Tool{{Type: "function", Function: api.ToolFunction{Name: "f"}}}
	body := func(tc string) string {
		b, _ := json.Marshal(map[string]any{"model": "m", "messages": []map[string]any{{"role": "user", "content": "hi"}}, "tool_choice": tc})
		return string(b)
	}

	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(body("none")), &req); err != nil {
		t.Fatal(err)
	}
	req.Tools = tools
	cr, err := FromChatRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(cr.Tools) != 0 {
		t.Errorf("tool_choice none still sent %d tools — the model can answer with a tool call the client forbade", len(cr.Tools))
	}

	// The other choices keep them.
	for _, tc := range []string{"auto", "required"} {
		var r2 ChatCompletionRequest
		if err := json.Unmarshal([]byte(body(tc)), &r2); err != nil {
			t.Fatal(err)
		}
		r2.Tools = tools
		c2, err := FromChatRequest(r2)
		if err != nil {
			t.Fatal(err)
		}
		if len(c2.Tools) != 1 {
			t.Errorf("tool_choice %q dropped the tools (%d left)", tc, len(c2.Tools))
		}
	}
}

// F14: a stop array that is not all strings is refused, both routes alike.
func TestStopArrayWithANonStringIsRefused(t *testing.T) {
	var req ChatCompletionRequest
	if err := json.Unmarshal([]byte(`{"model":"m","stop":[123,"STOP"],"messages":[{"role":"user","content":"hi"}]}`), &req); err != nil {
		t.Fatal(err)
	}
	chatReq, err := FromChatRequest(req)
	if err == nil {
		t.Errorf("the chat route accepted a non-string stop element (options = %v) — the string entries became a stop list the client did not write, and an all-non-string array leaves an empty list that DISABLES stopping", chatReq.Options)
	}

	var creq CompletionRequest
	if err := json.Unmarshal([]byte(`{"model":"m","stop":[123,"STOP"],"prompt":"hi"}`), &creq); err != nil {
		t.Fatal(err)
	}
	if _, err := FromCompleteRequest(creq); err == nil {
		t.Error("the completions route accepted a non-string stop element")
	}

	// A well-formed stop array still works on both.
	var ok ChatCompletionRequest
	if err := json.Unmarshal([]byte(`{"model":"m","stop":["A","B"],"messages":[{"role":"user","content":"hi"}]}`), &ok); err != nil {
		t.Fatal(err)
	}
	cr, err := FromChatRequest(ok)
	if err != nil {
		t.Fatalf("a valid stop array was refused: %v", err)
	}
	if len(cr.Options["stop"].([]string)) != 2 {
		t.Errorf("stop = %#v, want both sequences", cr.Options["stop"])
	}
}

// F11: output_index identifies an item in the RESPONSE, not in the chunk.
func TestStreamedToolCallsGetDistinctOutputIndexes(t *testing.T) {
	c := NewResponsesStreamConverter("r", "i", "m", ResponsesRequest{})
	var seen []int
	for n := 0; n < 3; n++ {
		args := api.NewToolCallFunctionArguments()
		args.Set("x", n)
		r := api.ChatResponse{Model: "m", Message: api.Message{Role: "assistant",
			ToolCalls: []api.ToolCall{{Function: api.ToolCallFunction{Name: "f", Arguments: args}}}}}
		for _, ev := range c.Process(r) {
			m, ok := ev.Data.(map[string]any)
			if !ok || ev.Event != "response.output_item.added" {
				continue
			}
			if idx, ok := m["output_index"].(int); ok {
				seen = append(seen, idx)
			}
		}
	}
	if len(seen) != 3 {
		t.Fatalf("saw %d output_item.added events, want 3 (%v)", len(seen), seen)
	}
	distinct := map[int]bool{}
	for _, i := range seen {
		if distinct[i] {
			t.Fatalf("output_index %v repeats — a stream may deliver tool calls one chunk at a time, and indexing by position within the chunk restarts at 0 for each", seen)
		}
		distinct[i] = true
	}
}
