package anthropic

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/auth"
	internalcloud "github.com/ollama/ollama/internal/cloud"
	"github.com/ollama/ollama/logutil"
)

// Error types matching Anthropic API
type Error struct {
	Type    string `json:"type"`
	Message string `json:"message"`
}

type ErrorResponse struct {
	Type      string `json:"type"` // always "error"
	Error     Error  `json:"error"`
	RequestID string `json:"request_id,omitempty"`
}

// NewError creates a new ErrorResponse with the appropriate error type based on HTTP status code
func NewError(code int, message string) ErrorResponse {
	var etype string
	switch code {
	case http.StatusBadRequest:
		etype = "invalid_request_error"
	case http.StatusUnauthorized:
		etype = "authentication_error"
	case http.StatusForbidden:
		etype = "permission_error"
	case http.StatusNotFound:
		etype = "not_found_error"
	case http.StatusRequestEntityTooLarge:
		// Anthropic's enum has a name for exactly this: the body was too big.
		// api_error told the client the server broke, so a client-side size
		// violation read as a server fault and was retried as one
		// (2026-09-26 audit, sixteenth round).
		etype = "request_too_large"
	case http.StatusTooManyRequests:
		etype = "rate_limit_error"
	case http.StatusServiceUnavailable, 529:
		etype = "overloaded_error"
	default:
		etype = "api_error"
	}

	return ErrorResponse{
		Type:      "error",
		Error:     Error{Type: etype, Message: message},
		RequestID: generateID("req"),
	}
}

// Request types

// MessagesRequest represents an Anthropic Messages API request
type MessagesRequest struct {
	Model         string          `json:"model"`
	MaxTokens     int             `json:"max_tokens"`
	Messages      []MessageParam  `json:"messages"`
	System        any             `json:"system,omitempty"` // string or []map[string]any (JSON-decoded ContentBlock)
	Stream        bool            `json:"stream,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	TopK          *int            `json:"top_k,omitempty"`
	StopSequences []string        `json:"stop_sequences,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Thinking      *ThinkingConfig `json:"thinking,omitempty"`
	Metadata      *Metadata       `json:"metadata,omitempty"`
	OutputConfig  *OutputConfig   `json:"output_config,omitempty"`
}

type OutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// MessageParam represents a message in the request
type MessageParam struct {
	Role    string         `json:"role"`    // "user" or "assistant"
	Content []ContentBlock `json:"content"` // always []ContentBlock; plain strings are normalized on unmarshal
}

func (m *MessageParam) UnmarshalJSON(data []byte) error {
	var raw struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}
	m.Role = raw.Role

	var s string
	if err := json.Unmarshal(raw.Content, &s); err == nil {
		m.Content = []ContentBlock{{Type: "text", Text: &s}}
		return nil
	}

	return json.Unmarshal(raw.Content, &m.Content)
}

// ContentBlock represents a content block in a message.
// Text and Thinking use pointers so they serialize as the field being present (even if empty)
// only when set, which is required for SDK streaming accumulation.
type ContentBlock struct {
	Type string `json:"type"` // text, image, tool_use, tool_result, thinking, server_tool_use, web_search_tool_result

	// For text blocks - pointer so field only appears when set (SDK requires it for accumulation)
	Text *string `json:"text,omitempty"`

	// For text blocks with citations
	Citations []Citation `json:"citations,omitempty"`

	// For image blocks
	Source *ImageSource `json:"source,omitempty"`

	// For tool_use and server_tool_use blocks
	ID    string                        `json:"id,omitempty"`
	Name  string                        `json:"name,omitempty"`
	Input api.ToolCallFunctionArguments `json:"input,omitzero"`

	// For tool_result and web_search_tool_result blocks
	ToolUseID string `json:"tool_use_id,omitempty"`
	Content   any    `json:"content,omitempty"` // string, []ContentBlock, []WebSearchResult, or WebSearchToolResultError
	IsError   bool   `json:"is_error,omitempty"`

	// Title labels a search_result block, whose `source` (an ImageSource's Ref,
	// above) names where the passages came from.
	Title string `json:"title,omitempty"`

	// For thinking blocks - pointer so field only appears when set (SDK requires it for accumulation)
	Thinking  *string `json:"thinking,omitempty"`
	Signature string  `json:"signature,omitempty"`
}

// Citation represents a citation in a text block
type Citation struct {
	Type           string `json:"type"` // "web_search_result_location"
	URL            string `json:"url"`
	Title          string `json:"title"`
	EncryptedIndex string `json:"encrypted_index,omitempty"`
	CitedText      string `json:"cited_text,omitempty"`
}

// WebSearchResult represents a single web search result
type WebSearchResult struct {
	Type             string `json:"type"` // "web_search_result"
	URL              string `json:"url"`
	Title            string `json:"title"`
	EncryptedContent string `json:"encrypted_content,omitempty"`
	PageAge          string `json:"page_age,omitempty"`
}

// WebSearchToolResultError represents an error from web search
type WebSearchToolResultError struct {
	Type      string `json:"type"` // "web_search_tool_result_error"
	ErrorCode string `json:"error_code"`
}

// ImageSource represents the source of an image
type ImageSource struct {
	Type      string `json:"type"` // "base64"
	MediaType string `json:"media_type,omitempty"`
	Data      string `json:"data,omitempty"`
	URL       string `json:"url,omitempty"`

	// Ref is the `source` of a block that states it as a bare string rather
	// than an object — a `search_result` names the origin of its passages that
	// way. Kept apart from the object fields above so the object form still
	// marshals exactly as it did.
	Ref string `json:"-"`
}

// UnmarshalJSON accepts both spellings of a block's `source`: the object an
// image or a document carries ({"type":"base64","media_type":…,"data":…}) and
// the bare URL string a `search_result` states. Without this the string form
// failed to unmarshal, and the failure is not local to the block: the whole
// request was rejected with a decoding error, so a session whose history
// contains one server-side web search turn could not be sent at all
// (2026-09-27 audit, round 36, A-F1).
func (s *ImageSource) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		var ref string
		if err := json.Unmarshal(b, &ref); err != nil {
			return err
		}
		*s = ImageSource{Ref: ref}
		return nil
	}
	type alias ImageSource
	var a alias
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*s = ImageSource(a)
	return nil
}

// MarshalJSON writes the form the source arrived in: a bare string when it is a
// reference and carries no object fields, the object otherwise.
func (s ImageSource) MarshalJSON() ([]byte, error) {
	if s.Ref != "" && s.Type == "" && s.Data == "" && s.URL == "" && s.MediaType == "" {
		return json.Marshal(s.Ref)
	}
	type alias ImageSource
	return json.Marshal(alias(s))
}

// Tool represents a tool definition
type Tool struct {
	Type        string          `json:"type,omitempty"` // "custom" for user-defined tools, or "web_search_20250305" for web search
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema,omitempty"`

	// Web search specific fields
	MaxUses int `json:"max_uses,omitempty"`
}

// ToolChoice controls how the model uses tools
type ToolChoice struct {
	Type                   string `json:"type"` // "auto", "any", "tool", "none"
	Name                   string `json:"name,omitempty"`
	DisableParallelToolUse bool   `json:"disable_parallel_tool_use,omitempty"`
}

// ThinkingConfig controls extended thinking
type ThinkingConfig struct {
	Type         string `json:"type"` // "enabled" or "disabled"
	BudgetTokens int    `json:"budget_tokens,omitempty"`
}

// Metadata for the request
type Metadata struct {
	UserID string `json:"user_id,omitempty"`
}

// Response types

// MessagesResponse represents an Anthropic Messages API response
type MessagesResponse struct {
	ID           string         `json:"id"`
	Type         string         `json:"type"` // "message"
	Role         string         `json:"role"` // "assistant"
	Model        string         `json:"model"`
	Content      []ContentBlock `json:"content"`
	StopReason   string         `json:"stop_reason,omitempty"`
	StopSequence string         `json:"stop_sequence,omitempty"`
	Usage        Usage          `json:"usage"`
}

// Usage contains token usage information.
//
// CacheReadInputTokens follows Anthropic's semantics: input_tokens counts
// the UNCACHED prompt tokens and cache_read_input_tokens the prefix served
// from cache, so a client's context-size arithmetic (input + cache_read)
// lands on the real prompt length. Claude Code sizes its auto-compaction on
// exactly that sum -- reporting the full prompt in input_tokens AND the
// cached part here would double-count and compact far too early;
// reporting 0 (what the streaming path did before 2026-08-30) never
// compacts at all and the session runs into the context wall.
type Usage struct {
	InputTokens          int  `json:"input_tokens"`
	CacheReadInputTokens *int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens         int  `json:"output_tokens"`
}

// UsageFromMetrics separates total prompt tokens into uncached and cache-read counts.
func UsageFromMetrics(metrics api.Metrics) Usage {
	total := max(0, metrics.PromptEvalCount)
	var cached *int
	if metrics.PromptEvalCachedCount != nil {
		count := min(max(0, *metrics.PromptEvalCachedCount), total)
		cached = &count
	}
	return Usage{
		InputTokens:          total - intValue(cached),
		CacheReadInputTokens: cached,
		OutputTokens:         metrics.EvalCount,
	}
}

func intValue(v *int) int {
	if v == nil {
		return 0
	}
	return *v
}

// Streaming event types

// MessageStartEvent is sent at the start of streaming
type MessageStartEvent struct {
	Type    string           `json:"type"` // "message_start"
	Message MessagesResponse `json:"message"`
}

// ContentBlockStartEvent signals the start of a content block
type ContentBlockStartEvent struct {
	Type         string       `json:"type"` // "content_block_start"
	Index        int          `json:"index"`
	ContentBlock ContentBlock `json:"content_block"`
}

// ContentBlockDeltaEvent contains incremental content updates
type ContentBlockDeltaEvent struct {
	Type  string `json:"type"` // "content_block_delta"
	Index int    `json:"index"`
	Delta Delta  `json:"delta"`
}

// Delta represents an incremental update
type Delta struct {
	Type        string `json:"type"` // "text_delta", "input_json_delta", "thinking_delta", "signature_delta"
	Text        string `json:"text,omitempty"`
	PartialJSON string `json:"partial_json,omitempty"`
	Thinking    string `json:"thinking,omitempty"`
	Signature   string `json:"signature,omitempty"`
}

// ContentBlockStopEvent signals the end of a content block
type ContentBlockStopEvent struct {
	Type  string `json:"type"` // "content_block_stop"
	Index int    `json:"index"`
}

// MessageDeltaEvent contains updates to the message
type MessageDeltaEvent struct {
	Type  string       `json:"type"` // "message_delta"
	Delta MessageDelta `json:"delta"`
	Usage DeltaUsage   `json:"usage"`
}

// MessageDelta contains stop information
type MessageDelta struct {
	StopReason   string `json:"stop_reason,omitempty"`
	StopSequence string `json:"stop_sequence,omitempty"`
}

// DeltaUsage contains cumulative token usage (same field semantics as Usage;
// the Anthropic SDKs merge these into the final message's usage, which is
// where streaming clients read the prompt size from).
type DeltaUsage struct {
	InputTokens          int  `json:"input_tokens"`
	CacheReadInputTokens *int `json:"cache_read_input_tokens,omitempty"`
	OutputTokens         int  `json:"output_tokens"`
}

// MessageStopEvent signals the end of the message
type MessageStopEvent struct {
	Type string `json:"type"` // "message_stop"
}

