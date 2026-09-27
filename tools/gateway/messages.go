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
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
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
	// Whether a nested tool_result image reaches the model is a property of the
	// MODEL, not of the block: the translation has to know before it flattens
	// the content, so the roster is read here — under the same lock the
	// forwarding path takes — rather than in completionHandler, where the
	// image bytes have already been replaced by a description (2026-09-27
	// audit, round 38, B-F5).
	modelID, _ := req["model"].(string)
	g.mu.RLock()
	gm, known := g.byID[modelID]
	g.mu.RUnlock()
	acceptsImages := known && gm.acceptsImages()

	openai, convErr := anthropicToOpenAI(req, acceptsImages)
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
	// The prompt size this request really carries, in the unit the rest of the
	// gateway measures prompts in: the serialized body with each inline image's
	// base64 replaced by its allowance. It is what the client is told when the
	// upstream states no usage at all, rather than the 0 that left a session's
	// context meter still and its auto-compaction unfired (2026-09-27 audit,
	// round 39, C-F3).
	bridge.promptEstimate = promptPayloadBytes(len(nb), openai) / 4
	g.completionHandler(bridge, r)
	bridge.finalize()
}

// anthropicToOpenAI converts a decoded Anthropic messages request into the
// OpenAI chat-completions map. Returns a non-empty error string on
// structurally impossible input (no messages, non-array content pieces we
// cannot represent).
func anthropicToOpenAI(req map[string]any, acceptsImages bool) (map[string]any, string) {
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
		converted, convErr := contentBlocksToOpenAI(role, m["content"], acceptsImages)
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

// firstNonEmptyStr is firstNonEmpty for plain strings: a field with more than
// one spelling on the wire (a tool call's name and arguments are written both
// nested under `function` and flat) takes whichever the upstream populated.
func firstNonEmptyStr(vals ...string) string {
	for _, v := range vals {
		if v != "" {
			return v
		}
	}
	return ""
}

// toolResultText flattens a tool_result's content into the single string the
// OpenAI tool wire carries. Text blocks are joined with newlines; a block of
// any other type is DESCRIBED in place rather than dropped, so the model can
// tell "the tool returned an image" from "the tool returned nothing".
//
// carryImages says the image itself is being sent alongside this text (see
// toolResultImageParts), in which case an image block contributes NOTHING here:
// describing a picture the request also attaches would tell the model about a
// screenshot it can already see, and the client leg — which sends the image —
// writes no such notice (2026-09-27 audit, round 38, B-F5).
func toolResultText(content any, carryImages bool) string {
	switch c := content.(type) {
	case nil:
		return ""
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, cb := range c {
			if bm, ok := cb.(map[string]any); ok {
				switch bm["type"] {
				case "text":
					// An empty text block is no text: joined as an empty part it
					// put a blank line in front of the real content, where the
					// client leg's copy skips it (2026-09-27 audit, round 39,
					// A-F6).
					if s, ok := bm["text"].(string); ok {
						if s != "" {
							parts = append(parts, s)
						}
						continue
					}
				case "image":
					if carryImages {
						continue
					}
				}
			}
			parts = append(parts, describeBlock(cb))
		}
		return strings.Join(parts, "\n")
	default:
		return describeBlock(c)
	}
}

// toolResultImageParts renders the image blocks of a tool_result's content as
// the OpenAI multimodal parts the model can see: a base64 source as a data URL,
// a url source as itself. Refusing to send them, which this gateway did, made
// the same body two different prompts: the client leg attaches the screenshot a
// tool returned and this one replaced it with a one-line notice, and the prompt
// meter already charged the payload as an image — so the session was billed for
// a picture the model never received (2026-09-27 audit, round 38, B-F5).
//
// The second return is a notice for every image block that produced NO part —
// a source with an empty url, a source type this wire cannot express, a base64
// source with no data. The caller writes them into the tool message's text, so
// an image the model cannot be shown is STATED rather than erased: dropped in
// silence, the message was byte-identical to "the tool returned nothing" and the
// model answered as if the tool had said nothing (2026-09-27 audit, round 39,
// C-F2).
func toolResultImageParts(content any) (parts []any, notices []string) {
	blocks, _ := content.([]any)
	parts = make([]any, 0, len(blocks))
	for _, cb := range blocks {
		bm, ok := cb.(map[string]any)
		if !ok || bm["type"] != "image" {
			continue
		}
		src, _ := bm["source"].(map[string]any)
		if src == nil {
			notices = append(notices, describeBlock(cb))
			continue
		}
		switch st, _ := src["type"].(string); st {
		case "url":
			u, _ := src["url"].(string)
			if u == "" {
				notices = append(notices, describeBlock(cb))
				continue
			}
			parts = append(parts, map[string]any{
				"type": "image_url", "image_url": map[string]any{"url": u}})
		case "base64", "":
			data, _ := src["data"].(string)
			if data == "" {
				notices = append(notices, describeBlock(cb))
				continue
			}
			media, _ := src["media_type"].(string)
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": "data:" + inlineImageMediaType(media, data) + ";base64," + data,
				}})
		default:
			notices = append(notices, describeBlock(cb))
		}
	}
	return parts, notices
}

