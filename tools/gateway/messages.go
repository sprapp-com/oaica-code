package main

// messages.go — Anthropic Messages-wire compatibility for the gateway
// (2026-09-01). The fleet's Claude Code sessions speak ONLY /v1/messages;
// the upstream vLLM fleet speaks ONLY /v1/chat/completions. Until now the
// gateway 404'd every /v1/messages request ("unknown route"), which took
// every oaica-hosted model out of the sonnet/subagent tier the moment a
// client relied on the Anthropic wire.
//
// The translation lives HERE, in the gateway, not in each client: the
// gateway is the one place that already knows the model roster, metering,
// admission control and error shapes. A client-side translator would have
// to be re-implemented by every consumer (oaica proxy, raw Anthropic SDKs,
// Claude Code pointed straight at us) — N clients × M models, versus one
// conversion table tested once.
//
// Request direction (Anthropic -> OpenAI):
//
//	system (string | [{text}])      -> {"role":"system"} message
//	messages[].content text         -> string content
//	  "" tool_use (assistant)       -> tool_calls[{id,function:{name,arguments}}]
//	  "" tool_result (user)         -> {"role":"tool","tool_call_id":...}
//	  "" image (base64 source)      -> image_url data URI
//	  "" thinking blocks            -> dropped (upstream has no thinking wire)
//	tools[].{name,description,input_schema} -> function tools
//	tool_choice auto|any|tool       -> auto|required|named function
//	stop_sequences                  -> stop
//	max_tokens, temperature, top_p  -> passthrough
//
// Response direction (OpenAI -> Anthropic), both shapes:
//
//	non-stream: buffered whole — choices[0].message (+ tool_calls, +
//	  reasoning) -> content blocks; usage -> {input,output}_tokens;
//	  finish_reason -> stop_reason (tool_calls->tool_use, length->max_tokens).
//	stream: OpenAI SSE chunks are translated incrementally into the
//	  Anthropic event stream (message_start, content_block_start,
//	  content_block_delta, content_block_stop, message_delta, message_stop).
//	  vLLM's "reasoning" delta field is surfaced as text: some of our
//	  backends put the user-visible answer in reasoning and leave content
//	  null (measured on oaica-35b-a3b-vision), so dropping it would deliver
//	  empty replies.
//	errors: OpenAI error JSON -> {"type":"error","error":{...}} in the same
//	  status code, so an upstream 4xx/5xx reads native to the client.

import (
	"log"
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"
)

// messagesHandler accepts an Anthropic /v1/messages request, translates it
// to the OpenAI chat-completions shape, and reuses completionHandler for
// EVERYTHING else — auth, entitlement, concurrency caps, admission control,
// context-fit clamping, metering, ledger. The translator wrapper converts
// the OpenAI response back to Anthropic on the way out.
func (g *gateway) messagesHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	body, err := readCappedBody(w, r)
	if err != nil {
		return // readCappedBody already wrote the error
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "body is not valid JSON")
		return
	}
	openai, convErr := anthropicToOpenAI(req)
	if convErr != "" {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", convErr)
		return
	}
	stream, _ := req["stream"].(bool)
	openai["stream"] = stream
	nb, err := json.Marshal(openai)
	if err != nil {
		writeAnthropicErr(w, http.StatusBadRequest, "invalid_request_error", "could not encode request")
		return
	}
	r.Body = readCloserBytes(nb)
	r.ContentLength = int64(len(nb))
	r.Header.Set("Content-Length", fmt.Sprint(len(nb)))
	// completionHandler forwards r.URL.Path verbatim; upstream oaicalb
	// answers /v1/messages itself (in Anthropic shape). We must hit its
	// OpenAI wire so the response is the one shape this file translates.
	r.URL.Path = "/v1/chat/completions"

	model, _ := req["model"].(string)
	bridge := newAnthropicBridge(w, stream, model)
	g.completionHandler(bridge, r)
	bridge.finalize()
}

