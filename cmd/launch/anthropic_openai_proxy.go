package launch

// anthropic_openai_proxy.go — a local HTTP server that presents an Anthropic
// /v1/messages endpoint to Claude Code and translates each request to OpenAI
// /v1/chat/completions, forwarding to a user-defined remote
// (~/.oaica/remotes.json) with that remote's own api key. This lets
// `oaica launch claude --model deepseek/deepseek-v4-flash` work end-to-end:
// Claude Code speaks the Anthropic Messages API, but DeepSeek (and most other
// user-remotes) speak OpenAI. Without this translation, Claude Code would be
// pointed at the OAICA router with the OAICA key, not the remote.
//
// Conversion strategy — REUSE the anthropic package's content-block/tool/stop
// mapping, add a mechanical Ollama api.ChatRequest ↔ OpenAI wire mapping:
//
//   Anthropic → api.ChatRequest  (anthropic.FromMessagesRequest — handles
//                                 content blocks, tool_use/tool_result,
//                                 system, images — the hard parts)
//   api.ChatRequest → OpenAI JSON (this file — mechanical field mapping)
//   OpenAI JSON → api.ChatResponse (this file — mechanical field mapping)
//   api.ChatResponse → Anthropic  (anthropic.ToMessagesResponse for non-stream,
//                                  anthropic.StreamConverter for streaming)
//
// Non-streaming and streaming are both supported. tool_calls (function
// calling) are accumulated across streamed chunks and flushed at completion so
// the StreamConverter sees each complete tool call in one Process call (it
// emits content_block_start + input_json_delta + content_block_stop per tool).

import (
	"bufio"
	"bytes"
	"compress/flate"
	"compress/gzip"
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
	"github.com/ollama/ollama/cmd/internal/httpbody"
)

// openAIMessage is the OpenAI chat-completions wire shape for one message.
type openAIMessage struct {
	Role       string             `json:"role"`
	Content    string             `json:"content,omitempty"`
	ToolCalls  []openAIToolCall   `json:"tool_calls,omitempty"`
	ToolCallID string             `json:"tool_call_id,omitempty"`
	Images     []openAIImageBlock `json:"-"`
}

// openAIImageBlock is one image in an OpenAI multimodal content array
// ({"type":"image_url","image_url":{"url":"data:...;base64,..."}}).
type openAIImageBlock struct {
	DataURL string `json:"-"`
}

// MarshalJSON emits multimodal content (text + image_url parts) when the
// message carries images, and the plain string form otherwise. Claude Code
// attaches Read-tool screenshots and pasted images as Anthropic `image`
// blocks; FromMessagesRequest decodes those into api.Message.Images, and the
// proxy must re-emit them in OpenAI vision format or the upstream model
// never sees the image (seen on .91 2026-08-27: "I can't visually see what
// it depicts").
func (m openAIMessage) MarshalJSON() ([]byte, error) {
	if len(m.Images) == 0 {
		type alias openAIMessage
		return json.Marshal(alias(m))
	}
	type alias struct {
		Role       string              `json:"role"`
		Content    []openAIContentPart `json:"content"`
		ToolCalls  []openAIToolCall    `json:"tool_calls,omitempty"`
		ToolCallID string              `json:"tool_call_id,omitempty"`
	}
	parts := make([]openAIContentPart, 0, len(m.Images)+1)
	if strings.TrimSpace(m.Content) != "" {
		parts = append(parts, openAIContentPart{Type: "text", Text: m.Content})
	}
	for _, img := range m.Images {
		parts = append(parts, openAIContentPart{
			Type:     "image_url",
			ImageURL: &openAIImageURL{URL: img.DataURL},
		})
	}
	return json.Marshal(alias{Role: m.Role, Content: parts, ToolCalls: m.ToolCalls, ToolCallID: m.ToolCallID})
}

type openAIContentPart struct {
	Type     string          `json:"type"` // "text" | "image_url"
	Text     string          `json:"text,omitempty"`
	ImageURL *openAIImageURL `json:"image_url,omitempty"`
}

type openAIImageURL struct {
	URL string `json:"url"`
}

type openAIToolCall struct {
	ID       string             `json:"id"`
	Type     string             `json:"type"`
	Function openAIToolFunction `json:"function"`
}

type openAIToolFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"` // OpenAI wants a JSON STRING here
}

// openAIChatRequest is the request body sent to the remote.
type openAIChatRequest struct {
	Model       string          `json:"model"`
	Messages    []openAIMessage `json:"messages"`
	Stream      bool            `json:"stream,omitempty"`
	MaxTokens   int             `json:"max_tokens,omitempty"`
	Temperature *float64        `json:"temperature,omitempty"`
	TopP        *float64        `json:"top_p,omitempty"`
	// TopK is not part of OpenAI's schema, but the upstreams this proxy talks
	// to (ollama and its forks) accept it, and the client asked for it:
	// anthropic.FromMessagesRequest folds it into Options and oaica's own
	// server honours it, so dropping it here made sampling differ by entry
	// point (2026-09-26 audit, fourth round).
	TopK          *int       `json:"top_k,omitempty"`
	Stop          []string   `json:"stop,omitempty"`
	Tools         []api.Tool `json:"tools,omitempty"`
	ToolChoice    any        `json:"tool_choice,omitempty"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options,omitempty"`
}

// openAIChatResponse is the non-streaming response from the remote.
type openAIChatResponse struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Index   int `json:"index"`
		Message struct {
			Role             string `json:"role"`
			Content          string `json:"content"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
			// Reasoning is an alias some vLLM reasoning-parser configs (e.g.
			// the qwen3 parser as of vLLM 0.24) emit instead of
			// reasoning_content. Same meaning, different key name; see
			// reasoningText() below for the field that call sites should
			// actually read.
			Reasoning string `json:"reasoning,omitempty"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Type     string `json:"type"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"message"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage,omitempty"`
}

// openAIUsage is the usage object of a chat completion (and of the final
// usage-only chunk of a stream with stream_options.include_usage).
// prompt_tokens_details.cached_tokens is populated by vLLM only with
// --enable-prompt-tokens-details (on in our fleet since 2026-08-29); it is
// the prefix-cache hit count and maps to Anthropic's cache_read_input_tokens.
type openAIUsage struct {
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	TotalTokens         int `json:"total_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details,omitempty"`
	// prompt_cache_hit_tokens is the same count under DeepSeek's/vLLM's other
	// name (and the only one some builds emit). Without this fallback a fully
	// cache-hit turn was reported to Claude Code as a full-prompt fresh input
	// read — inflating the displayed (and any downstream-billed) token count
	// on exactly the legs that do cache, which is the user-visible half of
	// "usage through oaica looks much higher than native" (2026-09-26 audit).
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens,omitempty"`
}

// reasoningOf returns whichever of reasoning_content / reasoning is
// non-empty. Different vLLM reasoning-parser configs emit different key
// names for the same concept (see the field comments above); this is the
// one place that ambiguity is resolved so every call site reads a single
// value instead of re-deriving it.
func reasoningOf(reasoningContent, reasoning string) string {
	if reasoningContent != "" {
		return reasoningContent
	}
	return reasoning
}

// cachedTokens returns the prefix-cache hit count, clamped to the prompt
// size so a malformed upstream can never yield a negative input_tokens.
func (u *openAIUsage) cachedTokens() int {
	if u == nil {
		return 0
	}
	c := 0
	switch {
	case u.PromptTokensDetails != nil:
		c = u.PromptTokensDetails.CachedTokens
	}
	// The fallback is on the VALUE, not the pointer. prompt_cache_hit_tokens
	// exists in this struct precisely because "some builds emit only the
	// other name" — but testing the details object for nil let an upstream
	// that sends BOTH keys with the details object zeroed (a build that
	// always emits the object, populated only when it has a hit) report a
	// fully uncached prompt while its own sibling field said 4096 cached
	// tokens. The user's cache-efficiency read-out was 0% for a real hit
	// (2026-09-26 audit).
	if c == 0 {
		c = u.PromptCacheHitTokens
	}
	if c < 0 {
		return 0
	}
	if c > u.PromptTokens {
		// The clamp target is itself unvalidated: a malformed upstream that
		// states a NEGATIVE prompt_tokens made `0 > -5` true and returned -5,
		// a cache-read count nobody stated, contradicting this function's own
		// contract (2026-09-26 audit, fifteenth round).
		if u.PromptTokens < 0 {
			return 0
		}
		return u.PromptTokens
	}
	return c
}

// statedPromptTokens / statedCompletionTokens report whether the usage object
// actually STATES that particular count. The presence of the object is not a
// statement about its fields: a build that always emits it, populated only when
// it has something to say, sends {"prompt_tokens":0,"completion_tokens":0} — an
// empty statement, not a measurement of zero — and a partially populated object
// states one count and says nothing about the other. Gating a fallback on the
// OBJECT (or on an OR of its two fields) therefore suppressed it for a field
// the upstream never spoke about, and the client was told input_tokens=0 for a
// turn whose prompt was real: a session that never appears to grow, so
// auto-compaction never fires, until the request hits the context wall — the
// 2026-08-30 failure reached through a different door (2026-09-26 audit,
// twelfth and thirteenth rounds).
//
// "Stated" is deliberately not "non-zero": a fully-cached turn reports
// prompt_tokens>0 with every one of them cached, so its uncached count is
// legitimately 0 and must not be replaced by an estimate.
func (u *openAIUsage) statedPromptTokens() bool {
	return u != nil && u.PromptTokens > 0
}

func (u *openAIUsage) statedCompletionTokens() bool {
	return u != nil && u.CompletionTokens > 0
}

// openAIStreamChunk is one SSE data: payload from a streaming response.
type openAIStreamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role,omitempty"`
			Content          string `json:"content,omitempty"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
			// Reasoning: see the identical field on openAIChatResponse's
			// Message struct above for why this alias exists.
			Reasoning string `json:"reasoning,omitempty"`
			ToolCalls []struct {
				// Index is a POINTER because its zero value is meaningful
				// here: an upstream that omits the field (several do) sent no
				// index at all, and reading that as 0 merged every tool call
				// in the stream into one accumulator (2026-09-26 audit,
				// fifteenth round).
				Index    *int   `json:"index"`
				ID       string `json:"id,omitempty"`
				Type     string `json:"type,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function,omitempty"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason,omitempty"`
	} `json:"choices"`
	Usage *openAIUsage `json:"usage,omitempty"`
}

// startsANewToolCall reports whether an index-less tool-call delta begins the
// NEXT call rather than continuing the accumulated one: it carries a fresh
// identity (a name or an id) and the accumulated arguments are already a
// COMPLETE JSON object, so the call in progress is finished.
//
// It exists because an upstream that omits `index` gives the accumulator
// nothing to key on, and reading the missing index as 0 put every call in the
// stream into one slot: the client got a single tool_use named after the last
// call with all of the calls' arguments concatenated into {"_raw": ...}. The
// rules are deliberately conservative in the direction that keeps ONE call
// whole — a name repeated on every argument fragment of the same call (which
// several upstreams do) is not a new call, because the arguments accumulated
// so far are not yet valid JSON (2026-09-26 audit, fifteenth round).
func startsANewToolCall(accID, accName, accArgs, deltaID, deltaName string) bool {
	if accID == "" && accName == "" {
		return false
	}
	if deltaID != "" && accID != "" && deltaID != accID {
		return true
	}
	if deltaName == "" {
		return false
	}
	raw := strings.TrimSpace(accArgs)
	return raw != "" && json.Valid([]byte(raw))
}

// mapToolChoice converts an Anthropic ToolChoice to an OpenAI tool_choice value.
func mapToolChoice(tc *anthropic.ToolChoice) any {
	if tc == nil {
		return nil
	}
	switch tc.Type {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		return "none"
	case "tool":
		return map[string]any{
			"type": "function",
			"function": map[string]any{
				"name": tc.Name,
			},
		}
	}
	return nil
}

// chatRequestToOpenAI builds the OpenAI request body from an Ollama
// api.ChatRequest plus the original Anthropic fields that FromMessagesRequest
// folds into Options (it does not preserve max_tokens/temperature/top_p/stop
// as top-level Anthropic fields, so we read them back from Options).
func chatRequestToOpenAI(chatReq *api.ChatRequest, anthropicReq anthropic.MessagesRequest, upstreamModel string) openAIChatRequest {
	oai := openAIChatRequest{
		Model:     upstreamModel,
		Stream:    anthropicReq.Stream,
		MaxTokens: anthropicReq.MaxTokens,
		Tools:     chatReq.Tools,
	}

	// Options carried over by FromMessagesRequest.
	if v, ok := chatReq.Options["temperature"]; ok {
		if f, ok := toFloat64(v); ok {
			oai.Temperature = &f
		}
	}
	if v, ok := chatReq.Options["top_p"]; ok {
		if f, ok := toFloat64(v); ok {
			oai.TopP = &f
		}
	}
	if v, ok := chatReq.Options["top_k"]; ok {
		if n, ok := toInt(v); ok {
			oai.TopK = &n
		}
	}
	if v, ok := chatReq.Options["stop"]; ok {
		switch s := v.(type) {
		case []string:
			oai.Stop = s
		case []any:
			for _, x := range s {
				if str, ok := x.(string); ok {
					oai.Stop = append(oai.Stop, str)
				}
			}
		}
	}

	oai.ToolChoice = mapToolChoice(anthropicReq.ToolChoice)

	if anthropicReq.Stream {
		oai.StreamOptions = &struct {
			IncludeUsage bool `json:"include_usage"`
		}{IncludeUsage: true}
	}

	// Map messages.
	for _, m := range chatReq.Messages {
		om := openAIMessage{Role: m.Role, Content: m.Content, ToolCallID: m.ToolCallID}
		for _, img := range m.Images {
			om.Images = append(om.Images, openAIImageBlock{DataURL: imageDataURL(img)})
		}
		for _, tc := range m.ToolCalls {
			om.ToolCalls = append(om.ToolCalls, openAIToolCall{
				ID:   tc.ID,
				Type: "function",
				Function: openAIToolFunction{
					Name:      tc.Function.Name,
					Arguments: tc.Function.Arguments.String(),
				},
			})
		}
		oai.Messages = append(oai.Messages, om)
	}
	oai.Messages = normalizeSystemFirst(oai.Messages)
	return oai
}

// normalizeSystemFirst makes every system message contiguous at the start of
// the conversation. The Anthropic→OpenAI translation (and some clients) can
// place a system message AFTER a user turn — e.g. Claude Code's request arrives
// as [system, user, system]. Strict chat templates raise on that (KAT-Coder's
// apex GGUF: "System message must be at the beginning"), 500ing every request.
// Concatenate all system content into ONE leading system message and keep the
// non-system messages in their original order.
func normalizeSystemFirst(msgs []openAIMessage) []openAIMessage {
	// Leave an already-ordered conversation byte-for-byte alone. The rewrite
	// below is only needed for a system message that arrives AFTER a non-system
	// one, and applying it unconditionally re-rendered the common case too:
	// several leading system messages were concatenated into one string, so the
	// prompt the upstream received differed from the prompt the client sent
	// (and from the previous turn's, defeating any prefix cache keyed on the
	// rendered text). Identity here is what makes the common path a no-op.
	ordered := true
	blank := false
	seenNonSystem := false
	for _, m := range msgs {
		if m.Role == "system" {
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
		return msgs
	}
	var system []string
	var rest []openAIMessage
	for _, m := range msgs {
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
	out := make([]openAIMessage, 0, len(msgs))
	out = append(out, openAIMessage{Role: "system", Content: strings.Join(system, "\n\n")})
	return append(out, rest...)
}

// imageDataURL sniffs the image magic bytes for the data-URL MIME type
// (api.ImageData carries raw bytes with no type). Defaults to jpeg — vLLM's
// Qwen3.5 vision preprocessor accepts the common web formats.
func imageDataURL(img api.ImageData) string {
	mime := "image/jpeg"
	switch {
	case len(img) >= 8 && img[0] == 0x89 && img[1] == 'P' && img[2] == 'N' && img[3] == 'G':
		mime = "image/png"
	case len(img) >= 3 && img[0] == 'G' && img[1] == 'I' && img[2] == 'F':
		mime = "image/gif"
	case len(img) >= 12 && string(img[0:4]) == "RIFF" && string(img[8:12]) == "WEBP":
		mime = "image/webp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)
}

// toFloat64 coerces numeric-ish values from the Options map.
func toFloat64(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case float32:
		return float64(n), true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	case json.Number:
		f, err := n.Float64()
		return f, err == nil
	}
	return 0, false
}

// toInt reads an integer-valued option out of the map FromMessagesRequest
// builds. Values arrive as whatever JSON decoded them into — a top_k is a
// float64 from an untyped decode and an int from a typed one — so both are
// accepted, and a non-integral value is refused rather than truncated.
func toInt(v any) (int, bool) {
	switch n := v.(type) {
	case int:
		return n, true
	case int64:
		return int(n), true
	case float64:
		if n != math.Trunc(n) {
			return 0, false
		}
		return int(n), true
	case json.Number:
		i, err := n.Int64()
		return int(i), err == nil
	}
	return 0, false
}

// mapFinishReason converts an OpenAI finish_reason to an Ollama DoneReason
// that anthropic.mapStopReason understands ("stop"/"length"/"tool_calls").
func mapFinishReason(reason string) string {
	switch reason {
	case "stop", "length", "tool_calls":
		return reason
	case "content_filter":
		return "stop"
	case "":
		return ""
	}
	return reason
}

// parseOpenAIToolCalls builds api.ToolCall slice from an OpenAI message's
// tool_calls. arguments is a JSON string; unmarshal it into the ordered map.
func parseOpenAIToolCalls(tcs []struct {
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
}) []api.ToolCall {
	if len(tcs) == 0 {
		return nil
	}
	out := make([]api.ToolCall, 0, len(tcs))
	// seen drops a repeat of a call already emitted, keyed exactly as the
	// streaming converter keys its accumulator (id when the upstream sent one,
	// else name + canonical arguments). Without it two identical id-less calls
	// each got the SAME synthesized id and the response carried two tool_use
	// blocks sharing one id — a shape Anthropic never emits, and one that makes
	// the tool_result round-trip ambiguous (one tool_result satisfies both
	// blocks, and the follow-up request carries two tool messages with the same
	// tool_call_id). The streaming path already emitted ONE block for this same
	// body, so the two paths disagreed about it; they agree now
	// (2026-09-26 audit, fourteenth round).
	seen := make(map[string]bool, len(tcs))
	for _, tc := range tcs {
		var args api.ToolCallFunctionArguments
		raw := strings.TrimSpace(tc.Function.Arguments)
		if raw == "" {
			raw = "{}"
		}
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			// Fall back to a single-key map carrying the raw string so we
			// never drop a tool call entirely.
			args = api.NewToolCallFunctionArguments()
			args.Set("_raw", raw)
		}
		id := tc.ID
		if id == "" {
			// The upstream sent no id (several OpenAI-compatible GGUF
			// backends). Passing it through left the client with a tool_use
			// block carrying NO id at all — ContentBlock.ID is omitempty —
			// so it could not name the call back, and its tool_result then
			// carried tool_use_id "" into the next request as an empty OpenAI
			// tool_call_id. Same rule as the streaming path, one helper
			// (2026-09-26 audit, thirteenth round). The key is the canonical
			// encoding of the parsed arguments, so two parallel calls that
			// differ only in whitespace are one call.
			key := raw
			if b, err := json.Marshal(args); err == nil {
				key = string(b)
			}
			id = anthropic.ToolCallIDFor(tc.Function.Name, key)
		}
		dedupKey := id
		if tc.ID == "" {
			// Same key the streaming accumulator uses for an id-less call
			// (anthropic.go, toolCallsSent): the call's own identity.
			dedupKey = "\x00" + tc.Function.Name + "\x00" + id
		}
		if seen[dedupKey] {
			continue
		}
		seen[dedupKey] = true
		out = append(out, api.ToolCall{
			ID:       id,
			Function: api.ToolCallFunction{Name: tc.Function.Name, Arguments: args},
		})
	}
	return out
}

// openAIResponseToChatResponse builds an api.ChatResponse from a complete
// OpenAI chat-completions response (non-streaming).
func openAIResponseToChatResponse(resp openAIChatResponse, upstreamModel string) api.ChatResponse {
	chatResp := api.ChatResponse{
		Model: upstreamModel,
		Done:  true,
	}
	if len(resp.Choices) > 0 {
		// A reasoning-only completion (content empty, reasoning set) becomes a
		// thinking block and no text — deliberately, because that is exactly
		// what the streaming path produces for the same model: the converter
		// turns thinking deltas into a thinking block and never invents text
		// for content it did not receive. tools/gateway/messages.go surfaces
		// reasoning AS text when content is empty, but that is a different
		// surface with its own client; inventing an answer out of a model's
		// reasoning here would make the two proxy paths disagree about the
		// same upstream body (2026-09-26 audit, thirteenth round — reported
		// and deliberately not changed).
		c := resp.Choices[0]
		chatResp.Message = api.Message{
			Role:      c.Message.Role,
			Content:   c.Message.Content,
			Thinking:  reasoningOf(c.Message.ReasoningContent, c.Message.Reasoning),
			ToolCalls: nil,
		}
		chatResp.Message.ToolCalls = parseOpenAIToolCalls(c.Message.ToolCalls)
		chatResp.DoneReason = mapFinishReason(c.FinishReason)
	}
	if resp.Usage != nil {
		// Anthropic semantics: input_tokens is the UNCACHED part; the cached
		// prefix is reported separately as cache_read_input_tokens (see
		// anthropic.Usage) so a client's input+cache_read sum is the real
		// prompt length -- neither double-counted nor missing.
		chatResp.Metrics.PromptEvalCount = resp.Usage.PromptTokens - resp.Usage.cachedTokens()
		chatResp.Metrics.EvalCount = resp.Usage.CompletionTokens
	}
	return chatResp
}

// ListenAnthropicOpenAIProxy binds a loopback listener on a free port and
// returns it along with the chosen port. The caller runs
// RunAnthropicOpenAIProxy(ln, remote, upstreamModel) in a goroutine.
func ListenAnthropicOpenAIProxy(remote userRemote, upstreamModel string) (net.Listener, int, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, 0, err
	}
	return ln, ln.Addr().(*net.TCPAddr).Port, nil
}