// inlineImageMediaType is the MIME type a data URL is written with: the type the
// image's own magic bytes name when they name one, and the type the client
// stated otherwise (jpeg when it stated none). Taking the client's word for it
// sent the upstream an image/png data URL holding JPEG bytes, which is a picture
// the backend decode fails on — the client leg sniffs the same bytes for the
// same reason (2026-09-27 audit, round 39, C-F11). The stated type is preferred
// over the sibling's blanket jpeg only where the bytes are of a format this
// sniffer does not know: there the client's own label is the better guess, and
// the sibling's default would call it a JPEG.
func inlineImageMediaType(media, data string) string {
	n := len(data)
	if n > 32 {
		n = 32
	}
	n -= n % 4 // a base64 quantum, so the head decodes
	var head []byte
	if n > 0 {
		if b, err := base64.StdEncoding.DecodeString(data[:n]); err == nil {
			head = b
		}
	}
	switch {
	case len(head) >= 8 && head[0] == 0x89 && head[1] == 'P' && head[2] == 'N' && head[3] == 'G':
		return "image/png"
	case len(head) >= 3 && head[0] == 'G' && head[1] == 'I' && head[2] == 'F':
		return "image/gif"
	case len(head) >= 12 && string(head[0:4]) == "RIFF" && string(head[8:12]) == "WEBP":
		return "image/webp"
	case len(head) >= 3 && head[0] == 0xFF && head[1] == 0xD8 && head[2] == 0xFF:
		return "image/jpeg"
	}
	if media != "" {
		return media
	}
	return "image/jpeg"
}

// searchResultOrigin reads a search_result's source — where the passage came
// from — as the sibling converter does: a bare string, or an object whose
// `ref` and then `url` carry it. Reading only `url` dropped the origin of a
// source that stated it as `ref`, so the same body reached the model with and
// without its provenance depending on which leg served it (2026-09-27 audit,
// round 39, C-F8).
func searchResultOrigin(src any) string {
	switch s := src.(type) {
	case string:
		return s
	case map[string]any:
		if ref, _ := s["ref"].(string); ref != "" {
			return ref
		}
		if u, _ := s["url"].(string); u != "" {
			return u
		}
	}
	return ""
}