// PingEvent is a keepalive event
type PingEvent struct {
	Type string `json:"type"` // "ping"
}

// StreamErrorEvent is an error during streaming
type StreamErrorEvent struct {
	Type  string `json:"type"` // "error"
	Error Error  `json:"error"`
}

// FromMessagesRequest converts an Anthropic MessagesRequest to an Ollama api.ChatRequest
func FromMessagesRequest(r MessagesRequest) (*api.ChatRequest, error) {
	logutil.Trace("anthropic: converting request", "req", TraceMessagesRequest(r))

	var messages []api.Message

	if r.System != nil {
		switch sys := r.System.(type) {
		case string:
			if sys != "" {
				messages = append(messages, api.Message{Role: "system", Content: sys})
			}
		case []any:
			// System can be an array of content blocks
			var content strings.Builder
			for _, block := range sys {
				if blockMap, ok := block.(map[string]any); ok {
					if blockMap["type"] == "text" {
						if text, ok := blockMap["text"].(string); ok {
							// Blocks are joined, not concatenated: a system
							// array is how --append-system-prompt arrives, and
							// gluing two blocks with nothing between them
							// merges the appended instruction into the
							// preceding one whenever it does not end in
							// punctuation. normalizeSystemFirst, which does
							// the same job for the other wire, separates with
							// a blank line; this path must agree with it
							// (2026-09-26 audit).
							if content.Len() > 0 {
								content.WriteString("\n\n")
							}
							content.WriteString(text)
						}
					}
				}
			}
			if content.Len() > 0 {
				messages = append(messages, api.Message{Role: "system", Content: content.String()})
			}
		}
	}

	for i, msg := range r.Messages {
		converted, err := convertMessage(msg)
		if err != nil {
			logutil.Trace("anthropic: message conversion failed", "index", i, "role", msg.Role, "err", err)
			return nil, err
		}
		messages = append(messages, converted...)
	}

	messages = normalizeSystemFirst(messages)

	options := make(map[string]any)

	options["num_predict"] = r.MaxTokens

	if r.Temperature != nil {
		options["temperature"] = *r.Temperature
	}

	if r.TopP != nil {
		options["top_p"] = *r.TopP
	}

	if r.TopK != nil {
		options["top_k"] = *r.TopK
	}

	if len(r.StopSequences) > 0 {
		options["stop"] = r.StopSequences
	}

	var tools api.Tools
	hasBuiltinWebSearch := false
	for _, t := range r.Tools {
		if strings.HasPrefix(t.Type, "web_search") {
			hasBuiltinWebSearch = true
			break
		}
	}

	// tool_choice: "none" means the model must not call a tool. This wire has
	// no field to carry that, so the faithful translation is to send no tools
	// at all — the same rule the OpenAI wire in this repo applies
	// (openai/openai.go). Decoded and unused, the request still carried its
	// tools and the model could answer with a call the caller had forbidden: an
	// agent that reads tool_use and then runs a side-effecting tool
	// (2026-09-26 audit, sixteenth round). "any" and {"type":"tool",…} ask for
	// the opposite — a call is required or forced — and have no representation
	// here either, but dropping the tools would INVERT the caller's
	// instruction, so they leave the tools in place.
	dropTools := r.ToolChoice != nil && strings.EqualFold(strings.TrimSpace(r.ToolChoice.Type), "none")
	if dropTools && len(r.Tools) > 0 {
		logutil.Trace("anthropic: tool_choice none — sending the request without tools", "tools", len(r.Tools))
	}

	for _, t := range r.Tools {
		if dropTools {
			break
		}
		// Anthropic built-in web_search maps to Ollama function name "web_search".
		// If a user-defined tool also uses that name in the same request, drop the
		// user-defined one to avoid ambiguous tool-call routing.
		if hasBuiltinWebSearch && !strings.HasPrefix(t.Type, "web_search") && t.Name == "web_search" {
			logutil.Trace("anthropic: dropping colliding custom web_search tool", "tool", TraceTool(t))
			continue
		}

		tool, _, err := convertTool(t)
		if err != nil {
			return nil, err
		}
		tools = append(tools, tool)
	}

	var think *api.ThinkValue
	normalizedEffort := ""
	if r.OutputConfig != nil {
		normalizedEffort = strings.ToLower(strings.TrimSpace(r.OutputConfig.Effort))
		if normalizedEffort == "xhigh" {
			normalizedEffort = "high"
		}
	}

	if r.Thinking != nil && r.Thinking.Type == "enabled" {
		think = &api.ThinkValue{Value: true}
	}
	if r.Thinking != nil && r.Thinking.Type == "disabled" {
		think = &api.ThinkValue{Value: false}
	}
	if think == nil && r.OutputConfig != nil {
		switch normalizedEffort {
		case "high", "medium", "low", "max":
			think = &api.ThinkValue{Value: normalizedEffort}
		}
	}

	stream := r.Stream
	convertedRequest := &api.ChatRequest{
		Model:    r.Model,
		Messages: messages,
		Options:  options,
		Stream:   &stream,
		Tools:    tools,
		Think:    think,
	}
	logutil.Trace("anthropic: converted request", "req", TraceChatRequest(convertedRequest))

	return convertedRequest, nil
}

// normalizeSystemFirst hoists every system message to the front of the
// conversation as one message. It is this leg's half of a rule the client-side
// proxy states in full (cmd/launch's normalizeSystemFirst): a client can send a
// top-level `system` beside a mid-conversation system message, so the
// conversation reaches the backend as [system, user, system] — and a strict
// chat template raises on that (KAT-Coder's apex GGUF answers "System message
// must be at the beginning"), which the proxy repaired and this leg did not.
// The same body was therefore answered by a 500 here and by an answer there,
// and the two legs handed their backends different prompts for one request
// (2026-09-27 audit, round 42, C42-5).
//
// An already-ordered conversation is returned untouched, byte for byte. The
// rewrite is only needed for a system message that arrives AFTER a non-system
// one, and applying it unconditionally re-rendered the common case — several
// leading system messages concatenated into one string, so the prompt differed
// from the one the client sent and from the previous turn's, defeating any
// prefix cache keyed on the rendered text.
//
// A system message carrying anything but text (an image, a call, a tool-result
// id, thinking) cannot be merged into that one string without dropping what it
// carries, so a conversation holding one is returned exactly as it arrived.
func normalizeSystemFirst(messages []api.Message) []api.Message {
	ordered := true
	blank := false
	seenNonSystem := false
	for _, m := range messages {
		if m.Role == "system" {
			if !systemMessageIsTextOnly(m) {
				return messages
			}
			if seenNonSystem {
				ordered = false
				break
			}
			if strings.TrimSpace(m.Content) == "" {
				blank = true
			}
			continue
		}
		seenNonSystem = true
	}
	if ordered && !blank {
		return messages
	}
	var system []string
	rest := make([]api.Message, 0, len(messages))
	for _, m := range messages {
		if m.Role == "system" {
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}
			continue
		}
		rest = append(rest, m)
	}
	if len(system) == 0 {
		return rest // all system messages were blank — drop them
	}
	out := make([]api.Message, 0, len(rest)+1)
	out = append(out, api.Message{Role: "system", Content: strings.Join(system, "\n\n")})
	return append(out, rest...)
}

// systemMessageIsTextOnly reports whether a system message carries nothing but
// its text, and so can be merged into the single leading system message
// normalizeSystemFirst builds.
func systemMessageIsTextOnly(m api.Message) bool {
	return len(m.Images) == 0 && len(m.ToolCalls) == 0 && m.ToolCallID == "" &&
		m.ToolName == "" && m.Thinking == ""
}