// RunAnthropicOpenAIProxy serves an Anthropic /v1/messages endpoint that
// translates to OpenAI /v1/chat/completions and forwards to the given
// user-defined remote with the remote's api key. upstreamModel is the bare
// model id to send to the remote (e.g. "deepseek-v4-flash"). Blocks until the
// listener closes.
//
// A per-launch client token is ALWAYS generated and enforced (2026-09-01
// security audit H1): the proxy injects the remote's real key upstream, and
// loopback is shared with every other process/user on the box — an
// unauthenticated proxy let any local process spend the key. The token is
// returned so the caller can hand it to its child as the bearer credential
// (the real remote key never enters a child environment). Callers that
// genuinely cannot consume a token must not use this entry point.
func RunAnthropicOpenAIProxy(ln net.Listener, remote userRemote, upstreamModel string) (string, error) {
	token, err := newProxyClientToken()
	if err != nil {
		return "", fmt.Errorf("generate proxy client token: %w", err)
	}
	return token, RunAnthropicOpenAIProxyRoutes(ln, singleRemoteRouteTable(remote, upstreamModel, token))
}

// singleRemoteRouteTable is the one-leg route table shared by the three
// single-remote entry points (RunAnthropicOpenAIProxy,
// StartAnthropicOpenAIProxy, ServeAnthropicProxyForRemote): one user-defined
// remote, translated unless its wire is native Anthropic.
func singleRemoteRouteTable(remote userRemote, upstreamModel, token string) proxyRouteTable {
	return proxyRouteTable{
		ClientToken: token,
		Default: proxyRoute{BaseURL: remote.openAIBase(), Key: remote.key(), KeyEnv: remote.keyEnvName(), APIKeyEnv: remote.APIKeyEnv, ModelsURL: remote.modelsURL(), UpstreamModel: upstreamModel, Label: "remote:" + remote.Name, Wire: remote.Descriptor().Wire,
			// An anthropic-wire remote is forwarded untranslated to its own
			// /messages (see routeFor's doc for why); a single-leg launch of
			// one takes exactly the same path as a tier-planned one.
			NativePassthrough: remote.Descriptor().Wire == "anthropic"},
	}
}

// StartAnthropicOpenAIProxy is the async form of RunAnthropicOpenAIProxy for
// callers that must keep resolving (the agent shim path): it generates the
// per-launch client token, starts the serve loop in a goroutine, and returns
// the token immediately.
func StartAnthropicOpenAIProxy(ln net.Listener, remote userRemote, upstreamModel string) (string, error) {
	token, err := newProxyClientToken()
	if err != nil {
		return "", fmt.Errorf("generate proxy client token: %w", err)
	}
	go func() {
		_ = RunAnthropicOpenAIProxyRoutes(ln, singleRemoteRouteTable(remote, upstreamModel, token))
	}()
	return token, nil
}

// proxyRoute is one upstream an Anthropic request can be forwarded to.
type proxyRoute struct {
	BaseURL string // OpenAI base including the version prefix (".../v1")
	Key     string // bearer sent upstream when KeyEnv is empty; empty = none
	// KeyEnv, when set, is the environment variable resolveKey() re-reads on
	// every request instead of trusting Key — see RemoteEndpoint.TokenEnv's
	// doc for why a one-time-resolved credential is wrong for a long-lived
	// proxy process.
	KeyEnv string
	// APIKeyEnv is the WHOLE api_key_env spec behind KeyEnv, for
	// tierPlan.credentialEnvNames — the child-environment scrubber must
	// remove every name the row can read a credential from, and KeyEnv holds
	// only the one name that was set (RemoteEndpoint.TokenEnv's doc). See
	// RemoteEndpoint.APIKeyEnv.
	APIKeyEnv string
	// ModelsURL is where this route's model list lives when it is not
	// BaseURL+"/models" (the version prefix is per-surface — see
	// RemoteEndpoint.ModelsURL). The context-window probe uses it.
	ModelsURL     string
	UpstreamModel string // model id the upstream expects
	Label         string // for the request log / diagnostics
	// ContextWindow is this route's real max context in tokens (probed via
	// context_window_remote.go / the model manifest), 0 = unknown. Used to
	// clamp an outgoing request's max_tokens so prompt+max_tokens never
	// exceeds it -- see the context-fit clamp in the /v1/messages handler.
	ContextWindow int
	// NativePassthrough marks a route as native Anthropic (claude/*,
	// anthropic/*) forwarded raw to api.anthropic.com — see
	// native_anthropic_auth.go and the /v1/messages handler's early branch.
	// When true, BaseURL/Key/KeyEnv/UpstreamModel are unused: the handler
	// skips OpenAI translation, the context-fit clamp, and usage logging
	// entirely (no OAICA cost is incurred on this leg, nothing to meter —
	// 2026-09-02 decision) and sends the ORIGINAL request body through
	// untouched, with the model id Claude Code sent, unchanged.
	NativePassthrough bool
	// DisplayModel, when set, is the model id echoed back to Claude Code in
	// every response instead of the requested id — the real request still
	// goes upstream under the actual OAICA model. Exists because Claude
	// Code persists the model id a past response claimed per-turn, and on
	// `--resume` VALIDATES that recorded id against its own hardcoded
	// model catalog: a custom id like "oaica-35b-a3b-vision" fails that
	// check and gets silently replaced with "claude-sonnet-5" ("Session
	// model ... could not be restored", 2026-09-02) — this only bites a
	// native Anthropic primary, where Claude Code owns real session
	// persistence (every other primary re-injects our env vars fresh each
	// launch, so its own stale record never matters). DisplayModel fixes
	// the restore by presenting an id Claude Code already recognizes, kept
	// deliberately distinguishable from the real thing (see
	// oaicaDisplayModelSuffix) rather than a bare "claude-sonnet-5" — this
	// is our own client talking to our own backend, nothing is presented
	// to Anthropic, but a human reading the transcript later should still
	// be able to tell the response didn't come from Anthropic's Sonnet.
	DisplayModel string
	// Weight, when >0, makes this route eligible for RouteWeighted's
	// consistent-hash distribution across ALL healthy legs (base +
	// Fallbacks) instead of the normal failover-only behavior where
	// Fallbacks sit idle until the base route's breaker opens — see
	// weightedRing in route_policy.go. 0 (the default for every existing
	// plan) opts a route OUT of weighted distribution: it stays a
	// failover-only leg, so this field is additive and changes nothing
	// for callers that never set it.
	Weight int
	// Wire is the protocol this route's upstream speaks: "" / "openai" means
	// the OpenAI-translation path (<base>/chat/completions, the default)
	// and "anthropic" means the remote serves /v1/messages natively.
	//
	// An anthropic-wire REMOTE (BaseURL set) rides the same passthrough a
	// native claude/* leg does — its Anthropic body goes upstream untouched
	// except for the model id (anthropicPassthroughTarget). This matters
	// because the translation path is the only way a remote could otherwise
	// be reached, and it cannot reach an Anthropic endpoint at all: z.ai's
	// /api/anthropic answers 404 {"detail":"Not Found"} to /chat/completions
	// (2026-09-25, "502 upstream HTTP 404" in Claude Code). opencode talks
	// to these plans through the Anthropic SDK and never had the problem;
	// this is that, in our proxy.
	Wire string
}

// oaicaDisplayModelSuffix marks a DisplayModel id as OAICA's own, not
// Anthropic's, while still being a suffix Claude Code's model-recognition
// tolerates on ids it already knows (unlike an unrecognized prefix, which
// would just trigger the exact restore failure DisplayModel exists to
// avoid) — see DisplayModel's doc.
const oaicaDisplayModelSuffix = "-oaica"

// resolveKey returns the bearer to send upstream, live: KeyEnv wins whenever
// it is set and currently non-empty in the environment (so exporting or
// rotating it takes effect on the very next request), falling back to the
// value resolved at launch time otherwise — either because the remote uses
// a literal api_key (KeyEnv empty) or because the env var is unset right
// now (rare; keeps old behavior rather than suddenly sending no credential).
// anthropicPassthroughTarget resolves where an anthropic-wire route's
// /v1/messages request goes and which credential it carries. Empty ok means
// the route is not an Anthropic-wire passthrough at all (the caller then
// takes the OpenAI-translation path).
//
// A remote's BaseURL already carries its version prefix (".../v1"), so the
// upstream is BaseURL + "/messages" — the same arithmetic the OpenAI path
// does with "/chat/completions". A native claude/* leg has no BaseURL and
// resolves the credential the user's own `claude /login` stored
// (native_anthropic_auth.go), which is what running unproxied would have
// used.
func (route proxyRoute) anthropicPassthroughTarget() (upstream, headerName, headerValue string, ok bool) {
	if route.Wire != "anthropic" {
		return "", "", "", false
	}
	if route.BaseURL != "" {
		key := route.resolveKey()
		if key == "" {
			// The `anthropic` catalog row is api.anthropic.com itself: a user
			// who signed in with `claude /login` has an OAuth session there
			// and no API key at all, so fall back to the native credential
			// (ANTHROPIC_API_KEY or that session) rather than refusing. Any
			// other vendor's keyless row still fails closed.
			if !isAnthropicAPIBase(route.BaseURL) {
				return "", "", "", false
			}
			auth, found := resolveNativeAnthropicAuth()
			if !found {
				return "", "", "", false
			}
			return strings.TrimRight(route.BaseURL, "/") + "/messages", auth.Header, auth.Value, true
		}
		return strings.TrimRight(route.BaseURL, "/") + "/messages", "x-api-key", key, true
	}
	auth, found := resolveNativeAnthropicAuth()
	if !found {
		return "", "", "", false
	}
	return nativeAnthropicUpstream, auth.Header, auth.Value, true
}

// anthropicRemoteModelsTarget is anthropicPassthroughTarget's /models
// sibling: same upstream base and same credential, one path segment over, for
// proxying GET /v1/models. Ok is false for a native claude/* leg (no BaseURL
// to list from) — its caller forwards to api.anthropic.com instead.
func (route proxyRoute) anthropicRemoteModelsTarget() (upstream, headerName, headerValue string, ok bool) {
	if route.BaseURL == "" {
		return "", "", "", false
	}
	upstream, headerName, headerValue, ok = route.anthropicPassthroughTarget()
	if !ok {
		return "", "", "", false
	}
	// A row that declares models_path serves its list there, not at the
	// sibling of its messages path — the same rule the translated branch below
	// follows, and the rule doctor and the context-window probe already
	// resolve through (2026-09-26 audit, third round: this branch was missed
	// when the translated one was fixed). No shipped row is affected today —
	// the one models_path row is wire "openai" — so this is the latent half.
	if route.ModelsURL != "" {
		return route.ModelsURL, headerName, headerValue, true
	}
	return strings.TrimSuffix(upstream, "/messages") + "/models", headerName, headerValue, true
}

// isAnthropicAPIBase reports whether a route's base URL points at
// api.anthropic.com itself (the `anthropic` catalog row), where the native
// credential — an OAuth session from `claude /login` or ANTHROPIC_API_KEY —
// is a valid substitute for a per-row key.
func isAnthropicAPIBase(baseURL string) bool {
	u, err := url.Parse(strings.TrimSpace(baseURL))
	if err != nil {
		return false
	}
	return strings.EqualFold(u.Hostname(), "api.anthropic.com")
}

func (route proxyRoute) resolveKey() string {
	if route.KeyEnv != "" {
		if v := strings.TrimSpace(os.Getenv(route.KeyEnv)); v != "" {
			return v
		}
	}
	return route.Key
}

// proxyRouteTable maps the model id Claude Code puts in each request to an
// upstream. Unknown ids fall back to Default with the id passed through
// unchanged (a single-model launch keeps working byte-identically: every
// tier is pinned to one id, which is either in ByModel or equals
// Default.UpstreamModel). This is what lets primary and --sonnet-model live
// on different remotes, or one on a remote and one on the local daemon.
type proxyRouteTable struct {
	Default proxyRoute
	ByModel map[string]proxyRoute
	// ClientToken, when set, is the only credential the proxy accepts from
	// its caller (Authorization: Bearer <t> or x-api-key: <t>). The proxy
	// listens on loopback but loopback is shared with every other process
	// and user on the box; without this, anyone local could spend the
	// launcher's real upstream keys. Claude.Run generates one per launch
	// and hands it to Claude Code as ANTHROPIC_AUTH_TOKEN, so the real keys
	// never enter the child environment. Empty = no check, which today means
	// only a test that builds a proxyRouteTable by hand: all three shipped
	// single-remote entry points generate one (the standalone
	// serve-anthropic-proxy subcommand included, which is why it PRINTS it).
	ClientToken string
	// SessionID, when set, is sent upstream as X-Session-Id on every
	// request this proxy forwards. It exists for load balancers that offer
	// consistent-hash routing (e.g. oaicalb's session_hash_addr): pinning
	// one launched Claude Code session to the same backend replica for its
	// whole lifetime lets that replica's own prefix cache actually get
	// reused turn-to-turn, instead of every turn risking a leastconn hop to
	// a cold-cache replica. One proxy process = one launched session is the
	// natural boundary here (a fresh `oaica launch claude`, including a
	// `resume`, starts a fresh proxy), so newProxySessionID is called once
	// per launch, not per request. Empty = no header sent (older callers,
	// or a backend with no session-aware LB in front of it — harmless
	// either way, since a leastconn-only LB just ignores an unknown header).
	SessionID string
	// Policy controls what happens when the selected route's upstream is
	// failing — see route_policy.go. Empty = RouteLocalFirst defaults.
	Policy routePolicy
	// Fallbacks are the OTHER legs of the plan (primary + each distinct
	// secondary endpoint), used (per Policy) only when the selected route's
	// breaker is OPEN and never mid-stream. Empty = no fallback, byte
	// identical to the pre-route-policy behavior.
	Fallbacks []proxyRoute
	// breakers is shared via pointer so table value-copies (the handler
	// closes over one, the poll goroutine gets another) see the same state.
	breakers *routeBreakers
	// escalations is the `auto` policy's per-session escalation state (see
	// route_policy.go), also shared via pointer for the same reason. Keyed
	// by SessionID; nil-safe and simply never escalates when unset.
	escalations *routeEscalations
	// Oversize (--oversize <model>) is the larger-context leg for requests
	// THIS leg's window cannot hold — the auto-compaction call being the
	// canonical case on a 262k local backend. Rule lives in the handler's
	// context-fit clamp (oversizeSwap) and honors Policy's pinned
	// localities and the breaker like everything else. ContextWindow is
	// probed at launch (withContextWindows) and must exceed the primary's.
	Oversize proxyRoute
	// FamilyLegs maps a Claude model FAMILY ("opus", "sonnet", "haiku",
	// "fable") to the plan leg that owns that TIER. Built by buildTierPlan
	// (tierFamilyRoutes). See resolve() for why it exists: Claude Code's
	// opusplan mode resolves its opus and haiku slots from its OWN built-in
	// catalog, NOT from ANTHROPIC_DEFAULT_{OPUS,HAIKU}_MODEL, so the request
	// arrives carrying a real Anthropic id ("claude-haiku-4-5-…") no matter
	// what we put in the environment — probing 2026-09-25 set both vars to
	// sentinel values and watched only the sonnet slot's value reach the
	// wire. Without this map such an id can only fall to Default, which sends
	// the haiku tier to the PRIMARY's model — the token-cost bug this fixed,
	// and one it only actually fixes when the configured haiku leg is
	// reachable by family, native or not (a haiku_model pointing at a remote
	// or router leg is the common case). Empty = every family id goes to
	// Default: identical to the pre-2026-09-25 behaviour for every id a
	// single-leg plan carries (its Default.UpstreamModel IS the requested
	// bare id). The one difference is ids the family matcher now recognises
	// as Claude-shaped where the old predicate did not — a bare "fable",
	// "anthropic" or "anthropic-<x>" — which used to be forwarded upstream
	// verbatim and now take the default leg's model instead (strictly a fix:
	// no backend serves those as model names).
	FamilyLegs map[string]proxyRoute
}