// searchResultText flattens a search_result's content — the passages a search
// returned — into the text the model is asked about. Same reading as
// toolResultText: a text block is its passage, and a block of another type is
// described rather than read for a "text" key it may happen to carry, so a
// nested block whose passage sits under another key is not silently dropped
// (2026-09-27 audit, round 37, A-F1/A-F3).
func searchResultText(content any) string {
	switch c := content.(type) {
	case nil:
		// A result with no passages has no text. Described, it put the literal
		// `null` in the prompt as though the search had returned it (2026-09-27
		// audit, round 39, C-F7).
		return ""
	case string:
		return c
	case []any:
		parts := make([]string, 0, len(c))
		for _, cb := range c {
			bm, ok := cb.(map[string]any)
			if ok && bm["type"] == "text" {
				// An empty passage is no passage: it fell through to the
				// describer and put `{"text":"","type":"text"}` in the prompt as
				// though the search had returned it, which the sibling converter
				// has skipped since round 38 (2026-09-27 audit, round 39,
				// C-F7/A-F6).
				if s, ok := bm["text"].(string); ok {
					if s != "" {
						parts = append(parts, s)
					}
					continue
				}
			}
			if desc := describeBlock(cb); desc != "" {
				parts = append(parts, desc)
			}
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
	case "search_result":
		// A passage list nested inside a tool result (or inside another
		// passage list): title, origin and passages, rendered through the same
		// flattener the message-level case uses. Without this arm the block
		// fell to the JSON fallback below and took a screenshot's base64 into
		// the prompt with it, while the client leg describes the identical
		// block (2026-09-27 audit, round 38, A-F4).
		var label []string
		if title, _ := bm["title"].(string); title != "" {
			label = append(label, title)
		}
		if origin := searchResultOrigin(bm["source"]); origin != "" {
			label = append(label, origin)
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
func contentBlocksToOpenAI(role string, content any, acceptsImages bool) ([]map[string]any, string) {
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
					"image_url": map[string]any{"url": "data:" + inlineImageMediaType(media, data) + ";base64," + data},
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
			carryImages := acceptsImages && t == "tool_result"
			content := toolResultText(bm["content"], carryImages)
			if t == "web_search_tool_result" {
				content = webSearchResultText(bm["content"])
			}
			var dropped []string
			if carryImages {
				// The images this model can be shown, and a sentence for each one
				// it cannot: an image that yields no part is stated in the text
				// (below), not erased (2026-09-27 audit, round 39, C-F2).
				var parts []any
				parts, dropped = toolResultImageParts(bm["content"])
				if len(parts) > 0 {
					if content == "" && len(dropped) > 0 {
						content = strings.Join(dropped, "\n")
						dropped = nil
					}
					arr := make([]any, 0, len(parts)+1)
					if content != "" {
						arr = append(arr, map[string]any{"type": "text", "text": content})
					}
					arr = append(arr, parts...)
					msg := map[string]any{"role": "tool", "content": arr}
					if id, ok := bm["tool_use_id"].(string); ok && id != "" {
						msg["tool_call_id"] = id
					}
					out = append(out, msg)
					continue
				}
			}
			if len(dropped) > 0 {
				if content != "" {
					content += "\n"
				}
				content += strings.Join(dropped, "\n")
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
		case "search_result":
			// A passage the search returned, carried in the turn the model is
			// asked about: its title, where it came from, and the text itself.
			// The sibling client-side converter gained this case and the two
			// legs must not disagree about whether the model received it — the
			// same body was 200-with-content through `oaica launch` and a 400
			// here, which is the disagreement that case exists to close
			// (2026-09-27 audit, round 37, A-F1). A source is a bare URL string
			// in the documented spelling and an object in the other; the client
			// leg reads both, so this one does too.
			var label []string
			if title, _ := bm["title"].(string); title != "" {
				label = append(label, title)
			}
			if origin := searchResultOrigin(bm["source"]); origin != "" {
				label = append(label, origin)
			}
			content := searchResultText(bm["content"])
			if content != "" {
				label = append(label, content)
			}
			if len(label) > 0 {
				parts = append(parts, map[string]any{"type": "text", "text": strings.Join(label, "\n")})
			}
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
	sse bridgeSSE
	// promptEstimate is the prompt size in tokens this gateway can work out for
	// itself from the request it just serialized. The upstream states the real
	// prompt with its usage, and that is what is reported whenever it does; but
	// an upstream that states NO usage at all left the client's message_delta
	// with input_tokens:0 — a session whose context meter never moved, so
	// auto-compaction never fired and the session walked into the context wall.
	// The client leg has always reported its estimate in that case
	// (cmd/launch/anthropic_openai_proxy.go, "!statedPrompt && estInputTokens >
	// 0"); this is the server-side half of the same rule (2026-09-27 audit,
	// round 39, C-F3).
	promptEstimate int
	// cur is the block whose content_block_start has been emitted and whose
	// content_block_stop has not. The Anthropic wire is strictly sequential — a
	// block's events bracket its deltas, and no block's events may arrive inside
	// another's — so nothing opens until this one has closed (2026-09-27 audit,
	// round 39, B-F7).
	cur *openBlock
	// nextIdx is the index the next block to open takes. Indices are assigned in
	// START order rather than reserved when a tool call is first seen: a call
	// whose name arrives late is held, and a text block that arrives in between
	// must take the lower index, or the held block's start would be emitted after
	// a higher-indexed block's (2026-09-27 audit, round 39, B-F2).
	nextIdx int
	// heldText is narration that arrived while a tool call's arguments were still
	// an unfinished JSON object. Closing that block to open a text block would
	// make the rest of its arguments undeliverable — half a call's JSON, the
	// shape round 37's B-F1 closed — so the text waits until the call is closed
	// and is then delivered as its own block.
	heldText strings.Builder
	// toolBlocks maps the key toolKey derives for an upstream tool call to the
	// block it is being written into.
	toolBlocks map[string]*toolBlock
	// toolOrder is every tool block in the order it was first seen, so the sweep
	// at end of stream is not a walk over a Go map (round 38's B-F7).
	toolOrder []*toolBlock
	// lastToolKey is the key of the last tool call a delta named, so an
	// upstream that sends the index-less fragments this wire permits (an
	// arguments-only continuation) keeps writing into the block it opened.
	lastToolKey string
	// lastToolName is the name that opened that block, so a later fragment
	// naming a DIFFERENT tool is understood to be introducing one.
	lastToolName string
	// synthSeq mints keys for calls whose upstream states neither an index nor
	// an id, so two such calls cannot share a block.
	synthSeq int
}

// toolBlock is one tool call being translated into an Anthropic content block.
// Its content_block_start is emitted LATE — at the first fragment that NAMES the
// call — because the start is the only event that can carry a name: an upstream
// that states an id in one fragment and the name in the next opened the block on
// the id, and the name that followed had nowhere to go, so the client was handed
// {"id":"call_1","name":""} and could neither name nor execute the call while
// stop_reason said tool_use (2026-09-27 audit, round 38, B-F2). Arguments that
// arrive before the name are held in the block and sent as one delta once the
// block opens.
//
// A block with no name is never opened at all. An upstream that never names a
// call and never finishes one still produces arguments (a fragment with no
// index, no id and no name), and opening the block for those argued the client
// into a tool_use it could not execute — the round-39 auditor reached that shape
// through a bare arguments fragment and through a held block swept at end of
// stream (2026-09-27 audit, round 39, B-F8).
type toolBlock struct {
	// index is the block's index on the wire, assigned when it opens; -1 until
	// then, because a block that opens later takes a later index (see nextIdx).
	index int
	key   string
	id    string
	name  string
	// started is whether content_block_start has been emitted for this block.
	started bool
	// closed is whether its content_block_stop has been emitted. A closed block
	// cannot be reopened: a fragment of its arguments that arrives afterwards can
	// no longer be delivered (see toolDelta).
	closed bool
	// args is every argument fragment the upstream has stated so far, kept so
	// toolKey can tell a repeat of the current call's name from the next call's
	// (see startsANewToolCall's rule, mirrored from the client leg).
	args strings.Builder
	// emitted counts how much of args has been sent as input_json_delta. It
	// trails args.Len() while the block is held, so the fragments that arrived
	// before the name are delivered — in one delta — the moment the block opens.
	emitted int
}

// openBlock is the block currently open on the wire: index, and the tool call it
// belongs to (nil for a text block).
type openBlock struct {
	index int
	tool  *toolBlock
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
	inTok    int
	cacheTok int
	outTok   int
	// statedPrompt and statedCompletion are whether the upstream STATED each
	// count, which is not the same as stating a non-zero one: a fully-cached turn
	// reports prompt_tokens>0 with all of it cached, and its uncached count is
	// legitimately 0. When a field was never stated the client is told this
	// gateway's own estimate instead of a hard 0 — a session that really did grow
	// then looks flat, and Claude Code sizes auto-compaction on that number
	// (2026-09-27 audit, round 39, C-F3).
	statedPrompt     bool
	statedCompletion bool
	// outBytes counts the answer's bytes as they are relayed (text and tool
	// argument fragments), the unit the client leg divides for the same fallback
	// (streamedText/4 + 1).
	outBytes int
	finished bool
	// startSent is whether message_start has been emitted.
	startSent bool
	// upstreamErr is the sentence from an error frame the upstream sent INSIDE
	// the stream. It is the truth about the turn: a stream that states a
	// failure and then ends is not a turn that finished, whatever the missing
	// finish_reason would otherwise be read as (round 38, B-F6).
	upstreamErr string
	// nonSSE holds the stream's non-"data:" lines, which is all of a whole
	// completion document sent in answer to a stream request (see
	// adoptWholeStream).
	nonSSE bytes.Buffer
}

func newAnthropicBridge(w http.ResponseWriter, stream bool, model string) *anthropicBridge {
	return &anthropicBridge{ResponseWriter: w, stream: stream, model: model, toolBlocks: map[string]*toolBlock{}}
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
		if b.sse.upstreamErr != "" {
			// The upstream reported a failure inside the stream. Whether or not
			// events were already sent, the client and the ledger must read the
			// turn as failed: the stream path answers it with an SSE error event
			// (finishStream), the not-yet-started one with the status below.
			return http.StatusBadGateway, b.sse.upstreamErr
		}
		if b.sse.startSent {
			return 0, ""
		}
		if _, ok := b.bufferedCompletion(); ok {
			// No data: line was ever sent, but the body is a whole completion:
			// finalize adopts it as the turn, so the client reads an answer and
			// the row must not call it a failure. LedgerStatus asks this
			// predicate BEFORE finalize runs — the row is written first — so
			// without this the ledger recorded 502 with zero tokens for a turn
			// the client was served and the upstream billed (2026-09-27 audit,
			// round 39, B-F3).
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

// EstimatedUsage reports the counts this bridge measured for itself, for a turn
// whose upstream stated no usage at all. The recorder reads the upstream's own
// bytes, so it has nothing to record for such a turn and its row read
// prompt=0/completion=0/cost=0 — the one record kept of a turn that was served
// and billed, saying it carried nothing (2026-09-27 audit, round 39, C-F3). The
// same numbers go out in the client's message_delta, so the row and the client
// cannot disagree. It reports false when the upstream stated anything, and the
// recorder's own reading then stands.
func (b *anthropicBridge) EstimatedUsage() (usage, bool) {
	if b.sse.statedPrompt || b.sse.statedCompletion {
		return usage{}, false
	}
	u := usage{PromptTokens: b.promptEstimate, CompletionTokens: b.sse.outTok}
	if !b.sse.statedCompletion {
		if est := b.outputEstimate(); est > 0 {
			u.CompletionTokens = est
		}
	}
	if u.PromptTokens <= 0 && u.CompletionTokens <= 0 {
		return usage{}, false
	}
	return u, true
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
	if b.stream {
		// A stream request the upstream answered with ONE completion document
		// instead of frames (or no frames at all) is ADOPTED as the turn, on
		// the same reading the client leg's adoptNonSSECompletion makes: the
		// answer exists, and reporting "upstream returned an empty stream" over
		// it discarded a turn the upstream had produced and billed (2026-09-27
		// audit, round 38, B-F4).
		b.adoptWholeStream()
	}
	if status, msg := b.noAnswer(); status != 0 {
		// A stream that has already sent events cannot be given a status: the
		// client is reading SSE, and an error document appended to the event
		// stream is not an answer it can parse. finishStream emits the failure
		// as an `error` event instead — the shape the Anthropic wire uses
		// mid-stream. Nothing has been committed before the first event, so
		// there the status is still ours to choose.
		if !(b.stream && b.sse.startSent) {
			log.Printf("oaica-gateway: /v1/messages upstream 200 answered %d: %s", status, msg)
			writeAnthropicErr(b.ResponseWriter, status, "api_error", msg)
			return
		}
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
		args := firstNonEmptyStr(tc.Function.Arguments, tc.Arguments)
		if args != "" {
			_ = json.Unmarshal([]byte(args), &input)
		}
		respBlocks = append(respBlocks, map[string]any{
			"type": "tool_use", "id": tc.ID,
			"name":  firstNonEmptyStr(tc.Function.Name, tc.Name),
			"input": input,
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
	// The answer's size, in the unit the stream path counts it in, so the
	// fallback below is one rule for both paths (2026-09-27 audit, round 39,
	// C-F3).
	b.sse.outBytes = len(content)
	if out <= 0 {
		out = b.outputEstimate()
	}
	if u.PromptTokens <= 0 {
		// Nothing stated about the prompt. Reporting 0 told the client its
		// context had not grown, and Claude Code sizes auto-compaction on this
		// field — the estimate below is what this gateway can work out for
		// itself, which is what the client leg reports in the same case
		// (2026-09-27 audit, round 39, C-F3).
		in = b.promptEstimate - cached
		if in < 0 {
			in = 0
		}
	}
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
			// Kept, not discarded: an upstream that answers a stream request
			// with a whole completion document sends no "data:" lines at all,
			// and the adoption below needs the bytes (2026-09-27 audit, round
			// 38, B-F4). Bounded, and only ever read when nothing was streamed.
			if b.sse.nonSSE.Len() < 1<<20 {
				b.sse.nonSSE.WriteString(line)
				b.sse.nonSSE.WriteByte('\n')
			}
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
				Delta        oaDelta  `json:"delta"`
				Message      *oaDelta `json:"message"`
				FinishReason *string  `json:"finish_reason"`
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
			// Raw, because a failure stated inside the stream is not always an
			// object: an upstream may send {"error":"upstream ran out of KV
			// cache"} as a bare string, and modelling only the object made the
			// whole frame fail to parse — so it was discarded, the client read a
			// turn that ended normally with stop_reason end_turn, and the ledger
			// recorded 200 for a request the upstream failed (2026-09-27 audit,
			// round 39, B-F6).
			Error json.RawMessage `json:"error"`
		}
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			b.sse.upstreamErr = upstreamErrorSentence(payload, errorFrameMessage(chunk.Error))
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
				b.sse.statedPrompt = true
			}
			if chunk.Usage.CompletionTokens > 0 {
				b.sse.outTok = chunk.Usage.CompletionTokens
				b.sse.statedCompletion = true
			}
			// The hit is REMEMBERED as stated and clamped only when it is
			// reported (promptSplit), against the prompt that is known by then.
			// Clamping it here measured it against the prompt known at this
			// instant — which is zero on a stream that states the hit BEFORE the
			// prompt size, so a real hit was thrown away and the client was told
			// cache_read_input_tokens=0 for a turn the ledger row for the same
			// request recorded 900 (2026-09-27 audit, round 39, B-F4/C-F4).
			hit := 0
			if chunk.Usage.PromptTokensDetails != nil {
				hit = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			if hit <= 0 {
				hit = chunk.Usage.PromptCacheHitTokens
			}
			if hit > 0 {
				b.sse.cacheTok = hit
			}
		}
		for _, ch := range chunk.Choices {
			// A whole completion inside a frame carries the same fields as a
			// delta, under `message`. Relay it when it is the FIRST thing the
			// stream has said: an upstream emulating streaming around a
			// non-streaming backend answers this way, and its answer (or its
			// tool calls) was discarded — the client got a 200 with empty content
			// and the ledger booked the discard (2026-09-27 audit, round 39,
			// B-F1). A `message` arriving after content has begun is not the
			// turn, and only its finish_reason is read.
			if ch.Message != nil && b.nothingRelayed() {
				b.relayDelta(*ch.Message)
			}
			b.relayDelta(ch.Delta)
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				b.sse.stopMsg = *ch.FinishReason
			}
		}
	}
	return len(p), nil
}

func (b *anthropicBridge) textDelta(s string) {
	if b.cur != nil && b.cur.tool != nil {
		// A tool call is open. Narration arriving between calls means that call's
		// arguments are as complete as the upstream will make them, so the block
		// is closed and the text block takes the next index. If its arguments are
		// still an unfinished JSON object the call is NOT over and the text is
		// held instead: closing the block would make the rest of the arguments
		// undeliverable, and the client would be handed a call whose partial_json
		// stops mid-object — the shape round 37's B-F1 closed.
		if !json.Valid([]byte(strings.TrimSpace(b.cur.tool.args.String()))) {
			b.heldText.WriteString(s)
			return
		}
		b.closeOpen()
	}
	if b.cur == nil {
		b.openText()
	}
	b.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.cur.index,
		"delta": map[string]any{"type": "text_delta", "text": s},
	})
}

// openText opens a text block at the next index. The caller has closed whatever
// was open.
func (b *anthropicBridge) openText() {
	idx := b.takeIdx()
	b.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": idx,
		"content_block": map[string]any{"type": "text", "text": ""},
	})
	b.cur = &openBlock{index: idx}
}

// takeIdx hands out the next wire index. Indices are handed out in the order
// blocks OPEN, which is the order their events go out, so the client always
// reads a block's start before that block's deltas and never reads a start
// inside another block.
func (b *anthropicBridge) takeIdx() int {
	idx := b.nextIdx
	b.nextIdx++
	return idx
}

// closeOpen ends the open block, if any. A tool block that closes is marked so:
// an argument fragment that arrives afterwards belongs to a call the client has
// already been told is finished, and the wire has no way to reopen it.
func (b *anthropicBridge) closeOpen() {
	if b.cur == nil {
		return
	}
	if b.cur.tool != nil {
		b.cur.tool.closed = true
	}
	b.emit("content_block_stop", map[string]any{"type": "content_block_stop", "index": b.cur.index})
	b.cur = nil
}

// flushHeldText delivers narration that was held behind a tool call whose
// arguments had not finished. The caller has closed the tool block; the text
// block that follows takes the next index.
func (b *anthropicBridge) flushHeldText() {
	if b.heldText.Len() == 0 {
		return
	}
	s := b.heldText.String()
	b.heldText.Reset()
	b.openText()
	b.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": b.cur.index,
		"delta": map[string]any{"type": "text_delta", "text": s},
	})
}