// convertMessage converts an Anthropic MessageParam to Ollama api.Message(s)
func convertMessage(msg MessageParam) ([]api.Message, error) {
	var messages []api.Message
	role := strings.ToLower(msg.Role)

	var textContent strings.Builder
	var images []api.ImageData
	var toolCalls []api.ToolCall
	var thinking string
	var toolResults []api.Message
	textBlocks := 0
	imageBlocks := 0
	toolUseBlocks := 0
	toolResultBlocks := 0
	serverToolUseBlocks := 0
	webSearchToolResultBlocks := 0
	thinkingBlocks := 0
	redactedThinkingBlocks := 0
	documentBlocks := 0
	documentTextBlocks := 0
	documentBinaryBlocks := 0
	searchResultBlocks := 0
	searchResultTextBlocks := 0

	for _, block := range msg.Content {
		switch block.Type {
		case "text":
			textBlocks++
			if block.Text != nil {
				// Blocks are joined, not concatenated, to match the system
				// path and anthropic.FromMessagesRequest: narration either
				// side of a tool call is two blocks, and gluing them turns
				// "read the file." + "Now run the tests." into one sentence
				// whenever the first does not end in punctuation.
				if textContent.Len() > 0 {
					textContent.WriteString("\n\n")
				}
				textContent.WriteString(*block.Text)
			}

		case "image":
			imageBlocks++
			if block.Source == nil {
				logutil.Trace("anthropic: invalid image source", "role", role)
				return nil, errors.New("invalid image source")
			}

			decoded, err := resolveImageSource(block.Source)
			if err != nil {
				logutil.Trace("anthropic: unsupported image source", "role", role, "source_type", block.Source.Type, "error", err)
				return nil, err
			}
			images = append(images, decoded)

		case "tool_use":
			toolUseBlocks++
			if block.ID == "" {
				logutil.Trace("anthropic: tool_use block missing id", "role", role)
				return nil, errors.New("tool_use block missing required 'id' field")
			}
			if block.Name == "" {
				logutil.Trace("anthropic: tool_use block missing name", "role", role)
				return nil, errors.New("tool_use block missing required 'name' field")
			}
			toolCalls = append(toolCalls, api.ToolCall{
				ID: block.ID,
				Function: api.ToolCallFunction{
					Name:      block.Name,
					Arguments: block.Input,
				},
			})

		case "tool_result":
			toolResultBlocks++
			resultContent, resultImages, err := convertToolResultContent(block.Content)
			if err != nil {
				logutil.Trace("anthropic: invalid tool_result content", "role", role, "error", err)
				return nil, err
			}

			toolResults = append(toolResults, api.Message{
				Role:       "tool",
				Content:    resultContent,
				Images:     resultImages,
				ToolCallID: block.ToolUseID,
			})

		case "thinking":
			thinkingBlocks++
			if block.Thinking != nil {
				// Joined, not overwritten: an assistant turn this fork itself
				// emits carries one thinking block per reasoning run (the
				// streaming converter opens a new block when reasoning resumes
				// after text, round 15), so keeping only the last silently
				// dropped every earlier run from the prompt the client echoed
				// back. The text blocks beside it are joined for the same
				// reason (2026-09-26 audit, sixteenth round).
				if thinking != "" {
					thinking += "\n\n"
				}
				thinking += *block.Thinking
			}

		case "redacted_thinking":
			// Encrypted reasoning Anthropic re-sends alongside a thinking block
			// when extended thinking is on. There is nothing usable in it on
			// this wire (api.Message.Thinking is not emitted as OpenAI
			// messages, and the payload is opaque), so it is dropped
			// deliberately rather than landing in the unknown-blocks counter
			// where a silent content loss would hide.
			redactedThinkingBlocks++

		case "document":
			// A file the client attached. The OpenAI wire has no document
			// block, but a document with a TEXT source carries its content
			// inline (source.data) -- dropping it silently told the model
			// nothing about a file the user can see in the transcript, and the
			// client keeps sending it on later turns, so the prompt grows
			// without ever gaining the content. Keep a text source; a base64
			// document (PDF and friends) has no representation here at all and
			// is REFUSED, not dropped: this converter used to count it and
			// answer the turn, so a question about an attached PDF reached a
			// model that never saw it and the client read a 200 for the
			// confident answer to the question alone. The gateway leg of this
			// same product refuses the body instead of answering it, and the
			// two legs must not disagree about whether the model was given the
			// document (2026-09-27 audit, round 35, A-F2).
			documentBlocks++
			if block.Source == nil {
				logutil.Trace("anthropic: document block without a source", "role", role)
				return nil, errors.New("document block without a source")
			}
			if block.Source.Type == "text" && block.Source.Data == "" {
				// Named apart from the unrepresentable-source refusal below: a
				// text source is the one kind this wire DOES carry, so telling
				// the operator that "text" cannot be represented sent them
				// looking for a converter that exists (2026-09-27 audit, round
				// 36, lead).
				logutil.Trace("anthropic: document block with an empty text source", "role", role)
				return nil, errors.New("document block with an empty text source")
			}
			if block.Source.Type == "text" {
				// The leading separator matters as much as the trailing
				// one: a trailing newline alone left the PREVIOUS block's
				// last sentence glued to this one's first ("read the
				// file." + "Now run the tests.\n" = "read the file.Now run
				// the tests."), which is the failure the text-block branch
				// above documents (2026-09-26 audit, sixteenth round).
				if textContent.Len() > 0 {
					textContent.WriteString("\n\n")
				}
				textContent.WriteString(block.Source.Data)
				if !strings.HasSuffix(block.Source.Data, "\n") {
					textContent.WriteString("\n")
				}
				documentTextBlocks++
			} else {
				documentBinaryBlocks++
				logutil.Trace("anthropic: unrepresentable document source",
					"role", role, "source_type", block.Source.Type, "media_type", block.Source.MediaType)
				return nil, fmt.Errorf("document source.type %q cannot be represented on the OpenAI wire", block.Source.Type)
			}

		case "server_tool_use":
			serverToolUseBlocks++
			toolCalls = append(toolCalls, api.ToolCall{
				ID: block.ID,
				Function: api.ToolCallFunction{
					Name:      block.Name,
					Arguments: block.Input,
				},
			})

		case "web_search_tool_result":
			webSearchToolResultBlocks++
			toolResults = append(toolResults, api.Message{
				Role:       "tool",
				Content:    formatWebSearchToolResultContent(block.Content),
				ToolCallID: block.ToolUseID,
			})

		case "search_result":
			// A passage the search returned, carried in the turn the model is
			// asked about: the block's content is the text itself and its
			// source/title say where it came from. It had no case here, so the
			// passages were counted and dropped and the turn answered 200: the
			// model was asked about search results it was never shown, and the
			// client read a confident answer to a question about text it had
			// sent (2026-09-27 audit, round 36, A-F1 — the class round 35
			// closed for a document). The gateway leg of this product carries
			// the same block, so the two legs must not disagree about whether
			// the model received it.
			searchResultBlocks++
			var sb strings.Builder
			if block.Title != "" {
				sb.WriteString(block.Title)
				sb.WriteString("\n")
			}
			if block.Source != nil {
				// The bare string is the documented spelling of a
				// search_result's source; the object form is off-spec but
				// accepted by UnmarshalJSON, and reading only Ref dropped a URL
				// the body did state (2026-09-27 audit, round 37, A-F6).
				ref := block.Source.Ref
				if ref == "" {
					ref = block.Source.URL
				}
				if ref != "" {
					sb.WriteString(ref)
					sb.WriteString("\n")
				}
			}
			sb.WriteString(searchResultText(block.Content))
			if sb.Len() > 0 {
				if textContent.Len() > 0 {
					textContent.WriteString("\n\n")
				}
				textContent.WriteString(sb.String())
				if !strings.HasSuffix(sb.String(), "\n") {
					textContent.WriteString("\n")
				}
				searchResultTextBlocks++
			}

		default:
			// A block of a type this converter does not name used to be
			// COUNTED and dropped: the turn was answered 200 with the block
			// missing from the prompt and nothing said to the client about it
			// (an image-only or unknown-only message left an empty user turn).
			// The gateway leg refuses the same body in words, so the same
			// request was a 400 on one leg and an answer to an altered prompt
			// on the other (2026-09-27 audit, round 39, C-F5). Naming the type
			// is what lets a client tell "you dropped my block" from a bug.
			return nil, fmt.Errorf("content block type %q cannot be represented on the OpenAI wire", block.Type)
		}
	}

	if role == "user" && len(toolResults) > 0 {
		messages = append(messages, toolResults...)
	}

	if textContent.Len() > 0 || len(images) > 0 || len(toolCalls) > 0 || thinking != "" {
		m := api.Message{
			Role:      role,
			Content:   textContent.String(),
			Images:    images,
			ToolCalls: toolCalls,
			Thinking:  thinking,
		}
		messages = append(messages, m)
	}

	// Add tool results as separate messages.
	if role != "user" || len(toolResults) == 0 {
		messages = append(messages, toolResults...)
	}
	logutil.Trace("anthropic: converted block message",
		"role", role,
		"blocks", len(msg.Content),
		"text", textBlocks,
		"image", imageBlocks,
		"tool_use", toolUseBlocks,
		"tool_result", toolResultBlocks,
		"server_tool_use", serverToolUseBlocks,
		"web_search_result", webSearchToolResultBlocks,
		"thinking", thinkingBlocks,
		"redacted_thinking", redactedThinkingBlocks,
		"document", documentBlocks,
		"document_text", documentTextBlocks,
		"document_binary_dropped", documentBinaryBlocks,
		"search_result", searchResultBlocks,
		"search_result_text", searchResultTextBlocks,
		"messages", TraceAPIMessages(messages),
	)

	return messages, nil
}

func formatWebSearchToolResultContent(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []WebSearchResult:
		var resultContent strings.Builder
		for _, item := range c {
			if item.Type != "web_search_result" {
				continue
			}
			fmt.Fprintf(&resultContent, "- %s: %s\n", item.Title, item.URL)
		}
		return resultContent.String()
	case []any:
		var resultContent strings.Builder
		for _, item := range c {
			itemMap, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch itemMap["type"] {
			case "web_search_result":
				title, _ := itemMap["title"].(string)
				url, _ := itemMap["url"].(string)
				fmt.Fprintf(&resultContent, "- %s: %s\n", title, url)
			case "web_search_tool_result_error":
				errorCode, _ := itemMap["error_code"].(string)
				if errorCode == "" {
					return "web_search_tool_result_error"
				}
				return "web_search_tool_result_error: " + errorCode
			}
		}
		return resultContent.String()
	case map[string]any:
		if c["type"] == "web_search_tool_result_error" {
			errorCode, _ := c["error_code"].(string)
			if errorCode == "" {
				return "web_search_tool_result_error"
			}
			return "web_search_tool_result_error: " + errorCode
		}
		data, err := json.Marshal(c)
		if err != nil {
			return ""
		}
		return string(data)
	case WebSearchToolResultError:
		if c.ErrorCode == "" {
			return "web_search_tool_result_error"
		}
		return "web_search_tool_result_error: " + c.ErrorCode
	default:
		data, err := json.Marshal(c)
		if err != nil {
			return ""
		}
		return string(data)
	}
}

// convertTool converts an Anthropic Tool to an Ollama api.Tool, returning true if it's a server tool
func convertTool(t Tool) (api.Tool, bool, error) {
	if strings.HasPrefix(t.Type, "web_search") {
		props := api.NewToolPropertiesMap()
		props.Set("query", api.ToolProperty{
			Type:        api.PropertyType{"string"},
			Description: "The search query to look up on the web",
		})
		return api.Tool{
			Type: "function",
			Function: api.ToolFunction{
				Name:        "web_search",
				Description: "Search the web for current information. Use this to find up-to-date information about any topic.",
				Parameters: api.ToolFunctionParameters{
					Type:       "object",
					Required:   []string{"query"},
					Properties: props,
				},
			},
		}, true, nil
	}

	var params api.ToolFunctionParameters
	if len(t.InputSchema) > 0 {
		if err := json.Unmarshal(t.InputSchema, &params); err != nil {
			logutil.Trace("anthropic: invalid tool schema", "tool", t.Name, "err", err)
			return api.Tool{}, false, fmt.Errorf("invalid input_schema for tool %q: %w", t.Name, err)
		}
	}

	return api.Tool{
		Type: "function",
		Function: api.ToolFunction{
			Name:        t.Name,
			Description: t.Description,
			Parameters:  params,
		},
	}, false, nil
}

// ToMessagesResponse converts an Ollama api.ChatResponse to an Anthropic MessagesResponse
func ToMessagesResponse(id string, r api.ChatResponse) MessagesResponse {
	// Non-nil from the start: Anthropic's message.content is an ARRAY, and a
	// nil slice marshals to `"content":null`. A client that iterates it — the
	// Anthropic SDKs do — has to special-case null for a turn that simply had
	// no text (a tool-call-only or empty turn). See ToMessagesResponse's
	// sibling in the stream converter, which always emits a block.
	content := make([]ContentBlock, 0, 1)

	if r.Message.Thinking != "" {
		content = append(content, ContentBlock{
			Type:     "thinking",
			Thinking: ptr(r.Message.Thinking),
		})
	}

	if r.Message.Content != "" {
		content = append(content, ContentBlock{
			Type: "text",
			Text: ptr(r.Message.Content),
		})
	}

	for _, tc := range r.Message.ToolCalls {
		id := tc.ID
		if id == "" {
			// The upstream sent no id (several OpenAI-compatible GGUF backends,
			// and the Go parsers). ContentBlock.ID is omitempty, so passing it
			// through emitted a tool_use block with NO "id" key at all — one
			// the client cannot name back, and whose tool_result then arrives
			// with tool_use_id "". The streaming converter and the proxy's
			// non-streaming parser both synthesize this id already; this path
			// was the third translation site and the only one that did not, so
			// one upstream turn produced two different blocks depending on
			// `stream` (2026-09-26 audit, sixteenth round).
			key, _ := json.Marshal(tc.Function.Arguments)
			id = ToolCallIDFor(tc.Function.Name, string(key))
		}
		content = append(content, ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  tc.Function.Name,
			Input: tc.Function.Arguments,
		})
	}

	stopReason := mapStopReason(r.DoneReason, len(r.Message.ToolCalls) > 0)

	return MessagesResponse{
		ID:         id,
		Type:       "message",
		Role:       "assistant",
		Model:      r.Model,
		Content:    content,
		StopReason: stopReason,
		Usage:      UsageFromMetrics(r.Metrics),
	}
}

