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
	"crypto/rand"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/big"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/ollama/ollama/anthropic"
	"github.com/ollama/ollama/api"
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
	Model         string          `json:"model"`
	Messages      []openAIMessage `json:"messages"`
	Stream        bool            `json:"stream,omitempty"`
	MaxTokens     int             `json:"max_tokens,omitempty"`
	Temperature   *float64        `json:"temperature,omitempty"`
	TopP          *float64        `json:"top_p,omitempty"`
	Stop          []string        `json:"stop,omitempty"`
	Tools         []api.Tool      `json:"tools,omitempty"`
	ToolChoice    any             `json:"tool_choice,omitempty"`
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
			ToolCalls        []struct {
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
}

// cachedTokens returns the prefix-cache hit count, clamped to the prompt
// size so a malformed upstream can never yield a negative input_tokens.
func (u *openAIUsage) cachedTokens() int {
	if u == nil || u.PromptTokensDetails == nil {
		return 0
	}
	c := u.PromptTokensDetails.CachedTokens
	if c < 0 {
		return 0
	}
	if c > u.PromptTokens {
		return u.PromptTokens
	}
	return c
}

// openAIStreamChunk is one SSE data: payload from a streaming response.
type openAIStreamChunk struct {
	Choices []struct {
		Index int `json:"index"`
		Delta struct {
			Role             string `json:"role,omitempty"`
			Content          string `json:"content,omitempty"`
			ReasoningContent string `json:"reasoning_content,omitempty"`
			ToolCalls        []struct {
				Index    int    `json:"index"`
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
	var system []string
	var rest []openAIMessage
	sawSystem := false
	for _, m := range msgs {
		if m.Role == "system" {
			sawSystem = true
			if strings.TrimSpace(m.Content) != "" {
				system = append(system, m.Content)
			}
			continue
		}
		rest = append(rest, m)
	}
	if !sawSystem {
		return msgs
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
		out = append(out, api.ToolCall{
			ID:       tc.ID,
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
		c := resp.Choices[0]
		chatResp.Message = api.Message{
			Role:      c.Message.Role,
			Content:   c.Message.Content,
			Thinking:  c.Message.ReasoningContent,
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
func RunAnthropicOpenAIProxy(ln net.Listener, remote userRemote, upstreamModel string) error {
	// Normalize the base URL (shared with fetchRemoteModels so the picker
	// and the proxy agree): openAIBase strips a trailing "/v1" from base_url
	// and re-appends the remote's API version (default "v1", "v4" for z.ai),
	// so the upstream endpoint is hit exactly once — otherwise a base_url that
	// already includes /v1 (e.g. https://api.deepseek.com/v1) would produce
	// /v1/v1/chat/completions and 404.
	return RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{
		Default: proxyRoute{BaseURL: remote.openAIBase(), Key: remote.key(), KeyEnv: strings.TrimSpace(remote.APIKeyEnv), UpstreamModel: upstreamModel, Label: "remote:" + remote.Name},
	})
}

// proxyRoute is one upstream an Anthropic request can be forwarded to.
type proxyRoute struct {
	BaseURL string // OpenAI base including the version prefix (".../v1")
	Key     string // bearer sent upstream when KeyEnv is empty; empty = none
	// KeyEnv, when set, is the environment variable resolveKey() re-reads on
	// every request instead of trusting Key — see RemoteEndpoint.TokenEnv's
	// doc for why a one-time-resolved credential is wrong for a long-lived
	// proxy process.
	KeyEnv        string
	UpstreamModel string // model id the upstream expects
	Label         string // for the request log / diagnostics
	// ContextWindow is this route's real max context in tokens (probed via
	// context_window_remote.go / the model manifest), 0 = unknown. Used to
	// clamp an outgoing request's max_tokens so prompt+max_tokens never
	// exceeds it -- see the context-fit clamp in the /v1/messages handler.
	ContextWindow int
}

// resolveKey returns the bearer to send upstream, live: KeyEnv wins whenever
// it is set and currently non-empty in the environment (so exporting or
// rotating it takes effect on the very next request), falling back to the
// value resolved at launch time otherwise — either because the remote uses
// a literal api_key (KeyEnv empty) or because the env var is unset right
// now (rare; keeps old behavior rather than suddenly sending no credential).
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
	// never enter the child environment. Empty = no check (the standalone
	// serve-anthropic-proxy subcommand and older callers).
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

// redactURL strips userinfo from a URL for logs.
func redactURL(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return raw
	}
	u.User = nil
	return u.String()
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
//     the 429/503 paths (rejected BEFORE backend work) never incur.
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
		http.StatusBadGateway,
		http.StatusServiceUnavailable,
		http.StatusGatewayTimeout:
		return true
	}
	return false
}

func proxyUpstreamRetryDo(req *http.Request, reqBytes []byte) (*http.Response, error) {
	var lastResp *http.Response
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
			lastResp = nil
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
	return lastResp, nil
}

func (t proxyRouteTable) resolve(requested string) (proxyRoute, string) {
	if requested == "" {
		return t.Default, t.Default.UpstreamModel
	}
	if r, ok := t.ByModel[requested]; ok {
		return r, r.UpstreamModel
	}
	return t.Default, requested
}

// RunAnthropicOpenAIProxyRoutes is RunAnthropicOpenAIProxy with a routing
// table; see proxyRouteTable.
func RunAnthropicOpenAIProxyRoutes(ln net.Listener, table proxyRouteTable) error {
	baseURL := table.Default.BaseURL

	// Route-policy fallback legs present: breakers exist and a background
	// probe keeps them honest (route_policy.go). Without Fallbacks there is
	// nothing to break over — no breaker, no probe, identical to before.
	if len(table.Fallbacks) > 0 || table.Oversize.BaseURL != "" {
		if table.breakers == nil {
			table.breakers = &routeBreakers{}
		}
		if table.escalations == nil {
			table.escalations = &routeEscalations{}
		}
		pollDone := make(chan struct{})
		defer close(pollDone)
		go table.startRouteHealthPoll(pollDone, 30*time.Second)
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
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !table.authorized(r) {
			writeAnthropicError(w, http.StatusUnauthorized, "missing or invalid proxy token")
			return
		}
		proxyPassThrough(w, r, baseURL+"/models", table.Default.resolveKey())
	})

	// /health — a trivial liveness probe so callers can confirm the proxy is up.
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	// POST /v1/messages — the real work.
	mux.HandleFunc("/v1/messages", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			writeAnthropicError(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		if !table.authorized(r) {
			writeAnthropicError(w, http.StatusUnauthorized, "missing or invalid proxy token")
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "read body: "+err.Error())
			return
		}

		var anthReq anthropic.MessagesRequest
		if err := json.Unmarshal(body, &anthReq); err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "invalid Anthropic request: "+err.Error())
			return
		}

		chatReq, err := anthropic.FromMessagesRequest(anthReq)
		if err != nil {
			writeAnthropicError(w, http.StatusBadRequest, "convert request: "+err.Error())
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
		// Always answer with the leg that will actually serve this request:
		// our gateway logs it as routed_to for spend attribution, and it
		// makes a silent --sonnet-model/fallback swap diagnosable from the
		// client side.
		w.Header().Set("X-Oaica-Route", route.Label)

		// Off by default (see entitlement.go) — a hook point for a future
		// license/entitlement product decision, not one made here.
		if allowed, reason := checkEntitlement(r, route.Label, reqModel); !allowed {
			writeAnthropicError(w, http.StatusForbidden, reason)
			return
		}

		started := time.Now()

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
		calibKey := table.SessionID
		if calibKey == "" {
			calibKey = route.Label
		}
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
					route = over
					oaiReq.Model = route.UpstreamModel
					w.Header().Set("X-Oaica-Route", route.Label)
					fitBudget = route.ContextWindow - estTokens - margin
				}
			}
			if fitBudget < minViableCompletion {
				// Anthropic's own wording -- see promptTooLongMessage for
				// why the exact phrasing is load-bearing for Claude Code's
				// recovery path.
				writeAnthropicError(w, http.StatusBadRequest,
					promptTooLongMessage(estTokens, route.ContextWindow-minViableCompletion))
				return
			}
			if oaiReq.MaxTokens > fitBudget {
				oaiReq.MaxTokens = fitBudget
			}
		}
		oaiBody, err := json.Marshal(oaiReq)
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "marshal openai request: "+err.Error())
			return
		}

		upstreamReq, err := http.NewRequestWithContext(r.Context(), http.MethodPost, route.BaseURL+"/chat/completions", bytes.NewReader(oaiBody))
		if err != nil {
			writeAnthropicError(w, http.StatusInternalServerError, "build upstream request: "+err.Error())
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
			table.breakers.recordFail(route.BaseURL)
			// Same signal feeds the `auto` policy's per-session escalation
			// (route_policy.go): consecutive failures escalate the session to
			// the stronger secondary leg.
			table.escalations.recordFail(table.SessionID)
			writeAnthropicError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
			return
		}
		defer resp.Body.Close()
		// Feed the circuit breaker (route_policy.go): 5xx-class answers that
		// survived the retry budget count as failures; 2xx proves recovery.
		// 4xx (bad request, context overflow) and 429 (shedding, alive) are
		// the leg WORKING and must not open the breaker. The `auto` policy's
		// per-session escalation eats the same signals (route_policy.go): a
		// success clears the consecutive-failure counter — but NOT an active
		// escalation, which only decays after autoEscalateHoldFor so one
		// lucky 200 on the secondary can't bounce the session back onto a
		// still-flapping primary.
		switch {
		case resp.StatusCode >= 500:
			table.breakers.recordFail(route.BaseURL)
			table.escalations.recordFail(table.SessionID)
		case resp.StatusCode < 300:
			table.breakers.recordOK(route.BaseURL)
			table.escalations.recordOK(table.SessionID)
		}

		// Same local-only log the router path used to keep (request_log.go):
		// model, which backend, sizes, status -- never content.
		lastLen, totalLen := extractLastAndTotalMessageLen(body)
		appendRequestLog(requestLogEntry{
			Timestamp:        time.Now().UTC().Format(time.RFC3339),
			Model:            anthReq.Model,
			Path:             r.URL.Path,
			Backend:          route.Label + " " + redactURL(route.BaseURL),
			LastMessageLen:   lastLen,
			TotalMessagesLen: totalLen,
			HardSignalMatch:  requestLogHardSignalRE.MatchString(string(body)),
			WouldBeHardByLen: lastLen > requestLogHardLengthThreshold || totalLen > requestLogHardLengthThreshold*3,
			StatusCode:       resp.StatusCode,
			DurationMs:       time.Since(started).Milliseconds(),
		})

		if resp.StatusCode != http.StatusOK {
			respBody, _ := io.ReadAll(resp.Body)
			text := strings.TrimSpace(string(respBody))
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
			writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("upstream HTTP %d: %s", resp.StatusCode, text))
			return
		}

		// Record the REAL prompt size for this session -- only from a usage
		// object the upstream actually sent on a 200 (see
		// context_calibration.go). Never from an error, never a guess.
		recordUsage := func(promptTokens int) { calib.record(calibKey, len(body), promptTokens) }

		if anthReq.Stream {
			handleStreamResponse(w, resp.Body, reqModel, recordUsage)
		} else {
			handleNonStreamResponse(w, resp.Body, reqModel, recordUsage)
		}
	})

	srv := &http.Server{Handler: mux}
	return srv.Serve(ln)
}