// toolKey identifies the anthropic block a tool-call delta belongs to. The
// upstream's index is the authority where it states one. Where it does not —
// the index is optional on this wire and several upstreams omit it — the call
// is keyed by the id that introduces it, and an id-less fragment by the block
// the previous delta opened. Keyed instead by a zero-valued index, every call
// after the first in such a stream filed into block 0: its id was discarded and
// its name and arguments were written into the first call's block, so a turn
// that chose two tools reached the client as one (2026-09-27 audit, round 36,
// B-F2).
//
// EVERY branch records the block it answers with, because the fragment that
// follows may omit everything but the arguments. Recording it only for an
// id-bearing delta sent that continuation to a freshly minted block with no
// name and no id — one call became two, the first holding half its own JSON and
// the second nothing, and the turn still reported stop_reason tool_use
// (2026-09-27 audit, round 37, B-F1). And a fragment that carries a NAME can
// only be introducing a call (an argument continuation carries arguments
// alone), so a name that differs from the one that opened the current block
// starts a new one: keyed by the block the previous delta opened, two calls in
// a stream that states neither an index nor an id shared one block, the second
// name discarded and its arguments concatenated onto the first's input
// (2026-09-27 audit, round 37, B-F2).
func (b *anthropicBridge) toolKey(upIdx *int, id, name, args string) string {
	if upIdx != nil {
		if name != "" {
			b.lastToolName = name
		}
		b.lastToolKey = "#" + strconv.Itoa(*upIdx)
		return b.lastToolKey
	}
	if id != "" {
		if name != "" {
			b.lastToolName = name
		}
		b.lastToolKey = "!" + id
		return b.lastToolKey
	}
	if b.lastToolKey != "" {
		// The current block's accumulated arguments decide whether this
		// nameless, id-less fragment continues that call or begins the next:
		// a fragment that repeats the current call's name while its arguments
		// are already a COMPLETE JSON object is the next call, which is the
		// rule the client leg's startsANewToolCall applies to the same wire —
		// without it two index-less calls to the SAME tool shared one block and
		// the second call's arguments were concatenated onto the first's, so
		// the client received one call whose partial_json held two objects
		// (2026-09-27 audit, round 38, B-F3).
		//
		// A bare repeat — the fragment names the call again and states no
		// arguments, over a call that has accumulated none — is the second of
		// two argument-less calls, and it too begins the next call: an empty
		// argument string is a COMPLETE argument list for a call that takes
		// none, and reading it as a continuation delivers one tool_use where the
		// model asked for two (2026-09-27 audit, round 39, B-F9). The cost of
		// that reading, an upstream that restates the name in a chunk of its
		// own before sending the arguments, is a call with no arguments — the
		// other reading's cost is a call the client never hears about, and only
		// one of the two can be reported to the model at all.
		if name != "" && b.lastToolName != "" {
			cur := b.toolBlocks[b.lastToolKey]
			acc := ""
			if cur != nil {
				acc = strings.TrimSpace(cur.args.String())
			}
			if name != b.lastToolName || (acc != "" && json.Valid([]byte(acc))) ||
				(cur != nil && acc == "" && strings.TrimSpace(args) == "") {
				b.synthSeq++
				b.lastToolKey = "?" + strconv.Itoa(b.synthSeq)
				b.lastToolName = name
				return b.lastToolKey
			}
		}
		return b.lastToolKey
	}
	b.synthSeq++
	b.lastToolKey = "?" + strconv.Itoa(b.synthSeq)
	if name != "" {
		b.lastToolName = name
	}
	return b.lastToolKey
}