// mapStopReason converts Ollama done_reason to Anthropic stop_reason.
//
// Anthropic's enum is end_turn / max_tokens / stop_sequence / tool_use /
// pause_turn / refusal, and a response ALWAYS carries one. This used to return
// "" for an empty reason — and because the field is `omitempty`, the key was
// absent from the JSON entirely — and "stop_sequence" for anything else it did
// not recognise, which included this package's own synthetic "load"/"unload"
// reasons and the "tool_calls" spelling of a tool turn with no calls attached.
// A client saw a truncation signal, or nothing at all, where the model had
// simply stopped (2026-09-26 audit, fourth round).
//
// "stop_sequence" is deliberately never produced: the stop_sequence field is
// only meaningful alongside the sequence that matched, and nothing in this
// pipe records one — claiming it would be worse than saying end_turn.
func mapStopReason(reason string, hasToolCalls bool) string {
	if hasToolCalls {
		return "tool_use"
	}

	switch reason {
	case "tool_calls", "tool_use", "function_call":
		// An upstream that stops to call a tool but attaches none (hasToolCalls
		// is already false here — see the guard above) must not be reported as
		// tool_use: "tool_use" promises the client a tool_use content block,
		// and an agent that reads it waits for a call that is not in the
		// message — a stalled turn the client cannot distinguish from a slow
		// one. The content was empty, so the turn is over (2026-09-26 audit,
		// fourth round).
		return "end_turn"
	case "length":
		return "max_tokens"
	default:
		// "stop", "", "load", "unload" and anything unknown: the turn ended.
		return "end_turn"
	}
}

// StreamConverter manages state for converting Ollama streaming responses to Anthropic format
type StreamConverter struct {
	ID                   string
	Model                string
	firstWrite           bool
	contentIndex         int
	inputTokens          int
	cacheReadTokens      *int
	outputTokens         int
	estimatedInputTokens int // Estimated tokens from request (used when actual metrics are 0)
	thinkingStarted      bool
	thinkingDone         bool
	textStarted          bool
	toolCallsSent        map[string]bool
}

func NewStreamConverter(id, model string, estimatedInputTokens int) *StreamConverter {
	return &StreamConverter{
		ID:                   id,
		Model:                model,
		firstWrite:           true,
		estimatedInputTokens: estimatedInputTokens,
		toolCallsSent:        make(map[string]bool),
	}
}

// StreamEvent represents a streaming event to be sent to the client
type StreamEvent struct {
	Event string
	Data  any
}

// Process converts an Ollama ChatResponse to Anthropic streaming events
func (c *StreamConverter) Process(r api.ChatResponse) []StreamEvent {
	var events []StreamEvent

	if c.firstWrite {
		c.firstWrite = false
		// Use actual metrics if available, otherwise use estimate
		usage := UsageFromMetrics(r.Metrics)
		c.inputTokens = usage.InputTokens
		c.cacheReadTokens = usage.CacheReadInputTokens
		if c.inputTokens == 0 && intValue(c.cacheReadTokens) == 0 && c.estimatedInputTokens > 0 {
			c.inputTokens = c.estimatedInputTokens
		}

		events = append(events, StreamEvent{
			Event: "message_start",
			Data: MessageStartEvent{
				Type: "message_start",
				Message: MessagesResponse{
					ID:      c.ID,
					Type:    "message",
					Role:    "assistant",
					Model:   c.Model,
					Content: []ContentBlock{},
					Usage: Usage{
						InputTokens:          c.inputTokens,
						CacheReadInputTokens: c.cacheReadTokens,
						OutputTokens:         0,
					},
				},
			},
		})
	}

	if r.Message.Thinking != "" && c.thinkingDone {
		// Reasoning that arrives AFTER the thinking block was closed — the
		// model answered (or a tool call was flushed) and only then emitted
		// more reasoning — used to produce no event at all: the guard below
		// is there to avoid re-opening a CLOSED block, not to discard text,
		// so the client silently lost it with no error and nothing to
		// diagnose, in exactly the shape agentic reasoning models produce
		// (they re-emit reasoning between tool calls). An assistant turn's
		// content is an ARRAY of blocks and the client renders them in
		// arrival order, so this opens a NEW thinking block at the current
		// index instead (2026-09-26 audit, fifteenth round).
		c.thinkingStarted = false
		c.thinkingDone = false
	}

	if r.Message.Thinking != "" && !c.thinkingDone {
		if c.textStarted {
			events = append(events, StreamEvent{
				Event: "content_block_stop",
				Data: ContentBlockStopEvent{
					Type:  "content_block_stop",
					Index: c.contentIndex,
				},
			})
			c.contentIndex++
			c.textStarted = false
		}

		if !c.thinkingStarted {
			c.thinkingStarted = true
			events = append(events, StreamEvent{
				Event: "content_block_start",
				Data: ContentBlockStartEvent{
					Type:  "content_block_start",
					Index: c.contentIndex,
					ContentBlock: ContentBlock{
						Type:     "thinking",
						Thinking: ptr(""),
					},
				},
			})
		}

		events = append(events, StreamEvent{
			Event: "content_block_delta",
			Data: ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: c.contentIndex,
				Delta: Delta{
					Type:     "thinking_delta",
					Thinking: r.Message.Thinking,
				},
			},
		})
	}

	if r.Message.Content != "" {
		if c.thinkingStarted && !c.thinkingDone {
			c.thinkingDone = true
			events = append(events, StreamEvent{
				Event: "content_block_stop",
				Data: ContentBlockStopEvent{
					Type:  "content_block_stop",
					Index: c.contentIndex,
				},
			})
			c.contentIndex++
		}

		if !c.textStarted {
			c.textStarted = true
			events = append(events, StreamEvent{
				Event: "content_block_start",
				Data: ContentBlockStartEvent{
					Type:  "content_block_start",
					Index: c.contentIndex,
					ContentBlock: ContentBlock{
						Type: "text",
						Text: ptr(""),
					},
				},
			})
		}

		events = append(events, StreamEvent{
			Event: "content_block_delta",
			Data: ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: c.contentIndex,
				Delta: Delta{
					Type: "text_delta",
					Text: r.Message.Content,
				},
			},
		})
	}

	seenInCall := make(map[string]int, len(r.Message.ToolCalls))
	for _, tc := range r.Message.ToolCalls {
		argsJSON, err := json.Marshal(tc.Function.Arguments)
		if err != nil {
			slog.Error("failed to marshal tool arguments", "error", err, "tool_id", tc.ID)
			continue
		}

		// An upstream that omits tool-call ids used to lose calls here: the
		// dedup map is keyed by id, so every id-less call after the first
		// looked like one already sent and was skipped — two parallel calls
		// in, one out, still reported as stop_reason "tool_use"
		// (2026-09-26 audit; several OpenAI-compatible GGUF backends send no
		// id). Such a message offers only the call itself as identity, so
		// name+arguments becomes the key (two genuinely different parallel
		// calls differ in arguments; two identical ones are
		// indistinguishable), and the client gets a stable synthesized id to
		// echo back in tool_result.
		//
		// The dedup is ACROSS Process calls, not within one: a list that names
		// the same id-less call twice says two calls — the caller's accumulator
		// splits a bare repeat (a name restated over a call that accumulated no
		// arguments) into a second call, which IS the rule both legs adopted
		// (2026-09-27 audit, round 39, B-F9), and collapsing the pair here
		// delivered one tool_use where the model asked for two, making that fix
		// inert on this wire (2026-09-27 audit, round 40, A40-6). The second and
		// later occurrences of a key within a list take a distinct key and a
		// distinct id — two tool_use blocks with one id cannot be answered
		// separately — while a restatement of an already-sent call in a LATER
		// call still dedups, which is what this map is for.
		key := tc.ID
		id := tc.ID
		if key == "" {
			base := "\x00" + tc.Function.Name + "\x00" + string(argsJSON)
			n := seenInCall[base]
			seenInCall[base] = n + 1
			key = base
			id = ToolCallIDFor(tc.Function.Name, string(argsJSON))
			if n > 0 {
				key = base + "\x00#" + strconv.Itoa(n)
				id = ToolCallIDFor(tc.Function.Name, string(argsJSON)+"#"+strconv.Itoa(n))
			}
		}
		if c.toolCallsSent[key] {
			continue
		}

		// Close thinking block if still open (thinking → tool_use without text in between)
		if c.thinkingStarted && !c.thinkingDone {
			c.thinkingDone = true
			events = append(events, StreamEvent{
				Event: "content_block_stop",
				Data: ContentBlockStopEvent{
					Type:  "content_block_stop",
					Index: c.contentIndex,
				},
			})
			c.contentIndex++
		}

		if c.textStarted {
			events = append(events, StreamEvent{
				Event: "content_block_stop",
				Data: ContentBlockStopEvent{
					Type:  "content_block_stop",
					Index: c.contentIndex,
				},
			})
			c.contentIndex++
			c.textStarted = false
		}

		events = append(events, StreamEvent{
			Event: "content_block_start",
			Data: ContentBlockStartEvent{
				Type:  "content_block_start",
				Index: c.contentIndex,
				ContentBlock: ContentBlock{
					Type:  "tool_use",
					ID:    id,
					Name:  tc.Function.Name,
					Input: api.NewToolCallFunctionArguments(),
				},
			},
		})

		events = append(events, StreamEvent{
			Event: "content_block_delta",
			Data: ContentBlockDeltaEvent{
				Type:  "content_block_delta",
				Index: c.contentIndex,
				Delta: Delta{
					Type:        "input_json_delta",
					PartialJSON: string(argsJSON),
				},
			},
		})

		events = append(events, StreamEvent{
			Event: "content_block_stop",
			Data: ContentBlockStopEvent{
				Type:  "content_block_stop",
				Index: c.contentIndex,
			},
		})

		c.toolCallsSent[key] = true
		c.contentIndex++
	}

	if r.Done {
		if c.textStarted {
			events = append(events, StreamEvent{
				Event: "content_block_stop",
				Data: ContentBlockStopEvent{
					Type:  "content_block_stop",
					Index: c.contentIndex,
				},
			})
		} else if c.thinkingStarted && !c.thinkingDone {
			events = append(events, StreamEvent{
				Event: "content_block_stop",
				Data: ContentBlockStopEvent{
					Type:  "content_block_stop",
					Index: c.contentIndex,
				},
			})
		}

		// A done event carrying no metrics must not ERASE what message_start
		// already told the client. The client SDK accumulates input_tokens
		// (and cache_read_input_tokens) off message_delta whenever the field is
		// present, so a metrics-less done must leave both alone — a hard 0 here
		// overwrites a real count, or the estimate seeded via NewStreamConverter's
		// estimatedInputTokens, and the session's context accounting silently
		// resets to empty (2026-09-26 audit, round 38). Output is genuinely
		// unknown if absent, so it still takes the done value. A prompt served
		// entirely from cache reports input 0 WITH cache_read set, which is why
		// the cache field is part of the test rather than a "> 0" on input.
		if usage := UsageFromMetrics(r.Metrics); usage.InputTokens > 0 || usage.CacheReadInputTokens != nil {
			c.inputTokens = usage.InputTokens
			c.cacheReadTokens = usage.CacheReadInputTokens
		}
		c.outputTokens = r.Metrics.EvalCount
		stopReason := mapStopReason(r.DoneReason, len(c.toolCallsSent) > 0)

		events = append(events, StreamEvent{
			Event: "message_delta",
			Data: MessageDeltaEvent{
				Type: "message_delta",
				Delta: MessageDelta{
					StopReason: stopReason,
				},
				Usage: DeltaUsage{
					InputTokens:          c.inputTokens,
					CacheReadInputTokens: c.cacheReadTokens,
					OutputTokens:         c.outputTokens,
				},
			},
		})

		events = append(events, StreamEvent{
			Event: "message_stop",
			Data: MessageStopEvent{
				Type: "message_stop",
			},
		})
	}

	return events
}

// generateID generates a unique ID with the given prefix using crypto/rand
func generateID(prefix string) string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		// Fallback to time-based ID if crypto/rand fails
		return fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano())
	}
	return fmt.Sprintf("%s_%x", prefix, b)
}

