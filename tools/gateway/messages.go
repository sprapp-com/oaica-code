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
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"log"
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
		converted, convErr := contentBlocksToOpenAI(role, m["content"])
		if convErr != "" {
			return nil, convErr
		}
		msgs = append(msgs, converted...)
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

// firstNonEmpty returns the first non-empty string among the pointers, or
// "". It exists because a backend's reasoning field has more than one
// spelling and only one of them is ever populated.
func firstNonEmpty(vals ...*string) string {
	for _, v := range vals {
		if v != nil && *v != "" {
			return *v
		}
	}
	return ""
}

// toolResultText flattens a tool_result's content into the single string the
// OpenAI tool wire carries. Text blocks are joined with newlines; a block of
// any other type is DESCRIBED in place rather than dropped, so the model can
// tell "the tool returned an image" from "the tool returned nothing".
func toolResultText(content any) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, cb := range c {
			bm, ok := cb.(map[string]any)
			if ok && bm["type"] == "text" {
				if s, ok := bm["text"].(string); ok {
					parts = append(parts, s)
					continue
				}
			}
			parts = append(parts, describeBlock(cb))
		}
		return strings.Join(parts, "\n")
	default:
		return describeBlock(c)
	}
}

// describeBlock renders one non-text tool_result block: an image as its media
// type and payload size, a document as its text or its size, anything else as
// its JSON. The base64 of an image is deliberately NOT inlined — a screenshot
// pasted into a text field is charged against the context window in tokens and
// every backend tokenizes it as noise, so the size is the useful part. A
// document used to fall to the JSON fallback and took its whole payload with
// it: a tool returning a 393 KB PDF put the base64 in the prompt, charged as
// ~98k estimated tokens, for a model that cannot read it — while the same
// block at message level is refused and the same-sized image is summarised
// (2026-09-27 audit, round 35, B-F2).
func describeBlock(raw any) string {
	bm, ok := raw.(map[string]any)
	if !ok {
		if b, err := json.Marshal(raw); err == nil {
			return string(b)
		}
		return "[tool result block that could not be represented]"
	}
	switch t, _ := bm["type"].(string); t {
	case "image":
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
	case "document":
		src, _ := bm["source"].(map[string]any)
		if src == nil {
			return "[document tool result omitted: no source]"
		}
		if st, _ := src["type"].(string); st == "text" {
			// A text source carries content the prompt can hold, and the
			// message-level case above forwards it for the same reason: the
			// model was asked about a file it can be shown.
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
	if b, err := json.Marshal(bm); err == nil {
		return string(b)
	}
	return "[tool result block that could not be represented]"
}

// webSearchResultText renders a web_search_tool_result's payload as the text
// the OpenAI tool wire carries: one "- title: url" line per result, the
// provider's error code when the search failed, and the JSON for anything
// else. The sibling client-side converter renders the same list the same way
// (anthropic.formatWebSearchToolResultContent); this is a copy rather than an
// import because tools/gateway is its own module, and the two must not drift —
// the same turn is served by either leg depending on how the user connects.
func webSearchResultText(content any) string {
	switch c := content.(type) {
	case string:
		return c
	case []any:
		var b strings.Builder
		for _, item := range c {
			im, ok := item.(map[string]any)
			if !ok {
				continue
			}
			switch im["type"] {
			case "web_search_result":
				title, _ := im["title"].(string)
				url, _ := im["url"].(string)
				fmt.Fprintf(&b, "- %s: %s\n", title, url)
			case "web_search_tool_result_error":
				return webSearchErrorText(im["error_code"])
			}
		}
		return b.String()
	case map[string]any:
		if c["type"] == "web_search_tool_result_error" {
			return webSearchErrorText(c["error_code"])
		}
		if b, err := json.Marshal(c); err == nil {
			return string(b)
		}
		return ""
	default:
		if b, err := json.Marshal(c); err == nil {
			return string(b)
		}
		return ""
	}
}

// webSearchErrorText names a failed search, with the provider's code when it
// sent one.
func webSearchErrorText(code any) string {
	s, _ := code.(string)
	if s == "" {
		return "web_search_tool_result_error"
	}
	return "web_search_tool_result_error: " + s
}

// contentBlocksToOpenAI converts one Anthropic message's content (string or
// block array) into one-or-more OpenAI messages: text/images ride the same
// message, tool_use becomes assistant tool_calls, tool_result becomes a
// separate role:"tool" message (the OpenAI wire has no other spelling).
// The second return is the input this wire cannot represent, propagated to
// anthropicToOpenAI's own error string so the client gets a 400 naming the
// block instead of an answer to a silently altered prompt (2026-09-27 audit,
// round 33, B-F2).
func contentBlocksToOpenAI(role string, content any) ([]map[string]any, string) {
	if s, ok := content.(string); ok {
		return []map[string]any{{"role": role, "content": s}}, ""
	}
	blocks, ok := content.([]any)
	if !ok {
		return []map[string]any{{"role": role, "content": ""}}, ""
	}
	var out []map[string]any
	var parts []map[string]any // text/image parts of THIS message
	var toolCalls []map[string]any
	// toolMsg is the index of the assistant message carrying tool_calls, or
	// -1. Writing back to out[len(out)-1] instead assumed that message was
	// still last — text between two tool_use blocks flushes a message after
	// it, so the second call was written onto the text message (as a copy of
	// the whole list) and the client saw the first call twice, on two
	// different messages (2026-09-26 audit, fourth round).
	toolMsg := -1
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
			// The source's TYPE decides what the OpenAI wire can carry, and
			// reading only media_type/data read an Anthropic url source as two
			// empty strings: the model was sent the literal blank image
			// "data:;base64," and the client read a 200 about a picture it never
			// sent (2026-09-27 audit, round 33, B-F2). A source this wire
			// cannot express is refused instead — a silent blank is a lie the
			// answer is built on.
			src, _ := bm["source"].(map[string]any)
			if src == nil {
				return nil, "image block without a source"
			}
			switch st, _ := src["type"].(string); st {
			case "url":
				u, _ := src["url"].(string)
				if u == "" {
					return nil, `image block with source.type "url" and no url`
				}
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": u},
				})
			case "base64", "":
				// "" keeps the clients that spell a source as media_type+data
				// with no type, which this case read before the type existed.
				media, _ := src["media_type"].(string)
				data, _ := src["data"].(string)
				if data == "" {
					return nil, "image block with no inline data"
				}
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:" + media + ";base64," + data},
				})
			default:
				return nil, fmt.Sprintf("image source.type %q cannot be represented on the OpenAI wire", st)
			}
		case "tool_use", "server_tool_use":
			flushText()
			if toolMsg < 0 {
				out = append(out, map[string]any{"role": role, "content": "", "tool_calls": []map[string]any{}})
				toolMsg = len(out) - 1
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
			out[toolMsg]["tool_calls"] = toolCalls
		case "tool_result", "web_search_tool_result":
			flushText()
			// The tool's answer rides back as a role:"tool" message. The
			// content may itself be a block array (tool_result allows text
			// or image blocks); flatten it to the single string the OpenAI
			// tool wire carries (toolResultText). Non-text blocks used to be
			// dropped, so an image-only result reached the model as
			// content:"" — indistinguishable from "the tool returned
			// nothing" (2026-09-26 audit, fourth round).
			//
			// web_search_tool_result is the same message with the provider's
			// own search payload: the client's transcript records the results
			// of the search its server_tool_use asked for, so it is rendered
			// as the titles and URLs it holds. Both types were refused by the
			// default case below, which 400s a turn this gateway answered
			// before that case existed and every turn after it — the blocks
			// are in the client's history from the moment it is answered
			// (2026-09-27 audit, round 35, B-F1/A-F4).
			content := toolResultText(bm["content"])
			if t == "web_search_tool_result" {
				content = webSearchResultText(bm["content"])
			}
			msg := map[string]any{"role": "tool", "content": content}
			if id, ok := bm["tool_use_id"].(string); ok && id != "" {
				msg["tool_call_id"] = id
			}
			out = append(out, msg)
		case "thinking", "redacted_thinking":
			// Dropped DELIBERATELY, not silently: the OpenAI wire has no
			// reasoning block, so there is nothing to translate and nothing the
			// answer depends on. A redacted payload is opaque ciphertext by the
			// provider's own contract — it cannot be read, replayed or
			// modified. Claude Code with extended thinking echoes both back on
			// every following turn, so refusing them (which the default case
			// below does to any type it does not name) 400s a session that was
			// answered before that case existed. The sibling client-side
			// converter drops the same two (cmd/launch's block switch).
		case "document":
			// A file the client attached. The OpenAI wire has no document
			// block, but one whose source is TEXT carries its content inline,
			// and the sibling converter (cmd/launch's) writes that text into
			// the prompt — refusing it here would answer a turn this product
			// serves on its other leg. A binary source (a PDF) has no
			// representation and is refused in words rather than dropped.
			src, _ := bm["source"].(map[string]any)
			if src == nil {
				return nil, "document block without a source"
			}
			if st, _ := src["type"].(string); st != "text" {
				return nil, fmt.Sprintf("document source.type %q cannot be represented on the OpenAI wire", st)
			}
			data, _ := src["data"].(string)
			if data == "" {
				return nil, `document block with source.type "text" and no data`
			}
			parts = append(parts, map[string]any{"type": "text", "text": data})
		default:
			// A block this bridge cannot represent is REFUSED in words, never
			// dropped: the switch had no default and no "document" case, so an
			// Anthropic document block (a PDF the client attached) fell
			// through silently, the model was asked about a document it never
			// received, and the client got a 200 for the answer — the same
			// silent-lie class the image case above refuses, and the attached
			// payload was never charged to the prompt either (2026-09-27
			// audit, round 34, B-F2).
			return nil, fmt.Sprintf("content block type %q cannot be represented on the OpenAI wire", t)
		}
	}
	flushText()
	if len(out) == 0 {
		out = append(out, map[string]any{"role": role, "content": ""})
	}
	return out, ""
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
	committed   bool // success status written to the client (see commit)
	errStatus   int
	errBody     bytes.Buffer

	// stream state
	sse        bridgeSSE
	textOpen   bool
	blockIdx   int
	toolBlocks map[int]int // upstream tool_call index -> anthropic block index
}