func (b *anthropicBridge) toolDelta(upIdx *int, id, name, args string) {
	key := b.toolKey(upIdx, id, name, args)
	tb, ok := b.toolBlocks[key]
	if !ok {
		tb = &toolBlock{key: key, index: -1}
		b.toolBlocks[key] = tb
		b.toolOrder = append(b.toolOrder, tb)
	}
	if id != "" && tb.id == "" {
		tb.id = id
	}
	if name != "" && tb.name == "" {
		tb.name = name
	}
	if args != "" {
		tb.args.WriteString(args)
	}
	if tb.closed {
		// The client has already been told this call is finished, and the wire
		// has no way to reopen a block: these arguments cannot be delivered. They
		// stay in the builder, so the block still holds everything the call
		// stated, and the divergence is a fragment the upstream sent across
		// another block's events — an interleaving the OpenAI wire permits and
		// Anthropic's does not (2026-09-27 audit, round 39, B-F7).
		log.Printf("oaica-gateway: tool call %q sent arguments after its block closed (%d bytes undelivered)", tb.name, len(args))
		return
	}
	if !tb.started {
		if tb.name == "" {
			// Nothing can open the block yet: content_block_start is the only
			// event that carries the name, and a start that goes out without one
			// can never be corrected. A fragment that states an id, or arguments,
			// is held (2026-09-27 audit, round 38, B-F2 / round 39, B-F8).
			return
		}
		b.closeOpen()
		b.flushHeldText()
		b.closeOpen() // the held text took a block of its own; the call follows it
		b.startToolBlock(tb)
	}
	if tb.emitted < tb.args.Len() {
		// Everything stated but not yet sent: the fragments that arrived while the
		// block was held are delivered now, as one delta, so no argument is lost
		// to the delay in the name.
		pending := tb.args.String()[tb.emitted:]
		tb.emitted = tb.args.Len()
		b.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": tb.index,
			"delta": map[string]any{"type": "input_json_delta", "partial_json": pending},
		})
	}
}