// GenerateMessageID generates a unique message ID
func GenerateMessageID() string {
	return generateID("msg")
}

// shortToolCallID derives a stable, opaque id for a tool call the upstream
// sent without one. Deterministic (same call, same id) so a re-Process of the
// same id-less message dedups instead of emitting the block twice, and unique
// per distinct call so two parallel calls stay two blocks. FNV-1a, hex — small
// enough to sit inside a tool_use id, and collision-resistant across the few
// calls one message carries.
// ToolCallIDFor returns a stable synthesized id for a tool call the upstream
// sent WITHOUT one, keyed on the call's own identity: its name and its
// arguments. Such a call offers only itself as identity (two genuinely
// different parallel calls differ in arguments, and two identical ones are
// indistinguishable), and the client needs some id to echo back in its
// tool_result.
//
// Exported because both translation paths must apply the same rule: the
// streaming converter, which learned it first for OpenAI-compatible GGUF
// backends that send no id, and the proxy's non-streaming parser, which passed
// the empty id through and left the client with a tool_use block it could not
// name back (2026-09-26 audit, thirteenth round).
func ToolCallIDFor(name, argsJSON string) string {
	return "call_" + shortToolCallID("\x00"+name+"\x00"+argsJSON)
}

func shortToolCallID(key string) string {
	var h uint32 = 2166136261
	for i := 0; i < len(key); i++ {
		h ^= uint32(key[i])
		h *= 16777619
	}
	return strconv.FormatUint(uint64(h), 16)
}

// IsImageURL reports whether an api.ImageData produced by resolveImageSource
// carries a URL to fetch rather than image bytes. An Anthropic image source of
// type "url" is carried through api.ImageData as its own URL text, because the
// wire this converter feeds (OpenAI's `image_url`) takes a URL as readily as a
// data URL — refusing the block instead was a divergence from the gateway leg,
// which forwards the same source, so one body was answered with a 400 here and
// with a request the model could see there (2026-09-27 audit, round 38, A-F3).
//
// The test is what a URL IS — a scheme (or a protocol-relative "//"), on the
// trimmed string — not the three lowercase prefixes it started as. Matching
// only "http://"/"https://"/"data:" re-encoded a url source the client wrote
// as "HTTPS://…" or "ftp://…" into a JPEG of the address text, so the model
// was asked about a picture of a URL and the backend was never sent the image
// the client pointed at (2026-09-27 audit, round 39, A-F1/C-F9). Every format
// this converter accepts starts with a magic number (PNG's 0x89, JPEG's 0xFF,
// GIF's 'G', RIFF's "RIFF"), none of which spells a scheme — and the base64
// arm of resolveImageSource refuses a payload that decodes to one, so a byte
// string can only reach here as bytes.
func IsImageURL(img api.ImageData) bool {
	return isURLText(string(img))
}

// isURLText reports whether s reads as a URL: "//" (protocol-relative),
// "data:" case-insensitively, or an alpha scheme followed by "://".
func isURLText(s string) bool {
	s = strings.TrimSpace(s)
	if strings.HasPrefix(s, "//") {
		return true
	}
	if len(s) >= 5 && strings.EqualFold(s[:5], "data:") {
		return true
	}
	i := strings.Index(s, "://")
	if i <= 0 {
		return false
	}
	for j := 0; j < i; j++ {
		c := s[j]
		alpha := (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
		tail := (c >= '0' && c <= '9') || c == '+' || c == '-' || c == '.'
		if !alpha && !(j > 0 && tail) {
			return false
		}
	}
	return true
}

func resolveImageSource(source *ImageSource) (api.ImageData, error) {
	switch source.Type {
	case "url":
		if source.URL == "" {
			return nil, errors.New(`invalid image source type: url, with no url`)
		}
		if !isURLText(source.URL) {
			// A source DECLARED as a url is carried as its text, and the only
			// thing that makes it an image rather than a string is that a
			// reader can fetch it. Text with no scheme is not fetchable: it
			// used to reach the media sniffer as plain characters, which it
			// labelled text/plain and forced to image/jpeg, so a client that
			// wrote "example.com/shot.png" had the model shown a picture of
			// the address while the picture itself was never requested — and
			// the gateway leg, given the same block, forwards the string as a
			// URL and lets the backend fail to fetch it. Refusing in words is
			// the same verdict both legs give a source this wire cannot
			// express (2026-09-27 audit, round 41, A41-5).
			// The value is not echoed: it can be as long as the client likes,
			// and an error message is not a place to paste a megabyte.
			return nil, errors.New(`invalid image source: url source carries text that is not url-shaped`)
		}
		return api.ImageData(source.URL), nil
	case "base64", "":
		// "" is the source a client spells as media_type+data with no type.
		// Refusing it 400'd a body the gateway leg has always read (2026-09-27
		// audit, round 39, C-F10).
		if source.Data == "" {
			// An empty payload decodes to zero bytes with no error, and the
			// blank image was carried into the request as a part the upstream
			// cannot decode — the client was told the turn succeeded. The
			// gateway leg refuses the same body in words (2026-09-27 audit,
			// round 38, A-F6).
			return nil, errors.New("invalid image source: base64 with no data")
		}
		decoded, err := base64.StdEncoding.DecodeString(source.Data)
		if err != nil {
			return nil, fmt.Errorf("invalid base64 image data: %w", err)
		}
		if isURLText(string(decoded)) {
			// A source that says "base64" and decodes to a URL is not an image.
			// Byte-sniffing sent it on as a URL the client never named — the
			// picture was dropped and the backend was told to fetch an address
			// out of the payload (2026-09-27 audit, round 39, A-F1, direction
			// 1). The gateway leg refuses the same payload, so the two legs
			// answer one body one way.
			return nil, errors.New("invalid image source: base64 data decodes to a URL, not image bytes")
		}
		return decoded, nil
	default:
		return nil, fmt.Errorf("invalid image source type: %s. Only base64 images are supported.", source.Type)
	}
}

// describeToolResultDocument renders a document block nested in a tool result:
// its text, where the source carries any, and a description naming the type and
// size of a binary one. The wording matches the gateway leg's describeBlock, so
// the same body is answered the same way whichever leg serves it.
func describeToolResultDocument(raw any) string {
	src, _ := raw.(map[string]any)
	if src == nil {
		return "[document tool result omitted: no source]"
	}
	if st, _ := src["type"].(string); st == "text" {
		if d, _ := src["data"].(string); d != "" {
			return d
		}
		return `[document tool result omitted: source.type "text" with no data]`
	}
	media, size := "unknown", 0
	if m, ok := src["media_type"].(string); ok && m != "" {
		media = m
	}
	if d, ok := src["data"].(string); ok {
		size = len(d)
	}
	return fmt.Sprintf("[document tool result omitted: %s, %d bytes of base64]", media, size)
}

// describeToolResultBlock renders a block nested in a tool result (or a search
// result's passage list) whose type this converter does not name: a document
// through the describer above, anything else as its JSON. The gateway leg's
// describeBlock does the same, and this is a copy rather than an import because
// the two modules cannot see each other's helpers — the same nested block used
// to be skipped here and described there, so the model was asked about content
// that only reached it on one of the two legs (2026-09-27 audit, round 37,
// A-F2/A-F3).
func describeToolResultBlock(bm map[string]any) string {
	switch t, _ := bm["type"].(string); t {
	case "document":
		return describeToolResultDocument(bm["source"])
	case "image":
		// An image reached through a path with no carrier for it — a passage
		// inside a search_result, say, where the tool_result reader that owns a
		// nested image is not the one walking this block. The JSON fallback
		// below pasted the whole base64 payload into the prompt as prose: a
		// 600 KB screenshot was billed to the model as the transport encoding
		// of a picture it cannot read, where the gateway leg sends this same
		// one-line notice and where the document arm beside this one exists for
		// exactly that reason (2026-09-27 audit, round 38, A-F4).
		media, size := "unknown", 0
		if src, ok := bm["source"].(map[string]any); ok {
			if m, ok := src["media_type"].(string); ok && m != "" {
				media = m
			}
			if d, ok := src["data"].(string); ok {
				size = len(d)
			}
		}
		return fmt.Sprintf("[image tool result omitted: %s, %d bytes of base64]", media, size)
	case "search_result":
		// A passage list nested inside a tool result (or inside another
		// passage list): its title, its origin and the passages themselves,
		// rendered through the flattener the message-level case uses. Without
		// this arm the block fell to the JSON fallback below, so a screenshot
		// sitting in one of its passages took its whole base64 payload into the
		// prompt — the image arm above describes the identical block one level
		// up (2026-09-27 audit, round 38, A-F4).
		var label []string
		if title, _ := bm["title"].(string); title != "" {
			label = append(label, title)
		}
		switch src := bm["source"].(type) {
		case string:
			if src != "" {
				label = append(label, src)
			}
		case map[string]any:
			ref, _ := src["ref"].(string)
			if ref == "" {
				ref, _ = src["url"].(string)
			}
			if ref != "" {
				label = append(label, ref)
			}
		}
		if body := searchResultText(bm["content"]); body != "" {
			label = append(label, body)
		}
		return strings.Join(label, "\n")
	}
	if b, err := json.Marshal(bm); err == nil {
		return string(b)
	}
	return "[tool result block that could not be represented]"
}

// searchResultText flattens a search_result's content — the passages a search
// returned — into the text the model is asked about. A text block is its
// passage; a block of any OTHER type is described rather than read for a
// "text" key it may happen to carry, which is what this did: the type was
// never consulted, so text from a block of an unknown type was pasted into the
// prompt while a block holding its passage under another key (a nested
// search_result) was silently dropped — the same silent omission the caller's
// case exists to close, one level down (2026-09-27 audit, round 37, A-F3).
func searchResultText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []ContentBlock:
		var sb strings.Builder
		for _, b := range c {
			if b.Type == "text" {
				// An empty passage is no passage: it used to fall through to
				// the describer and put `{"text":"","type":"text"}` in the
				// prompt as though the search had returned it, where the same
				// block JSON-decoded is skipped (2026-09-27 audit, round 38,
				// A-F8).
				if b.Text == nil || *b.Text == "" {
					continue
				}
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(*b.Text)
				continue
			}
			if desc := describeSearchBlock(b); desc != "" {
				if sb.Len() > 0 {
					sb.WriteString("\n")
				}
				sb.WriteString(desc)
			}
		}
		return sb.String()
	case []any:
		var sb strings.Builder
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			var desc string
			if t, _ := m["type"].(string); t == "text" {
				desc, _ = m["text"].(string)
			} else {
				desc = describeToolResultBlock(m)
			}
			if desc == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n")
			}
			sb.WriteString(desc)
		}
		return sb.String()
	}
	return ""
}

// describeSearchBlock renders a non-text passage block given as a typed block
// rather than a decoded map: to the same wording as its map form.
func describeSearchBlock(b ContentBlock) string {
	raw, err := json.Marshal(b)
	if err != nil {
		return "[tool result block that could not be represented]"
	}
	var m map[string]any
	if json.Unmarshal(raw, &m) != nil {
		return string(raw)
	}
	return describeToolResultBlock(m)
}