// handleNonStreamResponse reads one complete OpenAI JSON response, builds an
// api.ChatResponse, and emits an Anthropic MessagesResponse as JSON.
// onUsage, when non-nil, is called with the upstream's real
// usage.prompt_tokens so the caller can calibrate its prompt-size estimate.
func handleNonStreamResponse(w http.ResponseWriter, body io.Reader, upstreamModel string, onUsage func(int)) {
	respBody, err := io.ReadAll(body)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "read upstream body: "+err.Error())
		return
	}
	var oaiResp openAIChatResponse
	if err := json.Unmarshal(respBody, &oaiResp); err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "decode upstream response: "+err.Error())
		return
	}
	if onUsage != nil && oaiResp.Usage != nil && oaiResp.Usage.PromptTokens > 0 {
		onUsage(oaiResp.Usage.PromptTokens)
	}
	chatResp := openAIResponseToChatResponse(oaiResp, upstreamModel)
	anthResp := anthropic.ToMessagesResponse(anthropic.GenerateMessageID(), chatResp)
	anthResp.Usage.CacheReadInputTokens = oaiResp.Usage.cachedTokens()
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	_ = enc.Encode(anthResp)
}

// handleStreamResponse reads OpenAI SSE chunks from body, feeds incremental
// api.ChatResponse values through a single anthropic.StreamConverter, and
// writes each returned StreamEvent as an Anthropic SSE event.
// onUsage, when non-nil, is called with the real usage.prompt_tokens from
// the stream's final usage-only chunk (stream_options.include_usage) so the
// caller can calibrate its prompt-size estimate.
func handleStreamResponse(w http.ResponseWriter, body io.Reader, upstreamModel string, onUsage func(int)) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeAnthropicError(w, http.StatusInternalServerError, "streaming not supported by response writer")
		return
	}

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	flusher.Flush()

	conv := anthropic.NewStreamConverter(anthropic.GenerateMessageID(), upstreamModel, 0)

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
	finishReason := ""
	var finalUsage *openAIUsage

	emit := func(events []anthropic.StreamEvent) {
		for _, e := range events {
			data, err := json.Marshal(e.Data)
			if err != nil {
				continue
			}
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", e.Event, string(data))
		}
		flusher.Flush()
	}

	flushToolCalls := func() {
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
				args = api.NewToolCallFunctionArguments()
				args.Set("_raw", raw)
			}
			tcs = append(tcs, api.ToolCall{
				ID:       a.id,
				Function: api.ToolCallFunction{Name: a.name, Arguments: args},
			})
		}
		chatResp := api.ChatResponse{Model: upstreamModel, Message: api.Message{ToolCalls: tcs}}
		emit(conv.Process(chatResp))
		toolAccums = map[int]*toolAccum{}
	}

	for scanner.Scan() {
		line := scanner.Text()
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "[DONE]" {
			break
		}

		var chunk openAIStreamChunk
		if err := json.Unmarshal([]byte(payload), &chunk); err != nil {
			continue
		}

		for _, choice := range chunk.Choices {
			d := choice.Delta

			// Reasoning content → thinking delta.
			if d.ReasoningContent != "" {
				cr := api.ChatResponse{Model: upstreamModel, Message: api.Message{Thinking: d.ReasoningContent}}
				emit(conv.Process(cr))
			}

			// Text content delta.
			if d.Content != "" {
				cr := api.ChatResponse{Model: upstreamModel, Message: api.Message{Content: d.Content}}
				emit(conv.Process(cr))
			}

			// Tool-call deltas — accumulate by index; flush later.
			for _, tc := range d.ToolCalls {
				acc, exists := toolAccums[tc.Index]
				if !exists {
					acc = &toolAccum{}
					toolAccums[tc.Index] = acc
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

	// Flush any pending tool calls before the done event so StreamConverter
	// emits their content blocks first and sets stop_reason=tool_use.
	flushToolCalls()

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
	if finalUsage != nil {
		cached = finalUsage.cachedTokens()
		doneResp.Metrics.PromptEvalCount = finalUsage.PromptTokens - cached
		doneResp.Metrics.EvalCount = finalUsage.CompletionTokens
	}
	events := conv.Process(doneResp)
	if cached > 0 {
		for i := range events {
			if d, ok := events[i].Data.(anthropic.MessageDeltaEvent); ok {
				d.Usage.CacheReadInputTokens = cached
				events[i].Data = d
			}
		}
	}
	emit(events)
}

// proxyPassThrough forwards a request verbatim to the target URL, streaming
// the response back. Used for GET /v1/models.
func proxyPassThrough(w http.ResponseWriter, r *http.Request, target, key string) {
	req, err := http.NewRequestWithContext(r.Context(), r.Method, target, nil)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "build request: "+err.Error())
		return
	}
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		writeAnthropicError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
		return
	}
	defer resp.Body.Close()
	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// writeAnthropicError emits an Anthropic-shaped error response.
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

// ServeAnthropicProxyForRemote is the entry point used by the hidden CLI
// subcommand. It resolves the remote, binds a listener (on port, or a free
// one if port<=0), prints the chosen port to stdout, then blocks serving.
func ServeAnthropicProxyForRemote(remoteName, upstreamModel string, port int) error {
	remote, ok := findUserRemoteByName(remoteName)
	if !ok {
		return fmt.Errorf("no user-defined remote named %q in ~/.oaica/remotes.json", remoteName)
	}
	if upstreamModel == "" {
		return fmt.Errorf("--model is required (the bare upstream model id, e.g. deepseek-v4-flash)")
	}
	addr := "127.0.0.1:" + strconv.Itoa(port)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return fmt.Errorf("listen %s: %w", addr, err)
	}
	chosen := ln.Addr().(*net.TCPAddr).Port
	fmt.Println(strconv.Itoa(chosen))
	return RunAnthropicOpenAIProxy(ln, remote, upstreamModel)
}