// authorized reports whether r presents the table's client token.
func (t proxyRouteTable) authorized(r *http.Request) bool {
	if t.ClientToken == "" {
		return true
	}
	got := strings.TrimSpace(strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer"))
	if got == "" {
		got = strings.TrimSpace(r.Header.Get("x-api-key"))
	}
	return got != "" && subtle.ConstantTimeCompare([]byte(got), []byte(t.ClientToken)) == 1
}

// newProxyClientToken makes the per-launch credential (32 random bytes, hex).
func newProxyClientToken() (string, error) {
	b := make([]byte, 32)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "oaica-proxy-" + hex.EncodeToString(b), nil
}

// newProxySessionID returns a per-launch identifier for proxyRouteTable.SessionID
// (see its doc for why this is per-launch, not per-request). Only needs to
// be unique enough that a consistent-hash LB doesn't collide two unrelated
// sessions onto the same bucket by chance — 16 random bytes is far more
// than that requires, matching newProxyClientToken's margin.
func newProxySessionID() (string, error) {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		return "", err
	}
	return "oaica-session-" + hex.EncodeToString(b), nil
}

// proxyUpstreamClient bounds connection setup, never the response: a slow
// local model may legitimately stream for longer than any fixed timeout,
// and the request already carries the caller's context, which cancels
// when Claude Code disconnects. (A 5-minute Client.Timeout here truncated
// long streams -- review of 2026-08-26.)
var proxyUpstreamClient = &http.Client{Transport: &http.Transport{
	DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	MaxIdleConnsPerHost: 8,
	IdleConnTimeout:     90 * time.Second,
}}

// proxyUpstreamMaxRetries bounds client-side retrying of transient upstream
// failures, mirroring what the official Anthropic/OpenAI SDKs do client-side
// (anthropic: default 2 retries; openai-go: 3): a client SDK that hit a
// fleet hiccup would sit and back off instead of surfacing the error into
// the agent loop. 2026-08-31 context: the gateway now sheds load via
// standard signals -- 429 (+Retry-After) from per-key concurrency /
// large-context admission, 502 when a replica flips DOWN, 504 headers on
// long prefills -- and Claude Code behind this proxy should absorb the
// cheap ones instead of showing the user "API Error".
//
// Retry discipline (what makes this safe):
//   - ONLY before a response comes back (and therefore before any byte is
//     written to the caller): a mid-stream failure is NOT retried here,
//     because we may have already streamed tokens to Claude Code. The
//     caller sees a clean upstream_error instead.
//   - Idempotency: a completion that never answered did no billed work the
//     client can observe; the one risk is double-billed hidden work, which
//     the 429/503 paths (rejected BEFORE backend work) never incur. 502/504
//     do NOT have that property — both mean the request reached a backend (or
//     a gateway in front of one) that may well have run the whole prefill
//     before failing to answer, so re-POSTing the body sends that work a
//     second time; upstream is billed twice and, on a long-prefill timeout,
//     the retry is very likely to time out the same way. They are surfaced
//     instead of retried (2026-09-26 audit: the set listed them while this
//     comment argued only for 429/503, i.e. up to 3 duplicate full-body
//     sends of a prompt the backend had already processed).
//   - Backoff: Retry-After honored verbatim (the server tuned it), else
//     exponential 500ms/1s/2s with ±25% full jitter, 10s ceiling, and the
//     caller's context always wins (Claude Code disconnect cancels the
//     wait).
var proxyUpstreamMaxRetries = 3

func proxyUpstreamRetryDelay(resp *http.Response, attempt int) time.Duration {
	if resp != nil {
		if v := resp.Header.Get("Retry-After"); v != "" {
			if secs, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && secs > 0 {
				// Cap server-requested waits too: a broken upstream asking
				// for an hour must not wedge one Claude Code turn.
				if secs > 10 {
					secs = 10
				}
				return time.Duration(secs) * time.Second
			}
		}
	}
	// Full-ish jitter, AWS-recommended shape: uniform in [d/2, 3d/2).
	// attempt is 0-based here; the shift expresses 500ms * 2^attempt.
	d := time.Duration(500) * time.Millisecond << uint(attempt)
	jitter, err := rand.Int(rand.Reader, big.NewInt(int64(d/2)))
	if err != nil {
		jitter = big.NewInt(0) // crypto/rand unavailable: plain deterministic backoff still bounds retries
	}
	return d/2 + time.Duration(jitter.Int64())
}

func proxyUpstreamRetryable(resp *http.Response) bool {
	switch resp.StatusCode {
	case http.StatusTooManyRequests,
		http.StatusServiceUnavailable:
		return true
	}
	return false
}