// startToolBlock emits the content_block_start for one tool call, once. The
// caller has closed whatever was open.
func (b *anthropicBridge) startToolBlock(tb *toolBlock) {
	if tb.started {
		return
	}
	tb.started = true
	tb.index = b.takeIdx()
	b.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": tb.index,
		"content_block": map[string]any{"type": "tool_use", "id": tb.id, "name": tb.name, "input": map[string]any{}},
	})
	b.cur = &openBlock{index: tb.index, tool: tb}
}

// openToolBlocks reports whether any tool call was ever opened for the client.
// A finish_reason of tool_calls over zero tool blocks claims the model asked for
// a tool it never named (2026-09-27 audit, round 39, B-F8).
func (b *anthropicBridge) openToolBlocks() int {
	n := 0
	for _, tb := range b.toolOrder {
		if tb.started {
			n++
		}
	}
	return n
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
	prompt := b.sse.inTok
	if prompt <= 0 {
		// The upstream never stated the prompt size. The hit it DID state is still
		// what it served from its prefix cache, and it is reported against the
		// prompt this gateway can work out for itself, not against a prompt of
		// zero: clamping it there told the client cache_read_input_tokens=0 for a
		// turn whose ledger row recorded the same hit, which is the round-38
		// disagreement surviving a different chunk order (2026-09-27 audit,
		// round 39, B-F4/C-F4). With no estimate either, the hit is all that is
		// known of the prompt and it is reported as it stands.
		prompt = b.promptEstimate
		if prompt <= 0 {
			prompt = cached
		}
	}
	if cached > prompt {
		cached = prompt
	}
	return prompt - cached, cached
}

