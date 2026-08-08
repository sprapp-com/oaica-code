package agent

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
)

// anthropicSSEAccumulator turns a stream of inbound Anthropic Messages SSE
// events into api.ChatResponse deltas for the engine's chatRound callback.
//
// It owns the one piece of state the shim must carry across frames: content
// blocks accumulate by index, and a tool_use block is only complete when its
// content_block_stop arrives (input_json_delta fragments must be joined
// before the ToolCall can be emitted).
type anthropicSSEAccumulator struct {
	blocks map[int]*anthropicBlockAccum
	done   bool
}

type anthropicBlockAccum struct {
	index int
	kind  string // "text", "thinking", "tool_use"
	text  strings.Builder
	tool  *api.ToolCall
}

func newAnthropicSSEAccumulator() *anthropicSSEAccumulator {
	return &anthropicSSEAccumulator{blocks: make(map[int]*anthropicBlockAccum)}
}

// Feed processes one SSE frame (event type + raw JSON data) and returns the
// ChatResponse deltas to hand to the engine, plus done=true after
// message_stop. It never returns an empty delta — the engine's chatRound
// treats an empty message as a stream-end sentinel.
func (a *anthropicSSEAccumulator) Feed(eventType string, data []byte) (deltas []api.ChatResponse, done bool, err error) {
	switch eventType {
	case "message_start", "ping", "message_delta":
		return nil, false, nil
	case "content_block_start":
		var ev anthropic.ContentBlockStartEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse content_block_start: %w", err)
		}
		b := &anthropicBlockAccum{index: ev.Index}
		switch ev.ContentBlock.Type {
		case "tool_use":
			b.kind = "tool_use"
			b.tool = &api.ToolCall{
				ID: ev.ContentBlock.ID,
				Function: api.ToolCallFunction{
					Name:      ev.ContentBlock.Name,
					Arguments: api.ToolCallFunctionArguments{},
				},
			}
		default:
			b.kind = ev.ContentBlock.Type // "text", "thinking", or unknown
		}
		a.blocks[ev.Index] = b
		return nil, false, nil
	case "content_block_delta":
		var ev anthropic.ContentBlockDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse content_block_delta: %w", err)
		}
		b, ok := a.blocks[ev.Index]
		if !ok {
			return nil, false, nil // delta for a block we never saw; ignore
		}
		switch ev.Delta.Type {
		case "text_delta":
			b.text.WriteString(ev.Delta.Text)
			if ev.Delta.Text != "" {
				return []api.ChatResponse{{Message: api.Message{Content: ev.Delta.Text}}}, false, nil
			}
			return nil, false, nil
		case "thinking_delta":
			b.text.WriteString(ev.Delta.Thinking)
			if ev.Delta.Thinking != "" {
				return []api.ChatResponse{{Message: api.Message{Thinking: ev.Delta.Thinking}}}, false, nil
			}
			return nil, false, nil
		case "input_json_delta", "signature_delta":
			b.text.WriteString(ev.Delta.PartialJSON + ev.Delta.Signature)
			return nil, false, nil
		}
		return nil, false, nil
	case "content_block_stop":
		var ev anthropic.ContentBlockStopEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse content_block_stop: %w", err)
		}
		b, ok := a.blocks[ev.Index]
		if !ok {
			return nil, false, nil
		}
		if b.kind == "tool_use" && b.tool != nil {
			if s := strings.TrimSpace(b.text.String()); s != "" {
				var args map[string]any
				if err := json.Unmarshal([]byte(s), &args); err != nil {
					return nil, false, fmt.Errorf("parse accumulated tool_use input: %w", err)
				}
				for k, v := range args {
					b.tool.Function.Arguments.Set(k, v)
				}
			}
			return []api.ChatResponse{{Message: api.Message{ToolCalls: []api.ToolCall{*b.tool}}}}, false, nil
		}
		return nil, false, nil
	case "message_stop":
		if a.done {
			return nil, false, nil
		}
		a.done = true
		return []api.ChatResponse{{Done: true}}, true, nil
	case "error":
		var ev anthropic.StreamErrorEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse stream error: %w", err)
		}
		return nil, false, fmt.Errorf("upstream error: %s: %s", ev.Error.Type, ev.Error.Message)
	default:
		return nil, false, fmt.Errorf("unexpected SSE event type %q", eventType)
	}
}