func proxyUpstreamRetryDo(req *http.Request, reqBytes []byte) (*http.Response, error) {
	var lastResp *http.Response
	var lastErr error
	for attempt := 0; attempt < proxyUpstreamMaxRetries; attempt++ {
		attemptReq := req
		if attempt > 0 {
			// The body reader is consumed on the first attempt; replace it
			// (reqBytes is a buffered copy -- see the call site).
			attemptReq = req.Clone(req.Context())
			attemptReq.Body = io.NopCloser(bytes.NewReader(reqBytes))
		}
		resp, err := proxyUpstreamClient.Do(attemptReq)
		if err != nil {
			// Transport errors before any response are safe to retry EXCEPT
			// when the caller themselves hung up (context canceled) -- that
			// is not an upstream failure, and retrying would write to a
			// dead connection.
			if req.Context().Err() != nil {
				return nil, err
			}
			lastErr = err
			lastResp = nil
			if attempt == proxyUpstreamMaxRetries-1 {
				return nil, lastErr
			}
			delay := proxyUpstreamRetryDelay(nil, attempt)
			select {
			case <-time.After(delay):
				continue
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		if proxyUpstreamRetryable(resp) && attempt < proxyUpstreamMaxRetries-1 {
			delay := proxyUpstreamRetryDelay(resp, attempt)
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			select {
			case <-time.After(delay):
				continue
			case <-req.Context().Done():
				return nil, req.Context().Err()
			}
		}
		return resp, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	return lastResp, nil
}

func (t proxyRouteTable) resolve(requested string) (proxyRoute, string) {
	if requested == "" {
		return t.Default, t.Default.UpstreamModel
	}
	if r, ok := t.ByModel[requested]; ok {
		return r, r.UpstreamModel
	}
	// Unknown ids pass through by default — a single-remote tier split
	// (userRemoteEnvVars: ANTHROPIC_DEFAULT_SONNET_MODEL=<bare id> with NO
	// ByModel route for it) relies on exactly that.
	// EXCEPT real Anthropic family ids (claude-haiku-4-5-20251001 etc.):
	// Claude Code's own background calls (topic detection, title gen) send
	// those regardless of our ANTHROPIC_DEFAULT_HAIKU_MODEL, and no backend
	// of ours knows them — forwarding raw made the upstream 404. Map them
	// onto the leg that owns that family when the plan has one
	// (FamilyLegs), else onto the default leg as before.
	if family, ok := claudeModelFamily(requested); ok {
		if r, ok := t.FamilyLegs[family]; ok {
			return r, r.UpstreamModel
		}
		return t.Default, t.Default.UpstreamModel
	}
	return t.Default, requested
}

// claudeModelFamily reports which Claude family a model id names, for the
// ids Claude Code sends on its own: the bare tiers it resolves internally
// ("haiku", "opus", …), the "claude"/"anthropic." prefixes, and the real
// catalog ids of a family (claude-haiku-4-5-20251001 → "haiku",
// claude-fable-5-1 → "fable"). The family, not the exact id, is what the
// routing table can act on: the point release changes under us, the tier
// does not. Anything that is not an Anthropic id at all (glm-5.3,
// oaica-35b-a3b-vision) reports false — it must reach its upstream unchanged.
func claudeModelFamily(model string) (string, bool) {
	if model == "" {
		return "", false
	}
	switch {
	case model == "claude" || model == "anthropic" ||
		strings.HasPrefix(model, "anthropic.") || strings.HasPrefix(model, "anthropic-"):
		return "claude", true
	case isBareClaudeTier(model):
		return model, true
	}
	bare, ok := strings.CutPrefix(model, "claude-")
	if !ok {
		return "", false
	}
	for _, tier := range []string{"opus", "sonnet", "haiku", "fable"} {
		if bare == tier || strings.HasPrefix(bare, tier+"-") {
			return tier, true
		}
	}
	// Legacy versioned ids put the date and generation BEFORE the tier
	// (claude-3-5-sonnet-20241022, claude-3-opus-20240229) — those reached the
	// un-tagged fallback below and so billed the primary leg whatever the user
	// had configured for that tier. Match the tier as a hyphen-delimited
	// SEGMENT instead; the id got here by starting with "claude-", so a
	// segment match cannot fire on a foreign id. List order breaks any tie.
	for _, tier := range []string{"opus", "sonnet", "haiku", "fable"} {
		if strings.Contains(bare, "-"+tier+"-") || strings.HasSuffix(bare, "-"+tier) {
			return tier, true
		}
	}
	// Still no tier in it: shaped like an Anthropic id, so the default leg
	// owns it — never a raw forward to an upstream that would 404 it.
	return "claude", true
}

// isBareClaudeTier reports whether model is one of the tier names Claude Code
// resolves on its own, with no version attached.
func isBareClaudeTier(model string) bool {
	switch model {
	case "opus", "sonnet", "haiku", "fable":
		return true
	}
	return false
}

// RunAnthropicOpenAIProxyRoutes is RunAnthropicOpenAIProxy with a routing
// table; see proxyRouteTable.
func RunAnthropicOpenAIProxyRoutes(ln net.Listener, table proxyRouteTable) error {
	setRequestLogProxyPort(ln)
	baseURL := table.Default.BaseURL

	// Breaker/escalation state: created unconditionally. The messages
	// handler records a breaker signal on every upstream result, and an
	// escalation signal on every result that belongs to the leg selectRoute
	// chose (the oversize crossover's does not — crossoverEscalationLeg).
	// Nil-safe no-ops today, but a stale/future caller path that derefs
	// without the nil guard would nil-panic on a plain single-leg launch —
	// these are two empty structs; creating them always costs nothing.
	if table.breakers == nil {
		table.breakers = &routeBreakers{}
	}
	if table.escalations == nil {
		table.escalations = &routeEscalations{}
	}
	// Route-policy fallback legs present: a background probe keeps the
	// breakers honest (route_policy.go). Without Fallbacks there is nothing
	// to break over — no probe, identical to before.
	if len(table.Fallbacks) > 0 || table.Oversize.BaseURL != "" {
		pollCtx, cancelPoll := context.WithCancel(context.Background())
		defer cancelPoll() // aborts in-flight probes' HTTP requests too
		go table.startRouteHealthPoll(pollCtx, 30*time.Second)
	}

	// Per-session prompt-size calibration for the context-fit clamp below --
	// see context_calibration.go for the 2026-08-29 incident that made a
	// pure chars/4 estimate untenable. Scoped to this proxy instance (one
	// per launch) so nothing leaks between runs or between tests.
	calib := newPromptCalibrator(maxCalibratedSessions)

	mux := http.NewServeMux()

	// GET /v1/models — proxy straight to the remote's /models endpoint so
	// Claude Code's model probes resolve. The remote already speaks OpenAI
	// /models (z.ai serves it under /v4).
	//
	// When the DEFAULT route is native Anthropic passthrough, baseURL is
	// empty (NativePassthrough routes carry no BaseURL — see
	// proxyRoute.NativePassthrough's doc) and this endpoint had nothing
	// real to proxy to — objectively wrong regardless of what queries it.
	// Forward to the real Anthropic API instead. NOTE (2026-09-02): this
	// was added while chasing a native-primary + haiku-only-split failure
	// ("issue with the selected model (fable)", opusplan rejecting a
	// documented-valid alias) on the theory that opusplan's SDK-side
	// validation reads this endpoint — that theory did NOT hold; the bug
	// is still open. This fix stands on its own merit (a native default's
	// /v1/models must answer something real) but is not a fix for that bug.
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !table.authorized(r) {
			writeAnthropicError(w, http.StatusUnauthorized, "missing or invalid proxy token")
			return
		}
		// The model list is served on the DEFAULT leg's credentials, so it is
		// the default leg's entitlement that governs it — a caller denied that
		// leg must not still get its credential-backed inventory (2026-09-26
		// audit, second round). Empty reqModel: no single model is being asked
		// for, only the catalogue.
		//
		// The gate sits on the branches that serve a LEG THIS GATE GOVERNS, and
		// deliberately not on api.anthropic.com: entitlement.go's contract —
		// restated in entitlement_passthrough_test.go — is that the native
		// claude/* path is not gated, because it is the user's own credential
		// against Anthropic's own API rather than a self-hosted or user-remote
		// backend. A gate placed above this branch judged the native leg while
		// POST /v1/messages on the same leg stayed open: denied catalogue, then
		// served completions (2026-09-26 audit, third round).
		if table.Default.NativePassthrough {
			// The discriminator is the same one /v1/messages uses: an
			// anthropic-WIRE remote has a base URL, and a truly native claude/*
			// leg is the only kind with none (routeFor marks both
			// NativePassthrough — see the flag's doc).
			if table.Default.Wire == "anthropic" && table.Default.BaseURL != "" {
				// Anthropic-wire REMOTE (a plan row: zai-coding-plan and
				// friends): a user-remote leg, so it is gated exactly as its
				// /v1/messages is, and it answers with its OWN list. When its
				// target cannot be resolved — a row whose key is not set —
				// this used to fall through to nativeAnthropicModelsPassthrough
				// and answer a vendor's leg out of api.anthropic.com, under the
				// user's own Anthropic credential and past the gate, where
				// /v1/messages for the same row says "no credential for <model>
				// — run `oaica auth login`". A 404 from the row's own endpoint
				// is harmless: context_window_remote.go falls back to the
				// catalog's declared window.
				if allowed, reason := checkEntitlement(r, table.Default.Label, ""); !allowed {
					writeAnthropicError(w, http.StatusForbidden, reason)
					return
				}
				upstream, headerName, headerValue, ok := table.Default.anthropicRemoteModelsTarget()
				if !ok {
					writeAnthropicError(w, http.StatusUnauthorized,
						fmt.Sprintf("no credential for %s — run `oaica auth login %s`, or set the key's env var", table.Default.UpstreamModel, strings.TrimPrefix(table.Default.Label, "remote:")))
					return
				}
				anthropicModelsPassthrough(w, r, upstream, headerName, headerValue)
				return
			}
			nativeAnthropicModelsPassthrough(w, r)
			return
		}
		if allowed, reason := checkEntitlement(r, table.Default.Label, ""); !allowed {
			writeAnthropicError(w, http.StatusForbidden, reason)
			return
		}
		// ModelsURL when the row declares one: a per-surface-version remote
		// (Perplexity's version:"none" + models_path:"/v1/models") serves its
		// list somewhere other than <base>/models, and doctor and the
		// context-window probe both already resolve through it — the proxy
		// hard-wired the concatenation and answered those rows with the
		// host's 404 (2026-09-26 audit).
		modelsURL := baseURL + "/models"
		if table.Default.ModelsURL != "" {
			modelsURL = table.Default.ModelsURL
		}
		proxyPassThrough(w, r, modelsURL, table.Default.resolveKey())
	})

	// /health — a trivial liveness probe so callers can confirm the proxy is up.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// POST /v1/messages — the real work.
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		// A turn this proxy refuses LOCALLY — a bad proxy token, a body over
		// the cap, an unparseable request, a denied entitlement, a prompt the
		// window cannot hold — used to return with no row at all: the row was
		// built further down, behind every one of those gates, so `oaica usage`
		// reported ERR 0 for a session whose every turn was refused here,
		// including the prompt-too-long 400 (the auto-compaction case the
		// report exists to show). The row is created up front and written ONLY
		// by refuse(); a turn that reaches a leg logs its own row — the
		// translated path below, and the passthrough legs inside their own
		// functions — so nothing is counted twice (2026-09-26 audit, ninth
		// round).
		var refusalRow *requestLogEntry
		refusalStarted := time.Now()
		if r.Method == http.MethodPost {
			refusalRow = &requestLogEntry{
				Timestamp: time.Now().UTC().Format(time.RFC3339),
				Path:      r.URL.Path,
				Backend:   table.Default.Label + " " + redactBaseURL(table.Default.BaseURL),
			}
		}
		refuse := func(code int, msg string) {
			if refusalRow != nil {
				refusalRow.StatusCode = code
				refusalRow.DurationMs = time.Since(refusalStarted).Milliseconds()
				appendRequestLog(*refusalRow)
			}
			writeAnthropicError(w, code, msg)
		}

		if r.Method != http.MethodPost {
			refuse(http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !table.authorized(r) {
			refuse(http.StatusUnauthorized, "missing or invalid proxy token")
			return
		}
		// Bounded: this body is written by the launched client, and an
		// unbounded read of it is the proxy process's peak memory
		// (2026-09-26 audit).
		body, err := httpbody.ReadCapped(r.Body, httpbody.DefaultMax, "the request body")
		if err != nil {
			refuse(http.StatusRequestEntityTooLarge, "read body: "+err.Error())
			return
		}

		// A body that is not a JSON object is not an Anthropic Messages
		// request, and the decode below will not say so: json.Unmarshal
		// ACCEPTS a literal `null` into a struct as a documented no-op that
		// reports no error. Downstream that became a nil map, and assigning
		// the rewritten model into it panicked the handler on every
		// Anthropic-wire remote leg. net/http recovers the panic per
		// connection, so the client saw a dropped connection rather than an
		// error, and the panic ran before the request-log defer was
		// registered — so nothing, anywhere, recorded the failure
		// (2026-09-26 audit, thirteenth round).
		if !jsonObjectBody(body) {
			refuse(http.StatusBadRequest, "invalid Anthropic request: the body is not a JSON object")
			return
		}

		var anthReq anthropic.MessagesRequest
		if err := json.Unmarshal(body, &anthReq); err != nil {
			refuse(http.StatusBadRequest, "invalid Anthropic request: "+err.Error())
			return
		}
		// The body-derived features the report classifies on, filled in for a
		// refusal that happens after this point (a denied entitlement, a prompt
		// too long). A refusal BEFORE it — the token check, the body cap — had
		// no body to read them from and logs zeroes: there the row's job is to
		// record that the attempt happened at all.
		if refusalRow != nil {
			refusalRow.Model = anthReq.Model
			refusalRow.LastMessageLen, refusalRow.TotalMessagesLen = extractLastAndTotalMessageLen(body)
			refusalRow.HardSignalMatch = requestLogHardSignalRE.MatchString(string(body))
			refusalRow.WouldBeHardByLen = refusalRow.LastMessageLen > requestLogHardLengthThreshold ||
				refusalRow.TotalMessagesLen > requestLogHardLengthThreshold*3
		}

		// Native Anthropic passthrough (claude/*, anthropic/* tiers,
		// proxyRoute.NativePassthrough) branches out here, before OpenAI
		// translation — a native leg speaks Anthropic wire already, so
		// there is nothing to convert, no context-fit clamp to apply (we
		// don't know its real window and don't need to — Anthropic
		// enforces its own), and no usage/cost to log (no OAICA billing on
		// this leg at all, see nativeAnthropicPassthrough's doc). Routed on
		// the SAME anthReq.Model lookup every other tier uses, so opusplan
		// mixing a native primary with a native secondary still lands each
		// request on the right upstream model.
		// The upstream model selectRoute resolves is deliberately NOT bound
		// here: every leg inside this block names the model it sends
		// explicitly (route.UpstreamModel — see the gate below), and the
		// translated path that does need it resolves it again for its own
		// scope.
		if route, _, _ := table.selectRoute(anthReq.Model); route.NativePassthrough {
			// Set BEFORE either branch returns. A passthrough leg is answered
			// here and never reaches the assignment below, so the header was
			// missing on exactly the legs a silent swap is hardest to see —
			// while the comment there promises it is "always" answered
			// (2026-09-26 audit).
			w.Header().Set("X-Oaica-Route", route.Label)
			if route.Wire == "anthropic" && route.BaseURL != "" {
				// An Anthropic-wire REMOTE (a plan row like zai-coding-plan)
				// is the self-hosted/user-remote class entitlement.go's rule
				// names, so it goes through the same gate as every other
				// remote; returning above that call meant the one class the
				// rule promised it covers never reached it (2026-09-26
				// audit). The native claude/* leg below stays ungated: it is
				// api.anthropic.com under the user's own credential, neither
				// self-hosted nor user-remote.
				//
				// route.UpstreamModel, not reqModel: this branch REWRITES the
				// body to the leg's own model on the next line, so for any
				// spelling selectRoute does not recognise the two differ and
				// the gate judged what the client typed while the upstream was
				// sent — and the user billed for — something else. A policy
				// keyed on the model was bypassed by asking for
				// "vendor/glm-5.3-not-this", which lands on the same leg and
				// goes out as glm-5.3 (2026-09-26 audit). The translated path
				// passes reqModel because reqModel IS what it writes upstream,
				// and the crossover passes route.UpstreamModel for this same
				// reason — the gate must be asked about the model that is
				// SPENT, whichever spelling produced it.
				if allowed, reason := checkEntitlement(r, route.Label, route.UpstreamModel); !allowed {
					refuse(http.StatusForbidden, reason)
					return
				}
				// Its upstream wants the plan's own model id, not the picker
				// string Claude Code sent ("zai-coding-plan/glm-5.3" →
				// "glm-5.3"); everything else in the body goes through
				// untouched. A native claude/* leg needs no rewrite — its
				// UpstreamModel is the CLI alias the client already sent.
				rewritten, rerr := rewriteAnthropicRequestModel(body, route.UpstreamModel)
				if rerr != nil {
					refuse(http.StatusInternalServerError, "rewrite model for anthropic remote: "+rerr.Error())
					return
				}
				upstream, headerName, headerValue, ok := route.anthropicPassthroughTarget()
				if !ok {
					refuse(http.StatusUnauthorized,
						fmt.Sprintf("no credential for %s — run `oaica auth login %s`, or set the key's env var", route.UpstreamModel, strings.TrimPrefix(route.Label, "remote:")))
					return
				}
				status, relayed := anthropicPassthrough(w, r, rewritten, upstream, headerName, headerValue, table.SessionID)
				feedPassthroughRouteHealth(table, route, table.SessionID, passthroughBreakerKey(route, false), route.BaseURL,
					status, relayed, r.Context().Err() != nil)
				return
			}
			// attempted is not folded into clientGone here: the two mean
			// different things (the client left / this side never sent), and
			// the first is what the feed's clientGone documents. A wrapper
			// that refused locally reports passthroughNotAttempted, which the
			// feed handles on its own.
			// Checked here as well as inside the passthrough wrapper so the
			// refusal is a logged row: this is the one error the user must see,
			// and a native leg is the only refusal path that used to write none
			// (2026-09-26 audit, tenth round — the sibling anthropic-wire remote
			// branch below has always refused through refuse()).
			if _, ok := resolveNativeAnthropicAuth(); !ok {
				refuse(http.StatusUnauthorized, noNativeAnthropicCredential)
				return
			}
			status, relayed, _ := nativeAnthropicPassthrough(w, r, body, table.SessionID)
			feedPassthroughRouteHealth(table, route, table.SessionID, passthroughBreakerKey(route, false), route.BaseURL,
				status, relayed, r.Context().Err() != nil)
			return
		}

		chatReq, err := anthropic.FromMessagesRequest(anthReq)
		if err != nil {
			refuse(http.StatusBadRequest, "convert request: "+err.Error())
			return
		}

		// Tier-aware routing: Claude Code's opusplan mode (and its normal
		// Opus/Sonnet/Haiku tiering) sends a DIFFERENT model id per request
		// depending which ANTHROPIC_DEFAULT_*_MODEL env var it's currently
		// acting under — anthReq.Model carries that id. Honor it when set so
		// claude.go can pin distinct upstream models per tier (e.g. a
		// stronger model for planning, a cheaper one for execution) through
		// this SAME remote/proxy. Falls back to the fixed upstreamModel this
		// proxy was started with when the request carries none, so a normal
		// single-model launch (all tiers pinned to the same bare id) is
		// unaffected — byte-identical to before this existed.
		// With a routing table (tier_routing.go) the id also selects WHICH
		// upstream: primary and --sonnet-model can be different backends.
		route, reqModel, _ := table.selectRoute(anthReq.Model)
		// The leg the `auto` escalation may hear about — the one selectRoute
		// just chose and noted. It stops being that leg if an oversize
		// crossover serves this request somewhere else (see
		// crossoverEscalationLeg), and every record below goes through it
		// rather than route.BaseURL for exactly that reason.
		escalationLeg := route.BaseURL
		// Always answer with the leg that will actually serve this request:
		// our gateway logs it as routed_to for spend attribution, and it
		// makes a silent --sonnet-model/fallback swap diagnosable from the
		// client side. A NativePassthrough leg returns before this line, so
		// it sets the same header at its own entry (2026-09-26 audit).
		w.Header().Set("X-Oaica-Route", route.Label)

		// Off by default (see entitlement.go) — a hook point for a future
		// license/entitlement product decision, not one made here.
		if allowed, reason := checkEntitlement(r, route.Label, reqModel); !allowed {
			refuse(http.StatusForbidden, reason)
			return
		}

		started := time.Now()
		// The route is resolved from here down, so a refusal past this point is
		// about THAT leg, not the default one the row was created with.
		if refusalRow != nil {
			refusalRow.Backend = route.Label + " " + redactBaseURL(route.BaseURL)
		}

		oaiReq := chatRequestToOpenAI(chatReq, anthReq, reqModel)
		// Context-length-fit clamp -- real 2026-08-29 incident: Claude
		// Code's own automatic-context-compaction call failed outright
		// with "maximum context length is 262144 tokens... requested
		// 230145 input + 32000 output = 262145" -- one token over, with no
		// recovery path except /clear. CLAUDE_CODE_AUTO_COMPACT_WINDOW
		// (contextEnvVars, context_window_remote.go) already reserves
		// 32000 tokens as a soft advisory, but Claude Code's own token
		// counting can drift a few tokens from ours -- and the compaction
		// call is exactly the request most likely to land right on the
		// edge, since it fires BECAUSE the session is already near the
		// limit. This clamp is the hard guarantee: whatever Claude Code
		// asked for, never forward a request already doomed to 400. len
		// (body)/4 is the same coarse chars/4 estimate tools/gateway uses
		// server-side for the identical purpose -- consistent, not exact
		// by design.
		//
		// 2026-08-30: chars/4 is no longer the only estimator. calibKey
		// identifies the conversation; once one successful response has
		// reported its REAL usage.prompt_tokens we scale by the measured
		// tokens-per-byte for THIS session instead (context_calibration.go).
		// table.SessionID is the same value we send upstream as X-Session-Id
		// just below, so client and server calibrate on the same key.
		// Per leg as well as per session: see legCalibrationKey — one leg's
		// measured sample must never set another leg's margin.
		calibKey := legCalibrationKey(table.SessionID, route)
		if route.ContextWindow > 0 {
			// contextFitMarginRatio is NOT a flat token count -- a real
			// 2026-08-29 recurrence (same incident class, 22x in one
			// session, on the server-side gateway's identical clamp)
			// proved a fixed 2048-token margin isn't remotely enough: the
			// chars/4 estimate for that request was 183,315 tokens against
			// a REAL upstream count of 230,145 -- a 26% underestimate,
			// because dense code/tool-schema content tokenizes more
			// compactly than chars/4 assumes, and the miss scales with
			// prompt size. 30% is a deliberate buffer above that observed
			// error, not a guess -- still a heuristic, not a hard
			// guarantee, but matches the server-side gateway's identical
			// fix for consistency.
			//
			// 2026-08-30 UPDATE: that 30%-of-chars/4 pair is now only the
			// UNCALIBRATED path (first request of a session). It is kept
			// exactly as it was because it is the well-tested safe default,
			// but it is also what rejected an ~806 KB compaction call whose
			// real prompt was ~243,000 tokens against a 262,144 window --
			// est 201,670 x 1.30 = 262,171, over by a rounding error, while
			// ~19,000 tokens of real headroom sat unused. Once we have a
			// measured tokens-per-byte for this session, contextFitPlan uses
			// it with a 3% margin instead, and that whole failure mode goes
			// away.
			estTokens, margin, _ := contextFitPlan(calib, calibKey, len(body))
			fitBudget := route.ContextWindow - estTokens - margin
			// minViableCompletion: a real 2026-08-30 recurrence proved the
			// OLD unconditional "floor fitBudget at 256" rule was itself
			// unsafe -- that request's real prompt was already within 255
			// tokens of the ceiling on its own, so flooring max_tokens to
			// 256 still produced a request guaranteed to 400 upstream. When
			// the real prompt leaves less room than this, there is no safe
			// positive max_tokens to force -- reject client-side with a
			// clear reason instead of forwarding one still doomed to fail.
			const minViableCompletion = 16
			if fitBudget < minViableCompletion {
				// Oversize crossover (route_policy.go): this leg cannot hold
				// the request (the auto-compaction call being the canonical
				// case near a 262k ceiling) and a strictly larger-context
				// --oversize leg exists → serve on it instead of rejecting.
				// Re-derive the budget against the new leg's window.
				if over, swapped := table.oversizeSwap(route, estTokens, margin); swapped {
					// Every refusal inside this crossover is a judgement about
					// the leg being swapped TO, so the row must name it.
					if refusalRow != nil {
						refusalRow.Backend = over.Label + " " + redactBaseURL(over.BaseURL)
					}
					if over.NativePassthrough {
						// A native oversize leg has no probed ContextWindow
						// (it's always 0 — see oversizeSwap's doc) and no
						// clamp of ours applies: Anthropic enforces its own
						// real window. Redirect here, with the ORIGINAL
						// Anthropic-shaped body (not oaiReq, which only
						// exists for the OpenAI-translation path this leg
						// skips entirely) — same branch nativeAnthropicPassthrough
						// takes at the top of this handler for a native
						// PRIMARY, just reached via the oversize swap instead.
						// The body's own "model" field still carries the
						// OAICA leg's id (the client sent it for THAT
						// tier) — rewrite to the REAL Anthropic model id
						// before forwarding (resolveNativeModelAlias:
						// over.UpstreamModel is the CLI alias, "fable",
						// which Anthropic's wire API does not accept on
						// its own — confirmed live, "model: fable" not
						// found, 2026-09-02).
						// A REMOTE anthropic-wire leg (over.Wire == "anthropic",
						// over.BaseURL set) is the one exception: its
						// UpstreamModel is already the vendor's real id
						// ("glm-5.3"), not a claude-tier alias, so it goes
						// through unchanged and out to the remote's own
						// /messages with the remote's key.
						// BaseURL, not Wire, is the discriminator: the plan
						// builds the NATIVE leg with Wire == "anthropic"
						// too (tier_routing.go's sourceNativeAnthropic
						// branch), so keying the rewrite on Wire skipped
						// resolveNativeModelAlias for the one leg it
						// exists for and forwarded "fable" to
						// api.anthropic.com (the 2026-09-02 incident
						// above, re-opened on the oversize crossover).
						// A remote anthropic-wire leg is the leg WITH a
						// BaseURL; the native passthrough is the one
						// without.
						realModel := over.UpstreamModel
						if over.BaseURL == "" {
							realModel = resolveNativeModelAlias(r.Context(), over.UpstreamModel)
						}
						nativeBody, rerr := rewriteAnthropicRequestModel(body, realModel)
						if rerr != nil {
							refuse(http.StatusInternalServerError, "rewrite model for oversize crossover: "+rerr.Error())
							return
						}
						w.Header().Set("X-Oaica-Route", over.Label)
						// The gate above ran against the leg the request
						// STARTED on; this crossover serves it on a
						// DIFFERENT leg, so the leg that actually spends
						// upstream time has to be the one evaluated
						// (2026-09-26 audit — a gate denying the oversize
						// label let the request through because the
						// pre-swap label had been allowed).
						//
						// ... on the classes that rule covers. A native
						// claude/* leg (no BaseURL) is api.anthropic.com under
						// the user's own credential and is deliberately NOT
						// gated — the primary path says so in its own comment
						// above, and gating the crossover onto it denied a turn
						// the primary path would have served. The discriminator
						// is the same one the model rewrite above uses
						// (2026-09-26 audit, ninth round).
						if over.BaseURL != "" {
							if allowed, reason := checkEntitlement(r, over.Label, over.UpstreamModel); !allowed {
								refuse(http.StatusForbidden, reason)
								return
							}
						}
						if over.Wire == "anthropic" {
							upstream, headerName, headerValue, ok := over.anthropicPassthroughTarget()
							if !ok {
								refuse(http.StatusUnauthorized, fmt.Sprintf("no credential for %s — run `oaica auth login %s`, or set the key's env var", over.UpstreamModel, strings.TrimPrefix(over.Label, "remote:")))
								return
							}
							status, relayed := anthropicPassthrough(w, r, nativeBody, upstream, headerName, headerValue, table.SessionID)
							feedPassthroughRouteHealth(table, over, table.SessionID, passthroughBreakerKey(over, true),
								crossoverEscalationLeg(route.BaseURL, over.BaseURL), status, relayed, r.Context().Err() != nil)
							return
						}
						if _, ok := resolveNativeAnthropicAuth(); !ok {
							refuse(http.StatusUnauthorized, noNativeAnthropicCredential)
							return
						}
						status, relayed, _ := nativeAnthropicPassthrough(w, r, nativeBody, table.SessionID)
						feedPassthroughRouteHealth(table, over, table.SessionID, passthroughBreakerKey(over, true),
							crossoverEscalationLeg(route.BaseURL, over.BaseURL), status, relayed, r.Context().Err() != nil)
						return
					}
					route = over
					// The leg changed under this request, so the slot the
					// calibration is read from and written to changes with
					// it: the response about to be measured belongs to `over`,
					// not to the leg that could not hold the request.
					calibKey = legCalibrationKey(table.SessionID, route)
					// The crossover serves a leg on another host, so this
					// request's result is not evidence about the leg
					// selectRoute chose. The breaker below/above still records
					// against `over` itself; escalation is dropped, explicitly.
					escalationLeg = crossoverEscalationLeg(escalationLeg, over.BaseURL)
					oaiReq.Model = route.UpstreamModel
					w.Header().Set("X-Oaica-Route", route.Label)
					// Same reason as the passthrough crossover above: the
					// request is answered on `over`, so `over` is the leg
					// the gate has to judge (2026-09-26 audit).
					if allowed, reason := checkEntitlement(r, route.Label, route.UpstreamModel); !allowed {
						refuse(http.StatusForbidden, reason)
						return
					}
					fitBudget = route.ContextWindow - estTokens - margin
				}
			}
			if fitBudget < minViableCompletion {
				// Anthropic's own wording -- see promptTooLongMessage for
				// why the exact phrasing is load-bearing for Claude Code's
				// recovery path.
				refuse(http.StatusBadRequest,
					promptTooLongMessage(estTokens, route.ContextWindow-minViableCompletion))
				return
			}
			if oaiReq.MaxTokens > fitBudget {
				oaiReq.MaxTokens = fitBudget
			}
		}
		oaiBody, err := json.Marshal(oaiReq)
		if err != nil {
			refuse(http.StatusInternalServerError, "marshal openai request: "+err.Error())
			return
		}

		// Same local-only log the router path used to keep (request_log.go):
		// model, which backend, sizes, status -- never content.
		//
		// Built BEFORE the upstream call, not after it: the row used to be
		// created behind the request, so a transport failure (refused
		// connection, DNS, TLS, timeout) returned early and left no row at
		// all — `oaica usage` reported ERR 0 for a session whose every turn
		// failed, which is the failure a user opens the report to find
		// (2026-09-26 audit).
		//
		// The status logged is the one the CLIENT ended up with, not the one
		// the upstream answered with before its body was read: an upstream
		// that sends a JSON error object over HTTP 200 becomes a 502 for the
		// client, and logging the 200 made `oaica usage` report a session
		// whose every turn failed as errors: 0 (2026-09-26 audit). The
		// breaker below still keys on the UPSTREAM status, deliberately —
		// writeUpstreamError collapses a 4xx to a 502, and a 4xx is the leg
		// working, not failing.
		lastLen, totalLen := extractLastAndTotalMessageLen(body)
		entry := requestLogEntry{
			Timestamp:        time.Now().UTC().Format(time.RFC3339),
			Model:            anthReq.Model,
			Path:             r.URL.Path,
			Backend:          route.Label + " " + redactBaseURL(route.BaseURL),
			LastMessageLen:   lastLen,
			TotalMessagesLen: totalLen,
			HardSignalMatch:  requestLogHardSignalRE.MatchString(string(body)),
			WouldBeHardByLen: lastLen > requestLogHardLengthThreshold || totalLen > requestLogHardLengthThreshold*3,
			DurationMs:       time.Since(started).Milliseconds(),
		}

		upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, route.BaseURL+"/chat/completions", bytes.NewReader(oaiBody))
		if err != nil {
			// `entry` exists by now (it is built just above), so this failure is
			// recorded on it rather than dropped (2026-09-26 audit, ninth
			// round). Its defer is registered below this point, so this append
			// is the only one for this request.
			entry.StatusCode = http.StatusInternalServerError
			appendRequestLog(entry)
			writeAnthropicError(w, http.StatusInternalServerError, "build upstream request: "+redactErr(err).Error())
			return
		}
		upstreamReq.Header.Set("Content-Type", "application/json")
		if resolvedKey := route.resolveKey(); resolvedKey != "" {
			upstreamReq.Header.Set("Authorization", "Bearer "+resolvedKey)
		}
		if table.SessionID != "" {
			upstreamReq.Header.Set("X-Session-Id", table.SessionID)
		}

		resp, err := proxyUpstreamRetryDo(upstreamReq, oaiBody)
		if err != nil {
			// Retries exhausted against a transport failure: feed the circuit
			// breaker so later requests skip this leg immediately.
			//
			// Unless the CALLER hung up: the upstream request is built on
			// r.Context(), so a client that cancels mid-flight (Ctrl-C on a
			// slow turn, any client with a deadline) surfaces here as the
			// same transport error a dead leg produces. proxyUpstreamRetryDo
			// already draws that line ("that is not an upstream failure");
			// the health feeds must too, or three abandoned turns take a
			// healthy leg's circuit out for breakerOpenFor and, at two under
			// `auto`, send the session to another tier (2026-09-26 audit).
			if r.Context().Err() == nil {
				// Same signal feeds the `auto` policy's per-session escalation
				// (route_policy.go): consecutive failures escalate the session to
				// the stronger secondary leg. Only when the leg that failed is
				// the one selectRoute chose — escalationLeg is "" after an
				// oversize crossover onto another host, which is a signal the
				// escalation state does not track.
				table.breakers.recordFail(route.BaseURL)
				if escalationLeg != "" {
					table.escalations.recordFail(table.SessionID, escalationLeg)
				}
			}
			// The attempt is logged even though it never reached a backend:
			// a leg that refuses connections is the case an ERR column exists
			// for, and the one it used to miss (2026-09-26 audit).
			entry.StatusCode = http.StatusBadGateway
			appendRequestLog(entry)
			writeAnthropicError(w, http.StatusBadGateway, "upstream request failed: "+redactErr(err).Error())
			return
		}
		defer resp.Body.Close()
		// Feed the circuit breaker (route_policy.go): 5xx-class answers that
		// survived the retry budget count as failures. 4xx (bad request,
		// context overflow) and 429 (shedding, alive) are the leg WORKING and
		// must not open the breaker. The `auto` policy's per-session
		// escalation eats the same signals (route_policy.go): a success clears
		// the consecutive-failure counter — but NOT an active escalation,
		// which only decays after autoEscalateHoldFor so one lucky 200 on the
		// secondary can't bounce the session back onto a still-flapping
		// primary.
		//
		// The 2xx verdict is deliberately NOT taken here. The status byte
		// proves the leg ANSWERED, not that the turn arrived: two shapes this
		// proxy already recognises answer 200 and deliver nothing — a JSON
		// error object over HTTP 200 (upstreamErrorMessage), and a stream cut
		// short after the headers — and feeding those as successes gave a leg
		// that can never finish a turn a perfect health record, so its circuit
		// never opened and `auto` never escalated the session off it
		// (2026-09-26 audit). It is decided below, on what reached the client.
		if resp.StatusCode >= 500 {
			table.breakers.recordFail(route.BaseURL)
			if escalationLeg != "" {
				table.escalations.recordFail(table.SessionID, escalationLeg)
			}
		}

		rec := &statusCapturingWriter{ResponseWriter: w}
		w = rec
		// delivered is set by whichever relay runs below: false means the
		// leg's response never became a turn, whatever its status byte said.
		delivered := false
		defer func() {
			entry.StatusCode = rec.status()
			if !delivered && rec.status() < 300 && r.Context().Err() == nil {
				// The turn failed after the headers were already a 200 (a
				// stream cut short mid-answer): the client got an error event,
				// so the row must not read as a clean turn. `oaica usage`
				// counts its ERR column off this field, and a session in which
				// every turn was truncated reported ERR 0 (2026-09-26 audit).
				entry.StatusCode = http.StatusBadGateway
			}
			// Duration is set HERE, not where the entry was built: the row is
			// now constructed before the upstream call, so a duration captured
			// at construction would report the marshalling time and nothing
			// else — a field that means "how long the request took" would
			// silently become zero for every successful request.
			entry.DurationMs = time.Since(started).Milliseconds()
			appendRequestLog(entry)
		}()

		if resp.StatusCode != http.StatusOK {
			// A diagnostic, not an artifact: bounded so an upstream that
			// answers an error with a gigabyte cannot become this process's
			// memory (2026-09-26 audit).
			text := strings.TrimSpace(string(httpbody.ReadCappedOrEmpty(resp.Body, httpbody.DiagnosticMax, "the upstream error body")))
			// An upstream context overflow is not an opaque backend failure:
			// it is the SAME condition the clamp above tries to predict, and
			// vLLM states the real numbers. Two things follow. (1) Re-emit it
			// as Anthropic's "prompt is too long: N tokens > M maximum" 400
			// so Claude Code takes its context-recovery path instead of
			// retrying the identical request forever (the 2026-08-29
			// compaction loop). (2) The reported in-the-messages count is
			// ground truth for this session's tokens-per-byte -- better than
			// anything we can estimate -- so seed the calibration with it and
			// the very next request gets a measured budget.
			if resp.StatusCode == http.StatusBadRequest {
				if promptTokens, maxTokens, ok := parseUpstreamContextOverflow(text); ok {
					calib.record(calibKey, len(body), promptTokens)
					writeAnthropicError(w, http.StatusBadRequest, promptTooLongMessage(promptTokens, maxTokens))
					return
				}
			}
			// redactURL the upstream body/transport error before it reaches
			// the child (audit 2026-09-01 L1): url.Error embeds the full URL
			// — userinfo in a misconfigured remotes.json base_url would land
			// in an LLM-driven child's context. The key this leg injected goes
			// through redactSecret on top of those shapes — a vendor that names
			// the credential it refused is the ordinary rejection, and its text
			// is prose no pattern can recognise (2026-09-26 audit, ninth round).
			writeUpstreamError(w, resp, text, route.resolveKey())
			return
		}

		// Record the REAL prompt size for this session -- only from a usage
		// object the upstream actually sent on a 200 (see
		// context_calibration.go). Never from an error, never a guess.
		recordUsage := func(promptTokens int) { calib.record(calibKey, len(body), promptTokens) }

		// The response echoes DisplayModel when the route sets one (see its
		// doc — native+split session restore) instead of the real
		// upstream model id; the REAL id already went upstream above via
		// reqModel in chatRequestToOpenAI, unaffected by this.
		displayModel := reqModel
		if route.DisplayModel != "" {
			displayModel = route.DisplayModel
		}
		// The converter's fallback for an upstream that reports NO usage:
		// this session's calibrated tokens-per-byte when it has one, else
		// chars/4. An upstream that omits usage (a stream that ignores
		// stream_options.include_usage, a gateway that strips the usage
		// object) used to produce a hard "input_tokens":0, which Claude Code
		// believes — its context accounting never grew and auto-compaction
		// never fired, the 2026-08-30 wall. The converter has carried this
		// fallback since it was written (anthropic.go's "use actual metrics
		// if available, otherwise use estimate"); the call site hard-wired
		// the estimate to 0, which disabled it (2026-09-26 audit).
		estInputTokens, _, _ := contextFitPlan(calib, calibKey, len(body))
		if anthReq.Stream {
			delivered = handleStreamResponse(w, resp.Body, displayModel, recordUsage, estInputTokens, route.resolveKey())
		} else {
			delivered = handleNonStreamResponse(w, resp.Body, displayModel, recordUsage, estInputTokens, route.resolveKey())
		}
		// Now the 2xx verdict (see the note at the status feed above). A
		// request the client abandoned mid-turn says nothing about the leg —
		// the same line the transport-error branch and
		// feedPassthroughRouteHealth draw (2026-09-26 audit).
		if resp.StatusCode < 300 {
			switch {
			case r.Context().Err() != nil:
			case delivered:
				table.breakers.recordOK(route.BaseURL)
				if escalationLeg != "" {
					table.escalations.recordOK(table.SessionID, escalationLeg)
				}
			default:
				table.breakers.recordFail(route.BaseURL)
				if escalationLeg != "" {
					table.escalations.recordFail(table.SessionID, escalationLeg)
				}
			}
		}
	})

	srv := &http.Server{Handler: mux}
	return srv.Serve(ln)
}

