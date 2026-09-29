package anthropic

// round106_leg1_tool_result_name_test.go — F106-L1-3's pin for the Anthropic
// surface: a tool_result states only the id of the call it answers, and the chat
// converter states the call's name on the same history.

import (
	"encoding/json"
	"testing"
)

func TestMine106AnthropicNamesItsToolResults(t *testing.T) {
	var req MessagesRequest
	body := `{"model":"m","max_tokens":10,"messages":[` +
		`{"role":"user","content":"go"},` +
		`{"role":"assistant","content":[{"type":"tool_use","id":"toolu_1","name":"Bash","input":{}}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":"a.txt"}]}]}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	last := out.Messages[len(out.Messages)-1]
	if last.Role != "tool" || last.ToolName != "Bash" {
		t.Errorf("the result reads role=%s name=%q, want tool/Bash — chat names a result after the call it answers (2026-09-29 audit, round 106, F106-L1-3)", last.Role, last.ToolName)
	}
}

// TestMine107AnthropicOutputConfigFormatIsTheRunnersFormat is F107-L1-1's pin: a
// json_schema output_config.format reaches the runner as ChatRequest.Format, as
// chat's response_format and Responses' text.format do.
func TestMine107AnthropicOutputConfigFormatIsTheRunnersFormat(t *testing.T) {
	var req MessagesRequest
	body := `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":"hi"}],` +
		`"output_config":{"format":{"type":"json_schema","schema":{"type":"object","properties":{"a":{"type":"string"}}}}}}`
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatal(err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatal(err)
	}
	if string(out.Format) != `{"type":"object","properties":{"a":{"type":"string"}}}` {
		t.Errorf("format = %q, want the schema — output_config.format was discarded and the turn answered free text (2026-09-29 audit, round 107, F107-L1-1)", out.Format)
	}
}
