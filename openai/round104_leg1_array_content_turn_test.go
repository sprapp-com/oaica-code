package openai

// round104_leg1_array_content_turn_test.go — leg 1, round 104 (2026-09-29
// audit), F104-L1-1 and F104-L1-2.
//
// FromChatRequest read a message whose content is an ARRAY of parts through a
// branch that hung a turn's identity on `tool_calls`: the id and name a tool
// result answers, and the calls an assistant turn made, were attached to "the
// last message emitted" only when the turn carried calls, and to the last message
// of the WHOLE conversation when the array emitted nothing. One turn, two
// spellings of its content, two conversations:
//
//	tool  content "R"            → role=tool tool_call_id=call_1
//	tool  content [{text:"R"}]   → role=tool tool_call_id=""     (F104-L1-1)
//	asst  content null + call    → an assistant message with the call
//	asst  content []   + call    → the USER message gains the call (F104-L1-2)

import (
	"testing"

	"github.com/ollama/ollama/api"
)

func r104Convert(t *testing.T, msgs []Message) []api.Message {
	t.Helper()
	req, err := FromChatRequest(ChatCompletionRequest{Model: "m", Messages: msgs})
	if err != nil {
		t.Fatalf("FromChatRequest: %v", err)
	}
	return req.Messages
}

func r104Call() ToolCall {
	return ToolCall{ID: "call_1", Type: "function", Function: struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	}{Name: "f", Arguments: `{}`}}
}

// TestMine104AToolResultKeepsItsCallIdInEitherSpelling is F104-L1-1's pin.
func TestMine104AToolResultKeepsItsCallIdInEitherSpelling(t *testing.T) {
	asst := Message{Role: "assistant", Content: "", ToolCalls: []ToolCall{r104Call()}}
	str := r104Convert(t, []Message{{Role: "user", Content: "hi"}, asst, {Role: "tool", Content: "R", ToolCallID: "call_1"}})
	arr := r104Convert(t, []Message{{Role: "user", Content: "hi"}, asst,
		{Role: "tool", Content: []any{map[string]any{"type": "text", "text": "R"}}, ToolCallID: "call_1"}})
	s, a := str[len(str)-1], arr[len(arr)-1]
	if s.ToolCallID != "call_1" || s.ToolName != "f" {
		t.Fatalf("premise: the string spelling reads id %q name %q, want call_1/f", s.ToolCallID, s.ToolName)
	}
	if a.ToolCallID != s.ToolCallID || a.ToolName != s.ToolName || a.Content != s.Content {
		t.Errorf("the array spelling reads role=%s content=%q id=%q name=%q, want what the string spelling reads (%q %q %q) — the id a tool result answers is the turn's, not its calls' (2026-09-29 audit, round 104, F104-L1-1)",
			a.Role, a.Content, a.ToolCallID, a.ToolName, s.Content, s.ToolCallID, s.ToolName)
	}
}

// TestMine104AnAssistantTurnWithAnEmptyArrayIsItsOwnMessage is F104-L1-2's pin.
func TestMine104AnAssistantTurnWithAnEmptyArrayIsItsOwnMessage(t *testing.T) {
	got := r104Convert(t, []Message{{Role: "user", Content: "hi"},
		{Role: "assistant", Content: []any{}, ToolCalls: []ToolCall{r104Call()}}})
	if len(got) != 2 || got[0].Role != "user" || len(got[0].ToolCalls) != 0 {
		t.Fatalf("the user turn is %+v — an assistant turn whose array held no part handed its calls to the message before it (2026-09-29 audit, round 104, F104-L1-2)", got)
	}
	if got[1].Role != "assistant" || len(got[1].ToolCalls) != 1 {
		t.Errorf("the assistant turn is %+v, want its own message carrying the call", got[1])
	}
}
