package agent

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

func thinkValue(v any) *api.ThinkValue { return &api.ThinkValue{Value: v} }

// TestBuildMessagesRequestSystemExtraction: system messages move into the
// System field and never appear as a message param.
func TestBuildMessagesRequestSystemExtraction(t *testing.T) {
	req := &api.ChatRequest{
		Model: "deepseek-chat",
		Messages: []api.Message{
			{Role: "system", Content: "You are a helpful agent."},
			{Role: "user", Content: "hi"},
		},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{MaxOutputTokens: 2048})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.System != "You are a helpful agent." {
		t.Errorf("System = %q", out.System)
	}
	if len(out.Messages) != 1 || out.Messages[0].Role != "user" {
		t.Fatalf("Messages = %#v", out.Messages)
	}
	if out.MaxTokens != 2048 {
		t.Errorf("MaxTokens = %d, want 2048 from meta", out.MaxTokens)
	}
	if !out.Stream {
		t.Error("Stream must be true")
	}
}

// TestBuildMessagesRequestToolResultGrouping: consecutive role:"tool" messages
// collapse into one user message of tool_result blocks.
func TestBuildMessagesRequestToolResultGrouping(t *testing.T) {
	req := &api.ChatRequest{
		Model: "m",
		Messages: []api.Message{
			{Role: "assistant", Content: "", ToolCalls: []api.ToolCall{{ID: "toolu_1", Function: api.ToolCallFunction{Name: "read_file"}}}},
			{Role: "tool", ToolCallID: "toolu_1", Content: "file contents"},
			{Role: "tool", ToolCallID: "toolu_2", Content: "second result"},
			{Role: "user", Content: "thanks"},
		},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if len(out.Messages) != 3 {
		t.Fatalf("Messages = %d, want [assistant, user(tool_result), user]", len(out.Messages))
	}
	tr := out.Messages[1]
	if tr.Role != "user" {
		t.Errorf("tool result message role = %q, want user", tr.Role)
	}
	if len(tr.Content) != 2 {
		t.Fatalf("tool_result blocks = %d, want 2", len(tr.Content))
	}
	if tr.Content[0].Type != "tool_result" || tr.Content[0].ToolUseID != "toolu_1" || tr.Content[0].Content != "file contents" {
		t.Errorf("block[0] = %#v", tr.Content[0])
	}
	if tr.Content[1].ToolUseID != "toolu_2" {
		t.Errorf("block[1] ToolUseID = %q", tr.Content[1].ToolUseID)
	}
}

// TestBuildMessagesRequestToolUseBlock: assistant tool calls become tool_use
// blocks carrying the ordered arguments.
func TestBuildMessagesRequestToolUseBlock(t *testing.T) {
	args := api.ToolCallFunctionArguments{}
	args.Set("path", "/tmp/a.txt")
	req := &api.ChatRequest{
		Model: "m",
		Messages: []api.Message{
			{Role: "assistant", ToolCalls: []api.ToolCall{{ID: "toolu_9", Function: api.ToolCallFunction{Name: "read_file", Arguments: args}}}},
		},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	block := out.Messages[0].Content[0]
	if block.Type != "tool_use" || block.ID != "toolu_9" || block.Name != "read_file" {
		t.Fatalf("block = %#v", block)
	}
	if got := block.Input.ToMap()["path"]; got != "/tmp/a.txt" {
		t.Errorf("input path = %v", got)
	}
}

// TestBuildMessagesRequestTools: api.ToolFunction parameters marshal into the
// Anthropic input_schema.
func TestBuildMessagesRequestTools(t *testing.T) {
	req := &api.ChatRequest{
		Model: "m",
		Tools: api.Tools{{
			Function: api.ToolFunction{
				Name:        "read_file",
				Description: "Read a file",
				Parameters: api.ToolFunctionParameters{
					Type:       "object",
					Required:   []string{"path"},
					Properties: &api.ToolPropertiesMap{},
				},
			},
		}},
	}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if len(out.Tools) != 1 {
		t.Fatalf("Tools = %d, want 1", len(out.Tools))
	}
	tt := out.Tools[0]
	if tt.Type != "custom" || tt.Name != "read_file" || tt.Description != "Read a file" {
		t.Errorf("tool = %#v", tt)
	}
	var schema map[string]any
	if err := json.Unmarshal(tt.InputSchema, &schema); err != nil {
		t.Fatalf("InputSchema is not valid JSON: %v (%s)", err, tt.InputSchema)
	}
	if schema["type"] != "object" {
		t.Errorf("InputSchema type = %v, want object", schema["type"])
	}
	if req, ok := schema["required"].([]any); !ok || len(req) != 1 || req[0] != "path" {
		t.Errorf("InputSchema required = %#v, want [path]", schema["required"])
	}
}

// TestBuildMessagesRequestThink: a truthy Think enables Anthropic thinking.
func TestBuildMessagesRequestThink(t *testing.T) {
	req := &api.ChatRequest{Model: "m", Think: thinkValue(true)}
	out, err := buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.Thinking == nil || out.Thinking.Type != "enabled" {
		t.Errorf("Thinking = %#v, want enabled", out.Thinking)
	}

	req.Think = thinkValue("high")
	out, err = buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.Thinking == nil {
		t.Error("string think level should also enable thinking")
	}

	req.Think = thinkValue(false)
	out, err = buildMessagesRequest(req, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.Thinking != nil {
		t.Errorf("Thinking = %#v, want nil when disabled", out.Thinking)
	}
}

// TestBuildMessagesRequestMaxTokensDefault: no meta → 4096 floor.
func TestBuildMessagesRequestMaxTokensDefault(t *testing.T) {
	out, err := buildMessagesRequest(&api.ChatRequest{Model: "m"}, launch.AgentModelMeta{})
	if err != nil {
		t.Fatalf("buildMessagesRequest: %v", err)
	}
	if out.MaxTokens != defaultAgentMaxTokens {
		t.Errorf("MaxTokens = %d, want %d", out.MaxTokens, defaultAgentMaxTokens)
	}
}
