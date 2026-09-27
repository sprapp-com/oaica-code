package middleware

// round53_websearch_tool_frame_integrity_test.go — round 53, L1: the
// web-search writer's terminal tool_use block.
//
// writeStreamContentBlocks wrote that block WHOLE inside content_block_start —
// id, name and the arguments in the start event's input — and closed it with
// content_block_stop and no delta between. A client that builds a call the way
// every SDK does (an empty input at the start, the arguments from
// input_json_delta) accumulated an empty input, so a web-search turn handed the
// engine a tool call with its arguments gone, while the same block served
// through this leg's passthrough path carried them. The converter one branch
// over in middleware/anthropic.go emits the delta framing for the very same
// block (anthropic.StreamConverter), and the gateway leg relays it as frames
// too, so the framing below is the one the other two paths already used.

import (
	"encoding/json"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

func TestWebSearchToolUseArgumentsArriveAsADelta(t *testing.T) {
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	ginCtx, _ := gin.CreateTestContext(rec)

	writer := &WebSearchAnthropicWriter{
		BaseWriter: BaseWriter{ResponseWriter: ginCtx.Writer},
		stream:     true,
	}

	input := api.NewToolCallFunctionArguments()
	input.Set("path", "/etc/hosts")
	block := anthropic.ContentBlock{Type: "tool_use", ID: "toolu_9", Name: "read_file", Input: input}
	if err := writer.writeStreamContentBlocks([]anthropic.ContentBlock{block}); err != nil {
		t.Fatalf("writeStreamContentBlocks: %v", err)
	}

	events := parseSSEEvents(t, rec.Body.String())
	want := []string{"content_block_start", "content_block_delta", "content_block_stop"}
	if len(events) != len(want) {
		t.Fatalf("the turn streamed %d events (%v), want %v — a tool_use block that carries its arguments in the start event and no delta between is a block whose input the client's accumulator leaves empty",
			len(events), eventNames(events), want)
	}
	for i, name := range want {
		if events[i].event != name {
			t.Errorf("event[%d] is %q, want %q", i, events[i].event, name)
		}
	}

	var start anthropic.ContentBlockStartEvent
	if err := json.Unmarshal([]byte(events[0].data), &start); err != nil {
		t.Fatalf("parse content_block_start: %v", err)
	}
	if start.ContentBlock.Type != "tool_use" || start.ContentBlock.ID != "toolu_9" || start.ContentBlock.Name != "read_file" {
		t.Errorf("the start event announced %+v, want the call's id and name", start.ContentBlock)
	}
	if start.ContentBlock.Input.Len() != 0 {
		t.Errorf("the start event carried %v as the call's input; the input is accumulated from the deltas that follow, and a start event that already holds it is bytes a delta-framed client reads twice or not at all",
			start.ContentBlock.Input.ToMap())
	}

	// The accumulator the agent shim uses: the start's input plus every
	// input_json_delta of that index (cmd/agent/sse.go), which is also how the
	// Anthropic SDKs build a call.
	var delta anthropic.ContentBlockDeltaEvent
	if err := json.Unmarshal([]byte(events[1].data), &delta); err != nil {
		t.Fatalf("parse content_block_delta: %v", err)
	}
	if delta.Delta.Type != "input_json_delta" {
		t.Fatalf("the delta is %q, want input_json_delta", delta.Delta.Type)
	}
	accumulated := api.NewToolCallFunctionArguments()
	for k, v := range start.ContentBlock.Input.All() {
		accumulated.Set(k, v)
	}
	if err := json.Unmarshal([]byte(delta.Delta.PartialJSON), &accumulated); err != nil {
		t.Fatalf("the delta's partial_json %q does not decode as the call's input: %v", delta.Delta.PartialJSON, err)
	}
	if got, _ := accumulated.Get("path"); got != "/etc/hosts" {
		t.Errorf("the client accumulated %v as the call's input, want path=/etc/hosts\nthe engine runs a tool call with the input its accumulator built, so a call that arrives without one is a call made with nothing",
			accumulated.ToMap())
	}
}