// anthropicToOpenAI converts a decoded Anthropic messages request into the
// OpenAI chat-completions map. Returns a non-empty error string on
// structurally impossible input (no messages, non-array content pieces we
// cannot represent).
func anthropicToOpenAI(req map[string]any) (map[string]any, string) {
	out := map[string]any{"model": req["model"]}
	if v, ok := req["max_tokens"]; ok {
		out["max_tokens"] = v
	} else {
		// Anthropic requires max_tokens; OpenAI backends want one too.
		out["max_tokens"] = 4096
	}
	for _, k := range []string{"temperature", "top_p", "stream"} {
		if v, ok := req[k]; ok {
			out[k] = v
		}
	}
	if ss, ok := req["stop_sequences"].([]any); ok && len(ss) > 0 {
		out["stop"] = ss
	}
	var msgs []map[string]any
	rawMsgs, _ := req["messages"].([]any)
	if len(rawMsgs) == 0 {
		return nil, "messages is required"
	}
	for _, rm := range rawMsgs {
		m, _ := rm.(map[string]any)
		if m == nil {
			continue
		}
		role, _ := m["role"].(string)
		msgs = append(msgs, contentBlocksToOpenAI(role, m["content"])...)
	}
	// out["messages"] may already hold the system message; append.
	if sys := systemToMessage(req["system"]); sys != nil {
		msgs = append([]map[string]any{sys}, msgs...)
	}
	out["messages"] = msgs

	if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
		var oai []map[string]any
		for _, t := range tools {
			tm, _ := t.(map[string]any)
			if tm == nil {
				continue
			}
			fn := map[string]any{"name": tm["name"]}
			if d, ok := tm["description"]; ok {
				fn["description"] = d
			}
			if sc, ok := tm["input_schema"]; ok {
				fn["parameters"] = sc
			}
			oai = append(oai, map[string]any{"type": "function", "function": fn})
		}
		if len(oai) > 0 {
			out["tools"] = oai
		}
	}
	if tc, ok := req["tool_choice"].(map[string]any); ok {
		switch t, _ := tc["type"].(string); t {
		case "auto":
			out["tool_choice"] = "auto"
		case "any":
			out["tool_choice"] = "required"
		case "tool":
			name, _ := tc["name"].(string)
			out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
		}
	}
	return out, ""
}

// systemToMessage flattens Anthropic's system field (string or content
// blocks) into one OpenAI system message; nil when absent/empty.
func systemToMessage(v any) map[string]any {
	switch s := v.(type) {
	case string:
		if s != "" {
			return map[string]any{"role": "system", "content": s}
		}
	case []any:
		var parts []string
		for _, b := range s {
			if bm, ok := b.(map[string]any); ok && bm["type"] == "text" {
				if t, ok := bm["text"].(string); ok && t != "" {
					parts = append(parts, t)
				}
			}
		}
		if len(parts) > 0 {
			return map[string]any{"role": "system", "content": strings.Join(parts, "\n")}
		}
	}
	return nil
}

// contentBlocksToOpenAI converts one Anthropic message's content (string or
// block array) into one-or-more OpenAI messages: text/images ride the same
// message, tool_use becomes assistant tool_calls, tool_result becomes a
// separate role:"tool" message (the OpenAI wire has no other spelling).
func contentBlocksToOpenAI(role string, content any) []map[string]any {
	if s, ok := content.(string); ok {
		return []map[string]any{{"role": role, "content": s}}
	}
	blocks, ok := content.([]any)
	if !ok {
		return []map[string]any{{"role": role, "content": ""}}
	}
	var out []map[string]any
	var parts []map[string]any // text/image parts of THIS message
	var toolCalls []map[string]any
	flushText := func() {
		if len(parts) == 0 {
			return
		}
		if len(parts) == 1 {
			if t, ok := parts[0]["text"].(string); ok {
				out = append(out, map[string]any{"role": role, "content": t})
				parts = nil
				return
			}
		}
		out = append(out, map[string]any{"role": role, "content": parts})
		parts = nil
	}
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			continue
		}
		switch t, _ := bm["type"].(string); t {
		case "text":
			txt, _ := bm["text"].(string)
			parts = append(parts, map[string]any{"type": "text", "text": txt})
		case "image":
			if src, ok := bm["source"].(map[string]any); ok {
				media, _ := src["media_type"].(string)
				data, _ := src["data"].(string)
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:" + media + ";base64," + data},
				})
			}
		case "tool_use":
			flushText()
			if len(toolCalls) == 0 {
				out = append(out, map[string]any{"role": role, "content": "", "tool_calls": toolCalls})
				toolCalls = out[len(out)-1]["tool_calls"].([]map[string]any)
			}
			id, _ := bm["id"].(string)
			name, _ := bm["name"].(string)
			args, _ := json.Marshal(bm["input"])
			toolCalls = append(toolCalls, map[string]any{
				"id":   id,
				"type": "function",
				"function": map[string]any{
					"name":      name,
					"arguments": string(args),
				},
			})
			out[len(out)-1]["tool_calls"] = toolCalls
		case "tool_result":
			flushText()
			// The tool's answer rides back as a role:"tool" message. The
			// content may itself be a block array (tool_result allows text
			// or image blocks); flatten to the string OpenAI expects,
			// serializing anything else.
			var txt string
			switch c := bm["content"].(type) {
			case string:
				txt = c
			case []any:
				var sb []string
				for _, cb := range c {
					if cm, ok := cb.(map[string]any); ok && cm["type"] == "text" {
						if s, ok := cm["text"].(string); ok {
							sb = append(sb, s)
						}
					}
				}
				txt = strings.Join(sb, "\n")
			}
			msg := map[string]any{"role": "tool", "content": txt}
			if id, ok := bm["tool_use_id"].(string); ok && id != "" {
				msg["tool_call_id"] = id
			}
			out = append(out, msg)
		case "thinking":
			// Dropped: the OpenAI wire has no thinking-block representation,
			// and replaying prior reasoning is not required for coherence.
		}
	}
	flushText()
	if len(out) == 0 {
		out = append(out, map[string]any{"role": role, "content": ""})
	}
	return out
}

