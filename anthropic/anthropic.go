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
	StopSequences StopSequences   `json:"stop_sequences,omitempty"`
	Tools         []Tool          `json:"tools,omitempty"`
	ToolChoice    *ToolChoice     `json:"tool_choice,omitempty"`
	Thinking      *ThinkingConfig `json:"thinking,omitempty"`
	Metadata      *Metadata       `json:"metadata,omitempty"`
	OutputConfig  *OutputConfig   `json:"output_config,omitempty"`
}

type OutputConfig struct {
	Effort string `json:"effort,omitempty"`
}

// StopSequences is the client's list of strings that end a turn. It is a named
// type for one reason: the ELEMENTS have to be checked.
//
// A plain []string cannot distinguish `["STOP"]` from `[null]`. Go's decoder
// treats JSON null into a string as a no-op that reports no error, so `[null]`
// decoded to exactly the same slice as `[""]` — and an empty stop string is not
// a harmless nothing, it is a stop sequence that matches at every position. The
// metered gateway leg refuses such a body outright
// (tools/gateway/messages.go, round 49), so one client body was a 400 through
// the meter and a served turn here, with the backend handed a stop list that
// could truncate the reply to nothing (2026-09-27 audit, round 50, C50-4).
//
// The whole field being null is NOT refused: that is a client stating nothing,
// which the gateway also accepts, and the omitempty decode leaves the slice nil.
type StopSequences []string

func (s *StopSequences) UnmarshalJSON(data []byte) error {
	if bytes.Equal(bytes.TrimSpace(data), []byte("null")) {
		return nil
	}
	var raw []json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return fmt.Errorf("stop_sequences must be an array of strings")
	}
	out := make(StopSequences, 0, len(raw))
	for _, el := range raw {
		// The element must BE a JSON string, checked on the bytes: unmarshalling
		// null into a string is the no-op documented above, and so is any
		// element the decoder can leave alone.
		el = bytes.TrimSpace(el)
		if len(el) == 0 || el[0] != '"' {
			return fmt.Errorf("stop_sequences must be an array of strings")
		}
		var str string
		if err := json.Unmarshal(el, &str); err != nil {
			return fmt.Errorf("stop_sequences must be an array of strings")
		}
		out = append(out, str)
	}
	*s = out
	return nil
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
	// The object form carries `ref` too: the converter's own search_result arm
	// reads a `source` object's `ref` and then its `url` (round 37, A-F6 — the
	// gateway leg reads the same two keys in that order, round 39, C-F8), but
	// Ref is `json:"-"` so the object spelling never populated it and the
	// converter's fallback found nothing to fall back FROM: a body of
	// {"ref":"http://ref"} reached the model as its title and passages with the
	// reference gone, where the gateway wrote it — the two legs disagreeing
	// about the prompt bytes of one client body (2026-09-28 audit, round 55).
	var a struct {
		alias
		Ref string `json:"ref"`
	}
	if err := json.Unmarshal(b, &a); err != nil {
		return err
	}
	*s = ImageSource(a.alias)
	s.Ref = a.Ref
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