type bridgeSSE struct {
	tail bytes.Buffer
	// stopMsg is the upstream's finish_reason, mapped to Anthropic's
	// stop_reason for the closing message_delta.
	stopMsg string
	// inTok is the upstream's prompt_tokens and cacheTok the cached part of
	// it: the closing message_delta reports inTok-cacheTok as input_tokens and
	// cacheTok as cache_read_input_tokens, because Anthropic's input_tokens is
	// the UNCACHED prompt (2026-09-27 audit, round 23).
	inTok     int
	cacheTok  int
	outTok    int
	finished  bool
	startSent bool
}

func newAnthropicBridge(w http.ResponseWriter, stream bool, model string) *anthropicBridge {
	return &anthropicBridge{ResponseWriter: w, stream: stream, model: model, toolBlocks: map[int]int{}}
}

// WriteHeader records the upstream's status; it does not commit one to the
// client. Committing here was the round-24 defect: a 200 upstream whose body
// cannot be translated — no choices, or not JSON — is answered by finalize()
// with a 502, and an already-committed 200 turned that into a superfluous
// WriteHeader that net/http discards, so the client read a FAILED turn as a
// successful one (and the ledger recorded 200 for it). The success status is
// written by commit(), which the stream path calls when it has its first event
// to send and the non-stream path calls once its translation succeeded.
func (b *anthropicBridge) WriteHeader(code int) {
	if b.wroteHeader {
		return
	}
	b.wroteHeader = true
	b.status = code
	// The upstream's Content-Length describes the UPSTREAM's body, and every
	// path out of this bridge answers with a different one (see commit's doc).
	// Dropped here, at the one point every response passes through, so the
	// translated body is framed by the server instead of truncated to a
	// length that was never ours.
	b.ResponseWriter.Header().Del("Content-Length")
	if code >= 400 {
		// Error body arrives as OpenAI JSON; buffer and translate in
		// finalize() so the client sees the Anthropic error shape.
		b.errStatus = code
		return
	}
}