// handleNonStreamResponse reads one complete OpenAI JSON response, builds an
// api.ChatResponse, and emits an Anthropic MessagesResponse as JSON.
// onUsage, when non-nil, is called with the upstream's real
// usage.prompt_tokens so the caller can calibrate its prompt-size estimate.
// estInputTokens is the prompt count to report in place of a flat zero when
// the upstream sent no usage object at all.
//
// It returns whether the turn reached the client. False is not "the status was
// bad" — it is "this response was never a turn", which is what the caller's
// route health must be fed (2026-09-26 audit).
//
// secret is the credential this leg injected upstream, so an error object that
// names it is redacted literally as well as by shape (see
// redactUpstreamDiagnosis); "" is fine when the leg carries no key.
func handleNonStreamResponse(w http.ResponseWriter, body io.Reader, upstreamModel string, onUsage func(int), estInputTokens int, secret string) bool {
	// Bounded: the size of a whole turn is the upstream's choice
	// (2026-09-26 audit).
	respBody, err := httpbody.ReadCapped(body, httpbody.DefaultMax, "the upstream response")
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "read upstream body: "+redactErr(err).Error())
		return false
	}
	// An upstream can answer a JSON error object over HTTP 200 (vLLM and this
	// fleet's own gateway both do), and the streaming path refuses that shape
	// (see upstreamErrorMessage). Here it decoded as an empty response: the
	// client got no content and stop_reason "", the prompt estimate was
	// written into input_tokens, and the route's breaker recorded a healthy
	// 200 — a dead turn reported as a successful one that consumed tokens
	// (2026-09-26 audit).
	if msg := upstreamErrorMessage(string(respBody), secret); msg != "" {
		writeAnthropicError(w, http.StatusBadGateway, "upstream error: "+msg)
		return false
	}
	var oaiResp openAIChatResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "decode upstream response: "+redactErr(err).Error())
		return false
	}
	if len(oaiResp.Choices) == 0 {
		// A 200 that carries no completion is not a finished turn. Accepting
		// it meant the client was told the model answered nothing
		// (stop_reason end_turn, zero content blocks), was billed a prompt
		// that never existed, and the leg recorded a healthy 200 forever —
		// for the shapes a misconfigured or intercepted upstream actually
		// returns: `{"detail":"Not Found"}`, a bare `{}`, a health payload,
		// or an empty choices array. upstreamErrorMessage above recognises
		// only a top-level error OBJECT, and openAIResponseToChatResponse
		// has no else for an empty array. The streaming path already refuses
		// this same body with 502, and the server-side sibling refuses it
		// too ("upstream returned no completion choices",
		// tools/gateway/messages.go) — the verdict must not depend on
		// `stream: true` (2026-09-26 audit, thirteenth round).
		writeAnthropicError(w, http.StatusBadGateway, "upstream returned no completion choices")
		return false
	}
	if onUsage != nil && oaiResp.Usage != nil && oaiResp.Usage.PromptTokens > 0 {
		onUsage(oaiResp.Usage.PromptTokens)
	}
	chatResp := openAIResponseToChatResponse(oaiResp, upstreamModel)
	// Per FIELD, not per object: an object that states one count says nothing
	// about the other, and gating on the object suppressed the fallback for
	// the field the upstream never spoke about — the "session never appears to
	// grow" failure, reached through a partial statement (2026-09-26 audit,
	// thirteenth round).
	if !oaiResp.Usage.statedPromptTokens() && estInputTokens > 0 {
		// An estimate is the only honest number available for a prompt the
		// upstream did not count, and it keeps the client's context
		// accounting moving — see the note at the streaming call site.
		chatResp.Metrics.PromptEvalCount = estInputTokens
	}
	if !oaiResp.Usage.statedCompletionTokens() && oaiResp.Choices[0].Message.Content != "" {
		chatResp.Metrics.EvalCount = len(oaiResp.Choices[0].Message.Content)/4 + 1
	}
	anthResp := anthropic.ToMessagesResponse(anthropic.GenerateMessageID(), chatResp)
	anthResp.Usage.CacheReadInputTokens = oaiResp.Usage.cachedTokens()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	_ = enc.Encode(anthResp)
	return true
}

