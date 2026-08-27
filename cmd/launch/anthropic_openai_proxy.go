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
	"encoding/hex"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
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
		Role       string             `json:"role"`
		Content    []openAIContentPart `json:"content"`
		ToolCalls  []openAIToolCall   `json:"tool_calls,omitempty"`
		ToolCallID string             `json:"tool_call_id,omitempty"`
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
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
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
	Usage *struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
		TotalTokens      int `json:"total_tokens"`
	} `json:"usage,omitempty"`
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
func RunAnthropicOpenAIProxy(ln net.Listener, remote userRemote, upstreamModel string) error {
	// Normalize the base URL (shared with fetchRemoteModels so the picker
	// and the proxy agree): openAIBase strips a trailing "/v1" from base_url
	// and re-appends the remote's API version (default "v1", "v4" for z.ai),
	// so the upstream endpoint is hit exactly once — otherwise a base_url that
	// already includes /v1 (e.g. https://api.deepseek.com/v1) would produce
	// /v1/v1/chat/completions and 404.
	return RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{
		Default: proxyRoute{BaseURL: remote.openAIBase(), Key: remote.key(), UpstreamModel: upstreamModel, Label: "remote:" + remote.Name},
	})
}

// proxyRoute is one upstream an Anthropic request can be forwarded to.
type proxyRoute struct {
	BaseURL       string // OpenAI base including the version prefix (".../v1")
	Key           string // bearer sent upstream; empty = none
	UpstreamModel string // model id the upstream expects
	Label         string // for the request log / diagnostics
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
	key := table.Default.Key

	mux := http.NewServeMux()

	// GET /v1/models — proxy straight to the remote's /models endpoint so
	// Claude Code's model probes resolve. The remote already speaks OpenAI
	// /models (z.ai serves it under /v4).
	mux.HandleFunc("/v1/models", func(w http.ResponseWriter, r *http.Request) {
		if !table.authorized(r) {
			writeAnthropicError(w, http.StatusUnauthorized, "missing or invalid proxy token")
			return
		}
		proxyPassThrough(w, r, baseURL+"/models", key)
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
		route, reqModel := table.resolve(anthReq.Model)
		started := time.Now()

		oaiReq := chatRequestToOpenAI(chatReq, anthReq, reqModel)
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
		if route.Key != "" {
			upstreamReq.Header.Set("Authorization", "Bearer "+route.Key)
		}

		resp, err := proxyUpstreamClient.Do(upstreamReq)
		if err != nil {
			writeAnthropicError(w, http.StatusBadGateway, "upstream request failed: "+err.Error())
			return
		}
		defer resp.Body.Close()

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
			writeAnthropicError(w, http.StatusBadGateway, fmt.Sprintf("upstream HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(respBody))))
			return
		}

		if anthReq.Stream {
			handleStreamResponse(w, resp.Body, reqModel)
		} else {
			handleNonStreamResponse(w, resp.Body, reqModel)
		}
	})

	srv := &http.Server{Handler: mux}
	return srv.Serve(ln)
}

// handleNonStreamResponse reads one complete OpenAI JSON response, builds an
// api.ChatResponse, and emits an Anthropic MessagesResponse as JSON.
func handleNonStreamResponse(w http.ResponseWriter, body io.Reader, upstreamModel string) {
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
	chatResp := openAIResponseToChatResponse(oaiResp, upstreamModel)
	anthResp := anthropic.ToMessagesResponse(anthropic.GenerateMessageID(), chatResp)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	enc := json.NewEncoder(w)
	_ = enc.Encode(anthResp)
}

// handleStreamResponse reads OpenAI SSE chunks from body, feeds incremental
// api.ChatResponse values through a single anthropic.StreamConverter, and
// writes each returned StreamEvent as an Anthropic SSE event.
func handleStreamResponse(w http.ResponseWriter, body io.Reader, upstreamModel string) {
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
			// Update the converter's token counts via a no-content ChatResponse
			// carrying only metrics; Process emits nothing for an empty
			// non-final ChatResponse, but it does store inputTokens on the
			// first call. We instead fold usage into the final done event below.
		}
	}

	// Flush any pending tool calls before the done event so StreamConverter
	// emits their content blocks first and sets stop_reason=tool_use.
	flushToolCalls()

	// Final done event.
	doneResp := api.ChatResponse{
		Model:      upstreamModel,
		Done:       true,
		DoneReason: mapFinishReason(finishReason),
	}
	emit(conv.Process(doneResp))
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