func convertToolResultContent(content any) (string, []api.ImageData, error) {
	switch c := content.(type) {
	case nil:
		return "", nil, nil
	case string:
		return c, nil, nil
	case []ContentBlock:
		// The package's own typed content: the SDK's `content` field is a
		// `[]ContentBlock` in one spelling and decoded JSON in the other, and
		// only the decoded shape had a case — so a tool_result written with
		// typed blocks fell to the default and converted to an EMPTY tool
		// message with no error. The model was told the tool returned nothing
		// and the client read a 200, while the same logical content written as
		// JSON reached the model in full (2026-09-27 audit, round 38, A-F5).
		// Each block is re-read through the decoded path below so both
		// spellings are assembled by one implementation and cannot drift.
		items := make([]any, 0, len(c))
		for i := range c {
			raw, err := json.Marshal(c[i])
			if err != nil {
				continue
			}
			var m map[string]any
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			items = append(items, m)
		}
		return convertToolResultContent(items)
	case []any:
		var text strings.Builder
		var images []api.ImageData

		for _, cb := range c {
			cbMap, ok := cb.(map[string]any)
			if !ok {
				// A non-object element is not a content block: it was skipped
				// here and json.Marshal'd into the prompt by the gateway leg,
				// so the same body reached the model as two different prompts
				// ("after" vs "1\n\"x\"\nafter"). One rule at both depths on
				// both legs: refuse it, and name what was refused (2026-09-27
				// audit, round 39, C-F6).
				return "", nil, errors.New("tool_result content holds an element that is not a JSON object")
			}

			switch cbMap["type"] {
			case "text":
				if t, ok := cbMap["text"].(string); ok {
					// Separated so two blocks do not read as one sentence —
					// "first line" + "second line" = "first linesecond line"
					// (2026-09-26 audit, sixteenth round). The separator is the
					// gateway leg's toolResultText join: the two legs assemble
					// the same blocks, so the assembled text has to be the same
					// bytes (2026-09-27 audit, round 37, A-F7).
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(t)
				}
			case "document":
				// A file a tool returned, nested inside its result. This case
				// did not exist, so the block was skipped: a tool that read a
				// text file reported back nothing at all, and the model
				// answered about a file it was never shown while the client
				// read a 200 (2026-09-27 audit, round 36, A-F2 — the gateway
				// leg DESCRIBES the same nested block, so the same body was
				// answered differently depending on which leg served it). A
				// text source is content and is carried; a binary source is
				// described by its type and size rather than pasted, which is
				// that leg's wording and its reason: inlining the base64 bills
				// the model for the transport encoding of a file it cannot
				// read.
				if desc := describeToolResultDocument(cbMap["source"]); desc != "" {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(desc)
				}
			case "image":
				rawSource, ok := cbMap["source"].(map[string]any)
				if !ok {
					return "", nil, errors.New("invalid tool_result image source")
				}

				var source ImageSource
				if rawType, ok := rawSource["type"].(string); ok {
					source.Type = rawType
				}
				if rawMediaType, ok := rawSource["media_type"].(string); ok {
					source.MediaType = rawMediaType
				}
				if rawData, ok := rawSource["data"].(string); ok {
					source.Data = rawData
				}
				// The url is copied for the same reason the three fields above
				// are: this arm hand-builds the source, and a field it does not
				// copy is a field resolveImageSource cannot see. A nested url
				// source was therefore refused with "url, with no url" — a body
				// the gateway leg forwards to the model — so the same screenshot
				// reached the model or was lost depending on which leg served
				// the turn (2026-09-27 audit, round 39, C-F1/A-F2).
				if rawURL, ok := rawSource["url"].(string); ok {
					source.URL = rawURL
				}

				img, err := resolveImageSource(&source)
				if err != nil {
					return "", nil, err
				}
				images = append(images, img)
			default:
				// A nested block of a type this switch does not name is
				// DESCRIBED, not dropped — the same defect the document arm
				// above closed, one type over: a nested search_result carries
				// passages the model is asked about, and it was counted and
				// silently discarded here while the gateway leg put the same
				// block in the prompt. The turn was answered 200 about text the
				// model never received, on this leg only (2026-09-27 audit,
				// round 37, A-F2).
				if desc := describeToolResultBlock(cbMap); desc != "" {
					if text.Len() > 0 {
						text.WriteString("\n")
					}
					text.WriteString(desc)
				}
			}
		}

		return text.String(), images, nil
	default:
		// Any other shape — a bare object such as
		// {"type":"text","text":"hi"} where the spec asks for a one-element
		// array, a scalar, an SDK-typed result list — converted to an EMPTY
		// tool message with no error: the model was told the tool returned
		// nothing while the client read a 200, and the gateway leg, given the
		// same block, put its JSON in the prompt (describeBlock). Both legs
		// now DESCRIBE it the same way rather than one of them erasing the
		// tool's answer (2026-09-27 audit, round 41, C41-9).
		if m, ok := c.(map[string]any); ok {
			if desc := describeToolResultBlock(m); desc != "" {
				return desc, nil, nil
			}
			return "", nil, nil
		}
		if data, err := json.Marshal(c); err == nil && len(data) > 0 && string(data) != "null" {
			return string(data), nil, nil
		}
		return "", nil, nil
	}
}

// ptr returns a pointer to the given string value
func ptr(s string) *string {
	return &s
}

// CountTokensRequest represents an Anthropic count_tokens request
type CountTokensRequest struct {
	Model      string          `json:"model"`
	Messages   []MessageParam  `json:"messages"`
	System     any             `json:"system,omitempty"`
	Tools      []Tool          `json:"tools,omitempty"`
	ToolChoice *ToolChoice     `json:"tool_choice,omitempty"`
	Thinking   *ThinkingConfig `json:"thinking,omitempty"`
}

// EstimateInputTokens estimates input tokens from a MessagesRequest (reuses CountTokensRequest logic)
func EstimateInputTokens(req MessagesRequest) int {
	return estimateTokens(CountTokensRequest{
		Model:      req.Model,
		Messages:   req.Messages,
		System:     req.System,
		Tools:      req.Tools,
		ToolChoice: req.ToolChoice,
		Thinking:   req.Thinking,
	})
}

// CountTokensResponse represents an Anthropic count_tokens response
type CountTokensResponse struct {
	InputTokens int `json:"input_tokens"`
}

// estimateTokens returns a rough estimate of tokens (len/4).
// TODO: Replace with actual tokenization via Tokenize API for accuracy.
// Current len/4 heuristic is a rough approximation (~4 chars/token average).
func estimateTokens(req CountTokensRequest) int {
	var totalLen int

	// Count system prompt
	totalLen += systemBytes(req.System)

	for _, msg := range req.Messages {
		// Count role (always present)
		totalLen += len(msg.Role)
		// Count content
		totalLen += countAnyContent(msg.Content)
	}

	// Charge the tools the CONVERTER forwards, not the ones the body carries.
	// FromMessagesRequest drops every tool for tool_choice "none" and drops a
	// user-defined tool that collides with the built-in web_search, so a body
	// carrying a 400 KB schema it had just asked not to be given was charged
	// ~100 000 tokens against a converted prompt of 97 bytes — the estimate
	// seeds the client-visible input_tokens on the local leg, so the session's
	// meter and auto-compaction read a tool surface the model never saw
	// (2026-09-27 audit, round 42, A42-3).
	//
	// The predicates are the converter's own, spelled the same way: a
	// tool_choice type is matched case- and space-insensitively, and the
	// collision is a non-builtin tool NAMED web_search beside a builtin one.
	dropTools := req.ToolChoice != nil && strings.EqualFold(strings.TrimSpace(req.ToolChoice.Type), "none")
	hasBuiltinWebSearch := false
	for _, t := range req.Tools {
		if strings.HasPrefix(t.Type, "web_search") {
			hasBuiltinWebSearch = true
			break
		}
	}
	for _, tool := range req.Tools {
		if dropTools {
			break
		}
		if hasBuiltinWebSearch && !strings.HasPrefix(tool.Type, "web_search") && tool.Name == "web_search" {
			continue
		}
		totalLen += len(tool.Name) + len(tool.Description) + len(tool.InputSchema)
	}

	// Return len/4 as rough token estimate, minimum 1 if there's any content
	tokens := totalLen / 4
	if tokens == 0 && (len(req.Messages) > 0 || req.System != nil) {
		tokens = 1
	}
	return tokens
}

func countAnyContent(content any) int {
	if content == nil {
		return 0
	}

	switch c := content.(type) {
	case string:
		return len(c)
	case []ContentBlock:
		total := 0
		for _, block := range c {
			total += countContentBlock(block)
		}
		return total
	case []any:
		total := 0
		for _, item := range c {
			total += countContentItem(item)
		}
		return total
	default:
		if data, err := json.Marshal(content); err == nil {
			return len(data)
		}
		return 0
	}
}

// systemBytes charges the system prompt for what the converter WRITES for it.
// FromMessagesRequest reads a system string whole, and a system ARRAY for its
// "text" blocks only — joined with a blank line — dropping every other block
// and dropping a non-string, non-array system value entirely. Charging that
// input through the general content reader billed the blocks the converter
// discards: a 400 KB text document in the system array measured 100 002 tokens
// against a converted system message of 2 bytes, and the estimate seeds the
// client-visible input_tokens on the local leg, so the session compacted early
// on a prompt that was never sent (2026-09-27 audit, round 42, A42-4).
func systemBytes(system any) int {
	switch sys := system.(type) {
	case string:
		return len(sys)
	case []any:
		// The separator is the converter's "\n\n": the blocks it joins are
		// prompt bytes too, and leaving them out would let the two measures
		// drift by two bytes per block.
		total := 0
		for _, block := range sys {
			bm, ok := block.(map[string]any)
			if !ok || bm["type"] != "text" {
				continue
			}
			text, ok := bm["text"].(string)
			if !ok {
				continue
			}
			if total > 0 {
				total += 2
			}
			total += len(text)
		}
		return total
	}
	return 0
}

// countContentItem charges one content item that arrived as decoded JSON.
//
// It used to marshal the item and unmarshal it into a ContentBlock, and
// countContentBlock re-entered countAnyContent for a search_result's nested
// content — so every nesting level re-marshalled the whole remaining subtree
// and the total was quadratic in the depth. A 147 KB body nested 3000 deep
// (under encoding/json's own nesting limit) cost six seconds of CPU on
// /v1/messages BEFORE any upstream call, and the estimate is computed
// unconditionally by middleware/anthropic.go (2026-09-27 audit, round 38,
// A-F1). This walks the decoded value directly — the same fields with the same
// arithmetic as the typed path, in one pass.
func countContentItem(item any) int { return countContentItemIn(item, chargeMessage) }

// chargeContext is the carrier a content block travels in, because the
// converter treats the same block differently in each one and the estimate has
// to charge what that reader WRITES. A block's own JSON is the prompt only
// where the converter pastes its JSON; where it describes the block, the
// description is the prompt; where it carries the block, the carrier's unit is.
type chargeContext int

const (
	// chargeMessage is a block in a message's (or a passage list's own) content
	// array: the reader is convertMessage.
	chargeMessage chargeContext = iota
	// chargeToolResult is an element of a tool_result's content: the reader is
	// convertToolResultContent, which carries an image, describes a binary
	// document and drops a non-string text.
	chargeToolResult
	// chargeSearchResult is a passage in a search_result's content: the reader
	// is searchResultText, which reads a "text" passage and DESCRIBES every
	// other block — a picture among the passages is a one-line notice, not its
	// payload.
	chargeSearchResult
)