// handleStreamResponse reads OpenAI SSE chunks from body, feeds incremental
// api.ChatResponse values through a single anthropic.StreamConverter, and
// writes each returned StreamEvent as an Anthropic SSE event.
// onUsage, when non-nil, is called with the real usage.prompt_tokens from
// the stream's final usage-only chunk (stream_options.include_usage) so the
// caller can calibrate its prompt-size estimate. estInputTokens is the
// converter's fallback when no usage chunk ever arrives.
//
// It returns whether the whole answer reached the client. False means the
// stream was cut short or the upstream reported an error mid-answer — the
// status byte may already have been a 200 either way, which is why the
// caller's route health is fed this and not the status (2026-09-26 audit).
//
// secret is the credential this leg injected upstream; every recognition site
// below renders the upstream's own text to the client, so it is passed to
// upstreamErrorMessage for the literal redaction (see redactUpstreamDiagnosis).
func handleStreamResponse(w http.ResponseWriter, body io.Reader, upstreamModel string, onUsage func(int), estInputTokens int, secret string) bool {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "streaming not supported by response writer")
		return false
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	// The 200 is deliberately NOT written here. An upstream that fails before
	// producing a single frame (a JSON error body over HTTP 200, a refused
	// prefill) can still be reported with a real error status, which is what
	// makes the client SDK retry instead of accepting an empty turn
	// (2026-09-26 audit). Headers go out with the first event.
	started := false

	conv := anthropic.NewStreamConverter(anthropic.GenerateMessageID(), upstreamModel, estInputTokens)

	scanner := bufio.NewScanner(body)
	// DeepSeek streams can emit sizeable reasoning_content lines; raise the
	// per-line cap to 8 MiB so we don't bail mid-token on long thinking runs.
	scanner.Buffer(make([]byte, 0, 64*1024), 8*1024*1024)

	type toolAccum struct {
		id   string
		name string
		args strings.Builder
	}
	toolAccums := map[int]*toolAccum{}
	// syntheticToolSlot is the accumulator slot for streams whose upstream
	// omits tool-call indices; it advances when a delta starts a new call
	// (see the accumulation loop below).
	syntheticToolSlot := 0
	finishReason := ""
	var finalUsage *openAIUsage
	// nonSSE collects the lines that carry no "data:" prefix. An upstream that
	// ignores stream:true answers a stream request with a WHOLE completion as
	// one JSON body, and the frame reader below would otherwise walk it line by
	// line, match nothing, and report a truncated stream (see the adoption in
	// the tail below).
	var nonSSE strings.Builder

	emitErr := func(msg string) {
		// Mid-stream: the status is already 200 and bytes are already sent,
		// so the only honest signal left is the protocol's own error event.
		// Claude Code surfaces it and retries the turn; a silent
		// message_stop would leave a truncated answer looking complete
		// (2026-09-26 audit).
		ev := anthropic.StreamErrorEvent{
			Type:  "error",
			Error: anthropic.Error{Type: "api_error", Message: msg},
		}
		data, err := json.Marshal(ev)
		if err != nil {
			return
		}
		fmt.Fprintf(w, "event: error\ndata: %s\n\n", string(data))
		flusher.Flush()
	}

	emit := func(events []anthropic.StreamEvent) {
		if len(events) == 0 {
			return
		}
		if !started {
			started = true
			w.WriteHeader(http.StatusOK)
		}
		for _, e := range events {
			data, err := json.Marshal(e.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Event, string(data))
		}
		flusher.Flush()
	}

	// truncation is closed over rather than passed in: it is decided by the
	// finish_reason the stream carried, and it changes what a fragment means.
	flushToolCalls := func(truncated bool) {
		// Emit any accumulated tool calls in index order as one ChatResponse.
		if len(toolAccums) == 0 {
			return
		}
		// Stable order by index.
		indices := make([]int, 0, len(toolAccums))
		for i := range toolAccums {
			indices = append(indices, i)
		}
		// Simple sort.
		for i := 0; i < len(indices); i++ {
			for j := i + 1; j < len(indices); j++ {
				if indices[j] < indices[i] {
					indices[i], indices[j] = indices[j], indices[i]
				}
			}
		}
		var tcs []api.ToolCall
		for _, i := range indices {
			a := toolAccums[i]
			var args api.ToolCallFunctionArguments
			raw := strings.TrimSpace(a.args.String())
			if raw == "" {
				raw = "{}"
			}
			if err := json.Unmarshal([]byte(raw), &args); err != nil {
				if truncated {
					// The turn ended at the token limit with argument JSON that
					// never parsed: this is a fragment the model was still
					// writing, not a call. Emitting it as {"_raw": …} handed the
					// agent a complete, executable tool_use whose input is a key
					// the model never wrote — and mapStopReason then reported
					// "tool_use" for a turn the upstream had stopped with
					// finish_reason "length", so the truncation was invisible
					// too (2026-09-26 audit, round 16). Dropped rather than
					// fabricated; the done event below then reports max_tokens.
					continue
				}
				// Not a truncation: a model that emits freeform (non-JSON)
				// arguments. Keep the fragment under _raw as the non-streaming
				// path deliberately does, so the call is not lost entirely.
				args = api.NewToolCallFunctionArguments()
				args.Set("_raw", raw)
			}
			tcs = append(tcs, api.ToolCall{
				ID:       a.id,
				Function: api.ToolCallFunction{Name: a.name, Arguments: args},
			})
		}
		toolAccums = map[int]*toolAccum{}
		if len(tcs) == 0 {
			// Every accumulated call was a truncated fragment. Emitting an
			// empty ToolCalls response would still make the converter say
			// tool_use (it keys on the message, not the block), which is the
			// same false claim about a turn that produced nothing runnable.
			return
		}
		chatResp := api.ChatResponse{Model: upstreamModel, Message: api.Message{ToolCalls: tcs}}
		emit(conv.Process(chatResp))
	}

	// A stream is COMPLETE only when the upstream said so: a finish_reason on
	// a choice, or the [DONE] sentinel. Without one of those we have no idea
	// whether the answer we relayed was the whole answer, so the tail below
	// reports a failure instead of a clean turn (2026-09-26 audit).
	completed := false
	upstreamErr := ""
	// streamedText counts the characters actually sent to the client, so the
	// done event can report a usable output count even when the upstream
	// sent no usage (see the tally below).
	streamedText := 0
	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			// Not every failure arrives as an SSE frame: a 200 whose body is
			// a plain JSON error object has no "data:" prefix at all, and
			// used to be skipped line by line until the stream "ended"
			// cleanly with nothing in it.
			if m := upstreamErrorMessage(line, secret); m != "" {
				upstreamErr = m
			}
			if int64(nonSSE.Len()) < httpbody.DefaultMax {
				nonSSE.WriteString(line)
				nonSSE.WriteByte('\n')
			}
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			// The sentinel says the stream ENDED, not that it ever held a
			// turn. An upstream that opens a stream and terminates it with
			// [DONE] and no choice (an aborted generation, a gateway
			// stripping frames) sent nothing, and believing the sentinel
			// alone reported a successful EMPTY turn — a prompt billed and
			// closed with message_stop the client could not tell from a real
			// empty answer — and recorded the leg healthy, so its breaker
			// never opened and `auto` never moved the session off it. Both
			// sibling paths already refuse this exact condition
			// (handleNonStreamResponse's `len(Choices) == 0`,
			// adoptNonSSECompletion); the tail below now does too, including
			// the adoption attempt (2026-09-26 audit, fifteenth round).
			if started || len(toolAccums) > 0 || finishReason != "" {
				completed = true
			}
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			// Some upstreams report a mid-stream failure as an SSE frame
			// carrying an error object rather than a choice.
			if m := upstreamErrorMessage(payload, secret); m != "" {
				upstreamErr = m
			}
			continue
		}
		// A frame that PARSES can still be that same error object: it
		// unmarshals cleanly into a chunk with no error field and no choices,
		// so the recognition above never saw it — the loop body did nothing
		// and the client was told only "stream ended before the response was
		// complete", with the upstream's actual reason discarded (2026-09-26
		// audit, seventh round). The cheap substring guard keeps the probe off
		// the ordinary delta frames an answer is made of.
		if strings.Contains(payload, `"error"`) {
			if m := upstreamErrorMessage(payload, secret); m != "" {
				upstreamErr = m
				continue
			}
		}

		// A whole completion can arrive INSIDE an SSE frame, not only as a
		// bare JSON body: an upstream that emulates streaming around a
		// non-streaming backend answers stream:true with one data: frame
		// whose choice carries `message` instead of `delta`. The reader below
		// models only delta, so that frame parsed cleanly, contributed
		// nothing, and the finish_reason it carried completed the turn — the
		// client got 200, a plausible usage line and EMPTY content while the
		// upstream's real answer (or its tool calls) was discarded, and the
		// leg was recorded healthy (2026-09-26 audit, fifteenth round). Route
		// the frame through the same adoption the unframed body uses, so the
		// two shapes cannot disagree. `!started` keeps a mid-stream whole
		// message from being adopted as the turn.
		//
		// The gate is a SHAPE test, not a substring of the bytes: the first
		// version looked for the characters "message" anywhere in the frame,
		// and a delta frame carrying JSON — {"message": …} written by the model,
		// a `Write` argument naming that key — matched it. That frame was then
		// adopted as a whole completion with an EMPTY message, the rest of the
		// stream was discarded, and the client got 200 with no answer and a
		// healthy leg (2026-09-26 audit, sixteenth round).
		if !started && len(toolAccums) == 0 && frameCarriesWholeCompletion(payload) {
			if adoptNonSSECompletion(payload, conv, emit, onUsage, upstreamModel, &finishReason, &finalUsage, &streamedText) {
				completed = true
				break
			}
		}

		for _, choice := range chunk.Choices {
			d := choice.Delta

			// Reasoning content → thinking delta.
			if reasoning := reasoningOf(d.ReasoningContent, d.Reasoning); reasoning != "" {
				cr := api.ChatResponse{Model: upstreamModel, Message: api.Message{Thinking: reasoning}}
				emit(conv.Process(cr))
			}

			// Text content delta.
			if d.Content != "" {
				streamedText += len(d.Content)
				cr := api.ChatResponse{Model: upstreamModel, Message: api.Message{Content: d.Content}}
				emit(conv.Process(cr))
			}

			// Tool-call deltas — accumulate by index; flush later.
			for _, tc := range d.ToolCalls {
				slot := 0
				if tc.Index != nil {
					slot = *tc.Index
				} else {
					// The upstream sent no index, so there is nothing to key
					// on but the calls' own sequence: a delta that carries a
					// fresh identity (a name or an id) while the accumulated
					// arguments are already a COMPLETE JSON object begins the
					// next call. Without this every index-less call in the
					// stream landed in one accumulator (Go's zero value) and
					// the client got ONE tool_use named after the last call,
					// with all the calls' arguments concatenated into
					// {"_raw": ...} (2026-09-26 audit, fifteenth round).
					if acc, exists := toolAccums[syntheticToolSlot]; exists &&
						startsANewToolCall(acc.id, acc.name, acc.args.String(), tc.ID, tc.Function.Name) {
						syntheticToolSlot++
					}
					slot = syntheticToolSlot
				}
				acc, exists := toolAccums[slot]
				if !exists {
					acc = &toolAccum{}
					toolAccums[slot] = acc
				}
				if tc.ID != "" {
					acc.id = tc.ID
				}
				if tc.Function.Name != "" {
					acc.name = tc.Function.Name
				}
				if tc.Function.Arguments != "" {
					acc.args.WriteString(tc.Function.Arguments)
				}
			}

			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
				completed = true
			}
		}

		// Some remotes send a final chunk with usage but no choices.
		if chunk.Usage != nil {
			// The usage-only final chunk (stream_options.include_usage) is
			// the only place a streaming response reports prompt_tokens. It
			// feeds the per-session context-fit calibration AND the usage we
			// report to the client on the done event below. Before
			// 2026-08-30 it was only used for calibration: streamed
			// responses then carried input_tokens=0/output_tokens=0, so
			// Claude Code -- which always streams and sizes auto-compaction
			// on the reported usage -- never saw its context grow, never
			// compacted, and ran straight into the 262k wall (real .46
			// session at 253,958 tokens, 2026-08-30 16:13 UTC).
			finalUsage = chunk.Usage
			if onUsage != nil && chunk.Usage.PromptTokens > 0 {
				onUsage(chunk.Usage.PromptTokens)
			}
		}
	}

	// The stream did not finish. Say so, and do NOT flush tool calls: partial
	// argument JSON accumulated from the deltas is what the non-streaming
	// path deliberately keeps as {"_raw": ...} for a model that emits
	// freeform arguments — but mid-stream that fallback cannot tell
	// "freeform" from "the connection died between two argument fragments",
	// and it emitted a COMPLETE, executable tool_use with
	// stop_reason=tool_use either way. An agent then runs the fabricated
	// call instead of retrying the turn (2026-09-26 audit).
	if !completed {
		// Nothing streamed and no error seen: the body may be a whole
		// completion the upstream sent instead of frames. Adopt it as the turn
		// rather than reporting a failure over an answer that exists.
		if !started && upstreamErr == "" &&
			adoptNonSSECompletion(nonSSE.String(), conv, emit, onUsage, upstreamModel, &finishReason, &finalUsage, &streamedText) {
			completed = true
		}
	}

	if !completed {
		msg := upstreamErr
		if msg == "" {
			if err := scanner.Err(); err != nil {
				msg = "upstream stream failed: " + redactErr(err).Error()
			} else {
				msg = "upstream stream ended before the response was complete"
			}
		}
		if !started {
			// Nothing has been sent yet, so the caller gets a status it can
			// act on (and retry) rather than an empty successful message.
			writeAnthropicError(w, http.StatusBadGateway, msg)
			return false
		}
		emitErr(msg)
		return false
	}
	if upstreamErr != "" {
		// A completion marker arrived after an error object: the error is
		// still the truth about the turn. Nothing was lost by preferring it
		// over a fabricated stop_reason.
		if !started {
			writeAnthropicError(w, http.StatusBadGateway, upstreamErr)
			return false
		}
		emitErr(upstreamErr)
		return false
	}

	// Flush any pending tool calls before the done event so StreamConverter
	// emits their content blocks first and sets stop_reason=tool_use. A turn
	// the upstream ended at the token limit is passed through as such: the
	// calls it managed to write COMPLETELY are still runnable, but a fragment
	// must not be dressed up as one (see flushToolCalls).
	flushToolCalls(finishReason == "length")

	// Final done event, carrying the stream's real usage (see finalUsage
	// above). The converter turns Metrics into message_delta.usage; the
	// cache-read count has no Metrics field, so it is patched onto that
	// event after conversion.
	doneResp := api.ChatResponse{
		Model:      upstreamModel,
		Done:       true,
		DoneReason: mapFinishReason(finishReason),
	}
	cached := 0
	// Each field is taken only when the upstream STATED it. A usage chunk
	// that states nothing (see statedPromptTokens) otherwise suppressed both
	// fallbacks at once by being present, which is strictly worse than the
	// no-usage-chunk shape the fallbacks were added for (2026-09-26 audit,
	// twelfth round). "Stated" is not "non-zero": a fully-cached turn reports
	// prompt_tokens>0 with every one of them cached, so its uncached count is
	// legitimately 0 and must not be replaced by the estimate.
	statedPrompt := finalUsage.statedPromptTokens()
	statedCompletion := finalUsage.statedCompletionTokens()
	if finalUsage != nil {
		cached = finalUsage.cachedTokens()
	}
	if statedPrompt {
		doneResp.Metrics.PromptEvalCount = finalUsage.PromptTokens - cached
	}
	if statedCompletion {
		doneResp.Metrics.EvalCount = finalUsage.CompletionTokens
	}
	if !statedPrompt && estInputTokens > 0 {
		// Nothing stated for this field: report the prompt estimate the
		// converter was seeded with rather than a hard "input_tokens":0 the
		// client believes — a session that really did grow then looks flat,
		// and auto-compaction never fires (2026-09-26 audit).
		doneResp.Metrics.PromptEvalCount = estInputTokens
	}
	if !statedCompletion && streamedText > 0 {
		doneResp.Metrics.EvalCount = streamedText/4 + 1
	}
	events := conv.Process(doneResp)
	if finalUsage != nil && finalUsage.PromptTokens > 0 {
		// The converter only overwrites message_start's seeded estimate when
		// PromptEvalCount > 0, which is right for a done event that states
		// nothing — but here the upstream DID state usage, and an upstream
		// that reports the whole prompt as cache-read leaves the uncached
		// count at 0. The converter then kept the pre-turn estimate, so the
		// client was told input_tokens = the full prompt AND
		// cache_read_input_tokens = the same full prompt: 2x the real prompt
		// for a fully-cached turn, and Claude Code's context meter and
		// auto-compaction sum both fields (2026-09-26 audit). Usage was
		// stated, so this owns the field.
		for i := range events {
			if d, ok := events[i].Data.(anthropic.MessageDeltaEvent); ok {
				d.Usage.InputTokens = finalUsage.PromptTokens - cached
				d.Usage.CacheReadInputTokens = cached
				events[i].Data = d
			}
		}
	}
	emit(events)
	return true
}

// frameCarriesWholeCompletion reports whether an SSE payload is a whole
// non-SSE completion rather than one delta frame.
//
// It is the gate for adopting a completion that arrived inside a frame, and it
// is deliberately structural: the payload must parse, must have a choice, and
// that choice's `message` object must actually be populated. A byte-substring
// test for "message" is not enough — a DELTA frame whose content happens to
// contain that word (a model writing JSON, a tool argument naming the key)
// matches it, and adopting such a frame emits an empty message, ends the turn,
// and throws the rest of the stream away, leaving the client with 200 and no
// answer on a leg recorded healthy (2026-09-26 audit, sixteenth round).
func frameCarriesWholeCompletion(payload string) bool {
	var resp openAIChatResponse
	if err := json.Unmarshal([]byte(payload), &resp); err != nil {
		return false
	}
	if len(resp.Choices) == 0 {
		return false
	}
	m := resp.Choices[0].Message
	// Only fields the non-delta shape carries: a bare {"role":"assistant"}
	// frame is not an answer either, so require some content.
	return m.Role != "" || m.Content != "" || m.ReasoningContent != "" || m.Reasoning != "" || len(m.ToolCalls) > 0
}

// adoptNonSSECompletion handles a stream request the upstream answered with a
// whole completion instead of SSE frames, and reports whether the body was one.
//
// It leaves the reader in exactly the state a completed frame stream would
// have reached — content and tool calls emitted, finishReason, finalUsage and
// streamedText set — so the caller's existing tail runs unchanged and the
// per-field usage gating and cache-read patching apply to this shape too.
// Reporting it as a truncated stream instead cost the client a 502 with no
// content on every retry (the upstream produced and billed a whole answer) and
// marked the leg failed, so three such turns opened its breaker and moved the
// session off a leg that was serving it (2026-09-26 audit, fourteenth round).
func adoptNonSSECompletion(raw string, conv *anthropic.StreamConverter, emit func([]anthropic.StreamEvent), onUsage func(int), upstreamModel string, finishReason *string, finalUsage **openAIUsage, streamedText *int) bool {
	if strings.TrimSpace(raw) == "" {
		return false
	}
	var oaiResp openAIChatResponse
	if err := json.Unmarshal([]byte(raw), &oaiResp); err != nil {
		return false
	}
	// No choices is not an answer: the same body the non-streaming path
	// refuses ("upstream returned no completion choices"), so it keeps the
	// failure verdict here.
	if len(oaiResp.Choices) == 0 {
		return false
	}

	if onUsage != nil && oaiResp.Usage != nil && oaiResp.Usage.PromptTokens > 0 {
		onUsage(oaiResp.Usage.PromptTokens)
	}

	// Content and tool calls first, with Done unset, so the converter opens and
	// closes their content blocks before the tail's done event.
	chatResp := openAIResponseToChatResponse(oaiResp, upstreamModel)
	emit(conv.Process(api.ChatResponse{Model: upstreamModel, Message: chatResp.Message}))

	*streamedText = len(chatResp.Message.Content) + len(chatResp.Message.Thinking)
	*finishReason = oaiResp.Choices[0].FinishReason
	*finalUsage = oaiResp.Usage
	return true
}