// --- response bridge -----------------------------------------------------

// anthropicBridge sits between completionHandler's usageRecorder and the
// client, converting the OpenAI-wire response into the Anthropic shape.
type anthropicBridge struct {
	http.ResponseWriter
	stream bool
	model  string

	status      int
	wroteHeader bool
	errStatus   int
	errBody     bytes.Buffer

	// stream state
	sse        bridgeSSE
	textOpen   bool
	blockIdx   int
	toolBlocks map[int]int // upstream tool_call index -> anthropic block index
}

type bridgeSSE struct {
	tail      bytes.Buffer
	stopMsg   string
	inTok     int
	outTok    int
	finished  bool
	startSent bool
}

func newAnthropicBridge(w http.ResponseWriter, stream bool, model string) *anthropicBridge {
	return &anthropicBridge{ResponseWriter: w, stream: stream, model: model, toolBlocks: map[int]int{}}
}

func (b *anthropicBridge) WriteHeader(code int) {
	if b.wroteHeader {
		return
	}
	b.wroteHeader = true
	b.status = code
	if code >= 400 {
		// Error body arrives as OpenAI JSON; buffer and translate in
		// finalize() so the client sees the Anthropic error shape.
		b.errStatus = code
		return
	}
	b.ResponseWriter.WriteHeader(code)
	if b.stream {
		b.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
	}
}

func (b *anthropicBridge) Write(p []byte) (int, error) {
	if b.errStatus >= 400 {
		if b.errBody.Len() < 1<<20 {
			b.errBody.Write(p)
		}
		return len(p), nil
	}
	if !b.wroteHeader {
		b.WriteHeader(http.StatusOK)
	}
	if b.stream {
		return b.writeStream(p)
	}
	if b.bufCapOK() {
		b.sse.tail.Write(p) // reuse tail buffer as the non-stream body buffer
	}
	return len(p), nil
}

func (b *anthropicBridge) bufCapOK() bool { return b.sse.tail.Len() < 8<<20 }