func countContentItemIn(item any, ctx chargeContext) int {
	m, ok := item.(map[string]any)
	if !ok {
		// A non-object item has no block shape; the typed path charged it 0
		// because it could not be decoded into one.
		return 0
	}
	switch ctx {
	case chargeToolResult:
		return toolResultItemBytes(m)
	case chargeSearchResult:
		return searchResultItemBytes(m)
	}
	total := 0
	if s, ok := m["text"].(string); ok {
		total += len(s)
	}
	switch t, _ := m["type"].(string); t {
	case "tool_use":
		// The block's own JSON, exactly as the typed arm charges it — with the
		// inline binary held to the image allowance, so a nested payload the
		// converter DESCRIBES is not billed as its transport encoding.
		total += clampedJSONBytes(m)
	case "tool_result":
		// Not clampedJSONBytes: the converter does not paste this block, it
		// CONVERTS its content (convertToolResultContent), and the charge
		// follows that reader (see toolResultBytes).
		total += toolResultBytes(m)
	case "document":
		// What the converter WRITES for this block, which is not what the
		// block carries: a nested document reaches the prompt as its text, or
		// as the one-line notice naming the media type and the base64 size
		// (describeToolResultDocument — the very function the converter calls
		// for this arm). Charging the source's data plus ref billed the
		// transport encoding of a file the model is told about in sixty bytes:
		// a 400 KB PDF nested in a tool result measured 546 000 bytes against
		// a 74-byte wire, and since the estimate seeds the client-visible
		// input_tokens, a session carrying one attachment read as a prompt six
		// figures long and compacted early (2026-09-27 audit, round 41, A41-2).
		total += len(describeToolResultDocument(m["source"]))
	case "image":
		total += imageBlockBytes(m["source"])
	case "search_result":
		if title, ok := m["title"].(string); ok {
			total += len(title)
		}
		// The passages are read by searchResultText, whose own rule this walk
		// uses (see chargeSearchResult): a nested picture among them is
		// DESCRIBED, so charging it the image allowance billed ~4 100 bytes
		// against a five-byte notice and the session's meter read a prompt the
		// model was never sent (2026-09-27 audit, round 42, C42-1).
		total += countItemsIn(m["content"], chargeSearchResult)
		total += imageSourceBytes(m["source"])
	case "server_tool_use":
		// Charged exactly as the tool_use arm above, because the converter
		// writes the two the same way: this block's JSON IS the tool call, and
		// the escapes in it are transport that the receiver decodes. Charging
		// len(json.Marshal(m)) raw billed an identical call twice as much as
		// its tool_use twin — 515 tokens against 1 017 for the same 4 112-byte
		// prompt — and both arms sit in this same function, one of them routed
		// through clampedJSONBytes (2026-09-27 audit, round 42, A42-5).
		total += clampedJSONBytes(m)
	case "web_search_tool_result":
		// This arm is the DECODED-JSON reader, so the block here is a nested
		// one — inside a tool result or a passage list, where the converter
		// falls through to describeToolResultBlock's JSON fallback, which is
		// what the arms above charge for it. The message-level spelling is the
		// typed arm's business, and it is charged the formatted hits rather
		// than this JSON (2026-09-27 audit, round 41, A41-3). It is no longer
		// said to be the `system` path: system content is charged by
		// systemBytes for exactly the text blocks the converter reads
		// (2026-09-27 audit, round 42, A42-4).
		if data, err := json.Marshal(m); err == nil {
			total += len(data)
		}
	}
	return total
}

// countItemsIn charges a content array in the carrier named by ctx. It is the
// one reader for both spellings the converter takes: it decodes a typed
// []ContentBlock the way the converter does (through JSON, so the two cannot
// drift) and walks the items with the carrier's own rule.
func countItemsIn(content any, ctx chargeContext) int {
	switch c := content.(type) {
	case string:
		// A bare string where a list is expected: searchResultText returns it
		// whole, convertToolResultContent writes it whole.
		return len(c)
	case []ContentBlock:
		items := make([]any, 0, len(c))
		for i := range c {
			raw, err := json.Marshal(c[i])
			if err != nil {
				continue
			}
			var m map[string]any
			if json.Unmarshal(raw, &m) != nil {
				continue
			}
			items = append(items, m)
		}
		return countItemsIn(items, ctx)
	case []any:
		total := 0
		for _, item := range c {
			total += countContentItemIn(item, ctx)
		}
		return total
	}
	return 0
}

// toolResultBytes charges one tool_result block for what
// convertToolResultContent WRITES into the prompt for it: the block's own
// frame — its type, its tool_use_id and its is_error flag — plus the converted
// content.
//
// It does NOT charge clampedJSONBytes of the whole block, which is what the
// tool_use arm beside it charges and what this arm used to charge. The
// converter never pastes a tool_result's JSON: it converts the content, and a
// nested attachment is the clearest case — a 400 KB PDF inside a tool_result
// reaches the prompt as a 71-byte notice, so serializing the block billed the
// base64 of a file the model is told about in seventy bytes, and the same shape
// at 4 bytes measured 148 against a 156-byte wire, i.e. the charge tracked the
// payload rather than the notice (2026-09-27 audit, round 42, A42-2/A42-6).
func toolResultBytes(block map[string]any) int {
	frame := make(map[string]any, len(block))
	for k, v := range block {
		if k != "content" {
			frame[k] = v
		}
	}
	total := 0
	if data, err := json.Marshal(frame); err == nil {
		total += len(data) - jsonEscapeOverhead(data)
	}
	return total + toolResultContentBytes(block["content"])
}

// toolResultContentBytes charges one tool_result's `content` field the way
// convertToolResultContent converts it.
func toolResultContentBytes(content any) int {
	switch c := content.(type) {
	case nil:
		return 0
	case string:
		return len(c)
	case []ContentBlock, []any:
		// The converter joins the elements it converts with a newline and
		// REFUSES an element that is not a JSON object (the whole request is
		// answered 400 and no estimate is used for it), so a non-object item
		// writes nothing here either.
		return countItemsIn(c, chargeToolResult)
	case map[string]any:
		// A bare object is described, not carried: convertToolResultContent's
		// default arm calls describeToolResultBlock, whose fallback is the
		// block's own JSON — and that JSON is TEXT in the prompt, so its
		// escapes are prompt bytes and are charged in full. Subtracting them
		// (as clampedJSONBytes does for a JSON *string field*, where the
		// receiver decodes the escapes away) charged half the wire for the
		// shape round 41 added support for (2026-09-27 audit, round 42, A42-6).
		return len(describeToolResultBlock(c))
	}
	if data, err := json.Marshal(content); err == nil && len(data) > 0 && string(data) != "null" {
		return len(data)
	}
	return 0
}

// toolResultItemBytes charges one element of a tool_result's content, arm for
// arm with convertToolResultContent: a text element is its text when that text
// IS a string (the converter's case has no else, so a non-string text writes
// nothing), a document is described, an image is CARRIED — it is appended to
// the message's images, so it is charged the allowance this product's other two
// measures charge — and every other element is whatever
// describeToolResultBlock renders for it, which is exactly what the converter
// writes for it too.
func toolResultItemBytes(m map[string]any) int {
	switch t, _ := m["type"].(string); t {
	case "text":
		text, _ := m["text"].(string)
		return len(text)
	case "document":
		return len(describeToolResultDocument(m["source"]))
	case "image":
		return imageBlockBytes(m["source"])
	}
	return len(describeToolResultBlock(m))
}

// searchResultItemBytes charges one passage of a search_result's content, the
// way searchResultText reads it: a "text" block is its text — and a text that
// is not a string is no passage and is skipped — while any other block is
// DESCRIBED, so a picture among the passages is its one-line notice and not its
// payload.
func searchResultItemBytes(m map[string]any) int {
	if t, _ := m["type"].(string); t == "text" {
		text, _ := m["text"].(string)
		return len(text)
	}
	return len(describeToolResultBlock(m))
}

// imageSourceBytes is the content a block's `source` carries, in either
// spelling: a base64 payload, a bare-string reference (a search_result's URL),
// or the url field of the object form. It charges a REFERENCE — the search
// result's origin, which the converter writes into the prompt as text — not an
// image's payload, which imageBlockBytes charges.
func imageSourceBytes(source any) int {
	switch s := source.(type) {
	case string:
		return len(s)
	case map[string]any:
		data, _ := s["data"].(string)
		url, _ := s["url"].(string)
		ref, _ := s["ref"].(string)
		return len(data) + len(url) + len(ref)
	}
	return 0
}

// inlineImageByteAllowance is the bytes one inline image is charged, the unit
// cmd/launch/prompt_payload_bytes.go (inlineImageByteAllowance) and
// tools/gateway/main.go (imagePartByteAllowance) already use. The product has
// three prompt-size measures and this one charged an image its whole base64 —
// 133 335 tokens for a 400 KB PNG where the client leg's own unit charged
// 4 210 bytes for the same body. That is 127×, and it is the wrong direction:
// the estimate seeds the client-visible input_tokens on /v1/messages, so an
// image-bearing turn reported a prompt far larger than the model was sent and
// the session compacted early (2026-09-27 audit, round 39, A-F3).
const inlineImageByteAllowance = 4096

// imageBlockBytes charges one image block: the allowance when the source
// carries anything to show, nothing when it carries nothing. The converter
// sends a url source as a reference rather than bytes, and the allowance
// replaces the URL too — matching prompt_payload_bytes.go, which charges the
// same 4 096 bytes for a url-sourced image (2026-09-27 audit, round 39, A-F3).
func imageBlockBytes(source any) int {
	switch s := source.(type) {
	case string:
		if s == "" {
			return 0
		}
	case map[string]any:
		data, _ := s["data"].(string)
		url, _ := s["url"].(string)
		ref, _ := s["ref"].(string)
		if data == "" && url == "" && ref == "" {
			return 0
		}
	default:
		return 0
	}
	return inlineImageByteAllowance
}

// clampedJSONBytes is len(json.Marshal(v)) with every inline binary payload in
// it held to the image allowance. The converter DESCRIBES a nested payload
// rather than pasting it (a 400 KB image nested in a tool_result is carried as
// a 74-byte notice), so serializing the enclosing block pasted the base64 back
// in and billed a 74-byte prompt as 100 051 tokens. The clamp is the same
// distinction the converter draws, in bytes (2026-09-27 audit, round 39, A-F3).
//
// The walk runs over the DECODED view of v, not v itself. binaryOverflow reads
// the two shapes a decoder produces — map[string]any and []any — and a caller
// that passes a typed struct (the production path: MessageParam.Content is
// []ContentBlock) got the default arm and a clamp of zero, so the round-39 fix
// was inert exactly where it was meant to bite: a 400 KB document nested in a
// tool_result still measured 420 132 bytes against a 192-byte wire, and the
// estimate reported 100 086 tokens for it (2026-09-27 audit, round 40, A40-1).
// Decoding here is one rule for every caller, and it is the same reading the
// converter itself takes.
func clampedJSONBytes(v any) int {
	data, err := json.Marshal(v)
	if err != nil {
		return 0
	}
	var decoded any
	if json.Unmarshal(data, &decoded) != nil {
		return len(data) - jsonEscapeOverhead(data)
	}
	// The escapes come off for the same reason the base64 does: the upstream
	// JSON-decodes this text before anything is tokenized, so an escape
	// describes the transport and not the prompt. Without this a tool result
	// holding markup — every quote, backslash and newline in a file — was
	// charged six bytes per character where the product's other two measures
	// (cmd/launch/prompt_payload_bytes.go and tools/gateway/main.go, both of
	// which subtract this same quantity) charged the decoded text
	// (2026-09-27 audit, round 41, A41-4).
	return len(data) - binaryOverflow(decoded) - jsonEscapeOverhead(data)
}