// finishStream closes any open block and emits message_delta/message_stop.
// Called when the upstream stream ends WITHOUT a usage chunk too ([DONE] is
// the normal terminator) — the client must always get a well-formed end.
func (b *anthropicBridge) finishStream() {
	if !b.sse.startSent {
		return // upstream produced no events at all
	}
	b.closeOpen()
	b.flushHeldText()
	b.closeOpen()
	// A call the upstream never named is not a call the client can make: its
	// content_block_start could only go out with an empty name (the event that
	// carries the name has no second chance), so the block is never opened and
	// whatever arguments arrived are delivered as TEXT — the model's raw output,
	// which is at least readable — rather than as a tool_use Claude Code would
	// report as pending and could never run (2026-09-27 audit, round 39, B-F8).
	for _, tb := range b.toolOrder {
		if tb.started || tb.args.Len() == 0 {
			continue
		}
		log.Printf("oaica-gateway: a tool call the upstream never named (%d bytes of arguments) is relayed as text", tb.args.Len())
		b.openText()
		b.emit("content_block_delta", map[string]any{
			"type": "content_block_delta", "index": b.cur.index,
			"delta": map[string]any{"type": "text_delta", "text": tb.args.String()},
		})
		b.closeOpen()
	}
	if b.sse.upstreamErr != "" {
		// The upstream failed inside the stream. The turn ends with the error,
		// not with a fabricated stop_reason: the events already sent cannot be
		// un-sent, so the failure is delivered in the shape the Anthropic wire
		// defines for it, and the ledger row records 502 (noAnswer).
		b.emit("error", map[string]any{
			"type":  "error",
			"error": map[string]any{"type": "api_error", "message": b.sse.upstreamErr},
		})
		return
	}
	// input_tokens excludes what the upstream served from its prefix cache,
	// which is reported separately: the two partition prompt_tokens and sum to
	// it, so a client that adds them sees the real prompt and one that bills
	// input_tokens bills only the uncached part (2026-09-27 audit, round 23).
	fresh, cachedTok := b.promptSplit()
	outTok := b.sse.outTok
	if !b.sse.statedCompletion {
		// Nothing stated about the answer's size, so the client is told this
		// gateway's own count of what it relayed rather than a hard 0 — a turn
		// whose output_tokens reads 0 looks like a turn that said nothing
		// (2026-09-27 audit, round 39, C-F3).
		if est := b.outputEstimate(); est > 0 {
			outTok = est
		}
	}
	stop := stopReasonOpenAIToAnthropic(b.sse.stopMsg)
	if stop == "tool_use" && b.openToolBlocks() == 0 {
		// The upstream said it stopped to call a tool, and no call reached the
		// client (every one of them was unnamed, or there was none at all).
		// Reporting tool_use there told Claude Code to wait for a call it will
		// never receive, on a turn that is over (2026-09-27 audit, round 39,
		// B-F8).
		log.Printf("oaica-gateway: upstream stopped with tool_calls but named no call; reporting end_turn")
		stop = "end_turn"
	}
	b.emit("message_delta", map[string]any{
		"type":  "message_delta",
		"delta": map[string]any{"stop_reason": stop, "stop_sequence": nil},
		"usage": map[string]any{
			"input_tokens":            fresh,
			"cache_read_input_tokens": cachedTok,
			"output_tokens":           outTok,
		},
	})
	b.emit("message_stop", map[string]any{"type": "message_stop"})
}

// upstreamErrorSentence renders an error object the upstream sent as a stream
// frame into the sentence the client and the ledger are told. The message field
// is the explanation when it is there; the raw frame is the fallback, so a
// failure that arrives with an unfamiliar shape is still described rather than
// reduced to "the stream ended".
func upstreamErrorSentence(payload, message string) string {
	if strings.TrimSpace(message) != "" {
		// Redacted like every other error this gateway relays: the upstream's
		// own text can name the URL it failed to reach WITH its credentials in
		// it (an upstream that echoes back the request it rejected), and the
		// non-stream path redacts that same text (finalize → redactCredentialURLs).
		// The mid-stream path was the one place a credential reached the client
		// unredacted (2026-09-27 audit, round 39, B-F5).
		return redactCredentialURLs(strings.TrimSpace(message))
	}
	trimmed := strings.TrimSpace(payload)
	if len(trimmed) > 512 {
		trimmed = trimmed[:512] + "…"
	}
	return "upstream error frame: " + redactCredentialURLs(trimmed)
}

// adoptWholeStream translates a whole OpenAI completion that answered a STREAM
// request into the event sequence the client is waiting for. It is this leg's
// counterpart of the client proxy's adoptNonSSECompletion, and it exists for
// the same reason: an upstream (or a shim in front of one) that ignores
// stream:true answers with one application/json document, the frame reader
// finds no "data:" lines, and the turn was refused with "upstream returned an
// empty stream" and 502 — an answer the upstream had produced and billed was
// discarded and the ledger recorded the discard (2026-09-27 audit, round 38,
// B-F4). Returns true when the buffered body was adopted.
func (b *anthropicBridge) adoptWholeStream() bool {
	if !b.stream || b.sse.startSent {
		return false
	}
	resp, ok := b.bufferedCompletion()
	if !ok {
		return false
	}
	b.sse.startSent = true
	b.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": respID(resp.ID), "type": "message", "role": "assistant",
			"model": b.model, "content": []any{},
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	choice := resp.Choices[0]
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		b.textDelta(*choice.Message.Content)
	}
	if r := firstNonEmpty(choice.Message.Reasoning, choice.Message.ReasoningContent); r != "" {
		b.textDelta(r)
	}
	for i, tc := range choice.Message.ToolCalls {
		idx := i
		b.toolDelta(&idx, tc.ID, firstNonEmptyStr(tc.Function.Name, tc.Name),
			firstNonEmptyStr(tc.Function.Arguments, tc.Arguments))
	}
	if choice.FinishReason != "" {
		b.sse.stopMsg = choice.FinishReason
	}
	u := resp.Usage.nonNegative()
	if u.PromptTokens > 0 {
		b.sse.inTok = u.PromptTokens
		b.sse.statedPrompt = true
	}
	if u.CompletionTokens > 0 {
		b.sse.outTok = u.CompletionTokens
		b.sse.statedCompletion = true
	}
	b.sse.cacheTok = u.cachedTokens()
	return true
}

