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
	"image/gif"
	"image/png"
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
	Role string `json:"role"`
	// Content is always written, even when empty: `content` is a property of a
	// chat message on this wire, and both other legs state it — the local
	// converter through api.Message's own `json:"content"` (no omitempty) and
	// the gateway leg with content:"" — so a turn the client sent as an empty
	// content array reached some backends as a message with no content key at
	// all and the others as one with an empty string (2026-09-27 audit, round
	// 49, B-F6). Same rule as round 45's `"input":{}`: an absent key is not a
	// statement, and the empty value is.
	Content    string             `json:"content"`
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
	// Any non-empty text is text, whitespace included: the client stated a text
	// block, and the metered gateway leg delivers `{"type":"text","text":"   "}`
	// for the same turn. Trimming it here sent the model a message whose text
	// part had vanished beside an image, so one body became two prompts
	// depending on which leg served it (2026-09-27 audit, round 50). The EMPTY
	// string is not a part — nothing was stated — which is the case the
	// round-46 rule on this wire already draws.
	if m.Content != "" {
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
	TopK       *int       `json:"top_k,omitempty"`
	Stop       []string   `json:"stop,omitempty"`
	Tools      []api.Tool `json:"tools,omitempty"`
	ToolChoice any        `json:"tool_choice,omitempty"`
	// Think is the client's thinking control, carried exactly as the native
	// chat wire carries it (api.ChatRequest.Think): `thinking:{type:…}` becomes
	// a bool and an `output_config.effort` becomes a level. The OpenAI
	// spellings (reasoning / reasoning_effort) can say "none" or a level but
	// NOT the plain "yes, think" a `type:"enabled"` block means, so forwarding
	// the control through them would answer a client that asked for thinking
	// with the upstream's default instead — the same defect round 41 fixed on
	// the gateway leg. The upstream this proxy talks to is oaica's own server,
	// which reads this field (2026-09-27 audit, round 50).
	Think         *api.ThinkValue `json:"think,omitempty"`
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
			// The named openAIToolCall, not an anonymous twin: the two
			// round-55 helpers that read a nameless call's arguments take
			// []openAIToolCall, and a structurally identical anonymous struct
			// does not convert to it (slice element identity compares tags —
			// the "tags are ignored" rule is for direct struct conversion
			// only).
			ToolCalls []openAIToolCall `json:"tool_calls,omitempty"`
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

// toolCallArgumentsSize is the size of the argument JSON the tool_use blocks
// relay, for the output estimate. api.ToolCallFunction.Arguments is an ordered
// map rather than the upstream's raw string, so it is measured by its rendered
// form — the same bytes the converter writes into the client's block — and an
// empty argument map (which renders "{}") counts nothing (2026-09-27 audit,
// round 23).
func toolCallArgumentsSize(calls []api.ToolCall) int {
	n := 0
	for i := range calls {
		if s := calls[i].Function.Arguments.String(); s != "" && s != "{}" {
			n += len(s)
		}
	}
	return n
}

// statedCacheHit reports the cache-read count the upstream actually stated,
// with no clamp against the prompt total: a stated hit is evidence ABOUT the
// prompt, so a hit larger than the total means the total is short, not that the
// hit is wrong (2026-09-27 audit, round 40, A40-3). Details win over the
// sibling name and each keeps its own last positive statement; a non-positive
// count is no statement at all.
func (u *openAIUsage) statedCacheHit() int {
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
	// A NON-positive details value is no statement either, and it must not
	// suppress the sibling that does state the hit: testing `== 0` here let an
	// explicit cached_tokens: -5 short-circuit the fallback and then be
	// discarded, so a body the gateway leg reports as 900 cached tokens was
	// reported by this leg as 0 — the same usage object answered two ways,
	// decided by which leg served it (2026-09-27 audit, round 42, C42-2).
	if c <= 0 {
		c = u.PromptCacheHitTokens
	}
	if c < 0 {
		return 0
	}
	return c
}

// cachedTokens used to be the stated hit clamped to a stated prompt, and it had
// exactly one job left after round 41: the arithmetic that subtracted a hit
// from a total. It is gone, because that arithmetic never needed the clamp —
// every site that reports the hit raises its prompt total to the UNCLAMPED one
// first (the non-stream leg's PromptEvalCachedCount, the streaming tail's
// message_delta patch), which is the round-40 rule: a stated hit is evidence
// ABOUT the prompt, so the total is raised to it and never the hit clamped down
// to a size the upstream never stated. The one clamp left that could flatten a
// hit is UsageFromMetrics' own (cache_read against input_tokens), and by then
// the total has already been raised. Two readers for one usage object was how
// round 41's divergence stayed hidden: the streaming tail took the clamped one
// and the non-stream leg the unclamped one, so the same upstream object was
// answered input=0/cache_read=100 streamed and input=4900/cache_read=100 whole
// (2026-09-27 audit, round 42, A42-1).

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

// cacheReadPtr renders a measured cache-read count as the optional field the
// Anthropic wire carries (api.Metrics.PromptEvalCachedCount is *int): nil when
// the upstream stated no cache measurement at all, so a response with no
// reading keeps the shape it had, and a stated count — including a stated 0 —
// travels as the measurement it is.
func cacheReadPtr(u *openAIUsage) *int {
	if u == nil || (u.PromptTokensDetails == nil && u.PromptCacheHitTokens == 0) {
		return nil
	}
	// UNCLAMPED: this is a reporting site, and the prompt total above it has
	// already been raised to the hit. Clamping here reported
	// cache_read=100/input=4900 for an upstream that stated 5000 cached of a
	// 100-token prompt, while the streaming leg reported 0/100 for the same
	// object and the gateway's own row reported 0/5000 — three answers to one
	// usage object (2026-09-27 audit, round 42, A42-1).
	return intPtr(u.statedCacheHit())
}

// intPtr is a fresh *int per call: handing out the address of a loop variable
// would publish one cell to every site that reads it.
func intPtr(v int) *int { return &v }

// mergeUsage folds a later chunk's usage object into the accumulated one FIELD
// BY FIELD. SSE usage is not cumulative: a build may narrate the running counts
// per chunk and state only what it has just measured, so assigning each chunk
// whole kept whichever chunk arrived LAST — and a closing usage-only chunk that
// states the cache hit while omitting prompt_tokens zeroed the prompt it is
// clamped against, so cachedTokens() returned 0 and the client was told
// cache_read_input_tokens=0 for a turn the gateway's ledger recorded 900: the
// two records of the same request disagreed, and a session's cache-efficiency
// read-out showed a hit as a miss (2026-09-27 audit, round 38, B-F1). Only a
// positive count is a statement; silence keeps what an earlier chunk stated.
// The gateway's usage.merge is the same function on the server side.
func mergeUsage(acc, next *openAIUsage) *openAIUsage {
	if next == nil {
		return acc
	}
	if acc == nil {
		clone := *next
		if next.PromptTokensDetails != nil {
			d := *next.PromptTokensDetails
			clone.PromptTokensDetails = &d
		}
		return &clone
	}
	if next.PromptTokens > 0 {
		acc.PromptTokens = next.PromptTokens
	}
	if next.CompletionTokens > 0 {
		acc.CompletionTokens = next.CompletionTokens
	}
	if next.PromptCacheHitTokens > 0 {
		acc.PromptCacheHitTokens = next.PromptCacheHitTokens
	}
	if next.PromptTokensDetails != nil {
		if acc.PromptTokensDetails == nil {
			d := *next.PromptTokensDetails
			acc.PromptTokensDetails = &d
		} else if next.PromptTokensDetails.CachedTokens > 0 {
			acc.PromptTokensDetails.CachedTokens = next.PromptTokensDetails.CachedTokens
		}
	}
	return acc
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
//
// The one exception is a bare repeat: the delta names the call again and
// states no arguments over a call that has accumulated none. An empty argument
// string is a COMPLETE argument list for a call that takes no arguments, so
// two argument-less calls in one index-less stream are two calls — reading the
// second as a continuation delivered one tool_use where the model asked for
// two (2026-09-27 audit, round 39, B-F9). The gateway leg's toolKey applies the
// same rule to the same wire.
func startsANewToolCall(accID, accName, accArgs, deltaID, deltaName, deltaArgs string) bool {
	if accID == "" && accName == "" {
		return false
	}
	if deltaID != "" && accID != "" && deltaID != accID {
		return true
	}
	if deltaName == "" {
		return false
	}
	// A DIFFERENT name can only be introducing a call: an argument
	// continuation carries arguments alone. Keeping one call whole here when
	// the arguments are not yet valid JSON gave the client a single tool_use
	// named after the LAST call with the two calls' arguments concatenated —
	// a well-formed-looking call with the wrong name, which is worse than a
	// visibly truncated one, because nothing about it signals the damage. The
	// gateway leg's toolKey splits on exactly this name change, and one wire
	// must be answered one way (2026-09-27 audit, round 40, A40-8).
	if deltaName != accName {
		return true
	}
	raw := strings.TrimSpace(accArgs)
	if raw != "" && json.Valid([]byte(raw)) {
		return true
	}
	return raw == "" && strings.TrimSpace(deltaArgs) == ""
}

// mapToolChoice converts an Anthropic ToolChoice to an OpenAI tool_choice value.
func mapToolChoice(tc *anthropic.ToolChoice) any {
	if tc == nil {
		return nil
	}
	// The type is normalized before it is read, exactly as both sibling legs
	// normalize theirs (the converter's own dropTools test reads
	// EqualFold(TrimSpace(type), "none") at anthropic.go:589, and the metered
	// gateway lowercases and trims before its switch). Matching the bare
	// literals sent `{"type":"Any"}` and `{"type":"Tool","name":"read_file"}`
	// down the default arm, so the same body forced a tool call on the gateway
	// leg and left the model free here — and the converter's own case-insensitive
	// reading of "none" already proved the two legs disagree about the spelling
	// (2026-09-28 audit, round 54).
	switch strings.ToLower(strings.TrimSpace(tc.Type)) {
	case "auto":
		return "auto"
	case "any":
		return "required"
	case "none":
		// "none" states nothing on this wire. The client's instruction is
		// carried by the tool surface itself — FromMessagesRequest drops every
		// tool for this type — and the metered gateway leg, given the same
		// body, sends no choice field at all: the OpenAI `none` value is not
		// the shape the backends this fleet runs validate without objection,
		// and stating it beside an empty tool list is a field no sibling leg
		// sends (2026-09-27 audit, round 50).
		return nil
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
	// The thinking control FromMessagesRequest decoded, carried to the upstream
	// in the same shape the native chat wire carries it (see openAIChatRequest.
	// Think): the local leg hands the daemon this exact value, and the gateway
	// leg hands its backend the same switch as chat_template_kwargs.
	oai.Think = chatReq.Think

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
	if len(oai.Messages) == 0 {
		// A turn whose every block the conversion does not forward leaves no
		// message at all, and the wire spelling for that is `"messages":null`
		// — a malformed body, answered by the upstream with a 400 worded for
		// an OpenAI client and matched by no recovery path this proxy has. The
		// conversation the client sent really is empty of representable
		// content, so the honest request is an empty turn (2026-09-27 audit,
		// round 35: the client-leg half of A-F2's leads).
		oai.Messages = []openAIMessage{{Role: "user"}}
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
//
// A system message that carries anything BUT text — an image part, a tool call
// — cannot be merged into that one string, and the rewrite would silently drop
// what it carries: the same rule the other two legs apply (the local leg's
// anthropic.FromMessagesRequest and the gateway's /v1/messages bridge), and the
// reason all three return a conversation holding one exactly as it arrived
// (2026-09-27 audit, round 42, C42-5).
//
// That check is over the WHOLE conversation, so the scan below has no early
// exit: stopping at the first system message that arrived after a non-system
// one — the rewrite was already decided by then — left a later image-carrying
// system message unguarded, and the rewrite merged it into a bare string and
// dropped the image (2026-09-27 audit, round 43, A43-1).
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
			if !systemMessageIsTextOnly(m) {
				return msgs
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

// systemMessageIsTextOnly reports whether a system message carries nothing but
// its text: no image part, no tool call, no tool-result id. Only such a message
// can be merged into the single leading system string normalizeSystemFirst
// builds.
func systemMessageIsTextOnly(m openAIMessage) bool {
	return len(m.Images) == 0 && len(m.ToolCalls) == 0 && m.ToolCallID == ""
}

// imageDataURL sniffs the image magic bytes for the data-URL MIME type
// (api.ImageData carries raw bytes with no type). Defaults to jpeg — vLLM's
// Qwen3.5 vision preprocessor accepts the common web formats.
//
// The MIME type is not the client's to choose: the upstream is oaica's own
// OpenAI door, and that door carries EXACTLY four types in a data URL — jpeg,
// jpg, png and webp (openai.decodeImageURL, and its test pins the refusal of
// every other type with 400 "invalid image input"). The sniffer below also
// recognises GIF, so a client that sent one was handed back a data URL its own
// upstream refuses: the local leg serves the same body (it passes the raw bytes
// with no label at all and the runner labels them from the content), so the
// same client body was a served turn on one leg and a hard 400 on this one
// (2026-09-27 audit, round 50, A50-4). A GIF is re-encoded to PNG here instead
// — the same remedy llm.llamaServerMediaBytes already applies to WebP for the
// runner, and the same bytes the model would have seen, since a still image is
// what one frame of the conversation can carry either way.
func imageDataURL(img api.ImageData) string {
	// A source of type "url" is carried as its own URL text (see
	// anthropic.IsImageURL): the wire takes it as it stands, and encoding it as
	// base64 would send the upstream a picture of a string.
	if anthropic.IsImageURL(img) {
		return string(img)
	}
	mime := "image/jpeg"
	switch {
	case len(img) >= 8 && img[0] == 0x89 && img[1] == 'P' && img[2] == 'N' && img[3] == 'G':
		mime = "image/png"
	case len(img) >= 3 && img[0] == 'G' && img[1] == 'I' && img[2] == 'F':
		if encoded, ok := reencodeGIFAsPNG(img); ok {
			return "data:image/png;base64," + base64.StdEncoding.EncodeToString(encoded)
		}
		// It says GIF and decodes as no image at all. Falling through labels it
		// jpeg, the door takes it, and the runner labels it from the content
		// again — the client's bytes travel, mislabelled exactly as any payload
		// this sniffer does not know is mislabelled.
		mime = "image/jpeg"
	case len(img) >= 12 && string(img[0:4]) == "RIFF" && string(img[8:12]) == "WEBP":
		mime = "image/webp"
	}
	return "data:" + mime + ";base64," + base64.StdEncoding.EncodeToString(img)
}

// reencodeGIFAsPNG decodes a GIF and re-encodes its first frame as PNG,
// reporting whether it could.
func reencodeGIFAsPNG(data []byte) ([]byte, bool) {
	src, err := gif.Decode(bytes.NewReader(data))
	if err != nil {
		return nil, false
	}
	var buf bytes.Buffer
	if err := png.Encode(&buf, src); err != nil {
		return nil, false
	}
	return buf.Bytes(), true
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
func parseOpenAIToolCalls(tcs []openAIToolCall, truncated bool) []api.ToolCall {
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
	// usedIDs holds every id the upstream STATED in this list, so a
	// synthesized id cannot land on one of them (A46-4).
	usedIDs := make(map[string]bool, len(tcs))
	// seenID records, per stated id, the call it was first stated for, so a
	// second, different call under the same id is re-minted rather than
	// dropped (A45-3).
	seenID := make(map[string]string, len(tcs))
	// seenInList counts how often each call identity has appeared in this
	// list, so a repeat takes the streaming converter's "#n" rule (A45-2).
	seenInList := make(map[string]int, len(tcs))
	for _, tc := range tcs {
		if tc.ID != "" {
			usedIDs[tc.ID] = true
		}
	}
	for _, tc := range tcs {
		if strings.TrimSpace(tc.Function.Name) == "" {
			// A call the upstream never named is not a call on this path either:
			// its arguments reach the client as TEXT (relayUnnamedCallArguments,
			// the same fate the streaming arm gives such a fragment, and the one
			// both other legs' arms give it), so no block is built from it — and,
			// because no block is built, the id it states is not this call's to
			// claim. Claiming it here made an entry the client never sees decide
			// the id of a call it does: an upstream that numbers its first
			// fragment and then states that id again for the call itself (a
			// nameless fragment carrying the arguments, the call carrying the
			// name) reached the client with the stated id on the local server's
			// two paths and on this bridge's own whole-list arm, and with a
			// MINTED one here, because the reuse rule below read the dropped
			// fragment as the id's first owner — the id the model's call actually
			// answered to was gone, and two legs answered one body with two
			// different ids (2026-09-28 audit, round 65, F65-L2-2).
			//
			// The pre-pass above still records such an entry's id: it exists so a
			// minted id cannot land on an id the wire states, which is a question
			// about the TEXT of the turn and not about who owns it (the local
			// server's ToMessagesResponse reserves exactly the same way).
			continue
		}
		var args api.ToolCallFunctionArguments
		raw := strings.TrimSpace(tc.Function.Arguments)
		if raw == "" {
			raw = "{}"
		}
		if err := json.Unmarshal([]byte(raw), &args); err != nil {
			if truncated {
				// The turn stopped at the token limit with argument JSON that
				// never parsed: this is a fragment the model was still writing,
				// not a call. The `_raw` fallback below is for a model that
				// emits freeform arguments, and it cannot tell the two apart —
				// so on a truncated turn it hands the agent a complete,
				// executable tool_use whose input is a key the model never
				// wrote, while mapStopReason reports "tool_use" and hides the
				// "length" the upstream stated. The delta path drops the same
				// fragment (flushToolCalls); this is that rule for the paths
				// that never accumulate (2026-09-27 audit, round 17).
				continue
			}
			// Fall back to a single-key map carrying the raw string so we
			// never drop a tool call entirely.
			args = api.NewToolCallFunctionArguments()
			args.Set("_raw", raw)
		}
		key := raw
		if b, err := json.Marshal(args); err == nil {
			key = string(b)
		}
		// The call's own identity, keyed exactly as the streaming converter
		// keys it: name plus canonical arguments.
		identity := "\x00" + tc.Function.Name + "\x00" + key
		id := tc.ID
		minted := false
		if id != "" {
			// An id the upstream states is that call's own, unless it already
			// stated the same id for a DIFFERENT call. Deduping on the id alone
			// threw that second call away — the turn reached the client as one
			// tool_use under stop_reason "tool_use" while the model had asked
			// for two, and the streaming converter (and the gateway leg) each
			// recover it, so the same body had two answers (2026-09-27 audit,
			// round 45, A45-3). A reused id is re-minted below from the call
			// itself, never from a counter.
			if owner, ok := seenID[id]; ok && owner != identity {
				id = ""
			} else {
				seenID[id] = identity
			}
		}
		if id == "" {
			// The upstream sent no id (several OpenAI-compatible GGUF
			// backends), or reused one id for a second call. Passing an absent
			// id through left the client with a tool_use block carrying NO id
			// at all — ContentBlock.ID is omitempty — so it could not name the
			// call back, and its tool_result then carried tool_use_id "" into
			// the next request as an empty OpenAI tool_call_id. Same rule as
			// the streaming path, one helper (2026-09-26 audit, thirteenth
			// round). The key is the canonical encoding of the parsed
			// arguments, so two parallel calls that differ only in whitespace
			// are one call.
			//
			// Two IDENTICAL id-less calls are two calls, not one call stated
			// twice: the id is a pure function of name and arguments, so the
			// repeat takes the streaming converter's own rule — a distinct key
			// and a distinct "#n" id. Collapsing the pair delivered one
			// tool_use where the model asked for two, under stop_reason
			// "tool_use", on a wire the streaming path of this same leg (and
			// both paths of the local server) answers with two blocks
			// (2026-09-27 audit, round 45, A45-2; round 39, B-F9 and round 40,
			// A40-6 fixed the stream twins). A restatement of the same call
			// under the same STATED id is still a duplicate: `seen` below is
			// keyed on the id, and that is what it exists for.
			n := seenInList[identity]
			seenInList[identity] = n + 1
			mintedKey := key
			if n > 0 {
				mintedKey = key + "#" + strconv.Itoa(n)
			}
			id = anthropic.ToolCallIDFor(tc.Function.Name, mintedKey)
			// A synthesized id must not be one the upstream STATED for another
			// call in this same list: the id-less call and the call the
			// upstream named would then reach the client as two entries under
			// one id, which the local server's translation reads as one call
			// restated and drops — the agent ran one of the model's two calls.
			// The id is a pure function of the call, so a collision is bumped
			// onto the same "#n" rule the repeats already use
			// (2026-09-27 audit, round 46, A46-4).
			for k := 0; usedIDs[id]; k++ {
				id = anthropic.ToolCallIDFor(tc.Function.Name, mintedKey+"\x00#"+strconv.Itoa(k))
			}
			minted = true
			usedIDs[id] = true
		}
		// The dedup key namespaces a MINTED id apart from a STATED one. The
		// minted id for an id-less call is a pure function of its name and
		// arguments, so a later call that STATES that same string is a
		// different call whose id merely collides with it — deduping on the
		// bare string dropped a call the upstream had named, while the
		// streaming converter emitted both blocks under one id, so one body
		// had two answers and neither was two routable calls (2026-09-27
		// audit, round 46, A46-4). A restatement of the same call under the
		// same STATED id is still one call: `seen` keys the stated ids
		// verbatim, which is what that rule is for.
		dedupKey := "\x01" + id
		if minted {
			dedupKey = "\x00" + id
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
		// A turn the upstream ended at the token limit (finish_reason "length")
		// must not have its unfinished argument fragments dressed up as calls;
		// see parseOpenAIToolCalls.
		chatResp.Message.ToolCalls = parseOpenAIToolCalls(c.Message.ToolCalls, c.FinishReason == "length")
		chatResp.DoneReason = mapFinishReason(c.FinishReason)
	}
	if resp.Usage != nil {
		// The TOTAL prompt, uncached included: Metrics is the shape
		// anthropic.UsageFromMetrics splits, and it derives input_tokens as
		// total minus the cache read. Pre-subtracting the cached part here
		// made that function subtract it a second time and report
		// input_tokens 0 for a prompt the cache had served — a 5000-token
		// prompt with 4096 cached arrived at the client as {input 0,
		// cache_read 904} (2026-09-26 audit follow-up). The call site pairs
		// this with PromptEvalCachedCount, so input+cache_read is the real
		// prompt length: neither double-counted nor missing.
		chatResp.Metrics.PromptEvalCount = resp.Usage.PromptTokens
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
//
// credentialIsOurs reports WHOSE credential the resolved header carries: true
// when it is a key oaica resolved for this row, false when it is the user's
// own — which happens on a native claude/* leg, on the keyless
// api.anthropic.com row below, and on any row that declares the user's own
// ANTHROPIC_API_KEY. It is returned here, rather than left for the caller to
// infer from BaseURL, because the inference was wrong for exactly that row: its
// keyless fallback sends the user's credential, so a 401 under it was rewritten
// as if oaica's own key had been refused (round 57, F57-L2-1) — and reading
// "some key resolved" as oaica's own did the same to the row's keyed spelling
// (round 58, F58-L2-1).
func (route proxyRoute) anthropicPassthroughTarget() (upstream, headerName, headerValue string, credentialIsOurs, ok bool) {
	if route.Wire != "anthropic" {
		return "", "", "", false, false
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
				return "", "", "", false, false
			}
			auth, found := resolveNativeAnthropicAuth()
			if !found {
				return "", "", "", false, false
			}
			// The user's own credential, on a row that has none of oaica's.
			return strings.TrimRight(route.BaseURL, "/") + "/messages", auth.Header, auth.Value, false, true
		}
		// Whose key is it? A row that names ANTHROPIC_API_KEY in api_key_env
		// resolved the USER's own credential — the same variable, and the same
		// bytes on the wire, as the keyless branch above and the native
		// claude/* leg. Its 401 means the user re-runs `claude /login`, so it
		// is relayed, not rewritten as an oaica-side fault; deciding by "a key
		// was resolved" read the row's key source as its owner and sent the
		// user into a 502 retry loop over their own refused key (2026-09-28
		// audit, round 58, F58-L2-1). A key oaica stored for the row is still
		// ours.
		return strings.TrimRight(route.BaseURL, "/") + "/messages", "x-api-key", key, !route.userOwnAnthropicKeyEnv(), true
	}
	auth, found := resolveNativeAnthropicAuth()
	if !found {
		return "", "", "", false, false
	}
	return nativeAnthropicUpstream, auth.Header, auth.Value, false, true
}

// anthropicRemoteModelsTarget is anthropicPassthroughTarget's /models
// sibling: same upstream base and same credential, one path segment over, for
// proxying GET /v1/models. Ok is false for a native claude/* leg (no BaseURL
// to list from) — its caller forwards to api.anthropic.com instead.
func (route proxyRoute) anthropicRemoteModelsTarget() (upstream, headerName, headerValue string, ok bool) {
	if route.BaseURL == "" {
		return "", "", "", false
	}
	upstream, headerName, headerValue, _, ok = route.anthropicPassthroughTarget()
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
		// The request's own shape, before any translation. This is the one
		// translation site that had NO such check: the local server's handler
		// refuses a body with no max_tokens, a non-positive one, or no messages
		// at all (middleware/anthropic.go), and the gateway leg refuses the same
		// three (tools/gateway/messages.go), where here they were served: a body
		// asking for a refusal (max_tokens 0) had `max_tokens` dropped from the
		// upstream request so the backend applied its own cap, a negative one
		// was forwarded as written, and a body with no messages was converted
		// into an empty conversation — one body answered three ways depending on
		// which leg the client reached (2026-09-27 audit, round 49, B-F1).
		//
		// A MISSING model is deliberately not one of them: this proxy was
		// started with an upstream model and falls back to it, which is a
		// pinned, documented behaviour of this entry point
		// (TestProxyFallsBackToFixedModelWhenRequestOmitsIt) — a body the local
		// server cannot serve at all is servable here, so the two legs answer it
		// differently on purpose (2026-09-27 audit, round 49, rejection).
		if anthReq.MaxTokens <= 0 {
			refuse(http.StatusBadRequest, "max_tokens is required and must be positive")
			return
		}
		if len(anthReq.Messages) == 0 {
			refuse(http.StatusBadRequest, "messages is required")
			return
		}
		// promptBytes is the unit the context-fit clamp below and every
		// calibration read/write in this handler share: the CONVERTED body —
		// the OpenAI-wire request the upstream is handed, which is where every
		// block the conversion does not forward stops being charged — with an
		// inline image's transport encoding charged as an image
		// (prompt_payload_bytes.go). Computed once, so the ratio a sample
		// records is exactly the ratio a later estimate is scaled by.
		promptBytes := clientPromptBytes(body)
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
				upstream, headerName, headerValue, credentialIsOurs, ok := route.anthropicPassthroughTarget()
				if !ok {
					refuse(http.StatusUnauthorized,
						fmt.Sprintf("no credential for %s — run `oaica auth login %s`, or set the key's env var", route.UpstreamModel, strings.TrimPrefix(route.Label, "remote:")))
					return
				}
				status, relayed := anthropicPassthrough(w, r, rewritten, upstream, headerName, headerValue, table.SessionID, credentialIsOurs)
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

		// A url-source image is carried through api.ImageData as its own URL
		// text (anthropic.resolveImageSource), and imageDataURL hands it to the
		// upstream as it stands. This leg's upstream is oaica's OWN OpenAI door,
		// and that door carries exactly four image types in a `data:` URL and no
		// URL scheme at all: every http(s) address and every other data URL came
		// back 400 "invalid image input" / "image URLs are not currently
		// supported", which this proxy reported to the client as a 502 — a 5xx,
		// which clients retry, so an image-by-URL turn could neither succeed nor
		// be told apart from an upstream outage, while the local leg's own
		// /v1/messages handler refuses the same body in words
		// (middleware/anthropic.go, round 41, C41-13). The verdict is the same
		// one: this leg's model is given image BYTES, and a URL is not one
		// (2026-09-27 audit, round 51).
		for _, m := range chatReq.Messages {
			for _, img := range m.Images {
				if anthropic.IsImageURL(img) {
					refuse(http.StatusBadRequest,
						`image source.type "url" cannot be represented on this leg: the model is given image bytes, and this url arrived as the address text. Send the image as a base64 source instead.`)
					return
				}
			}
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
			estTokens, margin, _ := contextFitPlan(calib, calibKey, promptBytes)
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
				// Every leg is planned with its OWN calibration (see planFor):
				// the swap decision judges the destination by the numbers its
				// upstream earned, and so does the budget the request is
				// actually sent with.
				planFor := func(leg proxyRoute) (int, int) {
					est, m, _ := contextFitPlan(calib, legCalibrationKey(table.SessionID, leg), promptBytes)
					return est, m
				}
				if over, swapped := table.oversizeSwap(route, estTokens, margin, planFor); swapped {
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
							upstream, headerName, headerValue, credentialIsOurs, ok := over.anthropicPassthroughTarget()
							if !ok {
								refuse(http.StatusUnauthorized, fmt.Sprintf("no credential for %s — run `oaica auth login %s`, or set the key's env var", over.UpstreamModel, strings.TrimPrefix(over.Label, "remote:")))
								return
							}
							// credentialIsOurs comes from the target that resolved
							// the header, not from over.BaseURL: the keyless
							// api.anthropic.com row HAS a BaseURL and still goes out
							// under the user's own credential, so the BaseURL test
							// read a user's refused key as oaica's and rewrote the
							// 401 to a 502 (round 57, F57-L2-1).
							status, relayed := anthropicPassthrough(w, r, nativeBody, upstream, headerName, headerValue, table.SessionID, credentialIsOurs)
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
					// The estimate is the DESTINATION's now, not the leg this
					// request was planned on: the budget the request goes out
					// with has to be the destination's honest count of these
					// bytes, and so does the refusal message below if even that
					// one is not enough (2026-09-28 audit, round 55).
					estTokens, margin = planFor(route)
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
					calib.record(calibKey, promptBytes, promptTokens)
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
		recordUsage := func(promptTokens int) { calib.record(calibKey, promptBytes, promptTokens) }

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
		estInputTokens, _, _ := contextFitPlan(calib, calibKey, promptBytes)
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
	// A whole completion that says nothing — no text, no reasoning, no call
	// the client could run — is not an answer. Relayed, it reached the client
	// as a successful assistant turn with zero content blocks and end_turn:
	// nothing to render, nothing to run, so a session that should have
	// surfaced an upstream failure simply stopped, and the leg recorded a
	// healthy 200 for it. The gateway leg refuses the identical document with
	// 502 ("upstream returned an empty completion", messages.go), and this
	// path already refuses the same body with no choices at all — the verdict
	// must not depend on which of the two empty shapes the upstream sent
	// (2026-09-27 audit, round 45, A45-5).
	//
	// Asked of the UPSTREAM's document, not of the blocks that survived
	// translation: a call the upstream never named is not a call
	// (parseOpenAIToolCalls drops it above, and a truncated fragment with it),
	// so the document says nothing, exactly as the gateway leg's
	// documentSaysSomething reads the same body. A call that IS named says
	// something even when its arguments were cut off — the turn is refused
	// nowhere for that, it is answered with the stop_reason the upstream
	// stated.
	choice := oaiResp.Choices[0]
	// A call the upstream never named: content_block_start is the only event
	// that carries a name, so there is no block to open for it and it is not a
	// call the client can make. Its arguments are still the model's output and
	// they were billed, so they are relayed as TEXT — which is what BOTH arms of
	// the gateway do (messages.go: the non-stream arm appends a text block per
	// unnamed fragment, the stream arm opens a text block for it) and what this
	// leg's own streaming path does (flushToolCalls). This path alone dropped
	// them, and a whole completion whose ONLY payload was such a fragment was
	// refused 502 as an "empty completion" while the byte-identical stream
	// answered it 200 with the arguments as prose: the verdict depended on which
	// shape the upstream sent, not on what it said (2026-09-28 audit, round 54).
	//
	// Dropped from the parsed list as they are harvested: the estimate below
	// counts the arguments once, as the stream path counts them once
	// (streamedText += unnamedText.Len(), plus the NAMED calls only).
	relayUnnamedCallArguments(&chatResp.Message, choice.Message.ToolCalls)
	namedCall := false
	for _, tc := range choice.Message.ToolCalls {
		if strings.TrimSpace(tc.Function.Name) != "" {
			namedCall = true
			break
		}
	}
	// Not with TrimSpace: a text is content whatever it says, and the gateway
	// leg's documentSaysSomething already reads the identical body that way —
	// relayDelta relays a content of whitespace as a text block, so trimming
	// here refused a turn the other two legs answer (and this leg's own
	// streaming path answers too), on a body whose text the upstream did write
	// (2026-09-27 audit, round 44's rule; reintroduced here in round 45 and
	// caught in round 46, A46-2).
	if chatResp.Message.Content == "" &&
		firstNonEmpty(choice.Message.Reasoning, choice.Message.ReasoningContent) == "" &&
		!namedCall {
		writeAnthropicError(w, http.StatusBadGateway, "upstream returned an empty completion")
		return false
	}
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
	// A stated cache hit is evidence ABOUT the prompt: it says at least this
	// many tokens were there. Left alone, ToMessagesResponse derived
	// input_tokens from the estimate and UsageFromMetrics clamped the hit back
	// down to it, so an upstream that stated "900 of this prompt came from
	// cache" and no prompt size was reported to the client as 8 cached tokens
	// of an 8-token prompt — while the streaming leg, given the identical
	// usage object, reported 900. Same turn, two answers, decided by `stream`
	// (2026-09-27 audit, round 41: A41-1, C41-1). Raise the total to the hit,
	// exactly as the streaming tail and the gateway's entry() do.
	if hit := oaiResp.Usage.statedCacheHit(); hit > chatResp.Metrics.PromptEvalCount {
		chatResp.Metrics.PromptEvalCount = hit
	}
	// The estimate counts what was relayed to the client, and a reasoning
	// model's thinking is relayed as thinking deltas and billed as output just
	// like its answer is — counting the answer alone made a long reasoning turn
	// look nearly empty (2026-09-27 audit, round 17). A tool call's argument
	// JSON is relayed in the tool_use block and billed the same way, so a
	// tool-only turn is not an empty one (2026-09-27 audit, round 23).
	if !oaiResp.Usage.statedCompletionTokens() {
		produced := len(chatResp.Message.Content) + len(chatResp.Message.Thinking) +
			toolCallArgumentsSize(chatResp.Message.ToolCalls)
		if produced > 0 {
			chatResp.Metrics.EvalCount = produced/4 + 1
		}
	}
	// The cache-read count travels as METRICS, not as a patch on the finished
	// response: ToMessagesResponse derives input_tokens as the UNCACHED prompt
	// (total minus cache reads, Anthropic's own semantics — see UsageFromMetrics),
	// and assigning cache_read after the fact left input_tokens at the full
	// total, so a client summing the two read twice the real prompt.
	chatResp.Metrics.PromptEvalCachedCount = cacheReadPtr(oaiResp.Usage)
	anthResp := anthropic.ToMessagesResponse(anthropic.GenerateMessageID(), chatResp)
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
	// toolArrival is the order the stream first wrote each slot — the order the
	// calls were INTRODUCED, which is the order flushToolCalls hands them to the
	// client. The slot is the upstream's own index whenever it states one, so
	// ordering by slot ordered by that index: an upstream whose fragment order
	// and stated positions disagree (the call at index 1 written before the call
	// at index 0) had its calls delivered in an order its own whole-completion
	// document does not use — where this arm hands the client [b, a], the
	// whole-list arm reads the array and answers [a, b], and the gateway leg
	// keeps the wire's own order on both its arms (2026-09-28 audit, round 63,
	// F63-L2-1).
	toolArrival := map[int]int{}
	nextToolArrival := 0
	// nextFreeToolSlot is the slot for a call the upstream gave none: one that
	// omits the index entirely, or one that states an index already carrying a
	// different call. It is kept above every index the stream has stated, so a
	// numbered call can never land on a slot another call owns (2026-09-28
	// audit, round 58, F58-L3-2).
	nextFreeToolSlot := 0
	// lastToolSlot is the slot of the call this stream last named, so an
	// index-less argument continuation is written into THAT call and not into
	// whichever call happened to start at slot zero (2026-09-28 audit, round
	// 58, F58-L3-1).
	lastToolSlot := -1
	// indexSlot maps each index the stream has stated to the slot of the call
	// that index currently holds (see the indexed arm below): a vendor that
	// writes one index for every call of the turn leaves that index naming the
	// NEWEST call written into it, so a later fragment stating the index
	// continues that call rather than the first one (2026-09-28 audit, round
	// 59, F59-L2-1).
	indexSlot := map[int]int{}
	// indexChain is every slot one stated index has named, in the order the
	// stream named them — the turn's calls as the vendor that writes ONE index
	// for all of them introduced them. An argument fragment that states no id
	// and no name is matched to the oldest of those calls that has not yet
	// received its arguments, while the newest has received none of its own
	// (2026-09-28 audit, round 61, F61-L2-2).
	indexChain := map[int][]int{}
	// seenIndex records the indices the stream has stated. A fresh index is the
	// wire's own statement that a call begins there (2026-09-28 audit, round 61,
	// F61-L2-3).
	seenIndex := map[int]bool{}

	// argsFinished reports whether an argument text is a call's own finished
	// argument list: a complete JSON object — the one spelling the wire has for
	// a call that states its arguments — or freeform text rather than the
	// beginning of an object. An empty argument list is NOT finished: a fragment
	// that names a call states no arguments either, so an argument-less call and
	// a call whose arguments have not arrived yet look the same. This is the
	// gateway leg's argsAreFinished, and this leg's answer to the same wire has
	// to be the same answer.
	argsFinished := func(raw string) bool {
		s := strings.TrimSpace(raw)
		if s == "" {
			return false
		}
		if !strings.HasPrefix(s, "{") {
			return true
		}
		return json.Valid([]byte(s))
	}
	// finishedObjectArgs reports whether an argument text is a COMPLETE JSON
	// object and nothing else — the one shape two of which cannot be
	// concatenated into JSON. argsFinished above counts freeform text as
	// finished too, because freeform arrives once, whole; that is why the bytes
	// that have nowhere left to go are identified by this narrower test, and a
	// freeform continuation is left where the ordinary route puts it: more of
	// the same call's own line. It is the gateway leg's finishedObjectArgs, and
	// this leg's answer to the same wire has to be the same answer.
	finishedObjectArgs := func(raw string) bool {
		s := strings.TrimSpace(raw)
		return strings.HasPrefix(s, "{") && json.Valid([]byte(s))
	}
	// canonicalArgs re-encodes an argument text the way this leg's own mint and
	// its non-stream list encode it (the parsed object, or the `_raw` wrapper for
	// text that is not one), so two spellings of one call — whitespace, key order
	// — compare equal here as they do everywhere else in this file.
	canonicalArgs := func(raw string) string {
		s := strings.TrimSpace(raw)
		var args api.ToolCallFunctionArguments
		if err := json.Unmarshal([]byte(s), &args); err != nil {
			args = api.NewToolCallFunctionArguments()
			args.Set("_raw", s)
		}
		b, err := json.Marshal(args)
		if err != nil {
			return s
		}
		return string(b)
	}

	// canExtend reports whether a fragment's argument bytes can still be more of
	// the call whose arguments have accumulated as accArgs — the one question
	// that decides whether appending them is a continuation or damage. A COMPLETE
	// object takes nothing (two of them never concatenate into JSON); an object
	// in progress takes more of itself, INCLUDING a nested object — `{"query":`
	// ++ `{"sql":"select 1"}` ++ `,"limit":10}` is one call's value in progress
	// and the same vendor's document holds that whole string; a freeform line —
	// the model's whole command, delivered once (round 51's G1) — takes more of
	// the same line and not the beginning of an object, where a fragment after a
	// finished object is the NEXT call and the slot rules above own it
	// (2026-09-28 audit, round 61, F61-L2-4, F61-L2-5).
	//
	// What this predicate cannot do is split one call's fragments from two calls
	// that share them: `{"b":` ++ `{"c":3}` is a nested value to an object in
	// progress and a second call to a vendor that reuses its slots, and the bytes
	// are the same either way. The reading taken here is the one that agrees with
	// the SAME upstream's whole-list arm, where the turn's calls are separate
	// entries: fragments of one call are the pieces of that call's argument
	// string, so their concatenation is what the document holds — `{"b":` ++
	// `{"c":3}` ++ `2}` is `{"b":{"c":3}2}` on both arms, and it is only the
	// SLOT the fragment states that lets this arm tell two calls apart
	// (2026-09-28 audit, round 62; the append site is NOT guarded by the document
	// arm, which holds the same concatenation).
	canExtend := func(accArgs, delta string) bool {
		a := strings.TrimSpace(accArgs)
		d := strings.TrimSpace(delta)
		if d == "" || a == "" {
			return true
		}
		if finishedObjectArgs(a) {
			return false
		}
		if strings.HasPrefix(a, "{") {
			return true
		}
		return !strings.HasPrefix(d, "{")
	}

	// restatesAccumulatedCall reports whether one tool-call delta is the call the
	// accumulator already holds, restated — the same name (or none), the same
	// finished arguments, canonically equal — rather than more of it. Two
	// finished argument objects do not concatenate into JSON, so the only
	// reading that leaves the client a call it can run is that the upstream
	// listed the same call twice; appended, the client accumulated
	// `{"a":1}{"a":1}` for a call the model made once (2026-09-28 audit, round
	// 56, F1 and F2). Both this leg's non-stream list and the gateway leg answer
	// that wire with ONE call, and the local converter drops a stated id it has
	// already sent (seenStatedID).
	//
	// A delta that states an id at all must state THIS call's: an id the
	// accumulator does not hold is a second call the upstream numbered itself,
	// which the slot logic above and the split below are for (round 43's B43-3,
	// round 45's A45-3). A delta that states none never introduces one — it
	// arrived at this slot, and on the ordinary wire the slot is the upstream's
	// own index. The same wire listed one slot twice within a single delta (round
	// 56, F2): the second entry carried the same name and arguments and no id,
	// and the index it stated is the upstream's own slot identity, which is what
	// this answers.
	restatesAccumulatedCall := func(acc *toolAccum, id, name, args string) bool {
		if acc == nil || acc.name == "" {
			return false
		}
		if id != "" && id != acc.id {
			return false
		}
		if name != "" && name != acc.name {
			return false
		}
		accArgs := acc.args.String()
		return argsFinished(accArgs) && argsFinished(args) &&
			canonicalArgs(accArgs) == canonicalArgs(args)
	}

	// openSlotStating reports the slot of the accumulator a fragment's stated id
	// or name belongs to, when that call's arguments are still open — the call a
	// continuation is more of, whatever index the fragment carries. The id is
	// asked first and exactly (an id this stream has seen names the call it
	// introduced, wherever the vendor filed it); the name is asked only when
	// exactly ONE open call carries it, because two calls of one tool at one
	// index (round 60's F60-L2-1) are two calls by the vendor's own statement
	// and a name alone cannot choose between them. A call whose arguments are
	// already finished is never returned: more of a finished list is not a thing
	// the wire has, and the branches below are the ones that decide what such a
	// fragment is.
	//
	// The NAME is asked only of an index this stream has already stated (the
	// caller passes indexSeen). A fresh index is this leg's own signal for a new
	// call — nextFreeToolSlot is kept above every index the stream has stated for
	// exactly that reason — so a name-bearing fragment that states one is a call
	// of its own, not a continuation of a same-named call open elsewhere. Routed
	// by the name, its arguments were concatenated onto that call's open
	// arguments and the call it introduced did not exist, while the whole-list
	// arm reads the same body as three calls (2026-09-28 audit, round 61,
	// F61-L2-3). An id is still asked at any index: ids name calls, not slots.
	openSlotStating := func(id, name string, indexSeen bool) (int, bool) {
		if id != "" {
			byID, found := -1, false
			for s, a := range toolAccums {
				if a.id != id || argsFinished(a.args.String()) {
					continue
				}
				if !found || s < byID {
					byID, found = s, true
				}
			}
			if found {
				return byID, true
			}
		}
		if name != "" && indexSeen {
			byName, count := -1, 0
			for s, a := range toolAccums {
				if a.name != name || argsFinished(a.args.String()) {
					continue
				}
				if count == 0 || s < byName {
					byName = s
				}
				count++
			}
			if count == 1 {
				return byName, true
			}
		}
		return 0, false
	}
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

	// streamedText counts the characters actually sent to the client, so the
	// done event can report a usable output count even when the upstream
	// sent no usage (see the tally below). Declared here because flushToolCalls
	// adds to it the argument bytes of the calls it emits.
	streamedText := 0

	// truncation is closed over rather than passed in: it is decided by the
	// finish_reason the stream carried, and it changes what a fragment means.
	flushToolCalls := func(truncated bool) {
		// Emit any accumulated tool calls in index order as one ChatResponse.
		if len(toolAccums) == 0 {
			return
		}
		// Stable order by the order the calls were introduced. The slot the
		// fragment stated is the upstream's own index, and sorting by it put the
		// calls in the order that index names rather than the order the stream
		// wrote them: an upstream whose two disagree had this arm answer [b, a]
		// where its own whole-completion document and the gateway leg both answer
		// [a, b]. A slot with no recorded arrival (which cannot happen — every
		// accumulator is created through the one site that records it) sorts
		// last, so the order stays total whatever the map holds.
		indices := make([]int, 0, len(toolAccums))
		for i := range toolAccums {
			indices = append(indices, i)
		}
		arrival := func(slot int) int {
			if n, ok := toolArrival[slot]; ok {
				return n
			}
			return nextToolArrival
		}
		// Simple sort.
		for i := 0; i < len(indices); i++ {
			for j := i + 1; j < len(indices); j++ {
				if arrival(indices[j]) < arrival(indices[i]) {
					indices[i], indices[j] = indices[j], indices[i]
				}
			}
		}
		var tcs []api.ToolCall
		var unnamedText strings.Builder
		for _, i := range indices {
			a := toolAccums[i]
			if strings.TrimSpace(a.name) == "" {
				// A call the upstream never named. This path used to emit it
				// anyway, as a content_block_start with no "name" key at all and
				// a stop_reason of "tool_use": the client was told to expect a
				// call it could not name and could never run, and Claude Code
				// reports such a block as pending forever. content_block_start
				// is the only event that carries a name, so there is no second
				// chance to correct it — the gateway leg holds the fragment for
				// that reason (2026-09-27 audit, round 39, B-F8) and relays
				// whatever arguments arrived as TEXT, which is what this does
				// too: the model's raw output, readable, rather than a call the
				// client cannot make. The non-streaming path answers the same
				// wire the same way (round 54, A1).
				//
				// Asked BEFORE the argument JSON is parsed, because there is
				// nothing here for a parse to decide: a fragment the model was
				// still writing when it hit the token limit has unparseable
				// arguments too, and the truncation gate below dropped it whole
				// — so the same document's only output reached the client as
				// text on the non-stream arm and on both of the gateway's arms,
				// and vanished here (2026-09-28 audit, round 55).
				if s := strings.TrimSpace(a.args.String()); s != "" {
					unnamedText.WriteString(s)
				}
				continue
			}
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
		if unnamedText.Len() > 0 {
			// Emitted before the calls that follow it, in the order the
			// fragments arrived: it is the same turn's output either way.
			//
			// It is streamedText too, by the rule this function's tally below
			// is written to: what reaches the client as output is counted, and
			// this reaches it as a text block. Left out, an id-less turn whose
			// only output was the fragment relayed here reported
			// output_tokens=0 to a client reading that text — while the gateway
			// leg's own byte tally counts a nameless call's arguments (its
			// relayDelta books them into outBytes) and this leg's non-stream
			// path drops the call instead, so the same document's output count
			// depended on the path that answered it (2026-09-27 audit, round
			// 47, C-F5's sibling).
			streamedText += unnamedText.Len()
			emit(conv.Process(api.ChatResponse{Model: upstreamModel, Message: api.Message{Content: unnamedText.String()}}))
		}
		// Counted HERE, on the calls about to be emitted, not where the
		// fragments accumulated: a truncated unparseable fragment is dropped
		// above and reaches the client as no tool_use block at all, so counting
		// it billed output_tokens for a turn that relayed nothing runnable
		// (2026-09-27 audit, round 24). Same measure as the non-stream and
		// adopt paths, so the two agree (round 23).
		streamedText += toolCallArgumentsSize(tcs)
		toolAccums = map[int]*toolAccum{}
		// The arrival order is the map's own lifetime: a slot written again
		// after this flush is a call of the next turn's output, not one of these.
		toolArrival = map[int]int{}
		nextToolArrival = 0
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

	// toolAccumsRelaySomething reports whether the accumulated call fragments would
	// put anything in front of the client when they are flushed — the question the
	// empty-turn guard below and the whole-completion adoption gate both mean by
	// "this turn said something". The accumulator MAP is not the block: a fragment
	// with neither a name nor arguments opens nothing and relays nothing
	// (flushToolCalls skips it whole, contributing neither text nor a call), so
	// counting it as a turn marked a stream that never said a word as a complete
	// one — answered 200 + end_turn with empty content, and the leg recorded
	// healthy, while the byte-identical non-stream body and the gateway leg both
	// refuse it (2026-09-28 audit, round 56, F1/F2).
	//
	// A call the upstream NAMED counts even when its arguments never parsed: the
	// flush either runs it, or — when the token limit cut it short — drops it and
	// reports max_tokens, which is the shape this leg serves rather than refuses
	// (round 17). Dropping it from the tally would turn that turn into the refusal
	// this leg deliberately does not make. A call with no name is never executable;
	// its arguments are the model's raw output and reach the client as TEXT, so it
	// counts by its bytes — and only when it has any.
	relaysSomething := func(accums map[int]*toolAccum) bool {
		for _, a := range accums {
			if strings.TrimSpace(a.name) != "" {
				return true
			}
			if strings.TrimSpace(a.args.String()) != "" {
				return true
			}
		}
		return false
	}

	// A stream is COMPLETE only when the upstream said so: a finish_reason on
	// a choice, or the [DONE] sentinel. Without one of those we have no idea
	// whether the answer we relayed was the whole answer, so the tail below
	// reports a failure instead of a clean turn (2026-09-26 audit).
	completed := false
	upstreamErr := ""
	// adoptedSlots are the call slots an adopted whole completion wrote, so a
	// fragment later in the stream can be told apart: one at a slot the adoption
	// wrote continues a call the client already holds (it cannot be delivered);
	// one at a NEW slot is a call of the model's own (2026-09-27 audit, round
	// 49, A-F2). Keyed as the accumulator keys its slots: the call's own index,
	// or — for a completion whose calls carry none — the order they were
	// written in, which is the slot the index-less accumulator would have used.
	adoptedSlots := map[int]bool{}
	// adoptedCalls is whether the adopted completion wrote any tool call of its
	// own — the calls whose blocks the client already holds, and the only
	// fragments a later tool delta can be a continuation of.
	adoptedCalls := false
	// adoptedWhole is whether a whole completion that arrived inside a frame
	// became the turn (see the frameCarriesWholeCompletion branch below).
	adoptedWhole := false
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
		// Not `len(toolAccums) == 0`: a fragment with neither a name nor
		// arguments relays nothing when it is flushed, so letting it close this
		// gate discarded the whole completion that followed — the client got
		// 200, an empty message and output_tokens for an answer the upstream had
		// already written, while the identical frame without the fragment relays
		// that answer (and the gateway leg relays it too, its own nothingRelayed
		// asking the same question of the same fragment) (2026-09-28 audit,
		// round 56, F2).
		if !started && !relaysSomething(toolAccums) && frameCarriesWholeCompletion(payload) {
			if adopted, refused, wroteCalls, wroteSlots := adoptNonSSECompletion(payload, conv, emit, onUsage, upstreamModel, &finishReason, &finalUsage, &streamedText); adopted {
				completed = true
				// The turn's whole completion has been relayed, and the stream
				// goes on: the upstream stated the message and then kept
				// sending. Its TEXT is still text the model wrote, and both
				// other legs relay it — the local server's converter answers a
				// whole message followed by deltas with the concatenation, and
				// the gateway leg answers this exact wire with "hi there" where
				// this leg stopped at "hi", dropping the rest of the stream it
				// had already read. So the loop reads on (2026-09-27 audit,
				// round 47, C-F3).
				//
				// A TOOL fragment after this point is dropped only when the
				// adoption wrote calls of its OWN: the fragment then continues
				// one of those, whose block the client already holds under an
				// id it can answer once, and accumulating it would flush a
				// SECOND call under that id. An adoption that wrote only text
				// closed no call, so a tool fragment after it is a call the
				// model wrote — the gateway leg opens a block for exactly this
				// wire (its content_block_stop is out for the TEXT block, and a
				// tool call is a block of its own), and dropping it here made
				// the turn's answer depend on whether the text arrived as a
				// whole message or as deltas (2026-09-27 audit, round 48, A-F2
				// and C-F2).
				adoptedWhole = true
				adoptedCalls = wroteCalls
				adoptedSlots = wroteSlots
			} else if refused {
				// The frame IS a whole completion and it says nothing. Falling
				// through to the delta loop let the finish_reason it carried
				// complete an EMPTY turn with a 200, while the unframed twin of
				// the same document and the gateway leg both refuse it — the
				// verdict depended on the shape the upstream chose, which is
				// exactly what routing the frame through this adoption exists
				// to prevent (2026-09-27 audit, round 46, A46-3).
				upstreamErr = "upstream returned an empty completion"
				break
			}
		}

		for _, choice := range chunk.Choices {
			d := choice.Delta

			// Reasoning content → thinking delta. It counts toward the output
			// estimate below like the answer does: it was relayed to the client
			// and the upstream bills it as output (2026-09-27 audit, round 17).
			if reasoning := reasoningOf(d.ReasoningContent, d.Reasoning); reasoning != "" {
				streamedText += len(reasoning)
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
				indexed := tc.Index != nil
				if indexed {
					slot = *tc.Index
					indexSeen := seenIndex[slot]
					seenIndex[slot] = true
					if cur, ok := indexSlot[slot]; ok {
						// The index names the SLOT; the call that slot holds is
						// what a fragment stating the index continues. A vendor
						// that writes one index for every call of the turn
						// leaves the index naming the newest call written into
						// it — there is no other reading under which the second
						// call's own fragments are reachable at all — so the
						// split below re-points the index at the call it mints
						// (2026-09-28 audit, round 59, F59-L2-1).
						slot = cur
					}
					if slot >= nextFreeToolSlot {
						nextFreeToolSlot = slot + 1
					}
					// A fragment that STATES the id or the name of a call this
					// stream already opened belongs to that call whatever index
					// it carries: the vendor that does not update its index on a
					// continuation writes the call's own id there — the exact
					// wire round 59's F59-L2-1 describes one index over — and the
					// index then names a call this fragment is not about. Routed
					// into the block the index held, the continuation left the
					// call it belonged to with an empty input and the client ran
					// a Read it was never given, or a Bash whose arguments are
					// the other call's (2026-09-28 audit, round 60, F60-L2-2).
					// Asked only of a call whose arguments are still OPEN, so the
					// ordinary wire — where the stated id restates the call the
					// index names — is untouched, and round 56's F1 restatement,
					// whose call is finished, keeps its own slot.
					if stated, ok := openSlotStating(tc.ID, tc.Function.Name, indexSeen); ok {
						slot = stated
					}
					// A fragment that states NO id and no name is an argument
					// continuation, and the vendor that writes one index for
					// every call of the turn introduces its calls in one delta
					// and then streams their arguments one fragment each, in the
					// order the calls were introduced: the index names the
					// NEWEST call, so the first call's arguments landed in the
					// second call's block and the client ran a Read whose input
					// was Bash's `{"cmd":"ls"}` — a well-formed call with another
					// call's input, which signals nothing — while the second
					// call's own arguments were dropped and the whole-list arm of
					// this leg answered the same body with both calls' own
					// (2026-09-28 audit, round 61, F61-L2-2). Matched here, while
					// the newest call has received none of its own: a fragment
					// arriving after that one has started belongs to it.
					if tc.ID == "" && tc.Function.Name == "" {
						if acc, exists := toolAccums[slot]; exists &&
							(strings.TrimSpace(acc.args.String()) == "" || argsFinished(acc.args.String())) {
							// The index names the call the vendor wrote into it
							// last, and that call is either waiting for its
							// arguments (the vendor feeds its calls in the order
							// it introduced them, so the newest is fed last) or
							// already done with them (the vendor's newest call
							// closed while an earlier one it named is still
							// open). Either way the fragment belongs to the
							// EARLIEST call this index named that is still
							// waiting — the same answer the gateway leg's chain
							// gives, and without it the client ran the first call
							// with NO input while this call's own arguments were
							// dropped at the write (2026-09-28 audit, round 62).
							for _, s := range indexChain[*tc.Index] {
								if a, ok := toolAccums[s]; ok && s != slot && !argsFinished(a.args.String()) {
									slot = s
									break
								}
							}
						}
					}
					if acc, exists := toolAccums[slot]; exists &&
						!restatesAccumulatedCall(acc, tc.ID, tc.Function.Name, tc.Function.Arguments) &&
						((tc.ID != "" && acc.id != "" && tc.ID != acc.id) ||
							(tc.Function.Name != "" && acc.name != "" && tc.Function.Name != acc.name) ||
							((tc.ID != "" || tc.Function.Name != "") &&
								strings.TrimSpace(tc.Function.Arguments) != "" &&
								!canExtend(acc.args.String(), tc.Function.Arguments))) {
						// A call the upstream stated this slot for a SECOND time
						// begins the next one: a stated id this slot does not
						// hold (round 43's B43-3) or a name it does not carry
						// (round 40's A40-8, an argument continuation carries
						// arguments alone). That is round 36's B-F2 — every call
						// after the first filed into one block, its id discarded
						// and its arguments concatenated onto the first's — for
						// the vendor that writes index 0 for every call of the
						// turn, which keying by id could not cure because the
						// index it repeats is STATED, not absent. The gateway
						// leg's toolKey splits the same wire the same way, and
						// both arms that hold a whole list — this leg's adoption
						// and the gateway's document arm — already keep the two
						// calls apart (2026-09-28 audit, round 58, F58-L3-2).
						//
						// The third clause is the arguments' own answer, and it
						// is what reaches the ordinary chunked wire: a call's
						// SECOND object arrives in pieces (`{"b":` then `2}`), so
						// requiring the fragment itself to be a complete object
						// missed it and the pieces were appended to the first
						// call's finished one — the client accumulated
						// `{"a":1}{"b":2}` for a Bash it was asked to run, and
						// the model's second call did not exist on this arm
						// (2026-09-28 audit, round 61, F61-L2-1). canExtend is
						// the whole test: bytes a finished list cannot take are
						// not more of that call, whether they are a whole object
						// or its first piece. Round 56's restatement — the same
						// call listed again — is asked before this and stays one
						// call, and freeform continuations stay in theirs because
						// more of a line IS extendable.
						//
						// The call the slot already carries, restated, is asked
						// first and stays that call: round 56's F1 lists it again
						// under its own id at its own index.
						slot = nextFreeToolSlot
						nextFreeToolSlot++
						// The slot now names this call: the index is reused
						// rather than renumbered, so a fragment stating it again
						// — the arguments of the call just split off, stated at
						// the index the vendor writes for every call — must land
						// here and not on the call it was split from. That was
						// the one field apart failure: the split kept the calls
						// apart only when the continuation omitted the index, so
						// the same three frames with the index stated folded the
						// second call's arguments onto the first's — a real Read
						// executed with an empty input, or a single call carrying
						// a {"_raw":…} blob no tool accepts, under a stop_reason
						// of tool_use (2026-09-28 audit, round 59, F59-L2-1).
						indexSlot[*tc.Index] = slot
					} else if acc, exists := toolAccums[slot]; exists &&
						tc.ID == "" && tc.Function.Name == "" &&
						argsFinished(acc.args.String()) &&
						!canExtend(acc.args.String(), tc.Function.Arguments) {
						// An argument-only fragment whose slot already holds a
						// FINISHED argument list is not more of that call: two
						// finished objects do not concatenate into JSON, and the
						// whole-list arm would read them as two calls. It is the
						// call this stream last wrote to, when that one is still
						// open AND can take these bytes — the wire whose
						// continuations state an index the vendor did not update.
						// A call whose arguments are still open is left where the
						// index put it, which is the ordinary interleaved-parallel
						// order (2026-09-28 audit, round 59, F59-L2-1).
						//
						// argsFinished counts a freeform line as finished too, and
						// canExtend is what keeps its continuations whole: more of
						// the same line is still that call (round 60's gate), while
						// the beginning of an object after it is not (2026-09-28
						// audit, round 61, F61-L2-5). A fragment that is itself a
						// whole object can only start a call, so it is never
						// handed to one already holding a partial object: the two
						// do not concatenate into JSON, and the call left open
						// beside the finished slot kept the model's own arguments
						// whole only from the fragment after it (F61-L2-4).
						if last, ok := toolAccums[lastToolSlot]; lastToolSlot >= 0 && lastToolSlot != slot && ok &&
							!argsFinished(last.args.String()) && canExtend(last.args.String(), tc.Function.Arguments) {
							slot = lastToolSlot
						} else {
							// Nowhere left to put it: the slot's own call is
							// finished and so is the one the stream last wrote
							// to. Two finished objects do not concatenate into
							// JSON, and every other arm DROPS this fragment —
							// this leg's whole-list adoption answers the same
							// body with the call alone, and the gateway leg
							// refuses the bytes after a closed block. Appending
							// it here handed the client a call whose input is
							// `{"b":2}{"c":3}`, which no tool can parse, under a
							// stop_reason of tool_use (2026-09-28 audit, round
							// 60, F60-X-1 and F60-X-2).
							continue
						}
					}
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
					//
					// The fragment continues the call this stream last NAMED,
					// which need not be the one that started at slot zero: an
					// upstream that states the index on the fragment introducing
					// a call and omits it on the argument continuations — the
					// wire round 37's B-F1 is about — sent every continuation to
					// slot zero, so a call introduced at any other slot got no
					// arguments at all and a second, empty one appeared at the
					// slot that was not the call's (2026-09-28 audit, round 58,
					// F58-L3-1).
					if acc, exists := toolAccums[lastToolSlot]; lastToolSlot >= 0 && exists &&
						startsANewToolCall(acc.id, acc.name, acc.args.String(), tc.ID, tc.Function.Name, tc.Function.Arguments) {
						slot = nextFreeToolSlot
						nextFreeToolSlot++
					} else if lastToolSlot >= 0 {
						slot = lastToolSlot
					} else {
						slot = nextFreeToolSlot
						nextFreeToolSlot++
					}
				}
				if indexed {
					c := indexChain[*tc.Index]
					known := false
					for _, s := range c {
						if s == slot {
							known = true
							break
						}
					}
					if !known {
						indexChain[*tc.Index] = append(c, slot)
					}
				}
				lastToolSlot = slot
				if adoptedWhole && adoptedCalls {
					// A call the adoption already wrote in full (see above): its
					// block is closed on the client's side, so these arguments
					// cannot be delivered. Only that call, though — a fragment at
					// a slot the adoption never wrote is a call of the model's
					// own, which the gateway leg opens a block for, and dropping
					// it lost a tool the upstream had asked for (2026-09-27
					// audit, round 49, A-F2). A fragment carrying no index is
					// dropped either way: it cannot be told from a continuation
					// of the calls the adoption wrote.
					if !indexed || adoptedSlots[slot] {
						continue
					}
				}
				acc, exists := toolAccums[slot]
				if !exists {
					acc = &toolAccum{}
					toolAccums[slot] = acc
					toolArrival[slot] = nextToolArrival
					nextToolArrival++
				}
				if exists && restatesAccumulatedCall(acc, tc.ID, tc.Function.Name, tc.Function.Arguments) {
					// The call this slot already carries, listed again: its
					// arguments are on the wire and complete, so this fragment is
					// neither more of them nor a second call — see the helper.
					// Asked before the writes, because a text appended here is a
					// text already delivered.
					continue
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
			finalUsage = mergeUsage(finalUsage, chunk.Usage)
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
		if !started && upstreamErr == "" {
			if adopted, refused, _, _ := adoptNonSSECompletion(nonSSE.String(), conv, emit, onUsage, upstreamModel, &finishReason, &finalUsage, &streamedText); adopted {
				completed = true
			} else if refused {
				upstreamErr = "upstream returned an empty completion"
			}
		}
	}

	// A stream that ENDED — the upstream's own finish_reason on a choice, or its
	// [DONE] — and said NOTHING is not an answer. An empty delta frame carrying
	// a finish_reason was proof of a turn here: the client got 200, an empty
	// assistant message and end_turn, and the leg was recorded healthy, so its
	// breaker never opened and `auto` never moved the session off it. The
	// non-stream path refuses the same document with 502 ("upstream returned an
	// empty completion" — an empty content, no reasoning, no call the client
	// could run), the tail below refuses the wire whose sentinel carried no
	// choice at all, and the gateway leg refuses it too; the verdict must not
	// depend on whether the emptiness was framed as a delta or as a message
	// (2026-09-27 audit, round 47, A-F1). Text that was relayed counts as an
	// answer whatever it says, which is why this asks `started` rather than
	// re-reading the bytes (round 44's rule; round 46, A46-2).
	//
	// Not `len(toolAccums) == 0`: the map holds what ACCUMULATED, and a
	// fragment with neither a name nor arguments accumulates without ever
	// becoming a block — the flush above relays nothing of it. Counting it said
	// this stream had said something and answered 200 + end_turn with empty
	// content for a turn whose every framing (the non-stream body, a whole
	// completion in a frame, and the same delta stream with the fragment
	// removed) is refused, and recorded the leg healthy so `auto` never moved
	// the session off it (2026-09-28 audit, round 56, F1).
	if completed && !started && !relaysSomething(toolAccums) {
		upstreamErr = "upstream returned an empty completion"
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
		// The UNCLAMPED hit, exactly as the non-stream leg and the gateway's
		// entry() read it: `total` below is raised to this value, so clamping
		// it here would first hide the evidence and then report the turn as
		// fresh input the upstream never billed (2026-09-27 audit, round 42,
		// A42-1).
		cached = finalUsage.statedCacheHit()
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
	// The patch that carries the cache read onto the client. Its gate is the
	// cache hit, not the prompt size: an upstream that states
	// prompt_cache_hit_tokens and no prompt_tokens — the shape the non-stream
	// leg reports as cache_read_input_tokens for the identical usage object —
	// left `cached` computed and then thrown away here, and the client read the
	// whole estimate as FRESH input with no cache_read at all, byte-identical to
	// the same turn with no usage chunk (2026-09-27 audit, round 40, A40-2).
	// The prompt is partitioned the way the non-stream leg and the gateway's
	// promptSplit partition it: the stated size, else the estimate the converter
	// was seeded with, else the hit itself; a stated hit that is larger than
	// either is evidence that the reading is short, so the total is raised to
	// it rather than the hit clamped down to a prompt the upstream never stated
	// — clamping it there told the client input_tokens=cache_read=the estimate
	// for a turn whose upstream had just said 900 of its prompt came from cache,
	// which is the same A40-3 shape the server leg answered (2026-09-27 audit,
	// round 40).
	total := 0
	if finalUsage != nil {
		total = finalUsage.PromptTokens
	}
	if total <= 0 {
		total = estInputTokens
	}
	if total < cached {
		total = cached
	}
	if finalUsage != nil && (statedPrompt || cached > 0) {
		// An upstream that reports the whole prompt as cache-read leaves the
		// uncached count at 0, and the converter only overwrites message_start's
		// seeded estimate when PromptEvalCount > 0 — so without this the client
		// was told input_tokens = the full prompt AND cache_read_input_tokens =
		// the same full prompt: 2x the real prompt for a fully-cached turn, and
		// Claude Code's context meter and auto-compaction sum both fields
		// (2026-09-26 audit). A stated prompt, or a stated hit, owns the field.
		for i := range events {
			if d, ok := events[i].Data.(anthropic.MessageDeltaEvent); ok {
				d.Usage.InputTokens = total - cached
				if cached > 0 {
					d.Usage.CacheReadInputTokens = intPtr(cached)
				}
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

// unnamedCallArgumentsText is the concatenated arguments of every call the
// upstream never named: the model's raw output, which no block of this leg's
// can carry as a call (content_block_start is the only event that carries a
// name) and which therefore reaches the client as text.
func unnamedCallArgumentsText(calls []openAIToolCall) string {
	var b strings.Builder
	for _, tc := range calls {
		if strings.TrimSpace(tc.Function.Name) != "" {
			continue
		}
		if s := strings.TrimSpace(tc.Function.Arguments); s != "" {
			b.WriteString(s)
		}
	}
	return b.String()
}

// relayUnnamedCallArguments folds those arguments into the message's text and
// drops the calls from its list — the estimate counts each argument once, as
// the stream path counts them once (streamedText += unnamedText.Len(), plus the
// NAMED calls only).
//
// One implementation for both arms of this leg, because a whole completion and
// a stream of fragments must not answer the same document two ways: round 54's
// A1 taught the non-stream arm to relay them and left the ADOPTION — the arm a
// stream request to a non-streaming upstream actually takes — refusing 502 a
// document whose only payload was such a fragment, while the byte-identical
// stream answered it 200 with the arguments as prose (2026-09-28 audit, round
// 55).
func relayUnnamedCallArguments(msg *api.Message, upstreamCalls []openAIToolCall) string {
	text := unnamedCallArgumentsText(upstreamCalls)
	named := msg.ToolCalls[:0:0]
	for _, tc := range msg.ToolCalls {
		if strings.TrimSpace(tc.Function.Name) == "" {
			continue
		}
		named = append(named, tc)
	}
	msg.ToolCalls = named
	if text != "" {
		msg.Content += text
	}
	return text
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
func adoptNonSSECompletion(raw string, conv *anthropic.StreamConverter, emit func([]anthropic.StreamEvent), onUsage func(int), upstreamModel string, finishReason *string, finalUsage **openAIUsage, streamedText *int) (adopted, refused, wroteCalls bool, wroteSlots map[int]bool) {
	if strings.TrimSpace(raw) == "" {
		return false, false, false, nil
	}
	var oaiResp openAIChatResponse
	if err := json.Unmarshal([]byte(raw), &oaiResp); err != nil {
		return false, false, false, nil
	}
	// No choices is not an answer: the same body the non-streaming path
	// refuses ("upstream returned no completion choices"), so it keeps the
	// failure verdict here. refused is what tells the framed caller that the
	// body IS a whole completion rather than a stream to keep reading.
	if len(oaiResp.Choices) == 0 {
		return false, true, false, nil
	}
	// And neither is a whole completion that says nothing. Adopting one
	// emitted message_start and message_stop around no blocks at all — the
	// same empty turn the non-streaming path now refuses, told to a client
	// that waits for the answer and never retries. Asked of the document, as
	// the refusing path asks it: a call only counts when the upstream named
	// it, since an unnamed call is dropped by the converter and cannot be run
	// (2026-09-27 audit, round 45, A45-5).
	//
	// Whether it named one is the caller's business too: this is the adoption
	// that WROTE the turn's tool calls, so a tool fragment later in the stream
	// continues a call whose block the client already holds under an id it can
	// answer once (2026-09-27 audit, round 48, A-F2).
	wroteSlots = map[int]bool{}
	{
		m := oaiResp.Choices[0].Message
		// The slots these calls occupy in the accumulator's terms. A call of a
		// whole completion carries no index here (the document's tool_calls
		// entries have no index field), so the order they are written in is the
		// only slot they can be said to hold — and it is the slot an index-less
		// delta stream would have given them, since the accumulator keys those
		// by the same sequence.
		slot := 0
		for _, tc := range m.ToolCalls {
			if strings.TrimSpace(tc.Function.Name) == "" {
				continue
			}
			wroteSlots[slot] = true
			slot++
		}
		wroteCalls = len(wroteSlots) > 0
		// A call the upstream never named is not a call — but its arguments are
		// the model's output, this leg relays them as text on both of its arms,
		// and the gateway's own says-something predicate counts them for that
		// reason (round 53), so a document whose only payload is such a fragment
		// does not say nothing (2026-09-28 audit, round 55; see
		// relayUnnamedCallArguments).
		unnamedText := unnamedCallArgumentsText(m.ToolCalls)
		if m.Content == "" &&
			firstNonEmpty(m.Reasoning, m.ReasoningContent) == "" && !wroteCalls && unnamedText == "" {
			return false, true, false, nil
		}
	}

	if onUsage != nil && oaiResp.Usage != nil && oaiResp.Usage.PromptTokens > 0 {
		onUsage(oaiResp.Usage.PromptTokens)
	}

	// Content and tool calls first, with Done unset, so the converter opens and
	// closes their content blocks before the tail's done event.
	chatResp := openAIResponseToChatResponse(oaiResp, upstreamModel)
	// The same fold the non-stream arm makes of the same document, so the
	// verdict here cannot depend on which shape the request had (round 55).
	relayUnnamedCallArguments(&chatResp.Message, oaiResp.Choices[0].Message.ToolCalls)
	emit(conv.Process(api.ChatResponse{Model: upstreamModel, Message: chatResp.Message}))

	// The tool_use blocks this path relays are output too; see the streaming
	// and non-streaming estimates (2026-09-27 audit, round 23).
	*streamedText = len(chatResp.Message.Content) + len(chatResp.Message.Thinking) +
		toolCallArgumentsSize(chatResp.Message.ToolCalls)
	*finishReason = oaiResp.Choices[0].FinishReason
	*finalUsage = oaiResp.Usage
	return true, false, wroteCalls, wroteSlots
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
	// credentialIsOurs=false: this leg sends the user's own credential
	// (resolveNativeAnthropicAuth above), and native_anthropic_auth.go's doc
	// makes Anthropic's 401/403 theirs to see untouched.
	status, relayed := anthropicPassthrough(w, r, body, nativeAnthropicUpstream, auth.Header, auth.Value, sessionID, false)
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
//
// credentialIsOurs says whose credential the request went out under, and it
// is what decides how a 401/403 from the upstream is told to the client. On a
// plan row (true) the key is the one oaica resolved for that remote, so a
// refusal means OUR key is wrong and the translated path's rule applies: emit
// it as a 502, because handing Claude Code an authentication_error would send
// it into its own login flow over a credential it never had (writeUpstreamError).
// On a native claude/* leg (false) the credential is the user's OWN —
// ~/.claude/.credentials.json or ANTHROPIC_API_KEY, resolved fresh per request
// — and native_anthropic_auth.go's doc states the contract exactly: "An
// expired token simply gets Anthropic's own 401 back through the proxy
// untouched, exactly what would happen running Claude Code natively with that
// same stale token — the user re-runs `claude /login` as normal." Rewriting
// that 401 to a 502 would hide the one instruction the client needs (2026-09-28
// audit, round 55).
func anthropicPassthrough(w http.ResponseWriter, r *http.Request, body []byte, upstream, headerName, headerValue, sessionID string, credentialIsOurs bool) (int, bool) {
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
		// A 401/403 on a PLAN row is OUR credential failing, not the client's:
		// this leg injected its own key into the request the client never
		// authenticated with. The translated path re-emits that refusal as a
		// 502 for exactly that reason (writeUpstreamError: "they mean OUR key
		// for that remote is wrong, and handing Claude Code an
		// authentication_error would send it into its own login flow"), and this
		// branch relayed the vendor's raw authentication_error instead — so the
		// same body against the same refusing upstream took the client into its
		// login flow on one wire and told it the proxy was broken on the other.
		// The status fed to route health stays the upstream's own, so a
		// credential refusal is classified exactly as the translated path
		// classifies it (a 4xx is the leg answering; nothing is recorded for it)
		// rather than as a dead backend (2026-09-28 audit, round 54).
		//
		// On a NATIVE leg the same numbers mean the opposite thing and the
		// refusal is relayed untouched — see credentialIsOurs on this function:
		// the credential is the user's own, the doc's contract is that
		// Anthropic's 401 reaches them as it would natively, and a 502 here
		// swallowed the `claude /login` instruction the refusal carries
		// (2026-09-28 audit, round 55).
		if credentialIsOurs && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
			text := strings.TrimSpace(string(httpbody.ReadCappedOrEmpty(resp.Body, httpbody.DiagnosticMax, "the upstream error body")))
			resp.Body.Close()
			writeUpstreamError(w, resp, text, headerValue)
			return resp.StatusCode, false
		}
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
// passes the upstream's own 4xx verdict through, because that is the class
// every SDK reads as terminal.
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
// A 400 keeps its own status for the same reason the 429 does. The upstream is
// oaica's own door, its 400 names the field the client got wrong, and the
// sibling legs answer the same body 400 — while a 502 is a class every SDK
// RETRIES and reads as an outage, so a body that can never succeed was re-sent
// with the whole prompt each time and the client could not tell the cause
// (2026-09-27 audit, round 52).
//
// secret is the credential this leg injected upstream and is what lets the
// re-emitted text lose it: an upstream refusing the call names the key it
// refused, and that is prose no shape rule recognises (2026-09-26 audit, ninth
// round).
func writeUpstreamError(w http.ResponseWriter, resp *http.Response, text, secret string) {
	if v := resp.Header.Get("Retry-After"); v != "" {
		w.Header().Set("Retry-After", v)
	}
	msg := fmt.Sprintf("upstream HTTP %d: %s", resp.StatusCode, redactUpstreamDiagnosis(text, secret))
	switch {
	case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
		writeAnthropicError(w, http.StatusBadGateway, msg)
	case resp.StatusCode >= 400 && resp.StatusCode < 500,
		resp.StatusCode == http.StatusServiceUnavailable, resp.StatusCode == 529:
		writeAnthropicError(w, resp.StatusCode, msg)
	default:
		writeAnthropicError(w, http.StatusBadGateway, msg)
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
