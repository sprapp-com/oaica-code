package anthropic

// anthropic_integrity_test.go — two shapes the Anthropic wire contract fixes
// and this translation used to break (2026-09-26 audit, fourth round):
//
//   - an assistant turn whose content was several text blocks was flattened
//     into escaped JSON, so a model replaying its own prior turn saw block
//     metadata instead of the sentences;
//   - a reply with no content serialized "content": null, and a stop_reason
//     of tool_use could be reported for a turn that carried no tool_use block
//     at all — an agent reading that waits for a call that is not there.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// F8: several text blocks in one assistant message are prose, joined.
func TestAssistantTextBlocksBecomeProse(t *testing.T) {
	raw := `{"model":"m","max_tokens":10,"messages":[
		{"role":"assistant","content":[{"type":"text","text":"I will read the file."},{"type":"text","text":"Then I will run the tests."}]},
		{"role":"user","content":"go"}]}`
	var req MessagesRequest
	if err := json.Unmarshal([]byte(raw), &req); err != nil {
		t.Fatal(err)
	}
	cr, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := cr.Messages[0].Content
	if got != "I will read the file.\n\nThen I will run the tests." {
		t.Errorf("assistant content = %q — multiple text blocks must reach the model as prose, not as escaped block JSON", got)
	}
	if strings.Contains(got, `{\"`) || strings.Contains(got, `"type"`) {
		t.Errorf("the block metadata survived into the prompt: %q", got)
	}
}

// F7b: a contentless reply still serializes content as an array.
func TestContentlessReplyHasAnEmptyContentArray(t *testing.T) {
	b, err := json.Marshal(ToMessagesResponse("msg_1", api.ChatResponse{Model: "m", Done: true, DoneReason: "stop"}))
	if err != nil {
		t.Fatal(err)
	}
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		t.Fatal(err)
	}
	if _, ok := m["content"].([]any); !ok {
		t.Errorf("content = %#v in %s — Anthropic clients index content as an array and a null makes them dereference nothing", m["content"], b)
	}
}

// F7c: stop_reason tool_use requires a tool_use block.
func TestToolUseStopReasonRequiresAToolCall(t *testing.T) {
	for _, reason := range []string{"tool_calls", "tool_use", "function_call"} {
		if got := mapStopReason(reason, false); got != "end_turn" {
			t.Errorf("mapStopReason(%q, hasToolCalls=false) = %q, want end_turn — the upstream attached no tool call, so reporting tool_use promises the client a content block that is not in the message", reason, got)
		}
		if got := mapStopReason(reason, true); got != "tool_use" {
			t.Errorf("mapStopReason(%q, hasToolCalls=true) = %q, want tool_use", reason, got)
		}
	}
	// A reply that really did call a tool is still tool_use, whatever the
	// upstream's own reason string was.
	if got := mapStopReason("stop", true); got != "tool_use" {
		t.Errorf("mapStopReason(\"stop\", hasToolCalls=true) = %q, want tool_use", got)
	}
	if got := mapStopReason("length", false); got != "max_tokens" {
		t.Errorf("mapStopReason(\"length\", false) = %q, want max_tokens", got)
	}
}
