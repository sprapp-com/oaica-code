package agent

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/launch"
)

// defaultAgentMaxTokens is the fallback max_tokens when the launch inventory
// has no MaxOutputTokens for the resolved model (spec floor).
const defaultAgentMaxTokens = 4096

// thinkingBudget is the Anthropic thinking budget for enabled thinking. The
// engine passes no budget level through api.ChatRequest, so one fixed budget
// is used for every enabled level.
const thinkingBudget = 20000

// buildMessagesRequest converts the engine's api.ChatRequest into an
// Anthropic MessagesRequest. It is the reverse of anthropic.convertMessage:
// tool_use blocks become api.ToolCalls and consecutive role:"tool" messages
// are grouped into a single user message of tool_result blocks (Anthropic
// requires tool results to live in a user-role turn following the assistant's
// tool_use blocks).
//
// Assistant thinking is dropped on the way out: api.Message carries no
// thinking signature, and Anthropic rejects assistant thinking blocks without
// one. The model does not need the echo; the final text content is sent.
func buildMessagesRequest(req *api.ChatRequest, meta launch.AgentModelMeta) (*anthropic.MessagesRequest, error) {
	out := &anthropic.MessagesRequest{
		Model:     req.Model,
		MaxTokens: meta.MaxOutputTokens,
		Stream:    true,
	}
	if out.MaxTokens <= 0 {
		out.MaxTokens = defaultAgentMaxTokens
	}

	var system []string
	var messages []anthropic.MessageParam
	var pendingToolResults []anthropic.ContentBlock
	flushToolResults := func() {
		if len(pendingToolResults) == 0 {
			return
		}
		messages = append(messages, anthropic.MessageParam{Role: "user", Content: pendingToolResults})
		pendingToolResults = nil
	}

	for _, msg := range req.Messages {
		switch msg.Role {
		case "system":
			system = append(system, msg.Content)
		case "tool":
			pendingToolResults = append(pendingToolResults, anthropic.ContentBlock{
				Type:      "tool_result",
				ToolUseID: msg.ToolCallID,
				Content:   msg.Content,
			})
		default:
			flushToolResults()
			param := anthropic.MessageParam{Role: msg.Role}
			if msg.Content != "" {
				param.Content = append(param.Content, anthropic.ContentBlock{Type: "text", Text: &msg.Content})
			}
			for _, img := range msg.Images {
				data := []byte(img)
				param.Content = append(param.Content, anthropic.ContentBlock{
					Type: "image",
					Source: &anthropic.ImageSource{
						Type:      "base64",
						MediaType: imageMediaType(data),
						Data:      base64.StdEncoding.EncodeToString(data),
					},
				})
			}
			for _, tc := range msg.ToolCalls {
				param.Content = append(param.Content, anthropic.ContentBlock{
					Type:  "tool_use",
					ID:    tc.ID,
					Name:  tc.Function.Name,
					Input: tc.Function.Arguments,
				})
			}
			if len(param.Content) > 0 {
				messages = append(messages, param)
			}
		}
	}
	flushToolResults()

	if len(system) > 0 {
		out.System = strings.Join(system, "\n\n")
	}
	out.Messages = messages

	if len(req.Tools) > 0 {
		out.Tools = make([]anthropic.Tool, 0, len(req.Tools))
		for _, t := range req.Tools {
			params, err := json.Marshal(t.Function.Parameters)
			if err != nil {
				return nil, fmt.Errorf("marshal tool %q parameters: %w", t.Function.Name, err)
			}
			out.Tools = append(out.Tools, anthropic.Tool{
				Type:        "custom",
				Name:        t.Function.Name,
				Description: t.Function.Description,
				InputSchema: params,
			})
		}
	}

	if thinkEnabled(req.Think) {
		out.Thinking = &anthropic.ThinkingConfig{Type: "enabled", BudgetTokens: thinkingBudget}
	}

	return out, nil
}

// thinkEnabled reports whether a ThinkValue requests thinking (bool true or a
// known level string).
func thinkEnabled(t *api.ThinkValue) bool {
	if t == nil || t.Value == nil {
		return false
	}
	switch v := t.Value.(type) {
	case bool:
		return v
	case string:
		return v == "high" || v == "medium" || v == "low" || v == "max"
	default:
		return false
	}
}

// imageMediaType sniffs image bytes to fill Anthropic's required media_type.
func imageMediaType(b []byte) string {
	switch {
	case len(b) >= 12 && bytes.Equal(b[:4], []byte("RIFF")) && bytes.Equal(b[8:12], []byte("WEBP")):
		return "image/webp"
	case len(b) >= 4 && bytes.Equal(b[:4], []byte{0x89, 'P', 'N', 'G'}):
		return "image/png"
	case len(b) >= 3 && b[0] == 0xFF && b[1] == 0xD8 && b[2] == 0xFF:
		return "image/jpeg"
	case len(b) >= 3 && bytes.Equal(b[:3], []byte("GIF")):
		return "image/gif"
	default:
		return "image/png"
	}
}