// commit writes the success status exactly once. Callers must have set any
// Content-Type they want first: WriteHeader flushes the header block.
//
// Content-Length is DELETED here and never re-declared by hand: the reverse
// proxy copies the upstream's own Content-Length onto this response, and this
// bridge answers with a TRANSLATED body — a different length, sometimes a
// different shape entirely. net/http enforces a declared Content-Length on
// handler writes, so the client read a truncated body or none at all and
// `unexpected EOF`, on every non-stream /v1/messages turn and every
// /v1/messages error — including the "prompt is too long" wording Claude Code
// pattern-matches to its compaction recovery path, which without it retries
// the identical doomed request (2026-09-27 audit, round 34, B-F1). Without a
// declared length the server frames the body itself (chunked or computed),
// which is what every translating proxy must do. httptest.NewRecorder does not
// enforce a declared length, which is why the suite could not see this.
func (b *anthropicBridge) commit() {
	if b.committed {
		return
	}
	b.committed = true
	b.ResponseWriter.Header().Del("Content-Length")
	if b.stream {
		b.ResponseWriter.Header().Set("Content-Type", "text/event-stream")
	}
	b.ResponseWriter.WriteHeader(http.StatusOK)
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
// Suppressed until commit: an upstream Flush would make the underlying writer
// implicitly WriteHeader(200) (httptest and net/http both do this), locking
// the status at 200 before finalize() can write the real one — the translated
// 401, or the 502 for an untranslatable 200, then shipped as 200.
func (b *anthropicBridge) Flush() {
	if !b.committed {
		return
	}
	if f, ok := b.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// noAnswer reports the failure the bridge must answer when the upstream said
// 200 but sent nothing the client can use: the status (502) and the sentence
// the client is told. It returns (0, "") when the body does carry an answer, or
// when the upstream itself failed — that status is surfaced verbatim by
// finalize(), and it is not this predicate's business.
//
// One predicate with two callers, deliberately. finalize() answers the CLIENT
// with it; LedgerStatus reports it to the LEDGER, and the ledger row is written
// EARLIER — completionHandler writes it before messagesHandler calls finalize.
// The row therefore recorded the upstream's 200 for a turn the client read as a
// failure (2026-09-27 audit, round 25). Mirrored conditions in the two callers
// would drift apart; a shared one cannot.
func (b *anthropicBridge) noAnswer() (int, string) {
	if b.errStatus >= 400 {
		return 0, ""
	}
	if b.stream {
		if b.sse.startSent {
			return 0, ""
		}
		// The upstream ended without a single event. finishStream's own contract
		// ("the client must always get a well-formed end") cannot be met by an
		// empty body, and a client reading the stream waits for message_stop —
		// so answer the way the non-stream path answers an empty completion: 502,
		// which tells the client to retry instead of waiting (round 24). Nothing
		// has been committed yet, so the status is still ours to choose.
		return http.StatusBadGateway, "upstream returned an empty stream"
	}
	var resp openAICompletion
	if err := json.Unmarshal(b.sse.tail.Bytes(), &resp); err != nil {
		return http.StatusBadGateway, "unparseable upstream response"
	}
	if len(resp.Choices) == 0 {
		// Upstream 200 with no choices (some backends do this on refusal or
		// empty completion) — an empty message beats a panic.
		return http.StatusBadGateway, "upstream returned no completion choices"
	}
	return 0, ""
}

// LedgerStatus is the status the ledger should record for this turn, asked
// before the row is built because the bridge — not the upstream — decides the
// outcome the client sees. It is satisfied by the writer the gateway hands to
// completionHandler (see ledgerStatusWriter).
func (b *anthropicBridge) LedgerStatus(upstream int) int {
	if status, _ := b.noAnswer(); status != 0 {
		return status
	}
	return upstream
}

// finalize runs after completionHandler returns: emits the translated body
// (non-stream + error paths — the stream path already wrote incrementally).
func (b *anthropicBridge) finalize() {
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
	if status, msg := b.noAnswer(); status != 0 {
		log.Printf("oaica-gateway: /v1/messages upstream 200 answered %d: %s", status, msg)
		writeAnthropicErr(b.ResponseWriter, status, "api_error", msg)
		return
	}
	if b.stream {
		b.finishStream()
		return
	}
	// Non-stream success: translate the buffered OpenAI completion.
	var resp openAICompletion
	if err := json.Unmarshal(b.sse.tail.Bytes(), &resp); err != nil {
		writeAnthropicErr(b.ResponseWriter, http.StatusBadGateway, "api_error", "unparseable upstream response")
		return
	}
	if len(resp.Choices) == 0 {
		writeAnthropicErr(b.ResponseWriter, http.StatusBadGateway, "api_error", "upstream returned no completion choices")
		return
	}
	msg := resp.Choices[0].Message
	content := ""
	if msg.Content != nil {
		content = *msg.Content
	}
	// Backends that put the answer in a reasoning field (content null) — see
	// the file header; an empty text reply would be worse than surfacing it.
	// Both spellings are honoured: which one a backend uses is not something
	// the client can see or control.
	if content == "" {
		content = firstNonEmpty(msg.Reasoning, msg.ReasoningContent)
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
	// Anthropic's contract — the one this bridge translates INTO — is that
	// input_tokens is the UNCACHED prompt and cache_read_input_tokens the
	// prefix the upstream served from its cache; the two partition the prompt
	// and sum to what the upstream counted. Emitting the whole prompt as fresh
	// input read a 95% cache hit as a 0% hit, and any consumer that treats
	// input_tokens as fresh input over-counted by the cached amount. The client
	// proxy has split the same body this way since 2026-09-26 (see
	// anthropic_openai_proxy.go, and anthropic/anthropic.go for the contract);
	// this side never emitted the field at all (2026-09-27 audit, round 23).
	// nonNegative, like the stream path and the ledger row: an upstream that
	// states a negative prompt_tokens said nothing about the prompt, and the
	// raw subtraction handed the client input_tokens=-400 for the same turn
	// the ledger records as 0 (2026-09-27 audit, round 34, B-F3).
	u := resp.Usage.nonNegative()
	cached := u.cachedTokens()
	in, out := u.PromptTokens-cached, u.CompletionTokens
	b.ResponseWriter.Header().Set("Content-Type", "application/json")
	b.commit()
	json.NewEncoder(b.ResponseWriter).Encode(map[string]any{
		"id":            respID(resp.ID),
		"type":          "message",
		"role":          "assistant",
		"model":         b.model,
		"content":       respBlocks,
		"stop_reason":   stopReasonOpenAIToAnthropic(resp.Choices[0].FinishReason),
		"stop_sequence": nil,
		"usage": map[string]any{
			"input_tokens":            in,
			"cache_read_input_tokens": cached,
			"output_tokens":           out,
		},
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
	// forwarded — but nothing later may be: skipping the write once the
	// buffer was over the cap left it over the cap forever, so translation
	// stalled after the overlong line instead of resuming with the next one
	// (2026-09-27 audit, round 31).
	trimOverlongSSETail(&b.sse.tail, sseTailLimit)
	b.sse.tail.Write(p)
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
					// See the non-stream shape: the same field under
					// DeepSeek's/vLLM's spelling.
					ReasoningContent *string `json:"reasoning_content"`
					ToolCalls        []struct {
						Index     int    `json:"index"`
						ID        string `json:"id"`
						Name      string `json:"name"`
						Arguments string `json:"arguments"`
					} `json:"tool_calls"`
				} `json:"delta"`
				FinishReason *string `json:"finish_reason"`
			} `json:"choices"`
			Usage *struct {
				PromptTokens     int `json:"prompt_tokens"`
				CompletionTokens int `json:"completion_tokens"`
				// The same two spellings the ledger's usage type carries; the
				// cached part is reported to the client as
				// cache_read_input_tokens rather than folded into input_tokens
				// (2026-09-27 audit, round 23).
				PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
				PromptTokensDetails  *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if chunk.Usage != nil {
			// Per-field, not wholesale: an upstream that narrates usage per
			// chunk may state only what it has just measured, and the closing
			// chunk's silence about the cache is not a statement that the hit
			// was zero — replacing the whole usage lost a stated hit on both
			// the client's message_delta and the ledger row (2026-09-27 audit,
			// round 24).
			// Only a positive count is a statement: a chunk that states a
			// negative one has said nothing this gateway can bill or report
			// (2026-09-27 audit, round 33, sub-bar). A negative left to
			// overwrite a stated count turned the client's message_delta and
			// the ledger row into a credit.
			if chunk.Usage.PromptTokens > 0 {
				b.sse.inTok = chunk.Usage.PromptTokens
			}
			if chunk.Usage.CompletionTokens > 0 {
				b.sse.outTok = chunk.Usage.CompletionTokens
			}
			if c := (usage{
				PromptTokens:         chunk.Usage.PromptTokens,
				PromptCacheHitTokens: chunk.Usage.PromptCacheHitTokens,
				PromptTokensDetails:  chunk.Usage.PromptTokensDetails,
			}).cachedTokens(); c != 0 {
				b.sse.cacheTok = c
			}
		}
		for _, ch := range chunk.Choices {
			if r := firstNonEmpty(ch.Delta.Reasoning, ch.Delta.ReasoningContent); r != "" {
				b.textDelta(r)
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

// promptSplit returns the uncached and cached parts of the prompt as the
// closing message_delta states them, clamped to the partition the Anthropic
// contract requires: the two are parts of ONE prompt and sum to it. Subtracting
// the cache counter raw emitted input_tokens=-800 with
// cache_read_input_tokens=900 on a stream that stated a hit and then restated a
// smaller prompt — numbers that do not add up to any prompt, one of them
// negative, and per-field usage merging is what keeps the larger hit
// (2026-09-27 audit, round 33, B-F3).
func (b *anthropicBridge) promptSplit() (fresh, cached int) {
	cached = b.sse.cacheTok
	if cached < 0 {
		cached = 0
	}
	if cached > b.sse.inTok {
		cached = b.sse.inTok
	}
	if cached < 0 {
		// The prompt itself was stated negative; nothing of it is cached.
		cached = 0
	}
	return b.sse.inTok - cached, cached
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
	// input_tokens excludes what the upstream served from its prefix cache,
	// which is reported separately: the two partition prompt_tokens and sum to
	// it, so a client that adds them sees the real prompt and one that bills
	// input_tokens bills only the uncached part (2026-09-27 audit, round 23).
	fresh, cachedTok := b.promptSplit()
	b.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stopReasonOpenAIToAnthropic(b.sse.stopMsg), "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":            fresh,
			"cache_read_input_tokens": cachedTok,
			"output_tokens":           b.sse.outTok,
		},
	})
	b.emit("message_stop", map[string]any{"type": "message_stop"})
}

func (b *anthropicBridge) emit(event string, data any) {
	b.commit() // first event: the stream is a success, and it is now streamable
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
			// reasoning_content is the same field under the spelling
			// DeepSeek and several vLLM builds use. Unmodelled, it vanished
			// on the way in, and a backend that puts its whole answer there
			// produced an empty Anthropic reply (2026-09-26 audit, fourth
			// round).
			ReasoningContent *string `json:"reasoning_content"`
			ToolCalls        []struct {
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