// upstreamErrorMessage extracts the message from an OpenAI-shaped error
// payload — {"error": {...}} — and returns "" when the line is not one.
//
// It is how a stream's failure is RECOGNISED: an upstream can report a
// mid-stream error as an SSE frame carrying an error object instead of a
// choice (vLLM and the fleet's own gateway do), and can answer with a JSON
// error body over HTTP 200 with no SSE frames at all. Both used to be read as
// "nothing to do on this line" (2026-09-26 audit).
//
// The message is returned REDACTED, and redaction lives here rather than at
// the call sites because every caller renders the result to the launched
// client: the non-streaming body and the three streaming recognition sites.
// Only the non-200 branch sanitized, through writeUpstreamError's
// redactCredentials — so an upstream answering an error object over HTTP 200
// handed the client the credential its message quoted back (a request URL
// carrying userinfo or a query key), into an LLM's context window and the
// transcript the user pastes into a ticket (2026-09-26 audit, fifth round).
//
// secret is the credential this leg injected upstream. redactCredentials
// cannot see it — the shape rules match URLs, and a vendor naming the key it
// rejected writes it as ordinary prose — so it is redacted as a LITERAL too
// (2026-09-26 audit, ninth round).
func upstreamErrorMessage(s, secret string) string {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "{") {
		return ""
	}
	var probe struct {
		Error *struct {
			Message string `json:"message"`
			Type    string `json:"type"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(s), &probe); err != nil || probe.Error == nil {
		return ""
	}
	if m := strings.TrimSpace(probe.Error.Message); m != "" {
		return redactUpstreamDiagnosis(m, secret)
	}
	if t := strings.TrimSpace(probe.Error.Type); t != "" {
		return "upstream reported " + redactUpstreamDiagnosis(t, secret)
	}
	return "upstream reported an error"
}

// proxyPassThrough forwards a request verbatim to the target URL, streaming
// the response back. Used for GET /v1/models.
func proxyPassThrough(w http.ResponseWriter, r *http.Request, target, key string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "build request: "+redactErr(err).Error())
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "upstream request failed: "+redactErr(err).Error())
		return
	}
	defer resp.Body.Close()
	relayUpstreamResponse(w, resp, key)
}

// relayUpstreamResponse copies an upstream GET /v1/models response back to the
// client with every text surface run through redactCredentials.
//
// It used to be a verbatim io.Copy of the body plus every header, which put the
// key on the client's terminal — and the upstream here is deliberately NOT
// api.anthropic.com: it is a plan row's own base, or a user's mirror, reached
// with the key in the base URL. A mirror whose error page quotes the request
// URL, or that echoes the route in a diagnostic header, therefore leaked the
// key (2026-09-26 audit, seventh round).
//
// The response is bounded and its length recomputed: redaction can change the
// byte count, and a relayed Content-Length that no longer matches the body
// truncates or hangs the client. The transfer encoding is dropped for the same
// reason — this writes one fixed-length body.
//
// The bound is enforced, not just imposed: a LimitReader that is reached makes
// io.ReadAll return the truncated bytes with no error at all, and the body was
// then relayed with a Content-Length computed from itself, so the client could
// not tell the shortened document from the whole one. These callers relay GET
// /v1/models, where the body IS the document — a client that gets the first
// 16 MiB of a catalog parses it happily and concludes the models it cannot see
// do not exist (2026-09-26 audit).
// secret, when non-empty, is a credential this process injected into the
// request that produced this response (an upstream's bearer or api key). It is
// redacted as a LITERAL on top of the shape-based rules, because an upstream
// echoing a key back need not put it in a URL — quoting the header it was sent
// is the obvious way, and no pattern can tell that string from a word. The
// caller is the only one that knows it (2026-09-26 audit).
func relayUpstreamResponse(w http.ResponseWriter, resp *http.Response, secret string) {
	const maxRelayedBody = 16 << 20
	// The body has to be TEXT before any of this means anything. Redaction is
	// a string operation, and a compressed body is not the text the client
	// will read: a key inside it is not a substring of anything, the
	// Content-Encoding header was copied back verbatim, and the client — which
	// decodes exactly the encoding it asked for — read the working credential
	// in clear text (2026-09-26 audit, ninth round). Two sources of that
	// encoding are closed here: the callers no longer forward the CLIENT's
	// accept-encoding, and a body that arrives encoded anyway (a vendor that
	// compresses unconditionally) is decoded before redaction. An encoding this
	// process cannot read is refused outright rather than relayed — passing it
	// through would be vouching for bytes no rule here has seen.
	src := resp.Body
	if enc := contentEncodingOf(resp); enc != "" && !resp.Uncompressed {
		decoded, ok := decodeContentEncoding(src, enc)
		if !ok {
			writeAnthropicError(w, http.StatusBadGateway,
				fmt.Sprintf("upstream answered with a Content-Encoding this proxy cannot decode (%s), so its body cannot be relayed", enc))
			return
		}
		defer decoded.Close()
		src = decoded
	}
	// One byte past the cap is what distinguishes "exactly the limit" from
	// "there was more".
	body, err := io.ReadAll(io.LimitReader(src, maxRelayedBody+1))
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "read upstream response: "+redactErr(err).Error())
		return
	}
	if len(body) > maxRelayedBody {
		writeAnthropicError(w, http.StatusBadGateway,
			fmt.Sprintf("upstream response is larger than the %d-byte limit this proxy relays", maxRelayedBody))
		return
	}
	redact := func(s string) string { return redactSecret(redactCredentials(s), secret) }
	body = []byte(redact(string(body)))
	for k, vs := range resp.Header {
		// Content-Encoding is dropped with the length: what is written below is
		// one plaintext body of a known size, whatever arrived.
		if strings.EqualFold(k, "Content-Length") || strings.EqualFold(k, "Transfer-Encoding") ||
			strings.EqualFold(k, "Content-Encoding") {
			continue
		}
		for _, v := range vs {
			w.Header().Add(k, redact(v))
		}
	}
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(body)
}

// contentEncodingOf returns the response's Content-Encoding, lowercased and
// trimmed, or "" when it carries none (or only "identity").
func contentEncodingOf(resp *http.Response) string {
	enc := strings.ToLower(strings.TrimSpace(resp.Header.Get("Content-Encoding")))
	if enc == "" || enc == "identity" {
		return ""
	}
	return enc
}

// decodeContentEncoding wraps body in a decoder for the named Content-Encoding,
// and reports false for an encoding this process cannot read (br, zstd, ...) or
// a stream that does not decode as that encoding at all.
func decodeContentEncoding(body io.Reader, encoding string) (io.ReadCloser, bool) {
	switch encoding {
	case "gzip", "x-gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			return nil, false
		}
		return zr, true
	case "deflate":
		return flate.NewReader(body), true
	}
	return nil, false
}

// nativeAnthropicUpstream is api.anthropic.com's own /v1/messages endpoint
// — a var so tests can point it at a fixture server.
var nativeAnthropicUpstream = "https://api.anthropic.com/v1/messages"

// nativeAnthropicModelsUpstream is api.anthropic.com's own /v1/models
// endpoint — a var so tests can point it at a fixture server. See the
// /v1/models handler's doc for why this exists: opusplan's own SDK-side
// model validation queries this endpoint, and with no real answer it
// rejects a perfectly valid native alias like "fable" outright.
var nativeAnthropicModelsUpstream = "https://api.anthropic.com/v1/models"

// nativeAnthropicModelsPassthrough forwards GET /v1/models to the real
// Anthropic API with the same credential resolution as message requests
// (resolveNativeAnthropicAuth) — no request body to worry about, this is a
// simple GET relay.
func nativeAnthropicModelsPassthrough(w http.ResponseWriter, r *http.Request) {
	auth, ok := resolveNativeAnthropicAuth()
	if !ok {
		writeAnthropicError(w, http.StatusUnauthorized,
			"no Anthropic credential found — run `claude /login` or set ANTHROPIC_API_KEY")
		return
	}
	anthropicModelsPassthrough(w, r, nativeAnthropicModelsUpstream, auth.Header, auth.Value)
}

// anthropicModelsPassthrough relays GET /v1/models to an Anthropic-wire
// upstream (api.anthropic.com, or a plan row's own base) with the same
// credential handling as messages.
func anthropicModelsPassthrough(w http.ResponseWriter, r *http.Request, upstream, headerName, headerValue string) {
	req, err := http.NewRequestWithContext(r.Context(), http.MethodGet, upstream, nil)
	if err != nil {
		writeAnthropicError(w, http.StatusInternalServerError, "build upstream request: "+redactErr(err).Error())
		return
	}
	for k, vs := range r.Header {
		// accept-encoding is dropped for the same reason the credential is:
		// this relay redacts the upstream's text before the client sees it, and
		// a vendor that compresses (because WE asked on the client's behalf)
		// answers in bytes no string rule can sanitize — while the client, which
		// decodes exactly the encoding it asked for, reads the credential the
		// redaction was supposed to remove (2026-09-26 audit, ninth round).
		// Leaving it unset lets the transport negotiate and transparently
		// decode, so what reaches relayUpstreamResponse is plaintext.
		if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "x-api-key") ||
			strings.EqualFold(k, "accept-encoding") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	// The credential goes on through the same rule every other injection site
	// uses (applyNativeAnthropicAuth): an OAuth bearer is accompanied by the
	// beta that announces it, merged into whatever the client sent, while a
	// vendor key sent as x-api-key needs none and gets none. This relay set
	// the header bare, so an OAuth-only machine's model list 401'd and Claude
	// Code fell back to its built-in one (2026-09-26 audit, thirteenth round).
	applyNativeAnthropicAuth(req, nativeAnthropicAuth{Header: headerName, Value: headerValue})
	resp, err := (&http.Client{Timeout: 15 * time.Second}).Do(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "upstream request failed: "+redactErr(err).Error())
		return
	}
	defer resp.Body.Close()
	relayUpstreamResponse(w, resp, headerValue)
}

// nativeModelCatalogCache memoizes the CATALOG, not one resolved alias per
// entry. A short TTL, not "forever": Anthropic's catalog is the source of
// truth for which id "fable"/"opus"/etc currently means, and that mapping can
// change (a new point release) without this process restarting.
//
// Keyed by catalog rather than by alias because one launch asks about several
// tiers at once (opus for the primary slot, sonnet for --sonnet-model, haiku
// for opusplan, a fourth for the oversize leg), and each of those was its own
// cache key and therefore its own GET of the same list. On a network that
// drops packets instead of refusing them, that is the full timeout paid once
// per tier — around half a minute of a CLI that has printed nothing — before
// the child process has even started. There is one catalog; fetching it once
// makes the tiers lookups in data already in hand.
// inflight is the single-flight handle: non-nil while a fetch is running, and
// closed when it finishes. Callers that arrive during a fetch wait on it
// instead of on the mutex, so a stalled catalog costs them their own context,
// not the lock (2026-09-26 audit).
var nativeModelCatalogCache struct {
	sync.Mutex
	entries   []nativeCatalogEntry
	err       error
	expiresAt time.Time
	inflight  chan struct{}
}

// nativeCatalogEntry is one catalog row: the id that goes on the wire and the
// display name the alias is matched against.
type nativeCatalogEntry struct {
	ID          string `json:"id"`
	DisplayName string `json:"display_name"`
}

const (
	// nativeModelCatalogTTL is how long a good catalog is reused.
	nativeModelCatalogTTL = 10 * time.Minute
	// nativeModelCatalogFailureTTL is how long a FAILED fetch is remembered, so
	// the rest of this launch (and the next one, briefly) reads the failure
	// instead of paying the timeout again. Short, because a blip must not cost
	// native tiers for ten minutes.
	nativeModelCatalogFailureTTL = 30 * time.Second
)

// nativeModelCatalogTimeout bounds the catalog GET. A package var, not a
// constant, so tests can shrink it.
var nativeModelCatalogTimeout = 10 * time.Second

// resolveNativeModelAlias turns a Claude Code CLI alias ("fable", "opus",
// "sonnet", "haiku" — what nativeClaudeModelTier extracts from "claude/fable"
// etc, and what ANTHROPIC_DEFAULT_*_MODEL / --model already accept) into the
// REAL model id Anthropic's wire API expects (e.g. "claude-fable-5-1").
//
// Why this exists: Claude Code's own client resolves these aliases
// internally before ever putting them on the wire — running claude/fable
// via runNative (fully untouched native mode) works because THAT
// resolution happens inside the real binary, which we never see. Our own
// passthrough paths (nativeAnthropicPassthrough for a native tier, and the
// oversize-to-native crossover) build/forward the wire request ourselves
// and send the bare alias — Anthropic's real API returned "model: fable"
// not found the first time this was tested live (2026-09-02, an oversize
// crossover to claude/fable actually reaching production). No local alias
// table is hardcoded (a specific version string like "claude-fable-5-1"
// would silently go stale on the next model bump); this queries the same
// real /v1/models this proxy already forwards
// (nativeAnthropicModelsPassthrough) and matches on "Claude <Alias>" as a
// case-insensitive prefix of display_name, taking the FIRST match --
// verified live that Anthropic's catalog lists the newest point release of
// a family first (claude-fable-5-1 before claude-fable-5).
//
// If model is not a bare alias (nativeClaudeModelTier's caller already
// stripped "claude/"/"anthropic/", so this only ever receives the bare
// tier name) or resolution fails for any reason, model is returned
// unchanged — the caller's own request still goes out, worst case with
// the same "not found" Anthropic already gives for an unknown id, no worse
// than not attempting this at all.
//
// ctx bounds the catalog fetch this may do: it is the CALLER's context, so a
// request handler passes r.Context() and a client that hangs up stops the wait
// (2026-09-26 audit). Callers with no request to cancel pass
// context.Background(), leaving the fetch's own timeout as the only bound.
func resolveNativeModelAlias(ctx context.Context, model string) string {
	entries, err := nativeModelCatalog(ctx)
	if err != nil {
		return model
	}
	want := "claude " + strings.ToLower(model)
	for _, m := range entries {
		if strings.HasPrefix(strings.ToLower(m.DisplayName), want) {
			return m.ID
		}
	}
	return model
}

// nativeModelCatalog returns Anthropic's catalog, fetching it at most once per
// TTL — and at most once across concurrent callers: the first caller to find
// the cache stale fetches and the rest wait on that fetch's completion rather
// than starting their own.
//
// The mutex is NOT held across the network. It guards the cache fields and the
// single-flight handle, and is released before the request goes out. Holding it
// for the whole fetch (2026-09-26 audit) made every waiter's wait a
// sync.Mutex.Lock — not context-aware — so the request path that reaches this
// (resolveNativeModelAlias on the oversize crossover, from a live /v1/messages
// handler) could not be cancelled: a client that hung up left its goroutine
// parked on the lock until the fetch's own 10s timeout expired, and every
// concurrent request queued behind it. The fetch now runs on the caller's
// context, so a caller that goes away stops waiting.
func nativeModelCatalog(ctx context.Context) ([]nativeCatalogEntry, error) {
	for {
		nativeModelCatalogCache.Lock()
		if time.Now().Before(nativeModelCatalogCache.expiresAt) {
			entries, err := nativeModelCatalogCache.entries, nativeModelCatalogCache.err
			nativeModelCatalogCache.Unlock()
			return entries, err
		}
		// A fetch is already running: wait for it, or for this caller to give
		// up. Reading the result after it closes is safe because the fetch
		// publishes the entries and the expiry BEFORE closing the channel.
		if wait := nativeModelCatalogCache.inflight; wait != nil {
			nativeModelCatalogCache.Unlock()
			select {
			case <-wait:
			case <-ctx.Done():
				return nil, ctx.Err()
			}
			// Re-read rather than trusting the fetch's result: a fetch that
			// ended because ITS caller went away publishes nothing, and this
			// caller then becomes the one that refetches (2026-09-26 audit,
			// ninth round). A published result makes the loop's first branch
			// return immediately, so this is not a spin.
			continue
		}
		done := make(chan struct{})
		nativeModelCatalogCache.inflight = done
		nativeModelCatalogCache.Unlock()

		entries, err := fetchNativeModelCatalog(ctx)

		// The caller's context, not the fetch's own deadline: a fetch that
		// timed out on itself failed the same way for everyone, and the
		// failure TTL is there so the rest of the launch does not pay that
		// timeout again. A CALLER's cancellation is not evidence about the
		// upstream at all — it means one client stopped waiting. Caching it
		// wrote "the catalog is broken" into a process-wide, 30-second cache on
		// the strength of one hung-up request, and every other session on this
		// machine then resolved its tiers through that failure and fell back to
		// a bare alias the real API rejects (2026-09-26 audit, ninth round).
		callerGone := ctx.Err() != nil

		nativeModelCatalogCache.Lock()
		if callerGone {
			// Publish nothing. expiresAt is already in the past, so the next
			// live caller refetches.
			nativeModelCatalogCache.inflight = nil
			nativeModelCatalogCache.Unlock()
			close(done)
			return nil, err
		}
		ttl := nativeModelCatalogTTL
		if err != nil {
			ttl = nativeModelCatalogFailureTTL
		}
		nativeModelCatalogCache.entries, nativeModelCatalogCache.err = entries, err
		nativeModelCatalogCache.expiresAt = time.Now().Add(ttl)
		nativeModelCatalogCache.inflight = nil
		nativeModelCatalogCache.Unlock()
		close(done)
		return entries, err
	}
}

func fetchNativeModelCatalog(ctx context.Context) ([]nativeCatalogEntry, error) {
	auth, ok := resolveNativeAnthropicAuth()
	if !ok {
		return nil, errors.New("no Anthropic credential")
	}
	ctx, cancel := context.WithTimeout(ctx, nativeModelCatalogTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, nativeAnthropicModelsUpstream, nil)
	if err != nil {
		return nil, err
	}
	// applyNativeAnthropicAuth, not a bare Set: an OAuth bearer needs the
	// anthropic-beta header too (see its doc) and this GET has no client
	// request whose headers could supply it.
	applyNativeAnthropicAuth(req, auth)
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := (&http.Client{Timeout: nativeModelCatalogTimeout}).Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("catalog returned HTTP %d", resp.StatusCode)
	}
	// Bounded: this body is somebody else's answer, read into this process's
	// memory in full before it is parsed, and an unbounded read of it is the
	// peak memory of a long-lived proxy that resolves aliases in the
	// background. The real catalog is tens of KB; the cap is the same 64 MiB
	// every other buffered-and-parsed body in this package uses (2026-09-26
	// audit).
	body, err := httpbody.ReadCapped(resp.Body, nativeModelCatalogMaxBytes, "the native model catalog")
	if err != nil {
		return nil, err
	}
	var catalog struct {
		Data []nativeCatalogEntry `json:"data"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return nil, err
	}
	return catalog.Data, nil
}

// nativeModelCatalogMaxBytes is the cap fetchNativeModelCatalog buffers before
// parsing (see the call site). A package var only so a test can lower it to a
// few bytes rather than allocate 64 MiB to prove the cap is enforced.
var nativeModelCatalogMaxBytes = httpbody.DefaultMax

// rewriteAnthropicRequestModel returns body with its top-level "model"
// field replaced by newModel, preserving every other field and their
// ordering-independent JSON encoding — used only for the oversize-to-native
// crossover (anthropic_openai_proxy.go's /v1/messages handler): the
// client's original body names the OAICA leg that overflowed, not the
// native alias it's being redirected to.
func rewriteAnthropicRequestModel(body []byte, newModel string) ([]byte, error) {
	var m map[string]json.RawMessage
	if err := json.Unmarshal(body, &m); err != nil {
		return nil, err
	}
	if m == nil {
		// json.Unmarshal reports no error for a body that is not a JSON
		// object — `null` decodes into a nil map — and the assignment below
		// would panic on it. The caller's own shape check catches this
		// first, but a function that marshals a map must not panic whatever
		// it is handed (2026-09-26 audit, thirteenth round).
		return nil, fmt.Errorf("request body is not a JSON object")
	}
	encoded, err := json.Marshal(newModel)
	if err != nil {
		return nil, err
	}
	m["model"] = encoded
	return json.Marshal(m)
}

// jsonObjectBody reports whether body is a JSON object, ignoring leading
// whitespace. Deliberately a byte test rather than a second decode: the body
// can be a megabyte (httpbody.DefaultMax) and the only question being asked is
// whether the decode that follows will populate a struct or silently do
// nothing, which is exactly what a leading '{' answers.
func jsonObjectBody(body []byte) bool {
	for _, b := range body {
		switch b {
		case ' ', '\t', '\r', '\n':
			continue
		}
		return b == '{'
	}
	return false
}

// nativeAnthropicPassthrough forwards an Anthropic-wire request straight to
// api.anthropic.com, byte-for-byte: no OpenAI translation (there is nothing
// to translate — the wire format already matches), no context-fit clamp,
// no usage/cost ledger entry (see proxyRoute.NativePassthrough's doc — this
// leg is not OAICA-billed, so there is nothing to meter). The credential is
// resolved fresh on every call via resolveNativeAnthropicAuth
// (native_anthropic_auth.go) — an OAuth session from `claude /login` or a
// plain ANTHROPIC_API_KEY, exactly what native mode would have used
// running unproxied.
//
// Streaming responses are relayed as they arrive (Flush after every write)
// rather than buffered — Claude Code's own SSE parsing depends on timely
// chunk delivery, not just eventual byte-for-byte correctness.
//
// The third return says whether the request was ATTEMPTED at all: a request
// this side refuses for want of a credential never reached an upstream, so its
// caller must not feed the leg's health from it (2026-09-26 audit).
// passthroughBreakerKey is the breaker identity a passthrough leg must be
// filed under: the SAME string selectRoute and oversizeSwap read, or the feed
// writes to a key nobody consults and the leg reads healthy forever.
//
// An ordinary leg's identity is route.BaseURL — including a native claude/*
// leg, whose BaseURL is empty. The round-7 fix filed native legs under a
// constant "native-anthropic" instead, which nothing read (consumers call
// breakers.open(route.BaseURL)), so a native primary still served every
// failing turn with a healthy fallback configured: the exact defect the fix
// claimed to close, left open on the first leg it named (2026-09-26 audit,
// third round).
//
// The native OVERSIZE leg is the one exception, and it is route_policy.go's
// rule, not a new one: a native oversize leg has no BaseURL either, so filing
// it under "" would make it share the primary's circuit — keyed on
// nativeOversizeBreakerKey, which is what oversizeSwap checks.
func passthroughBreakerKey(route proxyRoute, oversize bool) string {
	if route.BaseURL != "" {
		return route.BaseURL
	}
	if oversize {
		return nativeOversizeBreakerKey
	}
	return "" // the native claude/* primary: consumers key on BaseURL, which is empty for it
}

// feedPassthroughRouteHealth feeds the circuit breaker and the `auto` policy's
// per-session escalation from a passthrough leg's upstream status — the same
// signal the OpenAI-translation path feeds at its own call site, and one the
// passthrough path never fed because it returns from the handler before that
// switch. Without it these legs' breakers read healthy forever, so
// docs/CLAUDE_TIERS.md's failover ("3 consecutive failures … open the circuit
// … when the selected leg's breaker is OPEN → fail over") was false for every
// native and Anthropic-wire leg (2026-09-26 audit).
//
// breakerKey comes from passthroughBreakerKey — the caller passes it because
// only the caller knows whether the leg it served is the ordinary one or the
// oversize crossover. Escalation is keyed on route.BaseURL, which is the leg
// selectRoute noted the session on.
//
// status is the upstream status the leg relayed, 0 when it never answered.
// The classification deliberately matches the translated path's: 5xx-class and
// transport failures count against the leg, sub-300 proves recovery, and 4xx /
// 429 are the leg WORKING (bad request, shedding) and must not open it.
// relayed says the upstream's body was relayed to its END — a 200 whose body
// died in the middle is a dead turn the client saw, and counting it healthy
// gave a leg that can never finish one a perfect record (2026-09-26 audit).
//
// clientGone says the request was built on a context the CALLER has since
// cancelled. Status 0 means "no response", which covers both a dead leg and a
// caller who hung up mid-flight — and those are opposite conclusions, so the
// caller passes the context's state rather than this reading a 0 as a failure.
// A cancelled request records NOTHING: it must not fail the leg, and it must
// not clear an existing failure streak either — and the same holds for a body
// left incomplete, which is what our own cancellation produces (2026-09-26
// audit).
//
// escalationLeg is the leg the `auto` policy's per-session escalation may hear
// about, and it is a parameter rather than route.BaseURL because the two
// differ on the oversize crossover (see crossoverEscalationLeg). An empty one
// feeds the breaker only.
func feedPassthroughRouteHealth(table proxyRouteTable, route proxyRoute, sessionID, breakerKey, escalationLeg string, status int, relayed, clientGone bool) {
	// Skipped outright for an untracked leg rather than handed to recordFail as
	// "": a native leg's BaseURL IS "", so an empty key would match a session
	// whose noted leg is one and record against it.
	escalateFail := func() {
		if escalationLeg != "" {
			table.escalations.recordFail(sessionID, escalationLeg)
		}
	}
	escalateOK := func() {
		if escalationLeg != "" {
			table.escalations.recordOK(sessionID, escalationLeg)
		}
	}
	switch {
	case status == passthroughNotAttempted:
		// This side answered without contacting the leg — no credential to
		// send, so there is no upstream result to classify. Status 0 (below)
		// is a leg that DID not answer, and the two are opposite conclusions
		// from the same symptom: recording a local configuration problem as a
		// dead backend opened the circuit on a leg that was healthy, moved the
		// session to a fallback the missing credential cannot serve either,
		// and changed what the user was billed for (2026-09-26 audit).
	case status == 0 && clientGone:
		// The client left before the leg answered. Says nothing about the leg.
	case status == 0 || status >= 500:
		table.breakers.recordFail(breakerKey)
		escalateFail()
	case status >= 300:
		// 4xx / 429: the leg answering at all — not a health signal.
	case clientGone:
		// The client left mid-relay: an incomplete body is then our cancel,
		// not the leg's doing.
	case relayed:
		table.breakers.recordOK(breakerKey)
		escalateOK()
	default:
		table.breakers.recordFail(breakerKey)
		escalateFail()
	}
}

