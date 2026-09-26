package anthropic

// tool_wire_fidelity_integrity_test.go — five defects on the Anthropic↔Ollama
// translation surface (2026-09-26/27 audit, sixteenth round).
//
//	1. ToMessagesResponse passed an id-less upstream tool call straight through,
//	   and ContentBlock.ID is omitempty — so the NON-streaming path emitted a
//	   tool_use block with no "id" key at all, one the client cannot name back in
//	   a tool_result. The streaming converter and the proxy's non-streaming
//	   parser both synthesize ToolCallIDFor; this path did not, so one input
//	   produced two different blocks depending on `stream`.
//	2. tool_choice was decoded and traced but never translated: {"type":"none"}
//	   still sent the tools, so a model could answer with a call the caller had
//	   forbidden (an agent that reads tool_use then runs a side-effecting tool).
//	3. tool_result text segments were concatenated with no separator.
//	4. a document with a text source was appended after a text block with only a
//	   TRAILING newline, so the two sentences glued anyway.
//	5. several thinking blocks in one assistant message overwrote instead of
//	   joining, dropping every reasoning run but the last.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// (1) Non-streaming and streaming must agree on the id of an id-less call.
func TestNonStreamingToolUseGetsAnIdLikeTheStreamingPath(t *testing.T) {
	resp := api.ChatResponse{
		Model: "m",
		Message: api.Message{
			Role: "assistant",
			ToolCalls: []api.ToolCall{{
				ID: "", // the upstream sent none
				Function: api.ToolCallFunction{
					Name:      "get_weather",
					Arguments: api.NewToolCallFunctionArguments(),
				},
			}},
		},
		Done:       true,
		DoneReason: "stop",
	}

	raw, err := json.Marshal(ToMessagesResponse("msg_1", resp))
	if err != nil {
		t.Fatal(err)
	}

	var decoded struct {
		Content []struct {
			Type  string          `json:"type"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}
	if len(decoded.Content) != 1 || decoded.Content[0].Type != "tool_use" {
		t.Fatalf("premise: expected one tool_use block, got %s", raw)
	}
	if decoded.Content[0].ID == "" {
		t.Fatalf("the non-streaming path emitted a tool_use block with no id at all — anthropic's schema requires one, and the client's tool_result then carries tool_use_id \"\":\n%s", raw)
	}

	// And it must be the SAME id the streaming converter derives, or one
	// upstream turn names a different call depending on `stream`.
	want := ToolCallIDFor("get_weather", "{}")
	if decoded.Content[0].ID != want {
		t.Errorf("non-streaming id = %q, streaming path would use %q", decoded.Content[0].ID, want)
	}
	var sawStreamed string
	conv := NewStreamConverter("msg_1", "m", 0)
	for _, ev := range conv.Process(resp) {
		if start, ok := ev.Data.(ContentBlockStartEvent); ok && start.ContentBlock.Type == "tool_use" {
			sawStreamed = start.ContentBlock.ID
		}
	}
	if sawStreamed != "" && sawStreamed != decoded.Content[0].ID {
		t.Errorf("the two paths disagree for one input: stream=%q, non-stream=%q", sawStreamed, decoded.Content[0].ID)
	}
}

// (2) tool_choice "none" must reach the model as "no tools".
func TestToolChoiceNoneSendsNoTools(t *testing.T) {
	req := MessagesRequest{
		Model:      "m",
		MaxTokens:  64,
		Tools:      []Tool{{Name: "get_weather", InputSchema: json.RawMessage(`{"type":"object"}`)}},
		ToolChoice: &ToolChoice{Type: "none"},
		Messages:   []MessageParam{{Role: "user", Content: mustContent(t, "hi")}},
	}

	converted, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(converted.Tools) != 0 {
		t.Errorf("tool_choice \"none\" still sent %d tool(s) to the model, so it can answer with a call the caller forbade", len(converted.Tools))
	}

	// A caller that said nothing (or "auto") keeps them — the fix must not
	// drop tools it was not asked to drop.
	for _, tc := range []*ToolChoice{nil, {Type: "auto"}, {Type: "any"}, {Type: "tool", Name: "get_weather"}} {
		req.ToolChoice = tc
		converted, err := FromMessagesRequest(req)
		if err != nil {
			t.Fatal(err)
		}
		if len(converted.Tools) != 1 {
			t.Errorf("tool_choice %+v dropped the tools (%d left); only \"none\" means that", tc, len(converted.Tools))
		}
	}
}

// (3)+(4) text segments are joined, never glued.
func TestToolResultAndDocumentTextSegmentsAreJoined(t *testing.T) {
	// A tool_result whose content is two text blocks.
	req := MessagesRequest{
		Model:     "m",
		MaxTokens: 64,
		Messages: []MessageParam{
			{Role: "assistant", Content: []ContentBlock{{Type: "tool_use", ID: "call_1", Name: "read", Input: api.NewToolCallFunctionArguments()}}},
			{Role: "user", Content: []ContentBlock{{Type: "tool_result", ToolUseID: "call_1", Content: []any{
				map[string]any{"type": "text", "text": "first line"},
				map[string]any{"type": "text", "text": "second line"},
			}}}},
		},
	}
	converted, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	var got string
	for _, m := range converted.Messages {
		if m.Role == "tool" {
			got = m.Content
		}
	}
	if got == "first linesecond line" {
		t.Errorf("the two tool_result text blocks were concatenated with no separator")
	}
	if !strings.Contains(got, "first line") || !strings.Contains(got, "second line") {
		t.Fatalf("premise: both segments must survive, got %q", got)
	}

	// A document with a text source right after a text block.
	req2 := MessagesRequest{
		Model:     "m",
		MaxTokens: 64,
		Messages: []MessageParam{{Role: "user", Content: []ContentBlock{
			{Type: "text", Text: ptr("read the file.")},
			{Type: "document", Source: &ImageSource{Type: "text", Data: "Now run the tests."}},
		}}},
	}
	converted2, err := FromMessagesRequest(req2)
	if err != nil {
		t.Fatal(err)
	}
	if got := converted2.Messages[0].Content; got == "read the file.Now run the tests.\n" || got == "read the file.Now run the tests." {
		t.Errorf("the document's text ran into the text block before it with no separator: %q", got)
	}
}

// (5) every reasoning run survives the round trip.
func TestInterleavedThinkingBlocksAreJoinedNotOverwritten(t *testing.T) {
	req := MessagesRequest{
		Model:     "m",
		MaxTokens: 64,
		Messages: []MessageParam{{Role: "assistant", Content: []ContentBlock{
			{Type: "thinking", Thinking: ptr("THINK-A")},
			{Type: "text", Text: ptr("the answer")},
			{Type: "thinking", Thinking: ptr("THINK-B")},
		}}},
	}
	converted, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	got := converted.Messages[0].Thinking
	if !strings.Contains(got, "THINK-A") {
		t.Errorf("the earlier reasoning run was dropped: thinking = %q", got)
	}
	if !strings.Contains(got, "THINK-B") {
		t.Errorf("the later reasoning run was dropped: thinking = %q", got)
	}
}

// (5b) 413 is a client-side size violation, not a server fault.
func TestError413IsRequestTooLarge(t *testing.T) {
	if got := NewError(413, "too large").Error.Type; got != "request_too_large" {
		t.Errorf("HTTP 413 maps to %q; anthropic's enum has request_too_large for exactly this, and api_error tells the client the server broke", got)
	}
}

func mustContent(t *testing.T, text string) []ContentBlock {
	t.Helper()
	return []ContentBlock{{Type: "text", Text: ptr(text)}}
}