// jsonEscapeOverhead counts the bytes json.Marshal spends writing a character
// as an escape sequence: one fewer than the escape occupies for the
// two-character forms, and as many as the rune it stands for for a `\uXXXX`.
// This is the gateway's function and cmd/launch's (which share the wording) —
// the three prompt-size measures of this product have to count the same bytes.
// The scan follows backslashes the way a JSON reader does, so text holding the
// literal characters `\n` is not miscounted.
func jsonEscapeOverhead(b []byte) int {
	extra := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			continue
		}
		switch c := b[i+1]; c {
		case '\\', '"', '/', 'b', 'f', 'n', 'r', 't':
			extra++
			i++
		case 'u':
			if i+5 < len(b) && isHex4(b[i+2:i+6]) {
				extra += 6 - escapedRuneLen(b[i+2:i+6])
				i += 5
			}
		}
	}
	return extra
}

// escapedRuneLen is the UTF-8 length of what a `\uXXXX` escape decodes to. The
// escapes json.Marshal writes stand for characters a JSON writer must escape —
// U+0000 to U+001F, U+2028 and U+2029 — and the last two are three bytes
// decoded; crediting every `\u` escape the single byte of a control escape
// measures a prompt made of line separators at a third of its size. A lone
// surrogate decodes to U+FFFD, three bytes.
func escapedRuneLen(p []byte) int {
	var v rune
	for _, c := range p {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			v |= rune(c-'a') + 10
		default:
			v |= rune(c-'A') + 10
		}
	}
	if v >= 0xD800 && v <= 0xDFFF {
		return 3
	}
	if n := utf8.RuneLen(v); n > 0 {
		return n
	}
	return 3
}

// isHex4 reports whether a four-character `\u` payload is hex — every escape of
// that shape is a character json.Marshal escaped, whatever it is.
func isHex4(p []byte) bool {
	if len(p) != 4 {
		return false
	}
	for _, c := range p {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// binaryOverflow is the length by which serializing v overstates the prompt:
// the base64 beyond the allowance, for every image or non-text document source
// anywhere inside it. A text document's data is left alone — the converter
// pastes that into the prompt, so it is charged in full.
func binaryOverflow(v any) int {
	switch t := v.(type) {
	case map[string]any:
		total := 0
		if typ, _ := t["type"].(string); typ == "image" || typ == "document" {
			if src, ok := t["source"].(map[string]any); ok {
				if st, _ := src["type"].(string); st != "text" {
					if d, ok := src["data"].(string); ok && len(d) > inlineImageByteAllowance {
						total += len(d) - inlineImageByteAllowance
					}
				}
			}
		}
		for _, x := range t {
			total += binaryOverflow(x)
		}
		return total
	case []any:
		total := 0
		for _, x := range t {
			total += binaryOverflow(x)
		}
		return total
	}
	return 0
}

func countContentBlock(block ContentBlock) int {
	total := 0
	if block.Text != nil {
		total += len(*block.Text)
	}
	// A thinking block is charged NOTHING, and a redacted one never was. A
	// 700 KB thinking blob — with a 400 KB redacted one beside it — took the
	// estimate from 5 502 tokens to 180 502 while the converted prompt was
	// byte-for-byte the same 22 084 bytes: the session's meter and its
	// auto-compaction read a prompt 32× its real size (2026-09-27 audit, round
	// 39, C-F13).
	//
	// Whether the blob reaches a prefill is the MODEL's template deciding: the
	// estimate has one consumer, middleware/anthropic.go's handler for the local
	// leg, and that leg renders api.Message.Thinking only if the template names
	// `.Thinking` — which none of the templates this repo bundles does, so for
	// every model the product ships, charging the block would be wrong by the
	// blob's size in the direction that breaks sessions (round-40 auditor A40-7
	// argued the block is rendered on that leg and read the round-39 wording as
	// claiming otherwise for it — the wording was about the OpenAI wire, and it
	// is corrected here rather than acted on: a third-party template that does
	// render `.Thinking` is out of scope for this fallback, and the upstream's
	// own count corrects the estimate whenever it states one).
	switch block.Type {
	case "tool_use":
		// A tool CALL is carried as JSON arguments, so its own JSON is the
		// prompt and the escapes in it are transport.
		total += clampedJSONBytes(block)
	case "tool_result":
		// A tool RESULT is CONVERTED, not pasted (convertToolResultContent), so
		// it is charged through that reader instead. The two arms shared one
		// case until the reader below existed: the same treatment billed a
		// nested attachment's base64 against a wire that holds a notice for it,
		// and billed a nested image nothing at all where the converter carries
		// it (2026-09-27 audit, round 42, A42-2/C42-1).
		total += toolResultBytes(map[string]any{
			"type":        block.Type,
			"tool_use_id": block.ToolUseID,
			"content":     block.Content,
		})
	case "document":
		// A text source's data is written into the prompt by the converter and
		// was charged nothing here, so a turn whose newest message was a large
		// attachment was estimated at a handful of tokens: the estimate is the
		// fallback middleware/anthropic.go seeds the stream converter's
		// input_tokens with when the upstream states no usage, so the session's
		// context meter never grew and its auto-compaction never fired
		// (2026-09-27 audit, round 36, A-F3).
		if block.Source != nil && block.Source.Type == "text" {
			// Only a TEXT source is written into the prompt (convertMessage's
			// document arm); any other source.type fails the conversion
			// outright, so that body never reaches a model to have a prompt at
			// all. Charging data PLUS ref billed the transport encoding of a
			// PDF that is not carried, and charged a `ref` the converter never
			// reads even on the text path it does carry (2026-09-27 audit,
			// round 41, A41-2).
			total += len(block.Source.Data)
		}
	case "search_result":
		// Same omission, same consequence: the passages and the label above
		// them are prompt content the converter now forwards. They are read by
		// searchResultText, so they are charged through that reader's rule
		// (chargeSearchResult) rather than through the message-level one — a
		// picture among the passages is described there, not carried.
		total += len(block.Title) + countItemsIn(block.Content, chargeSearchResult)
		if block.Source != nil {
			// Both spellings of a source are charged, because the converter
			// writes whichever one the body carried (A-F6): a URL stated in the
			// object form reached the prompt and was never counted, so the
			// estimate lagged the prompt it is supposed to seed.
			total += len(block.Source.Ref) + len(block.Source.URL)
		}
	case "image":
		// An image the converter carries is content the session pays for, and
		// the estimate charges it the same way the product's other two
		// prompt-size measures do: the inline allowance, not the base64. See
		// imageBlockBytes (2026-09-27 audit, round 39, A-F3).
		if block.Source != nil {
			total += imageBlockBytes(map[string]any{
				"data": block.Source.Data,
				"url":  block.Source.URL,
				"ref":  block.Source.Ref,
			})
		}
	case "server_tool_use":
		// The same omission, one type over: a server tool call reaches the
		// prompt as a tool call — id, name and arguments — which is this
		// block's own JSON within a couple of dozen bytes, and it was charged
		// nothing. Charged exactly as the tool_use arm charges its twin, and
		// for the same reason: the escapes in that JSON are transport the
		// receiver decodes, so serializing raw billed a call of 2 000 quotes
		// twice what the identical tool_use prompt cost (2026-09-27 audit,
		// round 42, A42-5).
		total += clampedJSONBytes(block)
	case "web_search_tool_result":
		// The same omission, and then the opposite error: the converter writes
		// the HITS, one line each, and nothing else — while serializing the
		// block pasted its encrypted_content back in. A search result carrying
		// a few hundred kilobytes of opaque blob was billed in full against a
		// wire holding a title, a URL and a newline per hit, and the estimate
		// seeds the client-visible input_tokens, so the session's context meter
		// and auto-compaction read a prompt the model was never sent
		// (2026-09-27 audit, round 41, A41-3). Charge what the converter
		// writes, through the very function that writes it.
		total += len(formatWebSearchToolResultContent(block.Content))
	}
	return total
}

// OllamaWebSearchRequest represents a request to the Ollama web search API
type OllamaWebSearchRequest struct {
	Query      string `json:"query"`
	MaxResults int    `json:"max_results,omitempty"`
}

// OllamaWebSearchResult represents a single search result from Ollama API
type OllamaWebSearchResult struct {
	Title   string `json:"title"`
	URL     string `json:"url"`
	Content string `json:"content"`
}

// OllamaWebSearchResponse represents the response from the Ollama web search API
type OllamaWebSearchResponse struct {
	Results []OllamaWebSearchResult `json:"results"`
}

var WebSearchEndpoint = "https://ollama.com/api/web_search"

func WebSearch(ctx context.Context, query string, maxResults int) (*OllamaWebSearchResponse, error) {
	if internalcloud.Disabled() {
		logutil.TraceContext(ctx, "anthropic: web search blocked", "reason", "cloud_disabled")
		return nil, errors.New(internalcloud.DisabledError("web search is unavailable"))
	}

	if maxResults <= 0 {
		maxResults = 5
	}
	if maxResults > 10 {
		maxResults = 10
	}

	reqBody := OllamaWebSearchRequest{
		Query:      query,
		MaxResults: maxResults,
	}

	body, err := json.Marshal(reqBody)
	if err != nil {
		return nil, fmt.Errorf("failed to marshal web search request: %w", err)
	}

	searchURL, err := url.Parse(WebSearchEndpoint)
	if err != nil {
		return nil, fmt.Errorf("failed to parse web search URL: %w", err)
	}
	logutil.TraceContext(ctx, "anthropic: web search request",
		"query", TraceTruncateString(query),
		"max_results", maxResults,
		"url", searchURL.String(),
	)

	q := searchURL.Query()
	q.Set("ts", strconv.FormatInt(time.Now().Unix(), 10))
	searchURL.RawQuery = q.Encode()

	signature := ""
	if strings.EqualFold(searchURL.Hostname(), "ollama.com") {
		challenge := fmt.Sprintf("%s,%s", http.MethodPost, searchURL.RequestURI())
		signature, err = auth.Sign(ctx, []byte(challenge))
		if err != nil {
			return nil, fmt.Errorf("failed to sign web search request: %w", err)
		}
	}
	logutil.TraceContext(ctx, "anthropic: web search auth", "signed", signature != "")

	req, err := http.NewRequestWithContext(ctx, "POST", searchURL.String(), bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("failed to create web search request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")
	if signature != "" {
		req.Header.Set("Authorization", fmt.Sprintf("Bearer %s", signature))
	}

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("web search request failed: %w", err)
	}
	defer resp.Body.Close()
	logutil.TraceContext(ctx, "anthropic: web search response", "status", resp.StatusCode)

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("web search returned status %d: %s", resp.StatusCode, string(respBody))
	}

	var searchResp OllamaWebSearchResponse
	if err := json.NewDecoder(resp.Body).Decode(&searchResp); err != nil {
		return nil, fmt.Errorf("failed to decode web search response: %w", err)
	}
	logutil.TraceContext(ctx, "anthropic: web search results", "count", len(searchResp.Results))

	return &searchResp, nil
}

func ConvertOllamaToAnthropicResults(ollamaResults *OllamaWebSearchResponse) []WebSearchResult {
	var results []WebSearchResult
	for _, r := range ollamaResults.Results {
		results = append(results, WebSearchResult{
			Type:  "web_search_result",
			URL:   r.URL,
			Title: r.Title,
		})
	}
	return results
}