// Flush satisfies http.Flusher (ReverseProxy asserts it to stream).
// Suppressed while an error response is pending: an upstream Flush would
// make the underlying writer implicitly WriteHeader(200) (httptest and
// net/http both do this), locking the status at 200 before finalize() can
// write the real one — the translated 401 then shipped as 200.
func (b *anthropicBridge) Flush() {
	if b.errStatus >= 400 {
		return
	}
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// finalize runs after completionHandler returns: emits the translated body
// (non-stream + error paths — the stream path already wrote incrementally).
func (b *anthropicBridge) finalize() {
	if b.stream && b.errStatus == 0 {
		b.finishStream()
		return
	}
	if b.errStatus >= 400 {
		// Surface the upstream error verbatim inside the Anthropic envelope:
		// its "message" is what actually explains the failure.
		var oe struct {
			Error any `json:"error"`
		}
		if json.Unmarshal(b.errBody.Bytes(), &oe) == nil && oe.Error != nil {
			writeAnthropicErrorVal(b.ResponseWriter, b.errStatus, oe.Error)
			return
		}
		writeAnthropicErr(b.ResponseWriter, b.errStatus, "api_error", redactCredentialURLs(strings.TrimSpace(b.errBody.String())))
		return
	}
	// Non-stream success: translate the buffered OpenAI completion.
	var resp openAICompletion
	if err := json.Unmarshal(b.sse.tail.Bytes(), &resp); err != nil {
		writeAnthropicErr(b.ResponseWriter, http.StatusBadGateway, "api_error", "unparseable upstream response")
		return
	}
	if len(resp.Choices) == 0 {
		// Upstream 200 with no choices (some backends do this on refusal or
		// empty completion) — an empty message beats a panic.
		log.Printf("oaica-gateway: /v1/messages upstream 200 with no choices")
		writeAnthropicErr(b.ResponseWriter, http.StatusBadGateway, "api_error", "upstream returned no completion choices")
		return
	}
	msg := resp.Choices[0].Message
	content := ""
	if msg.Content != nil {
		content = *msg.Content
	}
	// Backends that put the answer in "reasoning" (content null) — see the
	// file header; an empty text reply would be worse than surfacing it.
	if content == "" && msg.Reasoning != nil {
		content = *msg.Reasoning
	}
	respBlocks := []map[string]any{}
	if strings.TrimSpace(content) != "" {
		respBlocks = append(respBlocks, map[string]any{"type": "text", "text": content})
	}
	for _, tc := range msg.ToolCalls {
		var input any = map[string]any{}
		if tc.Function.Arguments != "" {
			_ = json.Unmarshal([]byte(tc.Function.Arguments), &input)
		}
		respBlocks = append(respBlocks, map[string]any{
			"type": "tool_use", "id": tc.ID, "name": tc.Function.Name, "input": input,
		})
	}
	if len(respBlocks) == 0 {
		respBlocks = append(respBlocks, map[string]any{"type": "text", "text": ""})
	}
	in, out := resp.Usage.PromptTokens, resp.Usage.CompletionTokens
	json.NewEncoder(b.ResponseWriter).Encode(map[string]any{
		"id":            respID(resp.ID),
		"type":          "message",
		"role":          "assistant",
		"model":         b.model,
		"content":       respBlocks,
		"stop_reason":   stopReasonOpenAIToAnthropic(resp.Choices[0].FinishReason),
		"stop_sequence": nil,
		"usage":         map[string]any{"input_tokens": in, "output_tokens": out},
	})
}

// --- SSE translation ------------------------------------------------------

// writeStream translates OpenAI SSE chunks into Anthropic events as they
// arrive. Chunk shapes handled: delta.content, delta.reasoning,
// delta.tool_calls fragments, finish_reason, the trailing usage-only chunk.
func (b *anthropicBridge) writeStream(p []byte) (int, error) {
	// Cap the partial-line buffer (2026-09-01 audit M5): an upstream
	// emitting one endless SSE line would otherwise grow tail unboundedly.
	// Past the cap the remaining line is dropped — its deltas were already
	// forwarded; only usage extraction from a hypothetical later segment of
	// the same line is lost.
	if b.sse.tail.Len() < 1<<20 {
		b.sse.tail.Write(p)
	}
	for {
		raw := b.sse.tail.Bytes()
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			break
		}
		line := strings.TrimSpace(string(raw[:i]))
		b.sse.tail.Next(i + 1)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if payload == "" || payload == "[DONE]" {
			continue
		}
		if !b.sse.startSent {
			b.sse.startSent = true
			b.emit("message_start", map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"id": respID(""), "type": "message", "role": "assistant",
					"model": b.model, "content": []any{},
					"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
				},
			})
		}
		var chunk struct {
			Choices []struct {
				Delta struct {
					Content   *string `json:"content"`
					Reasoning *string `json:"reasoning"`
					ToolCalls []struct {
						Index    int    `json:"index"`
						ID       string `json:"id"`
						Name     string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			b.sse.inTok = chunk.Usage.PromptTokens
			b.sse.outTok = chunk.Usage.CompletionTokens
		}
		for _, ch := range chunk.Choices {
			if ch.Delta.Reasoning != nil && *ch.Delta.Reasoning != "" {
				b.textDelta(*ch.Delta.Reasoning)
			}
			if ch.Delta.Content != nil && *ch.Delta.Content != "" {
				b.textDelta(*ch.Delta.Content)
			}
			for _, tc := range ch.Delta.ToolCalls {
				b.toolDelta(tc.Index, tc.ID, tc.Name, tc.Arguments)
			}
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				b.sse.stopMsg = *ch.FinishReason
			}
		}
	}
	return len(p), nil
}