// bufferedCompletion reports whether the bytes this stream has buffered are a
// whole OpenAI completion, and returns it. The body is the union of both
// buffers: a one-line document with no trailing newline is still in tail,
// because the frame reader only ever completes a LINE. One parser for the two
// callers — adoption, and the status the ledger is told before adoption runs —
// so the two can never disagree about whether the turn has an answer
// (2026-09-27 audit, round 39, B-F3).
func (b *anthropicBridge) bufferedCompletion() (openAICompletion, bool) {
	var resp openAICompletion
	if b.sse.nonSSE.Len() == 0 && b.sse.tail.Len() == 0 {
		return resp, false
	}
	body := strings.TrimSpace(b.sse.nonSSE.String() + b.sse.tail.String())
	if body == "" {
		return resp, false
	}
	if json.Unmarshal([]byte(body), &resp) != nil || len(resp.Choices) == 0 {
		return resp, false
	}
	return resp, true
}

// oaDelta is one assistant turn's fields as the OpenAI wire states them, in
// either of its two shapes: a streamed `delta` and the `message` of a whole
// completion carry the same content, so the two are read by one struct and
// relayed by one function (2026-09-27 audit, round 39, B-F1).
type oaDelta struct {
	Content   *string `json:"content"`
	Reasoning *string `json:"reasoning"`
	// See the non-stream shape: the same field under DeepSeek's/vLLM's spelling.
	ReasoningContent *string      `json:"reasoning_content"`
	ToolCalls        []oaToolCall `json:"tool_calls"`
}

// oaToolCall is one tool call on the OpenAI wire. The index is a POINTER
// because it is optional here and a zero default filed every call of a stream
// into block 0 — the second call's id, name and arguments were folded into the
// first call's (2026-09-27 audit, round 36, B-F2). The identity is also read
// NESTED under `function`, which is the spelling this wire uses, with the flat
// one kept as a fallback: read at the top level only, every streamed tool call
// reached the client as {"name":"","input":{}} with stop_reason tool_use and no
// input_json_delta (round 36, B-F1).
type oaToolCall struct {
	Index    *int   `json:"index"`
	ID       string `json:"id"`
	Type     string `json:"type"`
	Function struct {
		Name      string `json:"name"`
		Arguments string `json:"arguments"`
	} `json:"function"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

// relayDelta writes one turn's content into the bridge: reasoning, then text,
// then its tool calls. Both wire shapes go through it.
func (b *anthropicBridge) relayDelta(d oaDelta) {
	if r := firstNonEmpty(d.Reasoning, d.ReasoningContent); r != "" {
		b.sse.outBytes += len(r)
		b.textDelta(r)
	}
	if d.Content != nil && *d.Content != "" {
		b.sse.outBytes += len(*d.Content)
		b.textDelta(*d.Content)
	}
	for _, tc := range d.ToolCalls {
		b.sse.outBytes += len(firstNonEmptyStr(tc.Function.Arguments, tc.Arguments))
		b.toolDelta(tc.Index, tc.ID,
			firstNonEmptyStr(tc.Function.Name, tc.Name),
			firstNonEmptyStr(tc.Function.Arguments, tc.Arguments))
	}
}

// outputEstimate is the answer's size in tokens when the upstream stated no
// completion count: the bytes relayed divided by four, the same unit the client
// leg reports for the same case (streamedText/4 + 1, with the +1 there for a
// non-empty one-byte answer).
func (b *anthropicBridge) outputEstimate() int {
	if b.sse.outBytes <= 0 {
		return 0
	}
	return b.sse.outBytes/4 + 1
}

// nothingRelayed reports whether the stream has said nothing yet — no block
// opened and no text held — which is the condition for adopting a whole
// completion that arrives inside a frame as the turn itself.
func (b *anthropicBridge) nothingRelayed() bool {
	return b.nextIdx == 0 && len(b.toolOrder) == 0 && b.heldText.Len() == 0
}

// errorFrameMessage reads the explanation out of a mid-stream error frame,
// which the wire states either as an object with a message or as a bare string.
// It returns "" when the frame states neither, and the caller then falls back to
// the raw frame (upstreamErrorSentence).
func errorFrameMessage(raw json.RawMessage) string {
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return strings.TrimSpace(s)
	}
	var obj struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(raw, &obj) == nil {
		return strings.TrimSpace(obj.Message)
	}
	return ""
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
				ID string `json:"id"`
				// The nested spelling is the one the OpenAI wire documents,
				// and the flat one is what several backends actually write.
				// The streaming reader in this same file reads both, so a
				// non-streaming client of the identical upstream bytes must
				// not be the only one that works (2026-09-27 audit, round 37,
				// B-F3).
				Function struct {
					Name      string `json:"name"`
					Arguments string `json:"arguments"`
				} `json:"function"`
				Name      string `json:"name"`
				Arguments string `json:"arguments"`
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
