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
	// The token counts the upstream STATED for this turn, per field, delivered
	// on the message_stop delta — the only delta the engine stores as the
	// turn's last one (agent/session.go, *latest = response).
	//
	// Both frames that carry a usage are read, and the LATER statement wins,
	// because the two legs that serve this shim put the real numbers in
	// different frames: the metered gateway states zeros in message_start and
	// the prompt, the cache split and the output count in message_delta
	// (tools/gateway/messages.go, finishStream), while the local leg's
	// message_start carries an estimate (middleware/anthropic.go,
	// ensureStreamMessageStart) and its message_delta the observed usage. Only
	// a POSITIVE count is a statement — the gateway's own rule — so a frame
	// that is silent about a field, or states a zero because it has nothing to
	// say about it, does not overwrite what the other frame stated
	// (2026-09-28 audit, round 54: reading message_start alone kept 0 on the
	// gateway leg and an estimate elsewhere, so the "prompt_eval" trigger this
	// was meant to revive stayed dead, and cache_read_input_tokens was dropped
	// whenever the delta stated it).
	startInput, startCache, startOutput int
	input, cache, output               int
}

// usage is the api.Metrics the turn's stated counts add up to, in this
// product's convention: PromptEvalCount is the WHOLE prompt and
// PromptEvalCachedCount the part of it served from cache (anthropic.Usage's
// own reading — input_tokens excludes what cache_read_input_tokens counts, and
// a client's context arithmetic is their sum; anthropic.UsageFromMetrics
// converts back the same way). A field no frame stated stays unset, so a turn
// whose upstream said nothing about its prompt reports nothing rather than a
// zero.
func (a *anthropicSSEAccumulator) usage() api.Metrics {
	in, cacheTok, outTok := a.input, a.cache, a.output
	if in == 0 {
		in = a.startInput
	}
	if cacheTok == 0 {
		cacheTok = a.startCache
	}
	if outTok == 0 {
		outTok = a.startOutput
	}
	metrics := api.Metrics{EvalCount: outTok}
	if in > 0 || cacheTok > 0 {
		metrics.PromptEvalCount = in + cacheTok
	}
	if cacheTok > 0 {
		cached := cacheTok
		metrics.PromptEvalCachedCount = &cached
	}
	return metrics
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
	case "message_start":
		// A seed, not the count: see the accumulator's field comment. Kept so a
		// leg that states its usage only here is still read.
		var ev anthropic.MessageStartEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse message_start: %w", err)
		}
		a.startInput = ev.Message.Usage.InputTokens
		if cached := ev.Message.Usage.CacheReadInputTokens; cached != nil {
			a.startCache = *cached
		}
		a.startOutput = ev.Message.Usage.OutputTokens
		return nil, false, nil
	case "ping":
		return nil, false, nil
	case "message_delta":
		// The frame the turn is CLOSED with, and the one both legs put the real
		// numbers in. Only positive counts are statements, so a leg that is
		// silent about a field here does not erase what message_start stated.
		var ev anthropic.MessageDeltaEvent
		if err := json.Unmarshal(data, &ev); err != nil {
			return nil, false, fmt.Errorf("parse message_delta: %w", err)
		}
		if ev.Usage.InputTokens > 0 {
			a.input = ev.Usage.InputTokens
		}
		if cached := ev.Usage.CacheReadInputTokens; cached != nil && *cached > 0 {
			a.cache = *cached
		}
		if ev.Usage.OutputTokens > 0 {
			a.output = ev.Usage.OutputTokens
		}
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
				// Decoded into an order-preserving container, in the order the
				// upstream wrote: a plain map here kept none of it, and Go
				// randomizes map iteration, so one upstream stream was
				// re-emitted as three different argument orders across runs.
				// This agent loop writes the call straight back into the next
				// request body, so the next turn's prompt bytes differed run to
				// run for the same conversation and every prefix cache keyed on
				// them was recomputed (2026-09-27 audit, round 52).
				args := api.NewToolCallFunctionArguments()
				if err := json.Unmarshal([]byte(s), &args); err != nil {
					return nil, false, fmt.Errorf("parse accumulated tool_use input: %w", err)
				}
				for k, v := range args.All() {
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
		// The stated usage rides the terminal delta: this is the one the engine
		// keeps for the turn (its Message is empty, so the engine's delta
		// handler stores it as *latest), and a turn whose count is only stated
		// is a turn whose meter and compaction read a guess instead.
		return []api.ChatResponse{{Done: true, Metrics: a.usage()}}, true, nil
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