func (b *anthropicBridge) textDelta(s string) {
	if !b.textOpen {
		b.textOpen = true
		b.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": b.blockIdx,
			"content_block": map[string]any{"type": "text", "text": ""},
		})
	}
	b.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.blockIdx,
		"delta": map[string]any{"type": "text_delta", "text": s},
	})
}

func (b *anthropicBridge) toolDelta(upIdx int, id, name, args string) {
	block, ok := b.toolBlocks[upIdx]
	if !ok {
		if b.textOpen {
			b.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.blockIdx})
			b.blockIdx++
			b.textOpen = false
		}
		block = b.blockIdx
		b.blockIdx++
		b.toolBlocks[upIdx] = block
		b.emit("content_block_start", map[string]any{
			"type": "content_block_start", "index": block,
			"content_block": map[string]any{"type": "tool_use", "id": id, "name": name, "input": map[string]any{}},
		})
	}
	if args != "" {
		b.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": block,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": args},
		})
	}
}

// finishStream closes any open block and emits message_delta/message_stop.
// Called when the upstream stream ends WITHOUT a usage chunk too ([DONE] is
// the normal terminator) — the client must always get a well-formed end.
func (b *anthropicBridge) finishStream() {
	if !b.sse.startSent {
		return // upstream produced no events at all
	}
	if b.textOpen {
		b.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.blockIdx})
	}
	for _, block := range b.toolBlocks {
		b.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": block})
	}
	b.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReasonOpenAIToAnthropic(b.sse.stopMsg), "stop_sequence": nil},
		"usage": map[string]any{"input_tokens": b.sse.inTok, "output_tokens": b.sse.outTok},
	})
	b.emit("message_stop", map[string]any{"type": "message_stop"})
}

func (b *anthropicBridge) emit(event string, data any) {
	payload, _ := json.Marshal(data)
	fmt.Fprintf(b.ResponseWriter, "event: %s\ndata: %s\n\n", event, payload)
	b.Flush()
}

// stopReason maps OpenAI finish_reason to Anthropic stop_reason.
func stopReasonOpenAIToAnthropic(fr string) string {
	switch fr {
	case "tool_calls", "function_call":
		return "tool_use"
	case "length":
		return "max_tokens"
	case "stop":
		return "end_turn"
	default:
		if fr == "" {
			return "end_turn"
		}
		return "end_turn"
	}
}

// respID normalizes an OpenAI id to an Anthropic-looking one (Claude Code
// logs it; the prefix keeps client-side parsing honest about the shape).
func respID(openaiID string) string {
	if openaiID == "" {
		return "msg_" + fmt.Sprintf("%x", time.Now().UnixNano())
	}
	return "msg_" + strings.TrimPrefix(openaiID, "chatcmpl-")
}

// writeAnthropicErr writes an error in the Anthropic error envelope.
func writeAnthropicErr(w http.ResponseWriter, status int, code, msg string) {
	writeAnthropicErrorVal(w, status, map[string]any{"type": code, "message": msg})
}

func writeAnthropicErrorVal(w http.ResponseWriter, status int, errVal any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{"type": "error", "error": errVal})
}

// openAICompletion is the minimal non-stream completion shape the bridge
// needs to translate back.
type openAICompletion struct {
	ID      string `json:"id"`
	Choices []struct {
		FinishReason string `json:"finish_reason"`
		Message      struct {
			Content   *string `json:"content"`
			Reasoning *string `json:"reasoning"`
			ToolCalls []struct {
				ID       string `json:"id"`
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
			} `json:"tool_calls"`
		} `json:"message"`
	} `json:"choices"`
	Usage usage `json:"usage"`
}

// str reads a string field defensively.

// readCappedBody reads the request body with the same cap the OpenAI path
// enforces, writing the Anthropic-shaped error on failure.
func readCappedBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeAnthropicErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds limit")
		return nil, err
	}
	return body, nil
}

// readCloserBytes wraps bytes in a fresh NopCloser reader (replacement body).
func readCloserBytes(b []byte) io.ReadCloser { return io.NopCloser(bytes.NewReader(b)) }
