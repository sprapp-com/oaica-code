package agent

import (
	"strconv"
	"strings"
	"testing"
)

// textDeltaFrame builds a content_block_delta frame with a text_delta for the
// given index. These fixtures exercise the parser directly.
func textDeltaFrame(index int, text string) string {
	return `{"type":"content_block_delta","index":` + strconv.Itoa(index) + `,"delta":{"type":"text_delta","text":` + strconv.Quote(text) + `}}`
}

// TestFeedTextSequence: a text block streams deltas and terminates on
// message_stop with a Done frame.
func TestFeedTextSequence(t *testing.T) {
	acc := newAnthropicSSEAccumulator()

	deltas, done, err := acc.Feed("content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"text","text":""}}`))
	if err != nil || len(deltas) != 0 || done {
		t.Fatalf("start: deltas=%d done=%v err=%v", len(deltas), done, err)
	}
	for _, chunk := range []string{"Hello", ", ", "world"} {
		deltas, done, err = acc.Feed("content_block_delta", []byte(textDeltaFrame(0, chunk)))
		if err != nil {
			t.Fatalf("delta %q: %v", chunk, err)
		}
		if done || len(deltas) != 1 {
			t.Fatalf("delta %q: deltas=%d done=%v", chunk, len(deltas), done)
		}
		if deltas[0].Message.Content != chunk {
			t.Errorf("delta content = %q, want %q", deltas[0].Message.Content, chunk)
		}
	}
	deltas, done, err = acc.Feed("message_stop", []byte(`{"type":"message_stop"}`))
	if err != nil {
		t.Fatalf("stop: %v", err)
	}
	if !done || len(deltas) != 1 || !deltas[0].Done {
		t.Fatalf("stop: done=%v deltas=%#v", done, deltas)
	}
	// A second message_stop must be inert.
	_, done, _ = acc.Feed("message_stop", []byte(`{"type":"message_stop"}`))
	if done {
		t.Fatal("second message_stop should not set done again")
	}
}

// TestFeedToolUseAccumulation: input_json_delta fragments accumulate into a
// single ToolCall emitted once at content_block_stop.
func TestFeedToolUseAccumulation(t *testing.T) {
	acc := newAnthropicSSEAccumulator()

	_, _, err := acc.Feed("content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"tool_use","id":"toolu_1","name":"read_file","input":{}}}`))
	if err != nil {
		t.Fatalf("tool_use start: %v", err)
	}
	// Fragments deliberately split the JSON to prove accumulation.
	for _, frag := range []string{`{"pa`, `th":"/tmp/a.txt",`, `"max_chars":100}`} {
		frame := `{"type":"content_block_delta","index":0,"delta":{"type":"input_json_delta","partial_json":` + strconv.Quote(frag) + `}}`
		deltas, _, err := acc.Feed("content_block_delta", []byte(frame))
		if err != nil || len(deltas) != 0 {
			t.Fatalf("input_json_delta %q: deltas=%d err=%v", frag, len(deltas), err)
		}
	}

	deltas, done, err := acc.Feed("content_block_stop", []byte(`{"type":"content_block_stop","index":0}`))
	if err != nil || done {
		t.Fatalf("tool stop: done=%v err=%v", done, err)
	}
	if len(deltas) != 1 {
		t.Fatalf("expected 1 delta with the tool call, got %d", len(deltas))
	}
	calls := deltas[0].Message.ToolCalls
	if len(calls) != 1 {
		t.Fatalf("ToolCalls = %d, want 1", len(calls))
	}
	call := calls[0]
	if call.ID != "toolu_1" || call.Function.Name != "read_file" {
		t.Errorf("call = %q/%q, want toolu_1/read_file", call.ID, call.Function.Name)
	}
	args := call.Function.Arguments.ToMap()
	if args["path"] != "/tmp/a.txt" || args["max_chars"] != float64(100) {
		t.Errorf("args = %#v", args)
	}
}

// TestFeedThinkingDelta routes thinking_delta into Message.Thinking.
func TestFeedThinkingDelta(t *testing.T) {
	acc := newAnthropicSSEAccumulator()
	_, _, _ = acc.Feed("content_block_start", []byte(`{"type":"content_block_start","index":0,"content_block":{"type":"thinking","thinking":""}}`))
	frame := `{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"hmm"}}`
	deltas, _, err := acc.Feed("content_block_delta", []byte(frame))
	if err != nil {
		t.Fatalf("thinking delta: %v", err)
	}
	if len(deltas) != 1 || deltas[0].Message.Thinking != "hmm" {
		t.Fatalf("deltas = %#v", deltas)
	}
}

// TestFeedStreamError surfaces the upstream error from the event payload.
func TestFeedStreamError(t *testing.T) {
	acc := newAnthropicSSEAccumulator()
	_, _, err := acc.Feed("error", []byte(`{"type":"error","error":{"type":"overloaded_error","message":"try later"}}`))
	if err == nil {
		t.Fatal("expected error from stream error event")
	}
	if !strings.Contains(err.Error(), "try later") {
		t.Errorf("error = %v, want upstream message embedded", err)
	}
}