// UnmarshalJSON refuses a tool stated as JSON null.
//
// Go's decoder reads `null` into a struct as a no-op that reports no error, so
// `tools:[null]` decoded to exactly the same value as `tools:[{}]` — a tool
// with no name — and the turn was served with a function the client never
// defined. The metered gateway leg refuses that element in words ("tools
// element is not an object"), so one client body was a 200 here and a 400
// there (2026-09-27 audit, round 51). This is the same rule the stop-sequence
// list already applies to its elements: an element of an array the client
// wrote is a value, and `null` is not one of the shapes this wire defines.
func (t *Tool) UnmarshalJSON(data []byte) error {
	if strings.TrimSpace(string(data)) == "null" {
		return errors.New("tools element is not an object")
	}
	type plain Tool
	var p plain
	if err := json.Unmarshal(data, &p); err != nil {
		return err
	}
	*t = Tool(p)
	return nil
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
						// An empty text block contributes no text, so it earns no
						// separator either: writing the separator before knowing
						// whether the block has anything to say left a trailing
						// blank line on the system prompt of every array whose
						// last block was empty, where the gateway leg (and this
						// file's own normalizeSystemFirst, which skips empty
						// parts) drops the block entirely (2026-09-27 audit,
						// round 49, B-F5).
						if text, ok := blockMap["text"].(string); ok && text != "" {
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
		if len(converted) == 0 {
			// A turn this leg cannot represent — an Anthropic message whose
			// content array is empty, or an assistant turn holding nothing but
			// a replayed redacted_thinking block — used to be DELETED, which
			// silently rewrote the conversation the client sent: a contentless
			// assistant turn between two user turns vanished and left two user
			// turns in a row, and a leading one left a prompt whose first role
			// the client never chose. The gateway leg keeps the turn, empty of
			// content and of its own role, and one body must not become two
			// different prompts depending on which leg served it. The turn
			// carries nothing; that its role was stated is still part of the
			// conversation (2026-09-27 audit, round 44, C44-2).
			converted = []api.Message{{Role: strings.ToLower(msg.Role)}}
		}
		messages = append(messages, converted...)
	}

	messages = normalizeSystemFirst(messages)

	if !anyMessageCarriesContent(messages) {
		// Nothing in this conversation asks the model for anything: every turn
		// either converted to nothing or was a blank system message. The body
		// used to reach the wire as "messages":null, and the server answers a
		// turn-less body with a synthetic 200 (server/routes.go: no messages,
		// no generation) while the client is told a successful turn and charged
		// the middleware's estimate for a prompt no model ever read
		// (2026-09-27 audit, round 43, C43-1).
		//
		// The question has to be asked of CONTENT, and asked of the list the
		// wire receives rather than of the list that enters the hoist above.
		// Round 43 asked it of the length of the list before normalization, and
		// a blank system message is exactly what that step drops: a body whose
		// surviving turn was a whitespace-only system message arrived here as
		// [system "   "] — non-empty, so the guard did not fire — and then left
		// the rewrite as an empty rest, which marshals as "messages":[] and
		// takes the same synthetic-200 path as null (2026-09-27 audit, round
		// 44, A44-1). A conversation of blank system messages is empty whatever
		// its length, so this is a question about the messages, not about how
		// many of them there are.
		messages = []api.Message{{Role: "user"}}
	}

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
		// Converted to a plain []string on purpose: every reader of this option
		// type-switches on []string, and the named type would fall through every
		// arm of those switches and lose the stop list silently.
		options["stop"] = []string(r.StopSequences)
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
//
// That check is over the WHOLE conversation, which is why the scan below has no
// early exit. It used to stop at the first system message that arrived after a
// non-system one — the rewrite was already decided by then — so a conversation
// whose later system message carried an image never reached the guard: the
// scan ended first, and the rewrite merged both messages into one bare string
// and dropped the image on the floor. The estimate still charged it its 4096
// bytes, so the client was billed for an image the model never saw
// (2026-09-27 audit, round 43, A43-1).
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

// anyMessageCarriesContent reports whether any converted message asks the model
// for anything at all — text, an image, a call, a tool result, or a replayed
// thought. It is asked of the conversation AFTER the system hoist, which can
// drop messages: a turn that survives this check cannot be the one the hoist
// removes, because the hoist only ever removes blank system text.
//
// The five fields it reads are all of api.Message's payload: the same five
// systemMessageIsTextOnly walks, for the mirror-image question.
//
// A text is content whatever it says. Whitespace is not nothing on this wire:
// a prompt of a hundred thousand newlines is a prompt the model can be asked,
// it is what the client sent and what the estimate charges, and treating it as
// empty threw the whole turn away — the body collapsed to one bare user turn
// with no text in it, so the upstream was asked a different question from the
// one the client wrote, and the calibration unit measured 66 bytes for a
// hundred-kilobyte prompt (2026-09-27 audit, round 44, the round-36 estimator
// test caught it).
func anyMessageCarriesContent(messages []api.Message) bool {
	for _, m := range messages {
		if m.Content != "" || len(m.Images) > 0 || len(m.ToolCalls) > 0 ||
			m.ToolCallID != "" || m.ToolName != "" || m.Thinking != "" {
			return true
		}
	}
	return false
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

	// The turn's own blocks — text, documents, images, calls, replayed
	// reasoning — are written back in the order the client wrote them, split
	// into runs by the tool results between them. One aggregated own message,
	// placed before or after ALL the results by a single flag, hoisted the
	// client's text past the results it followed: [text, result, text] reached
	// the model as [text, result] with the second text moved into the first, and
	// [result, text, result] deferred the text past both — so one client body
	// was two different conversations, and the metered gateway leg writes the
	// same blocks in the order they arrived (tools/gateway/messages.go, the
	// text flushed before each tool message) (2026-09-27 audit, round 52).
	type ownRun struct {
		text      strings.Builder
		images    []api.ImageData
		toolCalls []api.ToolCall
		thinking  string
	}
	var parts []any // *ownRun, or an api.Message for a tool result
	var cur *ownRun
	own := func() *ownRun {
		if cur == nil {
			cur = &ownRun{}
			parts = append(parts, cur)
		}
		return cur
	}
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
				r := own()
				if r.text.Len() > 0 {
					r.text.WriteString("\n\n")
				}
				r.text.WriteString(*block.Text)
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
			r := own()
			r.images = append(r.images, decoded)

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
			r := own()
			r.toolCalls = append(r.toolCalls, api.ToolCall{
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

			parts = append(parts, api.Message{
				Role:       "tool",
				Content:    resultContent,
				Images:     resultImages,
				ToolCallID: block.ToolUseID,
			})
			// The next own block, if the turn has one, is a run of its own:
			// it came after this result and is written after it.
			cur = nil

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
				r := own()
				if r.thinking != "" {
					r.thinking += "\n\n"
				}
				r.thinking += *block.Thinking
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
				r := own()
				if r.text.Len() > 0 {
					r.text.WriteString("\n\n")
				}
				r.text.WriteString(block.Source.Data)
				if !strings.HasSuffix(block.Source.Data, "\n") {
					r.text.WriteString("\n")
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
			r := own()
			r.toolCalls = append(r.toolCalls, api.ToolCall{
				ID: block.ID,
				Function: api.ToolCallFunction{
					Name:      block.Name,
					Arguments: block.Input,
				},
			})

		case "web_search_tool_result":
			webSearchToolResultBlocks++
			parts = append(parts, api.Message{
				Role:       "tool",
				Content:    formatWebSearchToolResultContent(block.Content),
				ToolCallID: block.ToolUseID,
			})
			cur = nil

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
				r := own()
				if r.text.Len() > 0 {
					r.text.WriteString("\n\n")
				}
				r.text.WriteString(sb.String())
				if !strings.HasSuffix(sb.String(), "\n") {
					r.text.WriteString("\n")
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

	// The parts are written back in the order the client wrote them: each run
	// of the turn's own blocks where it stood, each tool result where it stood
	// (2026-09-27 audit, round 52).
	for _, part := range parts {
		switch p := part.(type) {
		case *ownRun:
			if p.text.Len() == 0 && len(p.images) == 0 && len(p.toolCalls) == 0 && p.thinking == "" {
				continue // a run that states nothing is not a message
			}
			messages = append(messages, api.Message{
				Role:      role,
				Content:   p.text.String(),
				Images:    p.images,
				ToolCalls: p.toolCalls,
				Thinking:  p.thinking,
			})
		case api.Message:
			messages = append(messages, p)
		}
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

	toolBlocks := 0
	seenInCall := map[string]int{}
	seenStatedID := map[string]string{}
	usedIDs := map[string]bool{}
	sentKey := map[string]bool{}
	for _, tc := range r.Message.ToolCalls {
		if tc.ID != "" {
			usedIDs[tc.ID] = true
		}
	}
	for _, tc := range r.Message.ToolCalls {
		if strings.TrimSpace(tc.Function.Name) == "" {
			// A call the upstream never named is not a call the client can
			// make: the block carries the name, and a tool_use whose name is
			// empty is one Claude Code reports as pending and can never run.
			// The streaming converter holds such a fragment, the proxy's
			// streaming path relays its arguments as text, and its
			// non-streaming path drops it, so this path answering with a
			// nameless block was the one site that told the client to expect a
			// call nobody could dispatch (2026-09-27 audit, round 45, A45-1).
			continue
		}
		// One body, one answer: this loop now carries the streaming
		// converter's own key/identity rule, so a list answers the same
		// whether or not the client asked for `stream`. It was applied to the
		// streaming twin in round 45 and not here, which left this path the
		// only one of the four translation sites that could emit two tool_use
		// blocks under one id (2026-09-27 audit, round 46, A46-1).
		// The identity a minted id is derived from is the arguments as the
		// UPSTREAM stated them, not the shape this leg WRITES: the JSON literal
		// null and the empty object are two different argument texts, and every
		// other site that mints an id for the same call hashes the text it was
		// given — the client proxy's whole-list parser marshals the map it
		// unmarshalled into (a null text marshals back to `null`, an empty one
		// to `{}`) and the gateway's canonicalCallArgs returns the literal for
		// exactly this reason (round 49). Folding the two here minted `{}`'s id
		// for a call the other two legs number from `null`: one upstream turn,
		// one id-less call, and the client got two different ids depending on
		// whether it had asked for `stream` — a retry through another leg, or a
		// backend that switches to a buffered response, then answers the next
		// turn's `tool_result` with an id no `tool_use` carried (2026-09-28
		// audit, round 66, F66-L2-1).
		argsJSON, _ := json.Marshal(tc.Function.Arguments)
		base := "\x00" + tc.Function.Name + "\x00" + string(argsJSON)

		id := tc.ID
		// dedupKey namespaces a STATED id apart from a MINTED one. A stated id
		// is the upstream's own correlation key, so a restatement of the same
		// call under it is one call; a minted id is this leg's own synthesis
		// (ToolCallIDFor), so a later call that merely STATES that same string
		// is a different call — deduping the two together dropped a call the
		// upstream had named while the client proxy's parser now keeps both
		// (2026-09-27 audit, round 46, A46-4).
		key := "\x01" + tc.ID
		if tc.ID != "" {
			// An id the upstream states is that call's own — unless it already
			// stated the same id for a DIFFERENT call. One id reused for two
			// calls is the wire the client leg's startsANewToolCall splits and
			// the gateway leg mints a fresh id for; treating the repeat as a
			// duplicate threw the second call away, so the agent ran one of the
			// model's two calls and was told the turn was a tool call. A
			// restatement of the SAME call under the same id is still a
			// duplicate, and the key below still drops it (2026-09-27 audit,
			// round 45, A45-3).
			if owner, ok := seenStatedID[key]; ok && owner != base {
				key, id = "", ""
			} else {
				seenStatedID[key] = base
			}
		}
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
			//
			// The synthesized id is a function of the call's name and
			// arguments, so a list that names the same id-less call twice
			// produced two blocks answering to ONE id — the shape the streaming
			// converter's comment calls out ("two tool_use blocks with one id
			// cannot be answered separately"): the client runs one call, sends
			// one tool_result, and the second block can never be satisfied.
			// The second and later occurrences take the streaming path's own
			// rule, the same key and the same "#n" suffix (2026-09-27 audit,
			// round 45, A45-2).
			n := seenInCall[base]
			seenInCall[base] = n + 1
			key = "\x00" + base
			id = ToolCallIDFor(tc.Function.Name, string(argsJSON))
			if n > 0 {
				key = "\x00" + base + "\x00#" + strconv.Itoa(n)
				id = ToolCallIDFor(tc.Function.Name, string(argsJSON)+"#"+strconv.Itoa(n))
			}
			// The synthesized id must not land on an id another call in this
			// same list STATES: the two calls would then reach this loop under
			// one id and read as one call restated, so the call the upstream
			// named was dropped (2026-09-27 audit, round 46, A46-4). Bumped
			// onto the same "#n" rule the repeats use.
			mintedKey := string(argsJSON)
			if n > 0 {
				mintedKey = string(argsJSON) + "#" + strconv.Itoa(n)
			}
			for k := 0; usedIDs[id]; k++ {
				id = ToolCallIDFor(tc.Function.Name, mintedKey+"\x00#"+strconv.Itoa(k))
			}
			usedIDs[id] = true
		}
		if sentKey[key] {
			continue
		}
		sentKey[key] = true
		// `input` is a required field of a tool_use block, and the zero value
		// of this type marshals to NO key at all (json:",omitzero" with an
		// unallocated map): a call with no arguments reached the client as a
		// block it could only read as a nil argument map, while the streaming
		// converter and the proxy both emit "input":{} for the same turn, so
		// the client's view of one call depended on `stream` (2026-09-27
		// audit, round 45, A45-4).
		input := tc.Function.Arguments
		if input.Len() == 0 {
			input = api.NewToolCallFunctionArguments()
		}
		toolBlocks++
		content = append(content, ContentBlock{
			Type:  "tool_use",
			ID:    id,
			Name:  tc.Function.Name,
			Input: input,
		})
	}

	// The blocks this turn actually carries decide its stop_reason, not the
	// upstream's stated call list: a turn whose only calls were nameless has no
	// block for the client to run, and reporting tool_use there told it to wait
	// for a call it will never receive (round 39's rule on the other two legs).
	stopReason := mapStopReason(r.DoneReason, toolBlocks > 0)

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
	// statedIDOwner records, per upstream-stated id, the call that id was
	// first stated for. The OpenAI wire keys a tool_result by tool_call_id, so
	// an upstream that reuses one id for the turn's second call states two
	// calls under one key — and deduping on the id alone delivered one
	// tool_use where the model asked for two, silently, on a turn that still
	// reported tool_use (2026-09-27 audit, round 45, A45-3).
	statedIDOwner map[string]string
	// mintedIDs holds every id this converter has STATED or minted, so a
	// synthesized id cannot land on one the upstream stated for another call
	// in the same turn (2026-09-27 audit, round 46, A46-4).
	mintedIDs map[string]bool
}

func NewStreamConverter(id, model string, estimatedInputTokens int) *StreamConverter {
	return &StreamConverter{
		ID:                   id,
		Model:                model,
		firstWrite:           true,
		estimatedInputTokens: estimatedInputTokens,
		toolCallsSent:        make(map[string]bool),
		statedIDOwner:        make(map[string]string),
		mintedIDs:            make(map[string]bool),
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
		if tc.ID != "" {
			c.mintedIDs[tc.ID] = true
		}
	}
	for _, tc := range r.Message.ToolCalls {
		// argsJSON is the argument text this leg DELIVERS — the fold of round 65,
		// so a null spelling reaches the client as the object its
		// content_block_start already announced. The id is minted from the text
		// the UPSTREAM stated instead (argsSeed): the two spellings are two
		// different texts, every other mint site hashes the one it was given,
		// and deriving the id from the folded shape left this arm and the
		// client proxy's whole-list arm answering one id-less call with two
		// different ids (2026-09-28 audit, round 66, F66-L2-1).
		argsJSON, err := json.Marshal(callArgumentsStated(tc.Function.Arguments))
		if err != nil {
			slog.Error("failed to marshal tool arguments", "error", err, "tool_id", tc.ID)
			continue
		}
		argsSeed, _ := json.Marshal(tc.Function.Arguments)
		if strings.TrimSpace(tc.Function.Name) == "" {
			// A call the upstream never named cannot be dispatched: this block
			// carries the name and there is no second event that does, so the
			// client was handed a tool_use it can only report as pending
			// forever. The non-stream converter and the proxy both drop such a
			// call on the paths that never accumulate, and the proxy's
			// streaming path relays its arguments as text — this converter was
			// the site that emitted the nameless block (2026-09-27 audit,
			// round 45, A45-1).
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
		base := "\x00" + tc.Function.Name + "\x00" + string(argsSeed)
		// As in ToMessagesResponse: a STATED id is the upstream's correlation
		// key, a MINTED one is this converter's own synthesis, and only the
		// former may be deduped on its own string — a call that merely states
		// an id this converter minted for an earlier id-less call is a
		// different call (2026-09-27 audit, round 46, A46-4).
		key := "\x01" + tc.ID
		id := tc.ID
		if tc.ID != "" {
			// An id the upstream states is that call's own — unless it already
			// stated the same id for a DIFFERENT call. One id reused for two
			// calls is the wire the client leg's startsANewToolCall splits (and
			// the gateway leg mints a fresh id for), and treating the repeat as
			// a duplicate here threw the second call away after the split had
			// recovered it: the agent ran one of the model's two calls and was
			// told the turn was a tool call (2026-09-27 audit, round 45,
			// A45-3). The reused id falls through to the identity-minting
			// branch below, which is keyed on the call itself and never on a
			// counter. A restatement of the SAME call under the same id is
			// still a duplicate, and toolCallsSent below still drops it.
			if owner, ok := c.statedIDOwner[key]; ok && owner != base {
				key, id = "", ""
			} else {
				c.statedIDOwner[key] = base
			}
		}
		if id == "" {
			n := seenInCall[base]
			seenInCall[base] = n + 1
			key = "\x00" + base
			id = ToolCallIDFor(tc.Function.Name, string(argsSeed))
			if n > 0 {
				key = "\x00" + base + "\x00#" + strconv.Itoa(n)
				id = ToolCallIDFor(tc.Function.Name, string(argsSeed)+"#"+strconv.Itoa(n))
			}
			// A synthesized id must not land on an id another call in this
			// same turn STATES: the two would reach the client under one id,
			// and the non-stream twin of this body reads such a pair as one
			// call restated (2026-09-27 audit, round 46, A46-4).
			mintedKey := string(argsSeed)
			if n > 0 {
				mintedKey = string(argsSeed) + "#" + strconv.Itoa(n)
			}
			for k := 0; c.mintedIDs[id]; k++ {
				id = ToolCallIDFor(tc.Function.Name, mintedKey+"\x00#"+strconv.Itoa(k))
			}
			c.mintedIDs[id] = true
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

// imageDataURLBytes decodes the `data:` URL forms this product's OpenAI door
// carries into raw image bytes — data:;base64,… and
// data:image/{jpeg,jpg,png,webp};base64,… — the same four media types and the
// same blank-type spelling openai.decodeImageURL accepts, so a URL one leg
// carries is not an address another leg refuses.
//
// handled is false for a string that is not one of those forms: the caller
// keeps its own reading of it (a fetchable address, or text that is not
// url-shaped at all). handled is true with an error for one of those forms that
// does not decode — a payload that is not base64, and a payload that decodes to
// a URL rather than to image bytes, which the door refuses in words too
// (openai.decodeImageURL, 2026-09-27 audit, round 52).
func imageDataURLBytes(url string) (decoded api.ImageData, handled bool, err error) {
	payload, isData := "", false
	if rest, ok := strings.CutPrefix(url, "data:;base64,"); ok {
		payload, isData = rest, true
	} else {
		for _, mediaType := range []string{"jpeg", "jpg", "png", "webp"} {
			if rest, ok := strings.CutPrefix(url, "data:image/"+mediaType+";base64,"); ok {
				payload, isData = rest, true
				break
			}
		}
	}
	if !isData {
		return nil, false, nil
	}
	img, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return nil, true, errors.New("data URL payload is not base64")
	}
	if isURLText(string(img)) {
		return nil, true, errors.New("data URL decodes to a URL, not image bytes")
	}
	return img, true, nil
}

func resolveImageSource(source *ImageSource) (api.ImageData, error) {
	switch source.Type {
	case "url":
		if source.URL == "" {
			return nil, errors.New(`invalid image source type: url, with no url`)
		}
		if decoded, isData, err := imageDataURLBytes(source.URL); isData {
			// A `data:` URL is not an address to fetch: it IS the image, and
			// the OpenAI door both other legs hand it to carries exactly these
			// forms (openai.decodeImageURL). Left as url TEXT it was refused by
			// the caller's url-source check — a 400 for a body the metered
			// gateway leg serves 200 and the door decodes — while this leg's own
			// model, given image bytes, was never asked to fetch anything
			// (2026-09-27 audit, round 52).
			if err != nil {
				return nil, fmt.Errorf("invalid image source: %w", err)
			}
			return decoded, nil
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

// searchResultPromptText is the string convertMessage writes for one
// search_result block — its title, the reference it came from and its passages,
// each of the first two followed by a newline, and the whole followed by one
// when it does not already end in one — and the empty string for a block that
// states none of the three. The converter writes NOTHING for that block, which
// is why both the estimator's text-only question and its separator/chunk walk
// ask it here instead of rebuilding the arm: a search_result carrying no title,
// no source and no passage is a block a system message loses nothing by having,
// and the two readings disagreed — the rewrite that DELETES a blank system
// message was suppressed while the walk charged its 6-byte role, two tokens for
// a message the model never read (2026-09-28 audit, round 59, F59-L1-2).
func searchResultPromptText(title, ref string, content any) string {
	var sb strings.Builder
	if title != "" {
		sb.WriteString(title)
		sb.WriteString("\n")
	}
	if ref != "" {
		sb.WriteString(ref)
		sb.WriteString("\n")
	}
	sb.WriteString(searchResultText(content))
	if sb.Len() == 0 {
		return ""
	}
	if !strings.HasSuffix(sb.String(), "\n") {
		sb.WriteString("\n")
	}
	return sb.String()
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
					// An empty text block is no text, and writing it — with the
					// separator this join puts in front of it — left a blank
					// line the gateway leg's toolResultText join does not write:
					// ["one",""] reached the model as "one\n" here and "one"
					// there, and a tool result that ends in a blank line reads
					// as an unfinished answer (2026-09-27 audit, round 53).
					// The sibling's rule is the one this arm follows: only a
					// string that carries bytes becomes a part (tools/gateway/
					// messages.go, the text case of its tool_result join).
					if t == "" {
						continue
					}
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

	// Count the conversation the CONVERTER writes: the system prompt and the
	// message list after normalizeSystemFirst's rewrite, not the ones the body
	// carried (see conversationBytes).
	totalLen += conversationBytes(req.Messages, req.System)

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
		// Charged as the CONVERTED tool, in the same unit as everything else
		// this function measures (clampedJSONBytes: the bytes of the object that
		// is sent, with the transport's escapes subtracted). Charging the
		// client's own bytes made the measure depend on the client's JSON
		// FORMATTING: the same tool put byte-identical parameters on the wire
		// whether it was written compactly or pretty-printed, and the estimate
		// read 35 against 41 tokens for it — the number that seeds the
		// client-visible input_tokens, and that a session's auto-compaction is
		// sized on, moved 17% on whitespace the model was never shown
		// (2026-09-27 audit, round 52).
		converted, _, err := convertTool(tool)
		if err != nil {
			// The converter refuses the whole request in words on this error,
			// so nothing is sent and there is nothing to charge.
			continue
		}
		totalLen += clampedJSONBytes(converted)
	}

	// Return len/4 as rough token estimate, minimum 1 if there's any content
	tokens := totalLen / 4
	if tokens == 0 && (len(req.Messages) > 0 || req.System != nil) {
		tokens = 1
	}
	return tokens
}

// conversationBytes charges the conversation the converter WRITES: the system
// prompt and the message list that reach the backend, after
// normalizeSystemFirst has hoisted, merged and dropped the system text.
//
// That rewrite fires for a conversation whose system text does not already lead
// it — a system message after a non-system one, which is the shape a client
// sends when it puts a per-session system prompt beside the top-level one — and
// for one holding a whitespace-only system message, which the rewrite deletes.
// It writes the surviving system texts as ONE message joined with a blank line.
// Charging the list as the client sent it billed a blank system message the
// rewrite deletes three tokens against one for the same prompt written without
// it, and billed a top-level system beside a later one as two messages with two
// roles while the model read one joined message — four tokens against five for
// the prompt those two bodies both produce. This estimate seeds the
// client-visible input_tokens whenever the upstream states no usage, so the
// difference is the client's context-window arithmetic: auto-compaction is
// sized on it (2026-09-28 audit, round 56, F56-1).
//
// A conversation the rewrite leaves alone — an already-ordered one with no blank
// system message — is charged exactly as it arrives, byte for byte. A system
// message carrying anything but text (an image, a call) cannot be merged into
// that one string without dropping what it carries, and normalizeSystemFirst
// returns such a conversation untouched for the same reason; it is charged as
// it arrives here too.
func conversationBytes(messages []MessageParam, system any) int {
	type entry struct {
		system bool
		text   string // the text the converter writes, for a system entry
		bytes  int    // the charge for this entry as it stands
		// sep and joinedSep are the two readings of the blank lines this turn's
		// content carries (messageShapeBytes): the converter's per-run count the
		// charge above uses, and the count the JOINED text has, which is the one
		// len(text) carries. The joined branch needs both, because it subtracts a
		// text that was measured the second way from a charge taken the first
		// (round 63, F63-L1-1).
		sep, joinedSep int
		// roleBytes is the role part of that charge (messageRoleBytesFor): what
		// the joined branch must leave off a system turn's extra, whose own role
		// the merged message already carries (round 62, F62-L1-3).
		roleBytes int
		// toolResults counts the blocks of this turn that become their OWN
		// role-"tool" messages (see the rewrite's charge below).
		toolResults int
		// ownRuns counts the blocks of this turn that become a message carrying
		// the client's OWN role, which for a system turn is what makes it a
		// system message the hoist can join or delete. A system turn with no own
		// run and a tool result writes no system message at all — only a
		// role-"tool" one — so its empty text is not a blank system message and
		// it is not the blank clause's business (2026-09-28 audit, round 66,
		// F66-L1-1).
		ownRuns int
		// runAfterResult reports that a run of the turn's own blocks follows a
		// tool result: the turn becomes [tool, system text] or [tool, system
		// text, tool], so its system text lands after a message of another role
		// and the hoist's orderedness test — which reads the CONVERTED list, not
		// the request's turn order — fails on it (2026-09-28 audit, round 64,
		// F64-L1-3; round 65, F65-L1-3).
		runAfterResult bool
		// endsWithTool reports that the last message the converter writes for
		// this turn carries "tool": the NEXT system turn's text is written after
		// a message of another role, which is the same orderedness test one turn
		// over (2026-09-28 audit, round 65, F65-L1-2).
		endsWithTool bool
		// allRunsText is every run's written text, the runs the merge drops
		// included, and joinedTextBytes/joinedSeparators are the text the merge
		// KEEPS and the blank lines between those runs: the joined branch charges
		// the merged string from the last two and takes the first off the entry's
		// per-run charge (2026-09-28 audit, round 65, F65-L1-4).
		allRunsText      int
		joinedTextBytes  int
		joinedSeparators int
	}
	entries := make([]entry, 0, len(messages)+1)
	if text, present := topLevelSystemText(system); present {
		// The role the converter writes in front of that text, charged here for
		// the same reason the message-level arm charges its own: the top-level
		// `system` field becomes a system MESSAGE, and leaving the role off the
		// charge billed one body's "hi" two tokens against the four the
		// message-level spelling of the same prompt was charged — the client's
		// input_tokens and auto-compaction threshold read small on the spelling
		// Claude Code actually sends (2026-09-28 audit, round 57, F57-L1-1).
		// The joined readings of that message are the hoist's: a top-level system
		// whose text is blank is a system message the rewrite DELETES (it is one
		// of the messages that makes the hoist rewrite at all), so it contributes
		// no text to the merged string and no boundary in front of it — the two
		// spellings of one prompt that differed here were a body carrying a blank
		// "system" field beside the same conversation written without it, and the
		// prompt is the same either way (2026-09-28 audit, round 65).
		joinedText := 0
		if strings.TrimSpace(text) != "" {
			joinedText = len(text)
		}
		entries = append(entries, entry{
			system:           true,
			text:             text,
			bytes:            len("system") + systemBytes(system),
			allRunsText:      len(text),
			joinedTextBytes:  joinedText,
			joinedSeparators: 0,
		})
	}
	mergeable := true
	carriesContent := false
	for _, msg := range messages {
		shape := messageShapeBytes(msg.Content)
		sep, ownRuns, toolResults, resultsStated := shape.separators, shape.ownRuns, shape.toolResults, shape.resultsStated
		role := messageRoleBytesFor(msg.Role, ownRuns, toolResults)
		if strings.EqualFold(msg.Role, "system") {
			textOnly := systemContentIsTextOnly(msg.Content)
			if !textOnly {
				// Only a system message that is nothing but text can be merged
				// into the one system string, so a conversation holding one that
				// is not is left exactly as it arrived (normalizeSystemFirst).
				mergeable = false
			}
			text := joinedMessageText(msg.Content)
			// A system message is a message like any other to the question the
			// early return below asks, and this arm used to be the one place the
			// answer was not taken: a body whose ONLY turn was a system message
			// carrying a payload — a document, a search result, an image, a
			// replayed call, a tool result — was read as carrying nothing, and
			// the whole conversation was charged as the one bare user message
			// the rewrite writes for a conversation of nothing. A 4034-byte
			// document prompt billed 1 token against 1001 for the same bytes
			// written as text, and an image-only one 1 against 1025, on the
			// estimate that seeds the client-visible input_tokens and sizes
			// auto-compaction (2026-09-28 audit, round 59, F59-L1-1).
			//
			// What counts is what SURVIVES the hoist, which deletes a system
			// message only when it is text-only and its text is blank: a
			// whitespace-only one is dropped (and is not content), while one
			// carrying an image or a call is kept whatever its text says.
			//
			// A stated tool result is content on its own terms: the hoist never
			// sees it as a system message at all, because the converter wrote it
			// as a role-"tool" message the prompt carries whatever this turn's
			// text says. It is asked of the whole turn and not of the text
			// below, so the question does not depend on the block being text
			// (2026-09-28 audit, round 61, F61-L1-2).
			if resultsStated > 0 || (ownRuns > 0 && (strings.TrimSpace(text) != "" || !textOnly)) {
				carriesContent = true
			}
			entries = append(entries, entry{
				system:           true,
				text:             text,
				bytes:            role + countAnyContent(msg.Content),
				roleBytes:        role,
				toolResults:      toolResults,
				ownRuns:          ownRuns,
				sep:              sep,
				joinedSep:        shape.joinedSeparators,
				runAfterResult:   shape.runAfterResult,
				endsWithTool:     shape.endsWithTool,
				allRunsText:      shape.allRunsText,
				joinedTextBytes:  shape.joinedTextBytes,
				joinedSeparators: shape.joinedSeparators,
			})
			continue
		}
		if ownRuns > 0 || resultsStated > 0 {
			carriesContent = true
		}
		entries = append(entries, entry{bytes: role + countAnyContent(msg.Content)})
	}

	// A conversation no turn carries anything in reaches the wire as ONE user
	// message: FromMessagesRequest's last step replaces the whole list, because
	// every turn either converted to nothing — the fallback's role-only message
	// above — or was a blank system message the rewrite deletes, and a
	// turn-less body is answered by a synthetic 200 the client reads as a
	// successful generation (anyMessageCarriesContent; round 44, A44-1). The
	// estimate has to charge that replacement rather than the messages it
	// replaced: a client that sends one contentless assistant turn is billed
	// for a one-word prompt, not for a role it never reached the model
	// (2026-09-28 audit, round 58, F58-L1-1).
	if !carriesContent {
		survivingSystemText := false
		for _, e := range entries {
			if e.system && strings.TrimSpace(e.text) != "" {
				survivingSystemText = true
			}
		}
		if !survivingSystemText {
			return len("user")
		}
	}

	rewrite := false
	seenNonSystem := false
	for _, e := range entries {
		if !e.system {
			seenNonSystem = true
			continue
		}
		// The hoist's own test, run over the shape the converter WRITES: a turn
		// whose last own run follows a tool result is written [system, tool,
		// system], so its system text is not first whichever turn the client
		// wrote it in, and normalizeSystemFirst rewrites that conversation
		// (see the entry's splitsAfterResult). Deciding the branch over the
		// request's turns alone left a leading result-split system turn charged
		// the as-is reading while its own converter wrote the joined one
		// (2026-09-28 audit, round 64, F64-L1-3).
		// Both clauses below ask a question ABOUT a system message the converter
		// wrote — is it not first, is it blank — and a system turn holding
		// nothing but tool results writes none: its text is empty because the
		// converter wrote a role-"tool" message for it, not because the hoist
		// found a blank one to delete. Reading its empty text as a blank system
		// message took the joined branch for a conversation
		// normalizeSystemFirst leaves alone, so the same 147-byte prompt was
		// charged as one merged system message (42) when the client wrote the
		// results-only turn as a system turn and as two (46) when it wrote the
		// same turn as a user turn — and this estimate is the client's
		// input_tokens and auto-compaction arithmetic whenever the upstream
		// states no usage (2026-09-28 audit, round 66, F66-L1-1).
		//
		// A system turn writes a system message when it has a run of its own, or
		// when it has no tool result to write as a message of another role — the
		// latter being the role-only fallback message, which is blank and IS the
		// hoist's to delete.
		if e.ownRuns > 0 || e.toolResults == 0 {
			if seenNonSystem || strings.TrimSpace(e.text) == "" || e.runAfterResult {
				rewrite = true
			}
		}
		// The turn the converter writes a tool message LAST for ends with a
		// message of another role, so a system turn written after it is not
		// first and the hoist rewrites the conversation (2026-09-28 audit,
		// round 65, F65-L1-2).
		if e.endsWithTool {
			seenNonSystem = true
		}
	}
	if !rewrite || !mergeable {
		total := 0
		for _, e := range entries {
			total += e.bytes
		}
		return total
	}

	// The rewrite's output: every non-system message as it stands, and the
	// surviving system texts as one message with one role, joined by the blank
	// line the converter writes between them.
	total := 0
	for _, e := range entries {
		if !e.system {
			total += e.bytes
			continue
		}
		// A system turn the converter writes a SEPARATE message for: each of its
		// tool results becomes a role-"tool" message that the hoist keeps beside
		// the merged system string, and the merge carries the turn's text alone.
		// Charging the whole entry in the joined branch would bill its text
		// twice, and charging nothing — which is what the joined branch did —
		// dropped the result from the prompt the client was billed for: the
		// 62-byte prompt of a system turn holding one result cost 13 tokens with
		// a blank system message beside it and 13 without, but a result-only turn
		// was billed nothing at all for the message it becomes (2026-09-28 audit,
		// round 61, F61-L1-2; round 60 had already taught the same arm to count a
		// stated result as content so the fallback is not taken).
		if e.toolResults > 0 {
			// What this turn keeps beside the merged text: the result messages,
			// which are charged their own role ("tool", one per result) — and NOT
			// the turn's own role, which the merged system message is charged for
			// once below. Charging it as well billed the same prompt its role
			// twice: a 114-byte prompt cost 16 tokens with the turn merged and 14
			// with the same prompt written with the text already first
			// (2026-09-28 audit, round 62, F62-L1-3).
			//
			// What comes off the charge is the TEXT of every run the converter
			// writes for the turn, which the merged string charges instead — run
			// by run, and not through len(e.text): the flat join counts the runs
			// the merge DELETES as well, and the boundary before one of those is
			// a blank line the prompt does not contain. Subtracting the flat text
			// and adding back the boundaries between the runs that survive is the
			// same arithmetic read from the merge's own side (2026-09-28 audit,
			// round 65, F65-L1-4; the flat-text spelling was rounds 63's and
			// 64's, F63-L1-1 and F64-L1-2).
			if extra := e.bytes - e.roleBytes + e.toolResults*len("tool") - e.allRunsText; extra > 0 {
				total += extra
			}
		}
	}
	joined, sysTexts := 0, 0
	for _, e := range entries {
		// The merged string is the runs the hoist KEEPS joined by the blank line
		// the merge writes, so a turn whose every run is blank — content the
		// rewrite deletes whole — contributes nothing to it.
		if !e.system || e.joinedTextBytes == 0 {
			continue
		}
		if sysTexts > 0 {
			joined += 2
		}
		joined += e.joinedTextBytes + e.joinedSeparators
		sysTexts++
	}
	if sysTexts > 0 {
		total += len("system") + joined
	}
	return total
}

// topLevelSystemText is the text FromMessagesRequest writes for the request's
// top-level `system` field, and whether it writes a system message for it at
// all: a bare string becomes one, and an array becomes the text blocks it
// holds, joined with a blank line, with the empty ones skipped — the field's
// own arm, which is not convertMessage's.
func topLevelSystemText(system any) (string, bool) {
	switch sys := system.(type) {
	case string:
		return sys, sys != ""
	case []any:
		var sb strings.Builder
		for _, block := range sys {
			bm, ok := block.(map[string]any)
			if !ok || bm["type"] != "text" {
				continue
			}
			text, ok := bm["text"].(string)
			if !ok || text == "" {
				continue
			}
			if sb.Len() > 0 {
				sb.WriteString("\n\n")
			}
			sb.WriteString(text)
		}
		return sb.String(), sb.Len() > 0
	}
	return "", false
}

// systemContentIsTextOnly reports whether the converter writes nothing but TEXT
// for a system message — the question normalizeSystemFirst asks of the CONVERTED
// message (systemMessageIsTextOnly, over its images, calls and thinking) and
// this asks of the blocks the converter writes it from.
//
// A block the converter writes NOTHING for is not an answer to that question:
// redacted_thinking never reaches the run at all and a textless thinking block
// leaves its run empty, so both leave a converted message with no images, no
// calls and no reasoning — text-only, whatever the client's block type says.
// Asking the client's types instead suppressed the rewrite for such a message,
// and the rewrite is what DELETES it: a system message holding only a replayed
// redacted_thinking block stayed in the conversation the estimate charged
// while the prompt the converter wrote had already lost it — six bytes of
// "system" for a message the model never read, on an estimate that seeds the
// client-visible input_tokens (2026-09-28 audit, round 58, F58-L1-3).
//
// The other half of the same question is what the block's TYPE does not say: a
// document with an inline text source and a search_result carrying a passage
// are written into the run as TEXT (convertMessage's own arms), so a system
// message holding them is text-only and IS merged into the single system string
// the rewrite builds. Reading their types as "not text" left the conversation
// un-merged (`mergeable` false), which routes the charge to the as-is branch —
// where a blank system message the rewrite DELETES beside them is charged in
// full: two bodies the converter turns into the same 35-byte prompt were billed
// 25003 tokens as a document and 2 as text, on an estimate that seeds the
// client-visible input_tokens and the auto-compaction threshold (2026-09-28
// audit, round 60, F60-L1-1).
func systemContentIsTextOnly(content []ContentBlock) bool {
	for _, block := range content {
		if blockKeepsSystemMessage(block) {
			return false
		}
	}
	return true
}

// blockKeepsSystemMessage reports whether the converter writes this block INTO
// the converted message as something the hoist's merge would lose — an image, a
// tool call, or replayed reasoning. Those three are the three fields of
// api.Message that systemMessageIsTextOnly reads (Images, ToolCalls, Thinking),
// and they are the answer to the merge question because merging concatenates
// the message's TEXT alone.
//
// Every other block says nothing about it, and asking the client's block TYPES
// instead (round 58's blockWritesText/blockWritesNothing pair) made the answer
// wrong in two directions at once:
//
//   - a block that writes NOTHING into the run is not a reason to refuse the
//     merge. A text block with no `text` key (the converter writes no run for
//     it — convertMessage's text arm writes `block.Text != nil`), a redacted
//     thinking block, an empty thinking block and an empty search_result all
//     leave the converted message exactly as blank as a `""` text block does,
//     and the hoist DELETES that message either way. Refusing the merge sent the
//     charge to the as-is branch, where the blank message beside it survived and
//     was billed: `{"type":"text"}` in a system turn cost 25004 tokens against
//     the 1 token the same converted 32-byte prompt cost spelled with `"text":""`
//     (2026-09-28 audit, round 61, F61-L1-1 — the same overcharge round 60 fixed
//     for a document, one block type over).
//   - a block the converter writes as a SEPARATE message is not a reason to
//     refuse it either. A tool_result and a web_search_tool_result become their
//     own role-"tool" messages (convertMessage sets cur = nil), which the hoist
//     keeps beside the merged system string and never merges into it; the
//     converted system message is left with no images, calls or reasoning, so it
//     IS text-only. Read as "not text", a system turn holding one refused the
//     merge and charged 25014 against the 13 its own 62-byte prompt costs
//     (2026-09-28 audit, round 61, F61-L1-2).
func blockKeepsSystemMessage(block ContentBlock) bool {
	switch block.Type {
	case "image", "tool_use", "server_tool_use":
		return true
	case "thinking":
		// The converter joins this block's text into the run only when the
		// pointer is set; an empty one writes nothing.
		return block.Thinking != nil && *block.Thinking != ""
	}
	return false
}

// blockWritesText reports whether convertMessage writes this block into the
// run's TEXT — the three arms that do: a text block, a document with an inline
// text source, and a search_result carrying a passage. It is the reader
// joinedMessageText walks, and the text it names is what the system hoist's
// merge carries; whether a system message is text-only is not this question and
// is asked by blockKeepsSystemMessage.
//
// A block whose source makes the converter REFUSE the whole request — a base64
// document — answers false here, which is of no consequence: nothing is sent
// and there is nothing to charge.
func blockWritesText(block ContentBlock) bool {
	switch block.Type {
	case "text":
		return block.Text != nil
	case "document":
		return block.Source != nil && block.Source.Type == "text" && block.Source.Data != ""
	case "search_result":
		ref := ""
		if block.Source != nil {
			ref = block.Source.Ref
			if ref == "" {
				ref = block.Source.URL
			}
		}
		return searchResultPromptText(block.Title, ref, block.Content) != ""
	}
	return false
}

// joinedMessageText is the text convertMessage writes for one message: each
// block's own written text, with a blank line before every block after the
// first — blocks are joined, not concatenated, so that narration either side of
// a call does not read as one sentence.
//
// The three arms that write into that text are the three blockWritesText names,
// and each contributes exactly what the converter writes for it: a text block's
// own text, a document's inline source plus the newline the arm adds when the
// data does not end in one, and a search result's built passage (title,
// reference, passages) with the same trailing newline. Reading the text blocks
// alone left a system message carrying a document looking BLANK to the rewrite
// that deletes blank system messages, so the text the prompt does contain was
// dropped from the charge (2026-09-28 audit, round 60, F60-L1-1).
func joinedMessageText(content []ContentBlock) string {
	var sb strings.Builder
	for _, block := range content {
		var chunk string
		switch block.Type {
		case "text":
			if block.Text == nil {
				continue
			}
			chunk = *block.Text
		case "document":
			if !blockWritesText(block) {
				continue
			}
			chunk = block.Source.Data
			if !strings.HasSuffix(chunk, "\n") {
				chunk += "\n"
			}
		case "search_result":
			if !blockWritesText(block) {
				continue
			}
			ref := ""
			if block.Source != nil {
				ref = block.Source.Ref
				if ref == "" {
					ref = block.Source.URL
				}
			}
			chunk = searchResultPromptText(block.Title, ref, block.Content)
			if !strings.HasSuffix(chunk, "\n") {
				chunk += "\n"
			}
		default:
			continue
		}
		if sb.Len() > 0 {
			sb.WriteString("\n\n")
		}
		sb.WriteString(chunk)
	}
	return sb.String()
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
		return total + messageJoinSeparatorBytes(c)
	case []any:
		total := 0
		for _, item := range c {
			total += countContentItem(item)
		}
		return total + messageJoinSeparatorBytes(c)
	default:
		if data, err := json.Marshal(content); err == nil {
			return len(data)
		}
		return 0
	}
}

// messageJoinSeparatorBytes charges the blank line convertMessage writes in
// front of each further text block of one message's content — the separator
// that keeps "read the file." and "Now run the tests." from reading as one
// sentence (the text case of convertMessage). Nothing charged it: a message of
// three text blocks reached the model as 20 bytes and was charged 12, and the
// shortfall is two bytes per block on a body whose every other byte is
// measured. The estimate seeds the client-visible input_tokens on the local
// leg, so the session's meter and its auto-compaction read a prompt smaller
// than the one that was sent (2026-09-27 audit, round 53).
//
// The walk is the converter's own, run by run: a tool_result and a
// web_search_tool_result end the run of own blocks the separator counts within
// (convertMessage sets cur = nil for both), and a text block after one of them
// opens a run of its own and takes no separator. A text block whose text is
// empty still opens one in the converter — it is written, and the separator in
// front of it is written with it — so it is counted here too, exactly as the
// converter writes it.
func messageJoinSeparatorBytes(content any) int {
	return messageShapeBytes(content).separators
}

// messageRoleBytesFor charges the ROLE bytes of every message the converter
// WRITES for one client message, from the counts messageShapeBytes reports: one
// message carrying the client's own role — lower-cased, as the converter writes
// it (convertMessage's `role := strings.ToLower(msg.Role)`) — per run of the
// turn's own blocks that states something, and one message carrying "tool" per
// tool_result / web_search_tool_result block.
//
// A message the converter writes NOTHING for is not absent from the prompt:
// FromMessagesRequest's own fallback writes one message carrying the client's
// role and no content, and the model reads that turn. Leaving its role off
// billed a body whose turn was an empty text block, an empty content array, or
// a replayed redacted_thinking one to two tokens against the three the same
// prompt written without the turn was charged — on an estimate that seeds the
// client-visible input_tokens and the auto-compaction threshold (2026-09-28
// audit, round 58, F58-L1-1).
//
// The message-level charge billed the client's own role once for the whole
// message instead: a message whose result blocks became a tool message was
// charged 9 bytes ("assistant") where the converter writes 4 ("tool"), and a
// message split into two own runs around a result was charged one role for two
// messages (2026-09-28 audit, round 57, F57-L1-3).
//
// The counts come from messageShapeBytes, the walk that already knows what the
// converter writes here — the same one that charges the join separators.
func messageRoleBytesFor(role string, ownRuns, toolResults int) int {
	if ownRuns == 0 && toolResults == 0 {
		return len(strings.ToLower(role))
	}
	total := toolResults * len("tool")
	if ownRuns > 0 {
		total += ownRuns * len(strings.ToLower(role))
	}
	return total
}

// messageRolesBytes is messageRoleBytesFor over one client message's content.
func messageRolesBytes(role string, content any) int {
	shape := messageShapeBytes(content)
	return messageRoleBytesFor(role, shape.ownRuns, shape.toolResults)
}

// toolResultStatesSomething asks whether the tool message the converter writes
// for a tool_result / web_search_tool_result block carries anything at all — the
// question FromMessagesRequest's own fallback asks of every message it built
// (anyMessageCarriesContent: content, images, a call id, a call name, or
// reasoning). It is the converter's own reader, so both spellings of the block's
// content get one answer: convertToolResultContent re-reads a typed
// []ContentBlock through the decoded path, and formatWebSearchToolResultContent
// is what the web-search arm writes.
//
// A block that states nothing is not absent from the prompt — the converter
// still writes its tool message — but it does not stop the rewrite, so a
// conversation whose every turn is one of these reaches the wire as the one bare
// user turn the fallback writes (2026-09-28 audit, round 60, F60-L1-3). Charging
// the role of the message it replaced billed a body that converts to
// [{"role":"user","content":""}] ten tokens against the one the same prompt
// without the turn is charged, and that estimate seeds the client-visible
// input_tokens and the auto-compaction threshold.
//
// A content the converter REFUSES is counted as stating something: the request
// fails conversion and never reaches the model, so no charge can be read off it.
func toolResultStatesSomething(blockType, toolUseID string, content any) bool {
	if toolUseID != "" {
		return true
	}
	if blockType == "web_search_tool_result" {
		return formatWebSearchToolResultContent(content) != ""
	}
	written, images, err := convertToolResultContent(content)
	if err != nil {
		return true
	}
	return written != "" || len(images) > 0
}

type messageShape struct {
	separators       int
	ownRuns          int
	toolResults      int
	resultsStated    int
	joinedSeparators int
	joinedTextBytes  int
	allRunsText      int
	lastRunStated    bool
	endsWithTool     bool
	runAfterResult   bool
}

// messageShape is what one client message's content becomes on the wire, read
// once and reported to every caller that has to agree about it: the charge for
// the blank lines inside the runs of that content (separators), the number of
// messages the converter writes carrying the CLIENT's own role (ownRuns) and
// carrying "tool" (toolResults), how many of those tool messages state anything
// (resultsStated — the question FromMessagesRequest's fallback asks of every
// message it built; see toolResultStatesSomething), and the same content read as
// the ONE text the hoist's merge writes: the blank lines BETWEEN the runs that
// survive that merge (joinedSeparators) and the text those runs hold
// (joinedTextBytes). allRunsText is every run's text, the runs the merge keeps
// and the blank ones it deletes alike, which is the number the per-run charge
// carries.
//
// The last three flags are the hoist's own orderedness test (normalizeSystemFirst)
// read over the CONVERTED list, which is not the request's turn order:
// lastRunStated reports that the last message the converter writes for this turn
// carries the client's own role, endsWithTool that it carries "tool" instead (the
// next system turn's text is then written after a message of another role), and
// runAfterResult that a run of the turn's own blocks follows a tool result (so
// its system text is not first either). See the caller in conversationBytes.
//
// A run is one of the former only when it states something the converter
// writes — text bytes, an image, a call, or reasoning; a lone text block
// holding the empty string leaves the run empty and the converter drops it
// (convertMessage's own `text.Len() == 0 && no images && no calls && no
// thinking` test), and the walk drops it here too. Every tool_result /
// web_search_tool_result block is one of the latter: the converter writes it as
// a tool message of its own and ends the run of own blocks around it. The
// caller that charges roles (messageRolesBytes) reads these two counts, so the
// rule about what the converter writes stays in ONE walk (2026-09-28 audit,
// round 57, F57-L1-3).
//
// The joined reading keeps only the runs the MERGE keeps, and that is not the
// test "the run wrote something": normalizeSystemFirst deletes a system message
// whose content is blank after TrimSpace, so a run of whitespace — a document
// whose data is two newlines, a text block of spaces — reaches no prompt however
// many bytes it wrote, and charging it (and the blank line the merge writes in
// front of it) billed two spellings of one prompt two amounts, unbounded in the
// number of such runs (2026-09-28 audit, round 65, F65-L1-4).
func messageShapeBytes(content any) messageShape {
	var shape messageShape
	run, stated := false, false
	// runText is the text the converter writes for the run the walk is inside —
	// what that one message's content is — and runNonBlank whether any of it
	// survives TrimSpace, which is the hoist's own test for the system messages
	// it deletes (see this function's header).
	runText := 0
	runNonBlank := false
	// afterResult reports that the walk has passed a tool result and not yet
	// closed the run that follows it: the run it opens next is written after a
	// message of another role.
	afterResult := false
	openedAfterResult := false
	jruns := 0
	// flush closes the run the walk is inside: a run that stated anything is a
	// message of the client's own role, and a run whose text is non-blank is one
	// of the runs the merge joins.
	flush := func() {
		if stated {
			shape.ownRuns++
			if openedAfterResult {
				shape.runAfterResult = true
			}
		}
		shape.allRunsText += runText
		if runText > 0 && runNonBlank {
			shape.joinedTextBytes += runText
			if jruns > 0 {
				shape.joinedSeparators += 2
			}
			jruns++
		}
		run, stated, runNonBlank, runText, openedAfterResult = false, false, false, 0, false
	}
	// open writes the separator in front of a block of the run the walk is
	// inside: it is written when the run ALREADY holds text, and the block is
	// what makes the run hold text from then on. The two tests are not the same
	// one: a text block that states the empty string writes no bytes of its own
	// yet still takes the separator behind a run that has some — the converter
	// writes two newlines first and the empty text after it — so a four-character
	// text followed by an empty one and four more is written 12 bytes by the
	// converter and was charged 10 here (round 53's own test, empty_between,
	// caught this in round 54's rewrite). A block whose text is not stated writes
	// nothing at all and never reaches this function.
	open := func() {
		if run {
			shape.separators += 2
			runText += 2
		}
		if afterResult {
			openedAfterResult = true
		}
	}
	// write is the run's text arm: a text block's own text, and nothing for one
	// that states the empty string. A block whose text is blank still OPENS the
	// run — the converter writes it, and the separator in front of it is written
	// with it — it is only the merged reading that does not carry it.
	write := func(text string) {
		open()
		if text == "" {
			return
		}
		run, stated = true, true
		runText += len(text)
		if strings.TrimSpace(text) != "" {
			runNonBlank = true
		}
	}
	// A document with a TEXT source and a search_result are the other two arms
	// that write into the same run (convertMessage), and they write more than
	// the bytes the content chargers bill them: the blank line they take when
	// the run already holds text, the newline the document arm adds when its
	// data does not end in one, and the newlines the search_result arm writes
	// after a title and a reference. Left out, a prompt whose newest blocks
	// were attachments was charged a few bytes short each — and this estimate
	// seeds the client-visible input_tokens whenever the upstream states no
	// usage, so the session's meter and its auto-compaction read a prompt
	// smaller than the one that was sent (2026-09-28 audit, round 54; the same
	// omission the text arm had in round 53).
	//
	// documentWritten is the text the converter writes for a document whose
	// source is inline text: the data, plus the trailing newline it adds when
	// the data does not end in one.
	documentWritten := func(sourceType, data string) string {
		if sourceType != "text" {
			return ""
		}
		if !strings.HasSuffix(data, "\n") {
			return data + "\n"
		}
		return data
	}
	// join is the attachment and tool arm: a document or a search_result writes
	// its text into the run beside the walk's separator, and a tool_result or a
	// web_search_tool_result ends the run and becomes a message of its own. The
	// bytes already billed by countContentBlock/countContentItemIn for each of
	// those arms are accounted for here, so the extra this walk charges is
	// exactly what the converter writes AROUND them.
	join := func(blockType, chunk string, billed int) {
		switch blockType {
		case "tool_result", "web_search_tool_result":
			// The converter sets cur = nil for both: the next own block opens a
			// run of its own and takes no separator, and the result itself is a
			// message of its own carrying role "tool". The role is charged for
			// every one of them; whether that tool message STATES anything is a
			// separate count the arm sites keep (resultsStated), because it is
			// the other question the caller asks of it.
			flush()
			shape.toolResults++
			afterResult = true
		default:
			switch blockType {
			case "image", "tool_use", "server_tool_use":
				// The converter writes each of these into the run beside the
				// text, and the run is a message whether or not it holds any
				// text: an image-only turn is written, and a lone empty text is
				// not (see this function's header). A thinking block is NOT one
				// of them — it joins this list only when it carries text, which
				// the arms below ask before coming here — and redacted_thinking
				// never reaches the run at all (convertMessage drops it whole)
				// (2026-09-28 audit, round 58, F58-L1-2).
				stated = true
			}
			if chunk == "" {
				return
			}
			open()
			// The extra this arm writes beyond what the content chargers bill —
			// the newline a document takes when its data lacks one, the title and
			// reference a search result states — is part of the run's text and of
			// the merged text the rewrite writes, so it is charged here and the
			// joined reading carries it too: the two readings are compared against
			// each other, and a byte that differs for a reason the merge has
			// nothing to do with billed a result-split turn backwards
			// (2026-09-28 audit, round 64, F64-L1-1).
			shape.separators += len(chunk) - billed
			runText += len(chunk)
			if strings.TrimSpace(chunk) != "" {
				runNonBlank = true
			}
			run, stated = true, true
		}
	}

	switch c := content.(type) {
	case []ContentBlock:
		for i := range c {
			b := c[i]
			switch b.Type {
			case "text":
				// A text block with no text pointer writes nothing at all — the
				// converter's case guards on block.Text != nil — so it is not a
				// block this walk may charge a separator for.
				if b.Text == nil {
					continue
				}
				write(*b.Text)
			case "document":
				if b.Source == nil {
					continue
				}
				chunk := documentWritten(b.Source.Type, b.Source.Data)
				billed := 0
				if b.Source.Type == "text" {
					billed = len(b.Source.Data)
				}
				join(b.Type, chunk, billed)
			case "search_result":
				ref := ""
				if b.Source != nil {
					ref = b.Source.Ref
					if ref == "" {
						ref = b.Source.URL
					}
				}
				chunk := searchResultPromptText(b.Title, ref, b.Content)
				billed := len(b.Title) + countItemsIn(b.Content, chargeSearchResult)
				if b.Source != nil {
					billed += len(b.Source.Ref) + len(b.Source.URL)
				}
				join(b.Type, chunk, billed)
			case "tool_result", "web_search_tool_result":
				join(b.Type, "", 0)
				if toolResultStatesSomething(b.Type, b.ToolUseID, b.Content) {
					shape.resultsStated++
				}
			case "thinking":
				// The decoded arm's rule, over the typed spelling: a thinking
				// block joins the run only when it carries a non-empty text.
				if b.Thinking != nil && *b.Thinking != "" {
					stated = true
				}
			case "redacted_thinking":
				// Dropped whole by the converter, so it joins no run and is
				// charged nothing: it never reaches join()'s arm list.
			default:
				join(b.Type, "", 0)
			}
		}
	case []any:
		for _, item := range c {
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			blockType, _ := m["type"].(string)
			switch blockType {
			case "text":
				if m["text"] == nil {
					// The same block the typed arm above skips: null and absent
					// both decode to no text pointer, and the converter writes
					// nothing for either.
					continue
				}
				blockText, _ := m["text"].(string)
				write(blockText)
			case "document":
				sourceType, data := "", ""
				switch src := m["source"].(type) {
				case map[string]any:
					sourceType, _ = src["type"].(string)
					data, _ = src["data"].(string)
				}
				chunk := documentWritten(sourceType, data)
				billed := 0
				if sourceType == "text" {
					billed = len(data)
				}
				join(blockType, chunk, billed)
			case "search_result":
				title, _ := m["title"].(string)
				ref := ""
				switch src := m["source"].(type) {
				case map[string]any:
					ref, _ = src["ref"].(string)
					if ref == "" {
						ref, _ = src["url"].(string)
					}
				case string:
					// The documented bare-string spelling of a search_result's
					// source, which UnmarshalJSON reads as Ref.
					ref = src
				}
				chunk := searchResultPromptText(title, ref, m["content"])
				billed := len(title) + countItemsIn(m["content"], chargeSearchResult)
				if src, ok := m["source"].(map[string]any); ok {
					if r, _ := src["ref"].(string); r != "" {
						billed += len(r)
					}
					if u, _ := src["url"].(string); u != "" {
						billed += len(u)
					}
				} else if ref != "" {
					billed += len(ref)
				}
				join(blockType, chunk, billed)
			case "tool_result", "web_search_tool_result":
				// The typed arm's rule, over the decoded spelling: the block is
				// a tool message whatever it says, but only one that STATES
				// something keeps the conversation from being replaced by the
				// fallback turn (see toolResultStatesSomething).
				join(blockType, "", 0)
				useID, _ := m["tool_use_id"].(string)
				if toolResultStatesSomething(blockType, useID, m["content"]) {
					shape.resultsStated++
				}
			case "thinking":
				// The typed arm's rule, over the decoded spelling: a thinking
				// block joins the run only when it carries a non-empty text.
				if text, ok := m["thinking"].(string); ok && text != "" {
					stated = true
				}
			default:
				join(blockType, "", 0)
			}
		}
	case string:
		// A bare-string content is the one text block the unmarshal writes for
		// it (MessageParam.UnmarshalJSON): an empty string states nothing.
		write(c)
	default:
		// Content of a shape the converter does not walk (nil, a map) writes no
		// message.
	}
	// The last run is a message too, if it said anything. Whether it is also the
	// LAST thing the converter writes for this turn — a run that follows a tool
	// message — is what the hoist's orderedness test turns on, because such a
	// turn is written [system, tool, system]: the system text lands after a
	// message of another role whatever the request's own turn order says, so the
	// rewrite moves it and the estimate has to charge the turn the way the
	// rewrite writes it (conversationBytes' rewrite; 2026-09-28 audit, round 64,
	// F64-L1-3).
	shape.lastRunStated = stated
	flush()
	// The last thing the converter writes for the turn decides whether the NEXT
	// system turn's text lands after a message of another role
	// (conversationBytes' rewrite).
	shape.endsWithTool = shape.toolResults > 0 && !shape.lastRunStated
	return shape
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
			if !ok || text == "" {
				// FromMessagesRequest writes nothing for a block whose text is
				// empty (its own `text != ""` gate), so it contributes no bytes
				// AND no separator: adding the separator before knowing whether
				// the block had anything to say charged 5 bytes for a
				// three-byte system prompt whose last block was empty
				// (2026-09-28 audit, round 55; the round-49 B-F5 rule, applied
				// one level up).
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
	blockType, _ := m["type"].(string)
	if blockType == "text" {
		// The same gate as the typed arm above: the converter writes a text
		// block's text and nothing else's, so a "text" key on a block of
		// another type is not prompt content this walk may charge.
		if s, ok := m["text"].(string); ok {
			total += len(s)
		}
	}
	switch blockType {
	case "tool_use":
		// The CALL's own JSON, exactly as the typed arm charges it — with the
		// inline binary held to the image allowance, so a nested payload the
		// converter DESCRIBES is not billed as its transport encoding — and
		// reduced to the fields the converter writes (round 55; see the typed
		// arm above).
		total += callBlockBytes(decodedCallBlock(m))
	case "tool_result":
		// Not clampedJSONBytes: the converter does not paste this block, it
		// CONVERTS its content (convertToolResultContent), and the charge
		// follows that reader (see toolResultBytes) — for the fields the
		// converter reads and no others, which is the same frame the typed arm
		// charges (toolResultFrame).
		id, _ := m["tool_use_id"].(string)
		total += toolResultBytes(toolResultFrame(id), m["content"])
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
		// The reference the converter writes, and ONLY it: searchResultText
		// names the source's ref (or its url) in the passage it builds, so a
		// `data` payload on a search result's source is not prompt bytes —
		// imageSourceBytes counted it and billed a 40000-byte blob the typed
		// arm charges nothing for, one spelling of one block apart (2026-09-28
		// audit, round 61, F61-L1-3).
		switch src := m["source"].(type) {
		case string:
			total += len(src)
		case map[string]any:
			ref, _ := src["ref"].(string)
			if ref == "" {
				ref, _ = src["url"].(string)
			}
			total += len(ref)
		}
	case "server_tool_use":
		// Charged exactly as the tool_use arm above, because the converter
		// writes the two the same way: this block's JSON IS the tool call, and
		// the escapes in it are transport that the receiver decodes. Charging
		// len(json.Marshal(m)) raw billed an identical call twice as much as
		// its tool_use twin — 515 tokens against 1 017 for the same 4 112-byte
		// prompt — and both arms sit in this same function, one of them routed
		// through clampedJSONBytes (2026-09-27 audit, round 42, A42-5). Same
		// reduction to the converter's own fields as its twin (round 55).
		total += callBlockBytes(decodedCallBlock(m))
	case "web_search_tool_result":
		// The HITS, exactly as the typed arm charges its twin: the converter
		// writes one line per hit and nothing else, so serializing the block
		// pasted its encrypted_content into the charge instead — a few hundred
		// kilobytes of opaque blob billed against a wire holding a title, a URL
		// and a newline, on an estimate that seeds the client-visible
		// input_tokens (2026-09-27 audit, round 41, A41-3; the decoded arm was
		// the last spelling still doing it, round 61, F61-L1-3). The frame is
		// charged too, from the same reader the typed arm uses, so the two
		// spellings of one block stay one charge (round 62, F62-L1-1).
		//
		// A NESTED one — inside a tool result or a passage list — never reaches
		// this arm: those carriers return above, through the readers that
		// describe what the converter writes for them.
		id, _ := m["tool_use_id"].(string)
		total += frameBytes(toolResultFrame(id)) +
			len(formatWebSearchToolResultContent(m["content"]))
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
		// The parts a tool_result's join assembles are separated by a newline
		// (convertToolResultContent, round 37 A-F7), and those separators are
		// prompt bytes like any other: nothing charged them, so a result of two
		// text blocks reached the model as seven bytes and was charged six, and
		// the shortfall grows by one per block (2026-09-27 audit, round 53).
		// The walk is the converter's own: a separator in front of every part
		// after the first that carried bytes, and none for the part that
		// carried none.
		written, separators := 0, 0
		for _, item := range c {
			total += countContentItemIn(item, ctx)
			if ctx != chargeToolResult {
				continue
			}
			m, ok := item.(map[string]any)
			if !ok {
				continue
			}
			text := toolResultItemText(m)
			if text == "" {
				continue
			}
			if written > 0 {
				separators++
			}
			written += len(text)
		}
		return total + separators
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
func toolResultBytes(frame map[string]any, content any) int {
	return frameBytes(frame) + toolResultContentBytes(content)
}

// frameBytes charges one converted block's frame — the JSON the counter builds
// as a stand-in for the keys the message carries BESIDE its content, with the
// transport's escapes subtracted (a call id of 2 000 quotes is decoded by the
// receiver, so serializing it raw billed the encoding twice over).
func frameBytes(frame map[string]any) int {
	if data, err := json.Marshal(frame); err == nil {
		return len(data) - jsonEscapeOverhead(data)
	}
	return 0
}

// toolResultFrame is the part of a tool_result the converter writes beside its
// content: the id only when the body stated one. The block's own TYPE is not one
// of them: a tool_result and a web_search_tool_result convert to the SAME
// role-"tool" message — convertMessage writes {role, content, tool_call_id} for
// each — and the prompt carries no type key at all, so a frame built from the
// client's own block name charged two spellings of one prompt two amounts, 11
// bytes for every result a session replays (2026-09-28 audit, round 65,
// F65-L1-1). The
// message the block becomes writes its tool_call_id with omitempty, so an
// id-less result adds no key to the prompt — and charging a fabricated
// `"tool_use_id":""` billed bytes the model was never sent (2026-09-28 audit,
// round 61, F61-L1-4). Every OTHER key the body carries is not prompt bytes
// either: convertMessage reads the type, the id and the content of a
// tool_result and writes nothing else, so a stray key beside them — which the
// decoded walk used to marshal straight into the charge — must not be billed
// (round 61, F61-L1-3).
func toolResultFrame(toolUseID string) map[string]any {
	frame := map[string]any{}
	if toolUseID != "" {
		frame["tool_use_id"] = toolUseID
	}
	return frame
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
	if t, _ := m["type"].(string); t == "image" {
		return imageBlockBytes(m["source"])
	}
	return len(toolResultItemText(m))
}

// toolResultItemText is the text convertToolResultContent WRITES for one
// element of a tool_result's content — the one reader both the charge above and
// the join separator below measure, so the two cannot drift apart:
//
//   - a text element is its text when that text IS a string, and an empty text
//     is no text at all (the converter skips it rather than writing a blank
//     line);
//   - an image writes no text — it is carried as the message's image;
//   - a document and every other block are DESCRIBED, and a description that
//     comes back empty writes nothing.
func toolResultItemText(m map[string]any) string {
	switch t, _ := m["type"].(string); t {
	case "text":
		text, _ := m["text"].(string)
		return text
	case "image":
		return ""
	case "document":
		return describeToolResultDocument(m["source"])
	}
	return describeToolResultBlock(m)
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

// callBlockBytes charges a tool call for the JSON the converter WRITES for it.
//
// convertMessage's tool_use and server_tool_use arms both build one
// api.ToolCall out of the block's type, id, name and input; every other key the
// block carries is read by nothing and reaches no prompt. Charging the block's
// own JSON billed those keys, so an off-spec `text` beside a call — a shape the
// decoded arms accept and the typed block models as ContentBlock.Text — made a
// 41 000-byte stray key into 10 269 tokens of estimate for a call whose
// converted form is unchanged, and the estimate seeds the client-visible
// input_tokens and the compaction threshold (2026-09-28 audit, round 55; the
// same rule round 54's B4 applied to a text block's `text` key).
func callBlockBytes(block ContentBlock) int {
	// An input that STATES the empty object is the same call as one that states
	// no input at all: convertMessage flattens both to the same
	// "arguments":{} (probed, round 57), so charging the stated key billed 11
	// bytes for a key the prompt does not contain — 2 to 3 tokens on every call
	// the client wrote as "input":{} . A stated NULL is not this case and is
	// left alone: it flattens to "arguments":null, a different prompt, whose
	// charge is its own (2026-09-28 audit, round 57, F57-L1-2).
	if callInputStatesNothing(block.Input) {
		block.Input = api.ToolCallFunctionArguments{}
	}
	return clampedJSONBytes(block)
}

// callInputStatesNothing reports whether a call's input is the empty object the
// converter writes the same arguments for as an absent one. The zero value of
// api.ToolCallFunctionArguments — what an absent or null key leaves behind,
// which the block omits entirely — marshals as "{}" through its own
// MarshalJSON, and so does a stated empty object; the NULL spelling marshals as
// "null" and is the case this must not fold (see callBlockBytes).
func callInputStatesNothing(input api.ToolCallFunctionArguments) bool {
	if input.Len() > 0 {
		return false
	}
	raw, err := json.Marshal(input)
	return err == nil && string(raw) == "{}"
}

// callArgumentsStated is the arguments of a call as this leg WRITES them to the
// client: a call that states nothing — an absent, empty, or JSON-literal-null
// argument text — states the empty object.
//
// The distinction the request direction draws does not exist in this one. A
// client's `"input":null` is a value the estimate charges on its own
// (callInputStatesNothing, F57-L1-2); an UPSTREAM's null is the literal an
// OpenAI-compatible backend writes for a call with no arguments, and every leg
// already answers it with {} — the non-stream converter folded input.Len()==0
// to the empty object for exactly this shape (round 45, A45-4), the gateway's
// callInput folds the literal (round 48, C-F5), and the client proxy's own
// whole-list parser emits an empty Input for it. This converter was the site
// that did not: it marshalled the null spelling into the tool_use block's own
// input_json_delta, so the client accumulated `{}` from content_block_start
// followed by `null` from the delta — JSON no SDK can parse (2026-09-28 audit,
// round 65, F65-L2-1).
//
// The fold is of the VALUE ONLY. Round 65 also seeded the minted id from this
// shape, on the belief that the proxy's whole document folds too — it does not:
// its parser mints from the text it was handed, as the gateway does, and a
// null-seeded hash from here split that leg against itself (round 66's
// F66-L2-1). The seed is tc.Function.Arguments, unmarshalled and marshalled
// back, which is `{}` for an empty text and `null` for the literal.
func callArgumentsStated(args api.ToolCallFunctionArguments) api.ToolCallFunctionArguments {
	if args.Len() == 0 {
		return api.NewToolCallFunctionArguments()
	}
	return args
}

// decodedCallBlock reduces a DECODED tool_use / server_tool_use block to the
// fields converter writes for it, key by key, so a key the body carries beside
// them is not charged. Only the keys the body actually stated are kept: the
// typed arm marshals a struct whose empty fields are omitted, so a key this
// dropped would otherwise be a "name":"" the call never had and the two arms
// would drift by exactly the keys the client did not write (see
// callBlockBytes; round 42's twin-parity test is what keeps the two equal).
func decodedCallBlock(m map[string]any) ContentBlock {
	var block ContentBlock
	if s, ok := m["type"].(string); ok {
		block.Type = s
	}
	if s, ok := m["id"].(string); ok {
		block.ID = s
	}
	if s, ok := m["name"].(string); ok {
		block.Name = s
	}
	if v, ok := m["input"]; ok {
		if raw, err := json.Marshal(v); err == nil {
			_ = json.Unmarshal(raw, &block.Input)
		}
	}
	return block
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
	if block.Type == "text" && block.Text != nil {
		// Only a text block's text is written by the converter: a "text" key on
		// a block of any other type is not a field of that block's shape, and
		// convertMessage never reads it. Charged for every type, a body like
		// [{"type":"image","text":"<40 KB>"}] took the estimate from ~1 025
		// tokens to ~11 025 against a prompt of 1 025 bytes, and the estimate is
		// what seeds the client-visible input_tokens whenever the upstream
		// states no usage (2026-09-28 audit, round 54).
		total += len(*block.Text)
	}
	// A thinking block is charged NOTHING, and a redacted one never was. A
	// 700 KB thinking blob — with a 400 KB redacted one beside it — took the
	// estimate from 5 502 tokens to 180 502 while the converted prompt was
	// byte-for-byte the same 22 084 bytes: the session's meter and its
	// auto-compaction read a prompt 32× its real size (2026-09-27 audit, round
	// 39, C-F13).
	//
	// Whether the blob reaches a prefill is the MODEL's own prompt builder, and
	// that is not one thing: a model rendered by a bundled Go template writes
	// api.Message.Thinking only if the template names `.Thinking`, which none of
	// this repo's templates does, while a model with a Config.Renderer — which
	// server/create.go assigns to the first-party models, and server/prompt.go
	// takes in preference to any template — writes it for an assistant turn
	// (model/renderers/cohere.go:161-164 and :185-188, nemotron3nano.go:177,
	// gemma4.go:90, qwen3vl.go:92, laguna.go:85). So the honest charge is
	// model-dependent, and the estimate cannot see the model: its one consumer,
	// middleware/anthropic.go's handler for the local leg, has a MessagesRequest
	// and nothing else. Round 39 measured what guessing costs on the wrong side
	// of that line — a 700 KB blob took the estimate from 5 502 tokens to
	// 180 502 for a wire that then held none of it — and the upstream's own
	// count corrects the estimate whenever it states one, so the block is
	// charged nothing and the divergence is named here rather than hidden
	// (2026-09-28 audit, round 60, F60-X-3: a replayed thinking turn of 4 000
	// bytes is charged 2 tokens by this fallback and written into the prompt by
	// every renderer above — a reasoned non-fix, not an oversight).
	switch block.Type {
	case "tool_use":
		// A tool CALL is carried as JSON arguments, so its own JSON is the
		// prompt and the escapes in it are transport. It is the CALL's JSON,
		// though, not the block's: convertMessage reads a tool_use block's type,
		// id, name and input and writes nothing else, so a key the block carries
		// beside them — an off-spec `text` next to a call, which the converter
		// never writes — is not prompt bytes and must not be billed. Serializing
		// the whole block charged a 41 000-byte stray key as 10 269 tokens
		// against a byte-identical call (2026-09-28 audit, round 55; the round-54
		// B4 rule, one arm over).
		total += callBlockBytes(ContentBlock{Type: block.Type, ID: block.ID, Name: block.Name, Input: block.Input})
	case "tool_result":
		// A tool RESULT is CONVERTED, not pasted (convertToolResultContent), so
		// it is charged through that reader instead. The two arms shared one
		// case until the reader below existed: the same treatment billed a
		// nested attachment's base64 against a wire that holds a notice for it,
		// and billed a nested image nothing at all where the converter carries
		// it (2026-09-27 audit, round 42, A42-2/C42-1).
		total += toolResultBytes(toolResultFrame(block.ToolUseID), block.Content)
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
		// round 42, A42-5). Reduced to the fields the converter writes for it,
		// exactly as its tool_use twin is (round 55).
		total += callBlockBytes(ContentBlock{Type: block.Type, ID: block.ID, Name: block.Name, Input: block.Input})
	case "web_search_tool_result":
		// The same omission, and then the opposite error: the converter writes
		// the HITS, one line each, and nothing else — while serializing the
		// block pasted its encrypted_content back in. A search result carrying
		// a few hundred kilobytes of opaque blob was billed in full against a
		// wire holding a title, a URL and a newline per hit, and the estimate
		// seeds the client-visible input_tokens, so the session's context meter
		// and auto-compaction read a prompt the model was never sent
		// (2026-09-27 audit, round 41, A41-3). Charge what the converter
		// writes, through the very function that writes it — and charge the
		// FRAME beside it, which is the same frame its tool_result twin is
		// charged: the message this block becomes writes its tool_call_id with
		// the content, so a stated id is prompt bytes the hits alone do not
		// cover. Charging the hits alone left the estimate 20 bytes behind the
		// wire the moment the block stated one (2026-09-28 audit, round 62,
		// F62-L1-1).
		total += frameBytes(toolResultFrame(block.ToolUseID)) +
			len(formatWebSearchToolResultContent(block.Content))
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