// crossoverEscalationLeg returns the leg an oversize crossover may feed the
// `auto` policy's escalation with — "" when it may not feed it at all, which
// is every crossover onto another host.
//
// The escalation state tracks exactly ONE leg per session: the one selectRoute
// chose (routeEscalations.noteLeg), and recordFail/recordOK ignore every other
// leg. oversizeSwap only ever returns an Oversize leg on a DIFFERENT base URL
// (its own doc), so a crossover's result can never be about that leg — the
// record used to be written anyway, four lines of code reading as a signal
// while the per-leg guard dropped it (2026-09-26 audit).
//
// Leaving it dropped, rather than promoting the crossover to an escalation
// signal, is deliberate: escalation moves the session OFF the leg selectRoute
// chose, and a request that did not fit the primary's window says nothing about
// that leg's health — escalating would move the session's ordinary traffic for
// autoEscalateHoldFor because of a size mismatch. Repeated crossover failures
// are handled by the crossover leg's OWN breaker, which oversizeSwap consults
// before swapping, so the next such request fails visibly with "prompt is too
// long" instead of silently landing on a dead leg.
func crossoverEscalationLeg(selectedBaseURL, servedBaseURL string) string {
	if servedBaseURL == selectedBaseURL {
		return servedBaseURL
	}
	return ""
}

// passthroughNotAttempted is the status a passthrough wrapper returns when the
// request never left this process — no credential to send. It is a NEGATIVE
// number so it cannot collide with an HTTP status or with 0 ("the upstream did
// not answer"), which is what makes it distinguishable at the health feed:
// only one of the two is evidence about the leg.
const passthroughNotAttempted = -1

// noNativeAnthropicCredential is the one wording for "this launch has no
// Anthropic credential to send on a native claude/* leg" — the handler refuses
// with it too, so that the refusal is a logged row (see the /v1/messages
// handler's refusal machinery).
const noNativeAnthropicCredential = "no Anthropic credential found — run `claude /login` or set ANTHROPIC_API_KEY"

func nativeAnthropicPassthrough(w http.ResponseWriter, r *http.Request, body []byte, sessionID string) (int, bool, bool) {
	auth, ok := resolveNativeAnthropicAuth()
	if !ok {
		writeAnthropicError(w, http.StatusUnauthorized, noNativeAnthropicCredential)
		// passthroughNotAttempted, not 0: nothing was sent, so this says
		// nothing about the leg's health (see the feed's switch).
		return passthroughNotAttempted, false, false
	}
	status, relayed := anthropicPassthrough(w, r, body, nativeAnthropicUpstream, auth.Header, auth.Value, sessionID)
	return status, relayed, true
}

// anthropicPassthrough forwards an Anthropic-wire request to an
// Anthropic-wire upstream, byte-for-byte, with the credential injected under
// headerName (api.anthropic.com wants x-api-key or an OAuth bearer; the
// vendors behind a plan row want x-api-key). Everything else in
// nativeAnthropicPassthrough's doc applies: no translation, no clamp, no
// ledger, streaming relayed as it arrives.
//
// It returns the upstream status it relayed (0 when it never got one) and
// whether a turn was relayed at all — the body reached its END, and there was a
// body. Those are the caller's signal for the circuit breaker and the `auto`
// escalation: this path returns from the handler BEFORE the OpenAI path's
// breaker switch, so without it a passthrough leg's breaker read healthy
// forever and a dead primary was never failed over — the promise
// docs/CLAUDE_TIERS.md's failover section makes for every leg. The second value
// is why the caller does not use the status alone: a body that dies after a 200
// is a dead turn the client saw, and so is no body at all, and counting either
// healthy is the same defect on this path (2026-09-26 audit).
func anthropicPassthrough(w http.ResponseWriter, r *http.Request, body []byte, upstream, headerName, headerValue, sessionID string) (int, bool) {
	// Every outcome on this leg is logged (request_log.go), the way the
	// translated path logs it. A plan row on the anthropic wire used to write
	// a row only when its upstream refused the connection, so the leg was
	// invisible to `oaica usage` in the two states a user opens the report to
	// see: a session where every turn SUCCEEDED (no rows at all — an
	// Anthropic-wire remote could serve a whole day of traffic and report
	// nothing), and one where every turn failed at the upstream with a 5xx,
	// which returned early and left no evidence either (2026-09-26 audit).
	//
	// body here is the REWRITTEN one, so Model is the id the upstream was
	// asked for rather than the picker spelling the client sent
	// ("glm-5.3", not "zai-coding-plan/glm-5.3"). That is deliberately left as
	// it was: it is the model that was spent, it is identical to the client's
	// spelling on the native claude/* leg (whose UpstreamModel IS the alias
	// the client sent), and it is the only spelling this function has — the
	// translated path logs the client's because its body is not rewritten.
	started := time.Now()
	lastLen, totalLen := extractLastAndTotalMessageLen(body)
	entry := requestLogEntry{
		Timestamp:        time.Now().UTC().Format(time.RFC3339),
		Model:            requestLogModelFromBody(body),
		Path:             r.URL.Path,
		Backend:          redactBaseURL(upstream),
		LastMessageLen:   lastLen,
		TotalMessagesLen: totalLen,
		HardSignalMatch:  requestLogHardSignalRE.MatchString(string(body)),
		WouldBeHardByLen: lastLen > requestLogHardLengthThreshold || totalLen > requestLogHardLengthThreshold*3,
	}

	req, err := http.NewRequestWithContext(r.Context(), http.MethodPost, upstream, bytes.NewReader(body))
	if err != nil {
		// `entry` exists by now and nothing has been written to the client, so
		// this return is the one the deferred row cannot cover — logged here
		// like the translated path's identical branch (2026-09-26 audit, round
		// 16). Without it a leg whose upstream could not even be built into a
		// request reported no traffic at all in `oaica usage`.
		entry.StatusCode = http.StatusInternalServerError
		entry.DurationMs = time.Since(started).Milliseconds()
		appendRequestLog(entry)
		writeAnthropicError(w, http.StatusInternalServerError, "build upstream request: "+redactErr(err).Error())
		return 0, false
	}
	// Forward the client's own Anthropic-protocol headers (anthropic-
	// version, anthropic-beta, content-type, ...) verbatim — Claude Code
	// set these correctly already, this proxy has no opinion on them. Only
	// the credential is ours to inject; anything the client sent under
	// these two header names is replaced, never merged, so a stale or
	// wrong client-side auth header can never leak through.
	for k, vs := range r.Header {
		// accept-encoding goes the way of the credential: this leg redacts the
		// upstream's failure text with the literal key it injected
		// (relayUpstreamResponse), which is only possible on a body it can read,
		// and forwarding the client's preference invited a vendor to gzip the
		// one body that has to be sanitized (2026-09-26 audit, ninth round).
		// Unset, the transport negotiates and decodes for us.
		if strings.EqualFold(k, "authorization") || strings.EqualFold(k, "x-api-key") ||
			strings.EqualFold(k, "accept-encoding") {
			continue
		}
		for _, v := range vs {
			req.Header.Add(k, v)
		}
	}
	req.Header.Set(headerName, headerValue)
	// The credential oaica injected, when it is the OAuth session `claude
	// /login` wrote, is only accepted by api.anthropic.com alongside the beta
	// that announces it (native_anthropic_auth.go's oauthBetaHeaderValue). The
	// client cannot be relied on for it: on this leg the client is a child
	// oaica launched against the proxy, whose Authorization header is the
	// proxy's own token (replaced above), and the beta set Claude Code emits
	// depends on how IT is authenticated, not on the credential the leg it is
	// being pointed at needs. Merged into whatever the client sent — never
	// replacing it — because those values carry the client's own betas
	// (prompt caching among them) and dropping one is a silent downgrade of the
	// request. A vendor key sent as x-api-key needs none and gets none.
	if strings.EqualFold(headerName, "Authorization") {
		req.Header.Set("anthropic-beta", mergeAnthropicBeta(req.Header.Get("anthropic-beta"), oauthBetaHeaderValue))
	}
	// The conversation-affinity header, on this leg too: docs/CLAUDE_TIERS.md
	// promises it is sent "on every request that launch's proxy forwards",
	// and the leg it exists for — a remote Anthropic-wire backend behind a
	// consistent-hash load balancer — is exactly this one, which returned
	// before the header was ever set (2026-09-26 audit).
	if sessionID != "" {
		req.Header.Set("X-Session-Id", sessionID)
	}

	// proxyUpstreamClient, not a client with its own response deadline: this
	// carries real completions, which legitimately run for minutes, and a
	// fixed deadline here is a wall-clock cut at a time that belongs to no
	// one. It is not the child's clock (the child's API_TIMEOUT_MS starts when
	// the child sends, this one when the proxy receives, and a retry loop or a
	// raised limit puts a healthy turn past it), and it is not needed to cover
	// the child giving up: a child that gives up CANCELS the request, which
	// feedPassthroughRouteHealth already reads as clientGone and excludes from
	// the leg's health entirely. What a deadline here did produce was a
	// truncated body after a 200 — indistinguishable, to that same health
	// feed, from a leg that cannot finish a turn, so three long healthy turns
	// opened the circuit and moved the session off a working backend
	// (2026-09-26 audit).
	//
	// Setup stays bounded: this client's transport has a 10s dial and a 90s
	// idle timeout, so an unreachable upstream still fails promptly rather
	// than hanging until the child's own timer.
	resp, err := proxyUpstreamClient.Do(req)
	if err != nil {
		// The attempt is logged even though it never reached a backend (the
		// defer below cannot cover this path: it is registered after Do, and
		// nothing has been written to the client yet for it to read a status
		// off). A leg that refuses connections is the case an ERR column
		// exists for, and a transport failure is what the retry budget on the
		// translated path already gave up on (2026-09-26 audit).
		entry.StatusCode = http.StatusBadGateway
		entry.DurationMs = time.Since(started).Milliseconds()
		appendRequestLog(entry)
		writeAnthropicError(w, http.StatusBadGateway, "upstream request failed: "+redactErr(err).Error())
		return 0, false
	}
	defer resp.Body.Close()

	// Everything from here on writes through rec, and the deferred row reads
	// the status the CLIENT ended up with — not the upstream's byte before its
	// body was read, which is the difference the translated path learned to
	// draw (see its own entry comment). relayed is set where the relay ends,
	// and the two non-deliveries it cannot see from the status alone are named
	// explicitly: a client that hung up mid-turn (clientGone), and any leg
	// whose sub-300 response never became a turn at all.
	rec := &statusCapturingWriter{ResponseWriter: w}
	w = rec
	delivered := false
	clientGone := false
	defer func() {
		entry.StatusCode = rec.status()
		if !delivered && rec.status() < 300 && !clientGone && r.Context().Err() == nil {
			entry.StatusCode = http.StatusBadGateway
		}
		entry.DurationMs = time.Since(started).Milliseconds()
		appendRequestLog(entry)
	}()

	// A FAILING upstream response is the vendor's diagnosis, and it is relayed
	// the way every other failure path in this file is relayed — redacted, with
	// the literal credential we injected removed on top of the shape-based
	// rules. This leg copied the upstream's headers and wrote its body through
	// untouched, so an error quoting the URL it was called with, or a
	// diagnostic header echoing it ("bad key: sk-live-…" — a shape no pattern
	// can recognise), handed the client a working credential; the client is
	// Claude Code, which prints it into the session transcript and into
	// whatever bug report the user pastes it into (2026-09-26 audit).
	//
	// A SUCCESSFUL body is the model's own answer and stays byte-for-byte —
	// the relay's whole contract — because redacting it would corrupt the
	// user's content, and a streamed body cannot be redacted safely at all (a
	// secret straddling two reads is invisible to any per-chunk rule). The
	// split is the one this proxy already makes everywhere: failures are ours
	// to sanitize, an answer is not.
	if resp.StatusCode >= 300 {
		relayUpstreamResponse(w, resp, headerValue)
		// relayed is false: a body the health feed only reads for a sub-300
		// status, and this is not one (see feedPassthroughRouteHealth's switch:
		// 5xx counts against the leg on the status alone, 4xx counts as the leg
		// working).
		return resp.StatusCode, false
	}

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)

	flusher, canFlush := w.(http.Flusher)
	buf := make([]byte, 32*1024)
	relayedBytes := 0
	for {
		n, readErr := resp.Body.Read(buf)
		if n > 0 {
			relayedBytes += n
			if _, writeErr := w.Write(buf[:n]); writeErr != nil {
				// The client's own connection failed. This is the caller's
				// client-gone case, not evidence about the leg — nor a failed
				// turn in the log: the leg delivered, this side had nowhere to
				// put it.
				clientGone = true
				return resp.StatusCode, false
			}
			if canFlush {
				flusher.Flush()
			}
		}
		if readErr != nil {
			// A clean end of body is the only evidence that the leg delivered
			// the whole answer; anything else (a body cut short, a read timed
			// out, the transport giving up) is a turn the client never
			// received in full.
			//
			// relayedBytes > 0 is the other half of that evidence, and it is
			// not implied by EOF: an upstream that answers 200 and closes
			// without a byte reaches EOF on its first read, so an EMPTY 200
			// was reported as a body relayed to its end (2026-09-26 audit).
			// A Messages response with no bytes is not a turn any client can
			// use, and the two translated paths already refuse the shape
			// (handleNonStreamResponse fails to decode it, handleStreamResponse
			// never sees a frame) — only this one, which does not parse the
			// wire, had to say so explicitly.
			delivered = relayedBytes > 0 && errors.Is(readErr, io.EOF)
			return resp.StatusCode, delivered
		}
	}
}

// statusCapturingWriter records the status the client actually received, so a
// request log row can name it. Writes with no explicit WriteHeader are a 200,
// which is what the streaming path relies on (headers go out with the first
// event), and Flush is forwarded because both streaming paths assert
// http.Flusher on the writer they are handed.
type statusCapturingWriter struct {
	http.ResponseWriter
	code int
}

func (s *statusCapturingWriter) WriteHeader(code int) {
	if s.code == 0 {
		s.code = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapturingWriter) Write(b []byte) (int, error) {
	if s.code == 0 {
		s.code = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusCapturingWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func (s *statusCapturingWriter) status() int {
	if s.code == 0 {
		return http.StatusOK
	}
	return s.code
}

// writeAnthropicError emits an Anthropic-shaped error response.
// writeUpstreamError re-emits a non-200 upstream response. It keeps the
// upstream's OWN retry contract for the statuses Claude Code understands --
// 429 (rate_limit_error) with Retry-After, 503/529 (overloaded_error) -- and
// collapses everything else to a 502.
//
// Every non-200 used to become an unconditional 502 with no Retry-After, so a
// rate-limited or draining replica looked to Claude Code like a broken proxy:
// it retried immediately, and each retry re-sent the whole prompt, which is
// exactly the prefill the 429 was rejecting (a replica under per-key
// concurrency pressure got hammered instead of backed off). 401/403 stay a
// 502 on purpose: they mean OUR key for that remote is wrong, and handing
// Claude Code an authentication_error would send it into its own login flow.
// (2026-09-26 audit.)
//
// secret is the credential this leg injected upstream and is what lets the
// re-emitted text lose it: an upstream refusing the call names the key it
// refused, and that is prose no shape rule recognises (2026-09-26 audit, ninth
// round).
func writeUpstreamError(w http.ResponseWriter, resp *http.Response, text, secret string) {
	switch resp.StatusCode {
	case http.StatusTooManyRequests, http.StatusServiceUnavailable, 529:
		if v := resp.Header.Get("Retry-After"); v != "" {
			w.Header().Set("Retry-After", v)
		}
		writeAnthropicError(w, resp.StatusCode, fmt.Sprintf("upstream HTTP %d: %s", resp.StatusCode, redactUpstreamDiagnosis(text, secret)))
	default:
		writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("upstream HTTP %d: %s", resp.StatusCode, redactUpstreamDiagnosis(text, secret)))
	}
}

func writeAnthropicError(w http.ResponseWriter, code int, msg string) {
	errResp := anthropic.NewError(code, msg)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	enc := json.NewEncoder(w)
	_ = enc.Encode(errResp)
}

// findUserRemoteByName looks up a configured remote by its name (used by the
// hidden serve-anthropic-proxy subcommand).
func findUserRemoteByName(name string) (userRemote, bool) {
	remotes, err := loadUserRemotes()
	if err != nil {
		return userRemote{}, false
	}
	for _, r := range remotes {
		if r.Name == name {
			return r, true
		}
	}
	return userRemote{}, false
}

// listenProxyForRemote is net.Listen, as a package var so a test can hold on to
// the listener it hands out — the serve loop below only returns when that
// listener closes, and the test needs a way to close it.
var listenProxyForRemote = net.Listen

// ServeAnthropicProxyForRemote is the entry point used by the hidden CLI
// subcommand. It resolves the remote, binds a listener (on port, or a free one
// if port<=0), prints the chosen port AND the client token to stdout, then
// blocks serving.
//
// Both go to stdout before the serve loop starts, and the token's line says
// what it is: the loop blocks until the process is killed, so anything printed
// after it never reached a human at all. That included the token, which made
// the documented smoke test unusable — `curl http://127.0.0.1:8799/v1/messages`
// gets 401, and the token it needs was the one thing the command never showed
// (2026-09-26 audit). There is no other way to learn it: the proxy listens on
// loopback, which is shared with every other process on the box, so it accepts
// nothing but this token (2026-09-01 security audit H1).
func ServeAnthropicProxyForRemote(remoteName, upstreamModel string, port int) error {
	remote, ok := findUserRemoteByName(remoteName)
	if !ok {
		return fmt.Errorf("no user-defined remote named %q in ~/.oaica/remotes.json", remoteName)
	}
	if upstreamModel == "" {
		return fmt.Errorf("--model is required (the bare upstream model id, e.g. deepseek-v4-flash)")
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ln, err := listenProxyForRemote("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	token, err := newProxyClientToken()
	if err != nil {
		return fmt.Errorf("generate proxy client token: %w", err)
	}
	fmt.Println(strconv.Itoa(ln.Addr().(*net.TCPAddr).Port))
	fmt.Println("client token: " + token)
	return RunAnthropicOpenAIProxyRoutes(ln, singleRemoteRouteTable(remote, upstreamModel, token))
}
