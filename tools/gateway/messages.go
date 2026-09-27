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
	// The rewrite is why the path the CLIENT asked for is recorded on the
	// context first: the ledger row is built from r.URL.Path, so every
	// bridged turn used to be booked under the upstream's OpenAI path and
	// no per-path rate card or accounting split could tell an Anthropic-wire
	// turn from an OpenAI-wire one (2026-09-27 audit, round 45, B45-3).
	r = withInboundPath(r)
	r.URL.Path = "/v1/chat/completions"

	model, _ := req["model"].(string)
	bridge := newAnthropicBridge(w, stream, model)
	// The prompt size this request really carries, in the unit the rest of the
	// gateway measures prompts in: messagesBytes, which charges the body the
	// upstream will DECODE rather than the bytes Go spent encoding it, with each
	// inline image's base64 replaced by its allowance. It is what the client is
	// told when the upstream states no usage at all, rather than the 0 that left
	// a session's context meter still and its auto-compaction unfired
	// (2026-09-27 audit, round 39, C-F3). Measuring len(nb) put the OTHER two
	// measures' bug in this one: encoding/json writes `"`, `\`, `<`, `>` and the
	// control characters as two- or six-byte escapes that are not prompt, so a
	// markup-heavy turn (the shape Claude Code sends, and every code file in it)
	// was estimated up to 6x its real size and the client's input_tokens — the
	// field auto-compaction is sized on — read high from the first turn the
	// upstream stated nothing for (2026-09-27 audit, round 40, C40-4).
	bridge.promptEstimate = messagesBytes(openai) / 4
	g.completionHandler(bridge, r)
	bridge.finalize()
}

// anthropicToOpenAI converts a decoded Anthropic messages request into the
// OpenAI chat-completions map. Returns a non-empty error string on
// structurally impossible input (no messages, non-array content pieces we
// cannot represent).
func anthropicToOpenAI(req map[string]any, acceptsImages bool) (map[string]any, string) {
	out := map[string]any{"model": req["model"]}
	// max_tokens is REQUIRED on this wire, and the sibling converter's handler
	// refuses a body that omits it or states a non-positive one — this gateway
	// invented 4096 instead, so the same body was answered with a different
	// output cap depending on which leg served it, and a client that wrote
	// max_tokens:0 (asking for a refusal) got a 4096-token answer
	// (2026-09-27 audit, round 41, C41-8).
	mt, ok := req["max_tokens"]
	if !ok {
		return nil, "max_tokens is required"
	}
	// A JSON null and a JSON string are not a positive count, and the sibling
	// converter's typed field refuses both at decode (null becomes 0, a string
	// fails to decode) — so forwarding them was the same body answered two
	// ways, and the upstream read a null or a quoted number as an output cap.
	// The rule beside this one, on the sampling fields below, already states it:
	// a value that is not the shape this wire means is not a statement
	// (2026-09-27 audit, round 42, B42-6).
	//
	// Both numeric shapes are accepted. A client's number arrives as float64,
	// but a body this process built itself (the clamps below rewrite req[k]
	// with a Go int — see outputBudget's doc) carries a plain int, and reading
	// only float64 refused a body that says exactly what this wire means: the
	// same mistake the round-32 budget reader had already recorded
	// (2026-09-27 audit, round 42, verification).
	n, isNum := 0.0, false
	switch v := mt.(type) {
	case float64:
		n, isNum = v, true
	case int:
		n, isNum = float64(v), true
	case int64:
		n, isNum = float64(v), true
	}
	if !isNum {
		return nil, "max_tokens is required and must be a positive number"
	}
	if n <= 0 {
		return nil, "max_tokens is required and must be positive"
	}
	if n != float64(int64(n)) {
		// The sibling's field is an int: 16.5 does not decode, so the request is
		// refused there. Sending 16.5 upstream would be this leg inventing an
		// answer the other leg refuses.
		return nil, "max_tokens is required and must be a positive integer"
	}
	out["max_tokens"] = mt
	// top_k is carried by the sibling converter (options["top_k"]) and was
	// dropped here, so one body asked for one sampling distribution and got two
	// (2026-09-27 audit, round 41, C41-8). A JSON null is not a statement — the
	// sibling omits it — and forwarding it read as 0 on the backend, which is
	// greedy decoding for a client that sent `"temperature":null` to mean
	// "unset" (2026-09-27 audit, round 41, C41-14b).
	// Every field below is carried by a TYPED field on the sibling legs
	// (MessagesRequest.Temperature/TopP/TopK/Stream/StopSequences), so a body
	// whose JSON shape is not the one this wire means does not decode there and
	// the request is refused. Forwarding the value verbatim served the same
	// body 200 here with a string where a backend's schema expects a number (or
	// a bool where a float belongs), dropped a stated stop sequence on the floor
	// because the type assertion below did not match, and — for a non-bool
	// `stream` — answered an SSE request with a single JSON object. The rule is
	// the one already stated beside max_tokens: a value that is not the shape
	// this wire means is not a statement (2026-09-27 audit, round 42, B42-6;
	// round 49, B-F2).
	//
	// A JSON null is NOT that case: the sibling's pointer fields decode it to
	// nil and omit the field, which is what the `v != nil` test below does.
	for _, k := range []string{"temperature", "top_p"} {
		v, ok := req[k]
		if !ok || v == nil {
			continue
		}
		if _, ok := jsonNumber(v); !ok {
			return nil, k + " must be a number"
		}
		out[k] = v
	}
	if v, ok := req["top_k"]; ok && v != nil {
		// The sibling's TopK is an *int: a fractional value does not decode
		// into it, so it is refused rather than rounded here.
		f, ok := jsonNumber(v)
		if !ok {
			return nil, "top_k must be an integer"
		}
		if f != float64(int64(f)) {
			return nil, "top_k must be an integer"
		}
		out["top_k"] = v
	}
	if v, ok := req["stream"]; ok && v != nil {
		// The handler reads this field with a type assertion and a failed one
		// reads as false, so a body asking to stream by any other spelling was
		// answered as one non-streaming object (2026-09-27 audit, round 49).
		if _, isBool := v.(bool); !isBool {
			return nil, "stream must be a boolean"
		}
		out["stream"] = v
	}
	if v, ok := req["stop_sequences"]; ok && v != nil {
		ss, isArr := v.([]any)
		if !isArr {
			return nil, "stop_sequences must be an array of strings"
		}
		for _, s := range ss {
			if _, isStr := s.(string); !isStr {
				return nil, "stop_sequences must be an array of strings"
			}
		}
		if len(ss) > 0 {
			out["stop"] = ss
		}
	}
	// The thinking control, in the spelling this product's own llama-server
	// takes for the same control (llm.llamaServerChatTemplateKwargs): the
	// sibling converter maps thinking.enabled/disabled to Think and an effort
	// level to Think=level, and that field becomes exactly these kwargs on the
	// wire the fleet's backends read. This gateway forwarded NEITHER, so a
	// client that asked for thinking OFF had it defaulted on and paid for
	// reasoning it had ruled out, and a client that asked for a level got the
	// model's own default — the same body answered two ways depending on which
	// leg served the turn (2026-09-27 audit, round 41, C41-3). Only a control
	// the client actually stated is sent: no thinking field and no effort
	// means no kwargs at all, as the sibling sends no Think.
	normalizedEffort := ""
	if oc, ok := req["output_config"].(map[string]any); ok {
		if e, ok := oc["effort"].(string); ok {
			normalizedEffort = strings.ToLower(strings.TrimSpace(e))
			if normalizedEffort == "xhigh" {
				normalizedEffort = "high"
			}
		}
	}
	thinkState := 0 // 0 = unstated, 1 = enabled, -1 = disabled
	if th, present := req["thinking"]; present && th != nil {
		thm, ok := th.(map[string]any)
		if !ok {
			// A control of the wrong shape was dropped in silence and the body
			// served: the sibling legs decode `thinking` into a typed struct and
			// fail the request on a bool, a string or a number, so one body was
			// a 400 there and a 200 here with the control ignored
			// (2026-09-27 audit, round 50). An explicit `null` is NOT this case
			// — that is a nil pointer on both siblings and stays unstated.
			return nil, "thinking must be an object"
		}
		if raw, present := thm["type"]; present && raw != nil {
			t, ok := raw.(string)
			if !ok {
				return nil, "thinking.type must be a string"
			}
			switch t {
			case "enabled":
				thinkState = 1
			case "disabled":
				thinkState = -1
			}
		}
	}
	effortLevel := ""
	switch normalizedEffort {
	case "high", "medium", "low", "max":
		effortLevel = normalizedEffort
	}
	explicitThinking := thinkState != 0
	if thinkState == 0 && effortLevel != "" {
		// An effort with no explicit switch turns thinking ON, which is what
		// the sibling's Think=level does (a string value is a true value).
		thinkState = 1
	}
	if thinkState != 0 {
		kwargs := map[string]any{"enable_thinking": thinkState == 1}
		if !explicitThinking && effortLevel != "" {
			// The effort rides the level only when no switch was stated: the
			// sibling's thinking field takes precedence over output_config, so
			// a body carrying both is a switch and no level.
			kwargs["reasoning_effort"] = effortLevel
		}
		out["chat_template_kwargs"] = kwargs
	}
	var msgs []map[string]any
	rawMsgs, _ := req["messages"].([]any)
	if len(rawMsgs) == 0 {
		return nil, "messages is required"
	}
	for _, rm := range rawMsgs {
		m, _ := rm.(map[string]any)
		if m == nil {
			// The sibling converter's decode fails on the same element and its
			// handler 400s, so skipping it here served a turn with one message
			// missing — the model was asked about a conversation it was not
			// given, and the client read 200 (2026-09-27 audit, round 41,
			// C41-14d).
			return nil, "messages element is not an object"
		}
		// Roles are lower-cased as the sibling converter lower-cases them: the
		// same body then reaches two backends with two spellings of its role,
		// and a backend that matches exactly rejects one of them
		// (2026-09-27 audit, round 41, C41-14a).
		role, _ := m["role"].(string)
		role = strings.ToLower(role)
		// An ABSENT content key is not the empty content. Both sibling legs
		// fail to decode a message that states no content at all (their
		// un-marshal of the missing raw field errors), where this leg read the
		// nil as `null` and served the turn with content:"" — one body was a
		// 400 on two legs and a metered 200 here (2026-09-27 audit, round 50).
		// An explicit `null` is the empty content on all three.
		content, present := m["content"]
		if !present {
			return nil, "message content is required"
		}
		converted, convErr := contentBlocksToOpenAI(role, content, acceptsImages)
		if convErr != "" {
			return nil, convErr
		}
		msgs = append(msgs, converted...)
	}
	// out["messages"] may already hold the system message; append.
	if sys := systemToMessage(req["system"]); sys != nil {
		msgs = append([]map[string]any{sys}, msgs...)
	}
	out["messages"] = normalizeSystemFirst(msgs)
	// A conversation that reaches the wire holding nothing asks the model for
	// nothing, and the backend either refuses the request or answers it without
	// a generation while the ledger books a metered success. Two ways to get
	// here: a whitespace-only system message that WAS the whole conversation and
	// was removed by the hoist above (the converter's keep-a-turn ran before it,
	// so the turn existed and the hoist took it), or a conversation that was
	// contentless to begin with. The sibling legs both keep one turn here — the
	// local leg substitutes {"role":"user"} — so this one does too, and the
	// question is asked of CONTENT rather than of the number of messages for the
	// same reason the local leg asks it that way: a list of blank messages is
	// empty whatever its length (2026-09-27 audit, round 44, B44-4; round 43,
	// C43-1).
	if !wireMessagesCarryContent(out["messages"]) {
		out["messages"] = []map[string]any{{"role": "user", "content": ""}}
	}

	// tool_choice is read BEFORE the tools, because "none" means the model is
	// given no tool surface at all: the sibling converter drops every tool for
	// that type, and this gateway forwarded all of them with no choice field,
	// so the same body had the model calling tools a client had just said not
	// to call (2026-09-27 audit, round 41, C41-2). Dropping the definitions
	// rather than stating "none" is the shape that backend validates without
	// objection, and it is the shape the other leg sends.
	choiceType := ""
	var tc map[string]any
	if raw, present := req["tool_choice"]; present && raw != nil {
		c, ok := raw.(map[string]any)
		if !ok {
			// Same rule as `thinking` above: the sibling legs decode this into a
			// typed struct and 400 a string, a number or an array, where this
			// leg read nothing and forwarded the request as if the control had
			// not been stated (2026-09-27 audit, round 50). `null` is a nil
			// pointer on both siblings and stays unstated.
			return nil, "tool_choice must be an object"
		}
		tc = c
		if raw, present := c["type"]; present && raw != nil {
			s, ok := raw.(string)
			if !ok {
				return nil, "tool_choice.type must be a string"
			}
			choiceType = s
		}
		if raw, present := c["name"]; present && raw != nil {
			if _, ok := raw.(string); !ok {
				return nil, "tool_choice.name must be a string"
			}
		}
	}
	// The type is normalized exactly as the sibling normalizes it before it
	// reads it (EqualFold over a trimmed value): matching the literal "none"
	// let `"NONE"` and `" none "` forward the whole tool surface, so the body
	// that says "do not call a tool" reached the model with every tool defined
	// on this leg and none on the other (2026-09-27 audit, round 42, B42-4).
	choiceType = strings.ToLower(strings.TrimSpace(choiceType))
	if choiceType != "none" {
		if tools, ok := req["tools"].([]any); ok && len(tools) > 0 {
			// The built-in web_search tool is a SERVER tool, and the sibling
			// converter rewrites it into the `web_search` function its backends
			// actually call — with a required `query` — and drops a client tool
			// of the same name from the same request so the model's call is not
			// ambiguous. This leg forwarded the definition as written: a
			// function with a name and nothing else, so one body handed the
			// model a query-less tool on one leg and a query-taking one on the
			// other, and a request carrying both put two identically-named
			// functions in one tools array, which a backend resolves arbitrarily
			// or rejects (2026-09-27 audit, round 49, B-F3).
			hasBuiltinWebSearch := false
			for _, t := range tools {
				if tm, ok := t.(map[string]any); ok {
					if ty, ok := tm["type"].(string); ok && strings.HasPrefix(ty, "web_search") {
						hasBuiltinWebSearch = true
						break
					}
				}
			}
			var oai []map[string]any
			for i, t := range tools {
				tm, _ := t.(map[string]any)
				if tm == nil {
					// The sibling's typed list refuses the whole request on the
					// same element, so skipping it served a turn with a tool
					// missing — the model was given a surface the client did not
					// send (2026-09-27 audit, round 42, B42-8).
					return nil, "tools element is not an object (index " + strconv.Itoa(i) + ")"
				}
				toolType, _ := tm["type"].(string)
				if strings.HasPrefix(toolType, "web_search") {
					oai = append(oai, map[string]any{"type": "function", "function": webSearchToolFunction()})
					continue
				}
				name, _ := tm["name"].(string)
				if hasBuiltinWebSearch && name == "web_search" {
					continue
				}
				fn := map[string]any{"name": name}
				if d, ok := tm["description"]; ok {
					fn["description"] = d
				}
				// The sibling carries parameters as a VALUE, not a pointer: a
				// tool that states no schema still puts one on the wire (the
				// zero value, `{"type":"","properties":null}`), and one whose
				// schema is not an object fails the whole request at decode.
				// Omitting the key sent a function with no parameters where the
				// sibling sent an empty schema, and forwarding an array or a
				// string put a shape no backend's validator accepts on the wire
				// for a request the other legs refuse (2026-09-27 audit, round
				// 49, verification of the tool surface).
				switch sc, ok := tm["input_schema"]; {
				case !ok || sc == nil:
					fn["parameters"] = map[string]any{"type": "", "properties": nil}
				default:
					if _, isObj := sc.(map[string]any); !isObj {
						return nil, "invalid input_schema for tool " + strconv.Quote(name) + ": not a JSON object"
					}
					fn["parameters"] = sc
				}
				oai = append(oai, map[string]any{"type": "function", "function": fn})
			}
			if len(oai) > 0 {
				out["tools"] = oai
			}
		}
	}
	switch choiceType {
	case "auto":
		out["tool_choice"] = "auto"
	case "any":
		out["tool_choice"] = "required"
	case "tool":
		name, _ := tc["name"].(string)
		out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
	}
	return out, ""
}

// webSearchToolFunction is the function the sibling converter rewrites an
// Anthropic built-in web_search tool into — the same name, the same
// description and the same required `query` property it builds in convertTool.
// The model is asked to call a tool by this signature; two legs offering two
// signatures for one body is two different asks of the same model.
func webSearchToolFunction() map[string]any {
	return map[string]any{
		"name":        "web_search",
		"description": "Search the web for current information. Use this to find up-to-date information about any topic.",
		"parameters": map[string]any{
			"type":     "object",
			"required": []any{"query"},
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "The search query to look up on the web",
				},
			},
		},
	}
}

// ensureTrailingNewline returns s with a newline appended when it does not
// already end in one, and s unchanged when it is empty. The sibling converter
// appends it to the multi-line parts it builds — a document's text and a search
// result's label — so the part's last sentence is not glued to the next part's
// first when flushText joins them.
func ensureTrailingNewline(s string) string {
	if s == "" || strings.HasSuffix(s, "\n") {
		return s
	}
	return s + "\n"
}

// wireMessagesCarryContent reports whether any message about to reach the
// upstream asks the model for anything at all — text, a part (an image), a
// call, or a tool result. It is the mirror of the local leg's
// anyMessageCarriesContent, asked of the same five payloads in this leg's
// spelling, and it is asked of the conversation AFTER the system hoist because
// the hoist is one of the two ways a non-empty list becomes an empty one.
//
// A text is content whatever it says, whitespace included: a prompt of a
// hundred thousand newlines is a prompt, and trimming it here threw the turn
// away on this leg exactly as it did on the local one — the upstream was asked
// a bare empty user turn instead of the question the client wrote
// (2026-09-27 audit, round 44).
func wireMessagesCarryContent(v any) bool {
	messages, _ := v.([]map[string]any)
	for _, m := range messages {
		if s, ok := m["content"].(string); ok && s != "" {
			return true
		}
		switch parts := m["content"].(type) {
		case []map[string]any:
			if len(parts) > 0 {
				return true
			}
		case []any:
			if len(parts) > 0 {
				return true
			}
		}
		if tcs, ok := m["tool_calls"].([]map[string]any); ok && len(tcs) > 0 {
			return true
		}
		if tcs, ok := m["tool_calls"].([]any); ok && len(tcs) > 0 {
			return true
		}
		if id, ok := m["tool_call_id"].(string); ok && id != "" {
			return true
		}
	}
	return false
}

// normalizeSystemFirst hoists every system message to the front of the
// conversation as one message — this bridge's half of a rule the client-side
// proxy states in full (cmd/launch's normalizeSystemFirst) and the local leg
// applies too (anthropic.FromMessagesRequest). A client that sends a top-level
// `system` beside a mid-conversation system message reaches the backend as
// [system, user, system], and a strict chat template raises on that
// (KAT-Coder's apex GGUF answers "System message must be at the beginning"):
// the same body was answered here by the template's 500 and, through the
// proxy, by an answer (2026-09-27 audit, round 42, C42-5).
//
// An already-ordered conversation is returned untouched, byte for byte. The
// rewrite is only needed for a system message that arrives AFTER a non-system
// one, and applying it unconditionally re-rendered the common case — several
// leading system messages concatenated into one string, so the prompt differed
// from the one the client sent and from the previous turn's, defeating any
// prefix cache keyed on the rendered text.
//
// A system message carrying anything but text — a part array, a tool call, a
// tool-result id, any key but role and content — cannot be merged into that one
// string without dropping what it carries, so a conversation holding one is
// returned exactly as it arrived.
//
// That check is over the WHOLE conversation, so the scan below has no early
// exit: stopping at the first system message that arrived after a non-system
// one — the rewrite was already decided by then — left a later carrier
// unguarded, and the rewrite merged it into a bare string and dropped what it
// carried (2026-09-27 audit, round 43, A43-1).
func normalizeSystemFirst(msgs []map[string]any) []map[string]any {
	ordered := true
	blank := false
	seenNonSystem := false
	for _, m := range msgs {
		if role, _ := m["role"].(string); role == "system" {
			if !systemMessageIsTextOnly(m) {
				return msgs
			}
			if seenNonSystem {
				ordered = false
			}
			if s, _ := m["content"].(string); strings.TrimSpace(s) == "" {
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
	rest := make([]map[string]any, 0, len(msgs))
	for _, m := range msgs {
		if role, _ := m["role"].(string); role == "system" {
			if s, _ := m["content"].(string); strings.TrimSpace(s) != "" {
				system = append(system, s)
			}
			continue
		}
		rest = append(rest, m)
	}
	if len(system) == 0 {
		return rest // all system messages were blank — drop them
	}
	out := make([]map[string]any, 0, len(rest)+1)
	out = append(out, map[string]any{"role": "system", "content": strings.Join(system, "\n\n")})
	return append(out, rest...)
}

// systemMessageIsTextOnly reports whether a system message carries nothing but
// its text: role and a string content, and no other key at all. Only such a
// message can be merged into the single leading system message
// normalizeSystemFirst builds.
func systemMessageIsTextOnly(m map[string]any) bool {
	if _, ok := m["content"].(string); !ok {
		return false
	}
	for k := range m {
		switch k {
		case "role", "content":
		default:
			return false
		}
	}
	return true
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
			// Blocks are joined with a BLANK LINE, the separator the sibling
			// converter uses for the same array (anthropic.FromMessagesRequest,
			// and normalizeSystemFirst on the other wire). A system array is how
			// an appended instruction arrives; one newline glued it to the
			// preceding one whenever that did not end in punctuation
			// (2026-09-27 audit, round 41, C41-11).
			return map[string]any{"role": "system", "content": strings.Join(parts, "\n\n")}
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
//
// The second return is the refusal the client leg already makes. A non-object
// element inside the array is not a content block — the other leg 400s the body
// — and marshalling it into the prompt here made one body two different prompts
// depending on which leg served it ("1\n\"x\"\nafter" against "1\nafter"),
// which is the divergence round 39's C-F6 closed on the client side only
// (2026-09-27 audit, round 40, A40-4). One rule on both legs: refuse it, and
// name what was refused.
func toolResultText(content any, carryImages bool) (string, string) {
	switch c := content.(type) {
	case nil:
		return "", ""
	case string:
		return c, ""
	case []any:
		parts := make([]string, 0, len(c))
		for _, cb := range c {
			bm, ok := cb.(map[string]any)
			if !ok {
				return "", "tool_result content holds an element that is not a JSON object"
			}
			switch bm["type"] {
			case "text":
				// An empty text block is no text: joined as an empty part it
				// put a blank line in front of the real content, where the
				// client leg's copy skips it (2026-09-27 audit, round 39,
				// A-F6). A text that is not a string is no text either — the
				// sibling's reader writes nothing for it and the describer
				// below would have pasted `{"text":42,"type":"text"}` into the
				// prompt as though the tool had returned that JSON
				// (2026-09-27 audit, round 42, B42-7/C42-3).
				s, ok := bm["text"].(string)
				if !ok {
					continue
				}
				if s != "" {
					parts = append(parts, s)
				}
				continue
			case "image":
				if carryImages {
					continue
				}
			}
			parts = append(parts, describeBlock(cb))
		}
		return strings.Join(parts, "\n"), ""
	default:
		return describeBlock(c), ""
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
//
// The third return is the refusal a payload the client leg refuses earns here
// too — see base64ImagePayload (2026-09-27 audit, round 40, A40-5).
func toolResultImageParts(content any) (parts []any, notices []string, refuse string) {
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
				// A base64 image with no data is not an image this wire can
				// express or describe: the sibling's resolveImageSource refuses
				// the whole request in words ("base64 with no data"), and the
				// local leg's comment says this leg refuses it too while this
				// leg answered 200 with a placeholder the model read as the
				// tool's answer (2026-09-27 audit, round 42, C42-4).
				return nil, nil, "invalid image source: base64 with no data"
			}
			if _, refuse := base64ImagePayload(data); refuse != "" {
				return nil, nil, refuse
			}
			media, _ := src["media_type"].(string)
			parts = append(parts, map[string]any{
				"type": "image_url",
				"image_url": map[string]any{
					"url": "data:" + inlineImageMediaType(media, data) + ";base64," + data,
				}})
		default:
			// Same rule for a source type neither leg can carry: the sibling
			// refuses it outright ("invalid image source type: weird. Only
			// base64 images are supported.") rather than passing a notice to
			// the model as the tool's answer (2026-09-27 audit, round 42,
			// C42-4).
			return nil, nil, "invalid image source type: " + st + ". Only base64 images are supported."
		}
	}
	return parts, notices, ""
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

// base64ImagePayload decodes an inline base64 image source and refuses the two
// payloads the client leg refuses, in its own words: a string that is not
// base64 at all, and a payload that decodes to a URL rather than to image
// bytes. This leg forwarded both as a data URI — an undecodable one reaches the
// upstream as an image it fails on, and a base64-of-a-URL is a payload the
// client never sent as bytes, which the other leg has refused in words since
// round 39 (2026-09-27 audit, round 40, A40-5). "" means the payload is image
// bytes; the second return is the refusal to hand the client.
func base64ImagePayload(data string) ([]byte, string) {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, "invalid base64 image data"
	}
	if imageURLText(string(decoded)) {
		return nil, "invalid image source: base64 data decodes to a URL, not image bytes"
	}
	return decoded, ""
}

// imageURLText reports whether s reads as a URL: "//" (protocol-relative),
// "data:" case-insensitively, or an alpha scheme followed by "://".
//
// The third copy of this predicate in the tree, deliberately: this module is
// built and deployed on its own (its go.mod has no ollama dependency), so it
// cannot import the converter's — the same reason the inline-image allowance is
// spelled out in three places. The two legs must answer "is this a URL" the
// same way for the same body, which is what the tests on both sides pin.
func imageURLText(s string) bool {
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
			if !ok {
				// A passage list holds BLOCKS. A bare element is not one, and
				// describing it put the literal `"x"` — quotes and all — in the
				// prompt as though the search had returned that passage. The
				// sibling converter skips it, and both legs now read the list
				// the same way (2026-09-27 audit, round 41, C41-10).
				continue
			}
			if bm["type"] == "text" {
				// An empty passage is no passage: it fell through to the
				// describer and put `{"text":"","type":"text"}` in the prompt as
				// though the search had returned it, which the sibling converter
				// has skipped since round 38 (2026-09-27 audit, round 39,
				// C-F7/A-F6). A passage whose text is not a STRING is no
				// passage either, and the sibling skips it in both places it
				// reads a passage list — a null or a number was described here,
				// so the model read `{"text":null,"type":"text"}` as a search
				// hit on this leg and nothing at all on the others
				// (2026-09-27 audit, round 42, B42-7/C42-3).
				s, ok := bm["text"].(string)
				if !ok {
					continue
				}
				if s != "" {
					parts = append(parts, s)
				}
				continue
			}
			if desc := describeBlock(cb); desc != "" {
				parts = append(parts, desc)
			}
		}
		return strings.Join(parts, "\n")
	default:
		// A block OBJECT is described — the sibling's reader has no arm for a
		// passage list stated as an object and drops it, which is the one
		// divergence this leg keeps in the model's favour. A bare scalar is not
		// a block and not a passage: describing it put the number or the boolean
		// in the prompt as though the search had returned it, where the sibling
		// writes nothing for a value of that shape (2026-09-27 audit, round 42,
		// C42-3).
		if bm, ok := c.(map[string]any); ok {
			return describeBlock(bm)
		}
		return ""
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
		// A content that is neither a string nor a block array is a shape this
		// wire cannot carry, and the sibling's typed field refuses it at decode
		// — an object or a number was answered 200 here with content:"" , so the
		// model was handed an empty user turn and the client read a successful
		// answer to a prompt it never sent. `null` is NOT this case: the sibling
		// decodes it to empty content too, which is what the caller does
		// (2026-09-27 audit, round 42, B42-5/C42-6).
		if content == nil {
			return []map[string]any{{"role": role, "content": ""}}, ""
		}
		return nil, "message content is neither a string nor an array of content blocks"
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
	// openToolMsg opens the assistant message a call rides on, once.
	openToolMsg := func() {
		if toolMsg < 0 {
			out = append(out, map[string]any{"role": role, "content": "", "tool_calls": []map[string]any{}})
			toolMsg = len(out) - 1
		}
	}
	// putContent places a delivered block of text (or image parts) on the wire.
	// An assistant turn's text and its calls are ONE message on this wire —
	// `content` and `tool_calls` are two fields of the same assistant message,
	// and that is the shape both other legs send and the shape the client sent
	// its blocks in. Flushing the text first opened a SECOND assistant message
	// per turn (one carrying prose, one carrying the calls with an empty
	// content), so a turn the client wrote as one was answered as two, and the
	// same body reached the model differently depending on which leg served it
	// (2026-09-27 audit, round 49, B-F4). Every other role keeps the order it
	// had: its text goes out as its own message first.
	putContent := func(content any) {
		if role != "assistant" || toolMsg < 0 {
			out = append(out, map[string]any{"role": role, "content": content})
			return
		}
		if s, ok := content.(string); ok {
			if cur, ok := out[toolMsg]["content"].(string); ok {
				if cur == "" {
					out[toolMsg]["content"] = s
				} else {
					// The separator between two text blocks of one turn is the
					// blank line the rest of this file joins them with.
					out[toolMsg]["content"] = cur + "\n\n" + s
				}
				return
			}
		}
		// Something other than a plain string — image parts, or text folded
		// onto an already-arrayed content: keep the parts, with whatever string
		// was already there in front of them.
		// The message's content is read back with the type the writer STORES.
		// This branch is reached only after the string arm above declined, so
		// what is on the message is either an empty string or the parts array
		// putContent itself wrote — and that array is `[]any`. Asserting
		// `[]map[string]any` read nothing, so the parts already accumulated
		// were dropped and the new text was appended to an empty array as a
		// BARE STRING, which is not a part any backend reads: an assistant
		// turn of text, an image and more text after a call reached the model
		// as the last text alone (2026-09-27 audit, round 50, B-F1).
		arr := []any{}
		if cur, ok := out[toolMsg]["content"].(string); ok && cur != "" {
			arr = append(arr, map[string]any{"type": "text", "text": cur})
		} else if cur, ok := out[toolMsg]["content"].([]any); ok {
			arr = append(arr, cur...)
		}
		switch c := content.(type) {
		case []map[string]any:
			for _, p := range c {
				arr = append(arr, p)
			}
		case string:
			// Once the message carries PARTS, a string is a text part: a bare
			// string inside a parts array is not a block on this wire, which
			// is what putContent wrote for every text block following a call
			// on a message that already held parts.
			arr = append(arr, map[string]any{"type": "text", "text": c})
		default:
			arr = append(arr, content)
		}
		out[toolMsg]["content"] = arr
	}
	flushText := func() {
		if len(parts) == 0 {
			return
		}
		// A message of text blocks alone is assembled into ONE string, with the
		// blank line the sibling converter puts between them: sent as separate
		// text parts, each backend chose its own separator for the same prompt
		// (vLLM joins with a newline, and a reader that concatenates parts
		// would glue two sentences together — "first linesecond line"), so one
		// body became two prompts depending on which leg served it and which
		// backend it reached (2026-09-27 audit, round 41, C41-12). Parts are
		// kept only when something other than text rides the message — an image
		// part has to stay a part.
		allText := true
		for _, p := range parts {
			if p["type"] != "text" {
				allText = false
				break
			}
		}
		if allText {
			texts := make([]string, 0, len(parts))
			for _, p := range parts {
				if t, ok := p["text"].(string); ok {
					texts = append(texts, t)
				}
			}
			putContent(strings.Join(texts, "\n\n"))
			parts = nil
			return
		}
		putContent(parts)
		parts = nil
	}
	for _, b := range blocks {
		bm, ok := b.(map[string]any)
		if !ok {
			// A content array holds BLOCKS. The sibling converter's decode fails
			// on an element of any other shape and its handler 400s; skipping it
			// here answered a turn with that element missing from the prompt, so
			// the model was asked about text the client had sent and never
			// received, and the client read a 200 (2026-09-27 audit, round 41,
			// C41-5). Refused by name rather than dropped in silence, which is
			// the rule the rest of this gateway keeps.
			return nil, "message content element is not an object"
		}
		switch t, _ := bm["type"].(string); t {
		case "text":
			// The field is a string on this wire, and the sibling legs decode it
			// into one — anthropic.ContentBlock.Text is a *string, so the same
			// body is a 400 there. Reading it with a comma-ok and ignoring the
			// miss wrote an empty string into the prompt instead: the model was
			// asked about a turn whose text the client had sent and this gateway
			// had dropped, the client read a 200, and one body got two answers
			// depending on which leg served it (2026-09-27 audit, round 43,
			// B43-2). Absent or null is not the same thing — that is a text block
			// with no text, which the sibling legs keep as an empty block.
			if raw, present := bm["text"]; present && raw != nil {
				txt, isStr := raw.(string)
				if !isStr {
					return nil, "message content text is not a string"
				}
				parts = append(parts, map[string]any{"type": "text", "text": txt})
				break
			}
			parts = append(parts, map[string]any{"type": "text", "text": ""})
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
				if !imageURLText(u) {
					// A source declared as a url carries text a reader must be
					// able to FETCH; text with no scheme is not fetchable, and
					// forwarding it told the backend to go and get an address
					// that names no protocol. The sibling converter refuses the
					// same block in words, and the two legs answer one body one
					// way (2026-09-27 audit, round 41, A41-5).
					return nil, `image block with source.type "url" whose url is not url-shaped`
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
				if _, refuse := base64ImagePayload(data); refuse != "" {
					return nil, refuse
				}
				parts = append(parts, map[string]any{
					"type":      "image_url",
					"image_url": map[string]any{"url": "data:" + inlineImageMediaType(media, data) + ";base64," + data},
				})
			default:
				return nil, fmt.Sprintf("image source.type %q cannot be represented on the OpenAI wire", st)
			}
		case "tool_use", "server_tool_use":
			// The call's message is opened BEFORE the pending text is flushed,
			// so an assistant's text folds onto the turn it belongs to (see
			// putContent). A user message keeps the order it had.
			if role == "assistant" {
				openToolMsg()
			}
			flushText()
			openToolMsg()
			id, _ := bm["id"].(string)
			name, _ := bm["name"].(string)
			// A tool_use without its id or its name is a call the client cannot
			// be answered about: the sibling converter refuses both in words
			// ("tool_use block missing required 'id' field"), where this leg put
			// `"id":""` on the wire and the backend answered a malformed call —
			// the client then read a tool result for a call it never made
			// (2026-09-27 audit, round 41, C41-6). server_tool_use is exempt:
			// the sibling carries it with whatever fields it has.
			if t == "tool_use" {
				if id == "" {
					return nil, "tool_use block missing required 'id' field"
				}
				if name == "" {
					return nil, "tool_use block missing required 'name' field"
				}
				// The sibling decodes this field into a JSON object and fails
				// the whole request on anything else; re-marshalling it here
				// wrote the raw string back as a JSON string, so a call whose
				// arguments the client leg refuses reached the model here with
				// arguments no tool can parse (2026-09-27 audit, round 42,
				// C42-6).
				if in, present := bm["input"]; present && in != nil {
					if _, isObj := in.(map[string]any); !isObj {
						return nil, "tool_use block 'input' must be a JSON object"
					}
				}
			}
			// An input the block does not state at all is a call made with no
			// arguments, and the canonical encoding of that on this wire is an
			// empty object: re-marshalling the missing key wrote the literal
			// string "null" into the arguments, and leg 1's own decoder (a
			// nil-map ToolCallFunctionArguments that marshals to {}) and the
			// client leg both say {} for the same body — an upstream tool parser
			// doing json.loads got None from one leg and {} from the other two
			// (2026-09-27 audit, round 43, C43-2). An input stated AS null is a
			// different body and stays null on all three.
			in := bm["input"]
			if _, present := bm["input"]; !present {
				in = map[string]any{}
			}
			args, _ := json.Marshal(in)
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
			var content string
			if t == "web_search_tool_result" {
				content = webSearchResultText(bm["content"])
			} else {
				var refuse string
				content, refuse = toolResultText(bm["content"], carryImages)
				if refuse != "" {
					return nil, refuse
				}
			}
			var dropped []string
			if carryImages {
				// The images this model can be shown, and a sentence for each one
				// it cannot: an image that yields no part is stated in the text,
				// not erased (2026-09-27 audit, round 39, C-F2).
				var parts []any
				var refuse string
				parts, dropped, refuse = toolResultImageParts(bm["content"])
				if refuse != "" {
					return nil, refuse
				}
				// The sentences are folded into the text HERE, before the parts
				// are built, so a tool_result that carries text AND one showable
				// image AND one unshowable one states the omission too. Folding
				// them only when the text was empty lost the notice for exactly
				// the mixed message, which is the erasure C-F2 forbids
				// (2026-09-27 audit, round 40, B40-5).
				if len(dropped) > 0 {
					if content != "" {
						content += "\n"
					}
					content += strings.Join(dropped, "\n")
					dropped = nil
				}
				if len(parts) > 0 {
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
			// The trailing newline the sibling converter adds, for the reason it
			// states in full: without it the document's last sentence is glued
			// to the next block's first ("read the file." + "Now run the tests."
			// = "...file.Now run the tests."), and the same client body became
			// two different prompts depending on which leg served it
			// (2026-09-27 audit, round 44, C44-6).
			parts = append(parts, map[string]any{"type": "text", "text": ensureTrailingNewline(data)})
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
				// Same trailing newline as the document case above, and for the
				// same reason: a search result's text is the sibling converter's
				// other multi-line part, and it appends one there too
				// (2026-09-27 audit, round 44, C44-6).
				parts = append(parts, map[string]any{"type": "text", "text": ensureTrailingNewline(strings.Join(label, "\n"))})
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
	// mintedIDs counts the calls this bridge has had to number itself, keyed by
	// each call's own identity (name and arguments), so the second and later
	// occurrence of one identity takes a distinct id — the rule the local leg's
	// converter applies to the same wire (anthropic.go, seenInCall). See
	// mintedToolCallID.
	mintedIDs map[string]int
	// grantedIDs is every id this bridge has given to a call — minted here, or
	// stated by the upstream and taken by a block. A minted id is a pure
	// function of the call's own identity, so without this set a call the
	// upstream NAMED could collide with the id minted for an earlier id-less
	// call, and two blocks under one id cannot be answered separately
	// (2026-09-27 audit, round 46, G45-1 and G45-5).
	grantedIDs map[string]bool
	// reservedIDs is every id a whole NON-stream list states, recorded before
	// any call in it is numbered: a mint walks the list in order, so without
	// this an id-less call early in the list took the id a later call states,
	// and the two legs that answer the same list keep the stated id and
	// renumber the minted one. Only the mint consults it — a stated id is taken
	// by its own call as before (2026-09-27 audit, round 48, B-F4).
	reservedIDs map[string]bool
	// statedIDOwner records, per upstream-stated id, the call identity it was
	// first stated for, so a second, DIFFERENT call under the same id is
	// numbered here instead of being folded into the first (the rule leg 1's
	// seenStatedID and the client leg's parser apply — round 45, A45-3).
	statedIDOwner map[string]string
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
	// statedID is whether id came from the upstream. A block this bridge had to
	// open before the upstream named its id carries a MINTED one, and an id the
	// upstream states afterwards is that same call's own id rather than the
	// introduction of another call — the distinction toolKey's merge arm needs
	// (2026-09-27 audit, round 45, B45-1).
	statedID bool
	// needsMint is whether the id is one this bridge has to number itself and
	// has not numbered yet. The mint waits for the block to open, because it
	// hashes the call's own arguments and those are not all in when the
	// fragment that names the call arrives (see toolDelta).
	needsMint bool
	// merged is whether this block turned out to be a restatement of a call
	// another block already carries — the same name and the same arguments
	// under the same stated id. It is not a call: it never opens, and the
	// fragments that follow it are dropped (see startToolBlock).
	merged bool
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
	inTok int
	// cacheTok is the hit as the upstream stated it, resolved by cacheHit().
	// The two spellings are kept apart rather than in one last-writer-wins
	// slot, because that is how the ledger's recorder resolves them (details
	// over sibling, each keeping its own last positive statement): a stream
	// that stated the hit as details in one chunk and in the sibling spelling
	// in another reported 900 to the ledger and 100 to the client
	// (2026-09-27 audit, round 41, B41-3). It is the UNCLAMPED hit — every
	// reporting site raises the prompt to it, per round 40's rule — so a
	// whole-completion answer stores the same number the frame path does
	// (2026-09-27 audit, round 41, B41-1).
	cacheDetails int
	cacheSibling int
	outTok       int
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
	// finished is whether the upstream said the stream was over — a choice's
	// finish_reason, a `[DONE]` sentinel that follows something relayed, or a
	// whole completion adopted as the turn. A stream is COMPLETE only when the
	// upstream says so, which is the rule the client leg applies: a connection
	// that closes with neither marker was cut off mid-answer, and relaying it as
	// a turn that finished — end_turn, message_stop, a booked 200 — sold a
	// truncated answer as the model's reply and told a caller that never got
	// message_stop to retry a turn it had already been charged for
	// (2026-09-27 audit, round 45, B45-2).
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
	return &anthropicBridge{
		ResponseWriter: w, stream: stream, model: model,
		toolBlocks: map[string]*toolBlock{}, mintedIDs: map[string]int{},
		grantedIDs: map[string]bool{}, statedIDOwner: map[string]string{},
		reservedIDs: map[string]bool{},
	}
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

// flushStreamTail feeds the frame reader the last line of a stream that ended
// without a newline. writeStream completes a line only when it sees "\n", so a
// terminal frame — the upstream's finish_reason, or its [DONE] — that arrived as
// the final bytes of the body stayed in the buffer and was never read: the turn
// was refused as an unterminated stream although the upstream had said it was
// complete. Called once, at end of body, before the stream is judged
// (2026-09-27 audit, round 46, G45-3).
func (b *anthropicBridge) flushStreamTail() {
	if !b.stream || b.sse.tail.Len() == 0 {
		return
	}
	// Only a frame: anything else in the buffer is a whole completion document
	// (adoptWholeStream's business) or bytes the frame reader already declined.
	if !strings.HasPrefix(strings.TrimSpace(b.sse.tail.String()), "data:") {
		return
	}
	b.writeStream([]byte("\n"))
}

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
		// Both readers of this turn — the ledger row and the client — ask this
		// predicate, and the ledger asks FIRST, so the terminal frame has to be
		// parsed before either of them judges the stream: a finish_reason that
		// arrived without a trailing newline is still the upstream saying it was
		// done, and reading it as an unterminated stream here also SET the
		// error finishStream later emits to the client (2026-09-27 audit, round
		// 46, G45-3). Idempotent: the buffer is empty once the line is read.
		b.flushStreamTail()
		if b.sse.upstreamErr != "" {
			// The upstream reported a failure inside the stream. Whether or not
			// events were already sent, the client and the ledger must read the
			// turn as failed: the stream path answers it with an SSE error event
			// (finishStream), the not-yet-started one with the status below.
			return http.StatusBadGateway, b.sse.upstreamErr
		}
		if b.sse.startSent {
			if b.nothingRelayed() {
				// Frames arrived and said nothing: no block was opened, no text
				// was held and no call was named. That is the same outcome as
				// the `[DONE]`-only body below — a turn the client cannot use —
				// and it used to be booked as a 200 success and metered, while
				// the byte-identical outcome with no frame at all was a 502.
				// A client that reads a completed empty turn never retries,
				// which is exactly what the 502 is for. It is recorded as this
				// stream's own failure so the two readers of one turn agree:
				// finishStream emits it to the client as an `error` event, and
				// this same predicate gives the ledger row its 502
				// (2026-09-27 audit, round 42, B42-3).
				b.sse.upstreamErr = "upstream returned an empty stream"
				return http.StatusBadGateway, b.sse.upstreamErr
			}
			if !b.sse.finished {
				// Frames were relayed and the upstream never said they were the
				// whole answer: no choice stated a finish_reason, no `[DONE]`
				// followed them, and the body is not a whole completion this
				// bridge adopted. The client leg reports the same condition in
				// the same words ("upstream stream ended before the response was
				// complete") and refuses to flush the calls it accumulated,
				// because partial argument JSON cannot be told from a model that
				// writes freeform arguments — and this leg used to relay the
				// turn as finished instead: end_turn, message_stop and a booked
				// 200, so a caller was sold a truncated answer as the model's
				// reply and never retried, and one that reads the missing
				// message_stop as a failure retried a turn already charged
				// (2026-09-27 audit, round 45, B45-2). Recorded as this stream's
				// own failure, so finishStream emits it as an `error` event and
				// this same predicate gives the ledger row its 502.
				b.sse.upstreamErr = "upstream stream ended before the response was complete"
				return http.StatusBadGateway, b.sse.upstreamErr
			}
			return 0, ""
		}
		if doc, ok := b.bufferedCompletion(); ok && documentSaysSomething(doc) {
			// No data: line was ever sent, but the body is a whole completion:
			// finalize adopts it as the turn, so the client reads an answer and
			// the row must not call it a failure. LedgerStatus asks this
			// predicate BEFORE finalize runs — the row is written first — so
			// without this the ledger recorded 502 with zero tokens for a turn
			// the client was served and the upstream billed (2026-09-27 audit,
			// round 39, B-F3).
			return 0, ""
		}
		// The upstream ended without a single event, or with a whole completion
		// that says nothing — no text, no reasoning, no call. finishStream's own
		// contract ("the client must always get a well-formed end") cannot be met
		// by an empty turn, and a client reading the stream waits for
		// message_stop — so answer the way the non-stream path answers an empty
		// completion: 502, which tells the client to retry instead of waiting
		// (round 24). Nothing has been committed yet, so the status is still
		// ours to choose.
		//
		// Asking only whether the body PARSED, as this arm used to, made the two
		// readers of one turn disagree: the row was written before finalize from
		// this arm and read 200, while the client — after adoption had set
		// startSent and the turn was found to hold nothing — was told the turn
		// failed by round 42's empty-stream arm (2026-09-27 audit, round 43,
		// B43-1).
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
	if !documentSaysSomething(resp) {
		// A whole completion that says nothing — no text, no reasoning, no call
		// the client could run — is the same turn the stream arms above refuse
		// and that finalize() answers 502 on this path. The row is written
		// BEFORE finalize() runs, and this predicate is how it learns the
		// client's answer: asking only whether the body PARSED told the ledger
		// the upstream's own 200 for a turn the client read as a 502 — the one
		// record of a refused turn saying it succeeded, with the upstream's
		// prompt booked against it, and no reconciliation between the two
		// records able to see it (2026-09-27 audit, round 45, B45-18). The
		// stream arms ask documentSaysSomething of a whole completion they are
		// holding; this is the same question for the path that holds no frames.
		return http.StatusBadGateway, "upstream returned an empty completion"
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
// cannot disagree.
//
// It is PER FIELD, which is what the client leg is. Gating the whole
// substitution on "the upstream stated NOTHING" meant a turn stating one of the
// two counts kept the other at zero in the row while the client was told this
// gateway's estimate for it — the same client/ledger disagreement the caller's
// comment promises cannot happen, reachable by any upstream that reports its
// prompt and not its answer (2026-09-27 audit, round 40, B40-2). A field the
// upstream DID state stands as the recorder read it: the caller folds this in
// with fillEmpty, which takes a field only where the row has nothing.
//
// A document the bridge is still HOLDING is read as the document, not through
// the counters: the non-stream leg copies the stated counts into them only in
// finalize(), which runs after this row is built, so reading the counters there
// answered the estimate for a prompt the upstream had stated — the same defect
// one layer down. bufferedCompletion() is the same bytes the recorder read
// (2026-09-27 audit, round 40, B40-2).
func (b *anthropicBridge) EstimatedUsage() (usage, bool) {
	if doc, ok := b.bufferedCompletion(); ok {
		u := doc.Usage.nonNegative()
		// The UNCLAMPED hit: the prompt is raised to it below, and reading it
		// through cachedTokens() first clamped it to the stated prompt, so this
		// arm and the frame arm reported different hits for one usage object
		// (2026-09-27 audit, round 41, B41-1).
		cached := u.statedCacheHit()
		prompt := u.PromptTokens
		if prompt <= 0 {
			prompt = b.promptEstimate
		}
		// A stated hit is evidence about the prompt, and the prompt is raised to
		// it rather than the hit clamped down to a size the upstream never
		// stated — the same rule promptSplit applies to the stream path, so a
		// non-stream turn and a streaming one report the same hit at the same
		// total (2026-09-27 audit, round 40, A40-3).
		if prompt < cached {
			prompt = cached
		}
		out := u.CompletionTokens
		if out <= 0 {
			out = b.documentOutputEstimate(doc)
		}
		if prompt <= 0 && out <= 0 {
			return usage{}, false
		}
		est := usage{PromptTokens: prompt, CompletionTokens: out}
		if cached > 0 {
			est.PromptCacheHitTokens = cached
		}
		return est, true
	}
	// The prompt is reported to the client as a PARTITION (uncached + cached),
	// so the row's total is the sum and its cache column is the hit — the same
	// numbers, read the way the ledger states them.
	fresh, cached := b.promptSplit()
	out := b.sse.outTok
	if !b.sse.statedCompletion {
		if est := b.outputEstimate(); est > 0 {
			out = est
		}
	}
	if fresh+cached <= 0 && out <= 0 {
		return usage{}, false
	}
	u := usage{PromptTokens: fresh + cached, CompletionTokens: out}
	if cached > 0 {
		u.PromptCacheHitTokens = cached
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
		// The upstream's last frame may have arrived without a trailing
		// newline — the frame reader only ever completes a LINE, so a terminal
		// finish_reason or [DONE] was left in the buffer and the turn read as
		// unterminated (2026-09-27 audit, round 46, G45-3).
		b.flushStreamTail()
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
	// A completion that says nothing — no text, no reasoning, no call the client
	// could run — never reaches here: noAnswer() above answers it with the 502
	// this path used to write itself, which is what the ledger row reads for it
	// too (2026-09-27 audit, round 44, B44-3; round 42, B42-3; round 45,
	// B45-18).
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
	// A text is content whatever it says: relayDelta relays a content of
	// whitespace as a text block, so trimming here dropped the text from a
	// completion this path had already decided is an answer — the client's
	// context meter was charged output_tokens for a turn whose body it was
	// never shown, while the stream path and both other legs served the same
	// text. Not TrimSpace, exactly as documentSaysSomething is not (2026-09-27
	// audit, round 45, C45-4; round 44 for the same decision about the status).
	if content != "" {
		respBlocks = append(respBlocks, map[string]any{"type": "text", "text": content})
	}
	// The blocks this turn carries decide its stop_reason, so the loop that
	// builds them and the count handed to stopReason must be the same set — and
	// the set is decided by the rule the client leg applies, not by the
	// upstream's stated call list.
	truncated := resp.Choices[0].FinishReason == "length"
	// Every id this list states is reserved before the loop takes a single
	// mint, so no call in it is numbered with an id another call of the same
	// list states — whatever order the upstream wrote them in (see
	// reservedIDs; 2026-09-27 audit, round 48, B-F4).
	for _, tc := range msg.ToolCalls {
		if id := tc.ID; id != "" {
			b.reservedIDs[id] = true
		}
	}
	toolBlocks := 0
	for _, tc := range msg.ToolCalls {
		if strings.TrimSpace(firstNonEmptyStr(tc.Function.Name, tc.Name)) == "" {
			// A call the upstream never named: content_block_start is the only
			// event that carries a name, so there is no block to open, and the
			// stream path holds it for that reason (2026-09-27 audit, round 39,
			// B-F8). Answering it here with a block whose "name" is empty told
			// the client to expect a call it could not name or run, which is the
			// same false claim in the non-streaming shape
			// (2026-09-27 audit, round 44, C44-4).
			continue
		}
		input, ok := callInput(firstNonEmptyStr(tc.Function.Arguments, tc.Arguments), truncated)
		if !ok {
			// A truncated fragment: the model was still writing this call's
			// arguments when it hit the token limit, so it is not a call. A
			// truncated turn that carries a COMPLETE call is tool_use — the
			// client can run it — but one carrying a fragment is max_tokens, and
			// the drop has to happen before the count for that to be what the
			// turn reports (2026-09-27 audit, round 44, C44-1 and B44-2; round
			// 43, C43-4 pinned the complete-call half).
			continue
		}
		name := firstNonEmptyStr(tc.Function.Name, tc.Name)
		// A call the upstream did not number is numbered here, by the rule the
		// stream path mints with. Passing the absent id through emitted a
		// tool_use block whose "id" is empty: the client answers a call by its
		// id, so it could never answer this one, and its tool_result went back
		// with tool_use_id "" — while the same upstream body answered as frames
		// carried a minted id, and both other legs mint one on this path too, so
		// a caller that is not streaming lost every tool call on the gateway and
		// kept them on the local server (2026-09-27 audit, round 45, B45-19 and
		// C45-1).
		// The id this call ends up with: the upstream's own when it stated one
		// for THIS call, and a numbered one otherwise — either because it
		// stated none, or because it stated an id that already belongs to
		// another call in this same list. The id-less call before it may have
		// been numbered with exactly that string (it is a pure function of the
		// call), and two blocks under one id cannot be answered separately
		// (2026-09-27 audit, round 46, G45-1).
		callArgs := firstNonEmptyStr(tc.Function.Arguments, tc.Arguments)
		// The identity is the call's own, in the canonical argument encoding the
		// mint hashes and both other legs key their own "same call restated"
		// rule by (see canonicalCallArgs): raw text made a restatement that
		// differed only in whitespace or key order read as a second call, and
		// the id the upstream had already given it was minted over
		// (2026-09-27 audit, round 47, C-F5).
		identity := toolCallIdentity(name, callArgs)
		id := tc.ID
		if id != "" {
			if owner, ok := b.statedIDOwner[id]; ok && owner == identity {
				// A restatement of the SAME call under the same id: one call,
				// listed twice. Both other legs answer it with one block and
				// drop the repeat — the client leg and the local converter both
				// key a stated id and skip what they have already sent
				// (anthropic.go's seenStatedID, the proxy's dedupKey), and a
				// repeat under a MINTED id is the different-call rule, not this
				// one. Emitting both ran the model's call twice, a side effect
				// the wire never asked for (2026-09-27 audit, round 48, C-F1).
				continue
			}
			if owner, ok := b.statedIDOwner[id]; ok && owner != identity {
				id = ""
			} else {
				b.statedIDOwner[id] = identity
				// A restatement of the SAME call under the same id is one
				// call's id, but the id is still this bridge's to hand out
				// only once: an id a minted call already carries cannot be
				// taken here as well.
				if b.grantedIDs[id] {
					id = ""
				} else {
					// An id this bridge has now given to a call is GRANTED,
					// exactly as the stream path records it when a block takes
					// a stated id (toolDelta). Without this the record was
					// write-only for mints here, so an id-less call later in
					// this same list minted the id the upstream had already
					// NAMED for another call — the two blocks shared it, and a
					// second call the upstream named with it kept it too,
					// while the stream path and the other two legs answer both
					// wires with distinct ids (2026-09-27 audit, round 47,
					// G47-1).
					b.grantedIDs[id] = true
				}
			}
		}
		if id == "" {
			id = b.mintedToolCallID(name, callArgs)
		}
		toolBlocks++
		respBlocks = append(respBlocks, map[string]any{
			"type": "tool_use", "id": id,
			"name":  name,
			"input": input,
		})
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
	// The same split promptSplit makes, for the same reason: a stated hit is
	// evidence about the prompt, so the total is raised to it rather than the
	// hit being clamped down to a size the upstream never stated — which here
	// would have reported input_tokens = the estimate, cache_read = the
	// estimate, or (with the subtraction below unraised) input_tokens = -900
	// (2026-09-27 audit, round 40, A40-3). The hit is the UNCLAMPED one for the
	// same reason: reading it through cachedTokens() clamped it to the stated
	// prompt, so this path reported a smaller hit than the streaming path did
	// for one identical usage object (2026-09-27 audit, round 41, B41-1).
	cached := u.statedCacheHit()
	promptTotal := u.PromptTokens
	if promptTotal < cached {
		promptTotal = cached
	}
	// The same three counters the streaming and adopt paths keep, so one place
	// holds "what the upstream stated" for every path this bridge can serve.
	// EstimatedUsage reads them, and a non-stream turn that left them at zero
	// had its STATED counts replaced by this gateway's estimate in the ledger row
	// (2026-09-27 audit, round 40, B40-2).
	if u.PromptTokens > 0 {
		b.sse.inTok = u.PromptTokens
		b.sse.statedPrompt = true
	}
	if u.CompletionTokens > 0 {
		b.sse.outTok = u.CompletionTokens
		b.sse.statedCompletion = true
	}
	b.stateCacheHit(u.detailsCachedTokens(), u.PromptCacheHitTokens)
	in, out := promptTotal-cached, u.CompletionTokens
	// The answer's size, in the unit the stream path counts it in, so the
	// fallback below is one rule for both paths (2026-09-27 audit, round 39,
	// C-F3). Counted through documentRelayedBytes, the same helper the ledger's
	// documentOutputEstimate and the adopted-stream arm use: counting the
	// content alone made this leg's output_tokens and its own row's
	// completion_tokens two different numbers for any answer carrying reasoning
	// or a tool call — a tool-only turn was relayed to the client as
	// output_tokens:0 while the row recorded 5, and the same document delivered
	// as SSE frames agreed with itself (2026-09-27 audit, round 42, B42-1).
	b.sse.outBytes = documentRelayedBytes(msg)
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
		"stop_reason":   stopReason(resp.Choices[0].FinishReason, toolBlocks),
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
		if payload == "" {
			continue
		}
		if payload == "[DONE]" {
			// The sentinel says the stream ENDED, not that it ever held a turn:
			// a body terminated by [DONE] with nothing relayed before it is the
			// empty stream the arms below refuse. When something WAS relayed it
			// is this stream's completion marker, which is what the client leg
			// reads it as ([DONE] with `started` or a finish_reason) — see
			// noAnswer's arm for a stream that ends without either
			// (2026-09-27 audit, round 45, B45-2).
			if !b.nothingRelayed() || b.sse.stopMsg != "" {
				b.sse.finished = true
			}
			continue
		}
		var chunk oaStreamChunk
		if json.Unmarshal([]byte(payload), &chunk) != nil {
			continue
		}
		if len(chunk.Error) > 0 && string(chunk.Error) != "null" {
			b.sse.upstreamErr = upstreamErrorSentence(payload, errorFrameMessage(chunk.Error))
			continue
		}
		// The stream is committed by the first frame that CARRIES something, not
		// by the first frame that arrives. A frame is not an answer: an upstream
		// that sends an empty delta (or a usage-only chunk, or the turn's
		// finish_reason alone) and then ends has said nothing, and committing on
		// it opened message_start, which fixed the status at 200 and left the
		// client reading an `error` event for a turn the ledger row — written
		// from the same predicate, noAnswer's empty-stream arm — booked as a
		// 502. The row and the client disagreed about one turn, and the client
		// leg answers the same wire with a 502 the caller can retry (its status
		// is still its own to choose, because it commits on the first event it
		// RELAYS) (2026-09-27 audit, round 47, C-F7). Until something is
		// relayed, the status is still this bridge's to choose: noAnswer's arm
		// above writes it (see the `!(b.stream && b.sse.startSent)` branch).
		//
		// An error frame does not commit either — it is checked above, and its
		// verdict is the upstream's failure, which the same branch answers with
		// its status.
		if !b.sse.startSent && frameRelaysSomething(chunk) {
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
			details := 0
			if chunk.Usage.PromptTokensDetails != nil {
				details = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			b.stateCacheHit(details, chunk.Usage.PromptCacheHitTokens)
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
				// A whole completion IS the answer, so a frame that carried one
				// as the first thing the stream said has terminated it even when
				// no finish_reason frame follows — the same reading the client
				// leg gives this wire (round 45, B45-2).
				//
				// Only when it actually relayed something: an EMPTY `message`
				// frame says nothing, so it terminates nothing — setting
				// finished on it made a stream that later ends with no
				// finish_reason and no [DONE] read as a complete turn (200,
				// end_turn) instead of the unterminated one B45-2 refuses
				// (2026-09-27 audit, round 46, G45-2).
				if !b.nothingRelayed() {
					b.sse.finished = true
				}
			}
			b.relayDelta(ch.Delta)
			if ch.FinishReason != nil && *ch.FinishReason != "" {
				b.sse.stopMsg = *ch.FinishReason
				// The upstream said the turn is over. This is the other
				// completion marker the OpenAI wire uses, and either one is
				// enough for the stream to count as finished — a connection that
				// closes with neither was cut off mid-answer (round 45, B45-2).
				b.sse.finished = true
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
		// The call is over, so the narration that was held behind it comes out
		// FIRST — before the text arriving now. Draining it only from toolDelta
		// and finishStream left the two chunks emitted in the order they were
		// unblocked rather than the order they were written, and the client read
		// the model's prose reversed ("SECOND" then "FIRST") (2026-09-27 audit,
		// round 40, B40-4: a regression of the round-39 hold).
		b.flushHeldText()
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
		key := "!" + id
		if b.toolBlocks[key] == nil && name != "" && b.idHeldByAnotherCall(id) {
			// The upstream states an id this bridge has already given to a
			// DIFFERENT call — most often the id it minted for an id-less call,
			// which is a pure function of that call and therefore reproducible.
			// Taking it here would put two blocks under one id, which the client
			// can answer only once; the fragment is numbered as its own call
			// instead (2026-09-27 audit, round 46, G45-1).
			//
			// Only for a fragment that NAMES its call: a call is introduced by
			// its name (see splitFromCurrent), and the client leg's
			// startsANewToolCall merges exactly here — `deltaName == ""` over a
			// call in progress returns false whatever the id is. Reading the id
			// alone split the ordinary "the id arrives on the fragment AFTER the
			// name" wire whenever that id happened to be the one this bridge
			// minted, and the arguments of the call in progress were relayed to
			// the client as prose beside a tool_use with an empty input
			// (2026-09-27 audit, round 47, G47-2).
			b.synthSeq++
			b.lastToolKey = "?" + strconv.Itoa(b.synthSeq)
			if name != "" {
				b.lastToolName = name
			}
			return b.lastToolKey
		}
		// The id names the call, but a repeated id is not proof this fragment
		// continues it: an upstream that reuses one id for the turn's second call
		// is the same wire the id-less path below splits on, and returning on the
		// id alone answered it two ways — the block merged, the second name
		// dropped, and its arguments concatenated onto the first's partial_json
		// (2026-09-27 audit, round 43, B43-3). An id no block carries yet is a
		// new call outright, which is why this asks only about a key already
		// accumulated.
		if b.toolBlocks[key] != nil && b.splitFromCurrent(key, name, args) {
			b.synthSeq++
			b.lastToolKey = "?" + strconv.Itoa(b.synthSeq)
			b.lastToolName = name
			return b.lastToolKey
		}
		if b.toolBlocks[key] == nil && name == "" {
			// An id on a fragment that names NOTHING is not introducing a call:
			// a call is introduced by its name (see this function's note about a
			// fragment that carries a NAME), and the client leg's
			// startsANewToolCall merges exactly here — a fresh id starts a call
			// only once the accumulator HAS one, and `deltaName == ""` over an
			// unnumbered call in progress is a continuation. Opening a second
			// block instead sent the client a tool_use named Bash with input {}
			// beside the call's real arguments relayed to the agent as prose,
			// while the leg that merges ran the call the model wrote
			// (2026-09-27 audit, round 45, B45-1). The block is re-keyed to the
			// id, so every fragment after this one continues it here too.
			//
			// The block's id, not its emptiness, is what the test turns on: the
			// name alone opens a block, and one opened before the id arrived
			// carries a MINTED id — which `cur.id == ""` read as "a call that
			// already has its own id", so the merge never ran for exactly the
			// wire it was written for and the arguments went to a second block
			// that could never be opened. A block whose id the UPSTREAM stated
			// (statedID) is the other case, and there the id is a new call's.
			// A minted id is replaced by the stated one when the block has not
			// opened yet, so a tool_result keyed on this call carries the id the
			// upstream itself used; once content_block_start is out the client
			// already holds the minted id and it is kept (2026-09-27 audit,
			// round 45, B45-1).
			if cur := b.toolBlocks[b.lastToolKey]; cur != nil && !cur.statedID {
				if !cur.started {
					cur.id = ""
				}
				delete(b.toolBlocks, b.lastToolKey)
				b.toolBlocks[key] = cur
				cur.key = key
				b.lastToolKey = key
				return key
			}
			// A block in progress whose id the UPSTREAM stated is left where it
			// is: the fall-through below keys this fragment's own new block by
			// the id it states, which is what the client leg's
			// startsANewToolCall answers the same wire with (a delta id over an
			// accumulator that already carries one begins the next call, under
			// the id the delta stated). A round-46 candidate that numbered this
			// call instead was removed: it changed no wire but this one, and
			// here it diverged — the second call's arguments were stranded on a
			// block that could never be named, and the id the upstream stated
			// was never the call's (2026-09-27 audit, round 46, G45-4).
		}
		if name != "" {
			b.lastToolName = name
		}
		b.lastToolKey = key
		return b.lastToolKey
	}
	if b.lastToolKey != "" {
		if b.splitFromCurrent(b.lastToolKey, name, args) {
			b.synthSeq++
			b.lastToolKey = "?" + strconv.Itoa(b.synthSeq)
			b.lastToolName = name
			return b.lastToolKey
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

// splitFromCurrent reports whether a fragment begins the NEXT tool call rather
// than continuing the block accumulated under key. It is the client leg's
// startsANewToolCall in the terms this bridge keeps state in, and both of
// toolKey's keyless arms ask it, so one wire cannot be answered two ways.
//
// A fragment that repeats the current call's name while its arguments are
// already a COMPLETE JSON object is the next call (2026-09-27 audit, round 38,
// B-F3). A bare repeat — the fragment names the call again and states no
// arguments over a call that has accumulated none — is the second of two
// argument-less calls, and it too begins the next call: an empty argument
// string is a COMPLETE argument list for a call that takes none, and reading it
// as a continuation delivers one tool_use where the model asked for two
// (2026-09-27 audit, round 39, B-F9). The cost of that reading, an upstream
// that restates the name in a chunk of its own before sending the arguments, is
// a call with no arguments — the other reading's cost is a call the client
// never hears about, and only one of the two can be reported to the model at
// all. A DIFFERENT name is always the next call: an argument continuation
// carries arguments alone, and folding two names into one block hands the
// client a call it cannot execute under the name it received (round 40, A40-8).
//
// No block accumulated under the key yet is not a split — there is nothing to
// continue — and neither is a fragment that names nothing.
func (b *anthropicBridge) splitFromCurrent(key, name, args string) bool {
	if name == "" || b.lastToolName == "" {
		return false
	}
	cur := b.toolBlocks[key]
	if cur == nil {
		return false
	}
	acc := strings.TrimSpace(cur.args.String())
	return name != b.lastToolName ||
		(acc != "" && json.Valid([]byte(acc))) ||
		(acc == "" && strings.TrimSpace(args) == "")
}

// idHeldByAnotherCall reports whether a call block other than the one keyed by
// id's own key already carries this id — a minted id from an id-less call, or
// an id the upstream stated for a different call. Two blocks under one id
// cannot be answered separately (2026-09-27 audit, round 46, G45-1).
func (b *anthropicBridge) idHeldByAnotherCall(id string) bool {
	for _, tb := range b.toolOrder {
		if tb.id == id && tb.key != "!"+id {
			return true
		}
	}
	return false
}

// blockCarrying returns the block other than tb that already carries id, or nil
// when no block does.
func (b *anthropicBridge) blockCarrying(id string, tb *toolBlock) *toolBlock {
	if id == "" {
		return nil
	}
	for _, other := range b.toolOrder {
		if other == tb || other.merged || other.id != id {
			continue
		}
		return other
	}
	return nil
}

// settledIdentity is what a call is, as far as the ids this bridge hands out are
// concerned — its name and its arguments — and whether the arguments are far
// enough along for the comparison to mean anything. An argument text the
// upstream is still writing is half an object, so two blocks showing the same
// half need not be the same call; the identity is not reported as settled for
// one, and the caller treats them as different calls.
func settledIdentity(tb *toolBlock) (string, bool) {
	if argsAreMidObject(tb.args.String()) {
		return "", false
	}
	return toolCallIdentity(tb.name, tb.args.String()), true
}

// toolCallIdentity is what a call IS, as far as the ids this bridge hands out
// are concerned: its name and its arguments, the two fields gatewayToolCallIDFor
// mints from and the shape the non-stream list records as statedIDOwner. Two
// blocks that share it are one call restated, not two calls.
func toolCallIdentity(name, args string) string {
	return "\x00" + name + "\x00" + canonicalCallArgs(args)
}

// canonicalCallArgs re-encodes a tool call's argument text the way both other
// legs hash it — anthropic.go's `json.Marshal(tc.Function.Arguments)` and the
// proxy's mintedKey are the same re-encoding of the parsed arguments: the
// object's own keys in the order the upstream wrote them, each value re-encoded
// by encoding/json, whitespace gone. Hashing the RAW text instead made the same
// call mint two ids depending on the leg that answered it — a call whose
// arguments the upstream wrote as {"z":1,"a":2}, or as a pretty-printed object,
// hashed differently here than through the local server or the client proxy,
// and the id is the promise that a tool_result written against one leg's answer
// stays valid when the retry goes through another (2026-09-27 audit, round 47,
// C-F5; round 47's G47-4 is the same promise for the "#n" suffix).
//
// The encoding is deliberately the generic one, not a faithful copy of the
// input: a key order is the only thing the raw text holds that the parsed value
// does not, and numbers re-encode exactly as both other legs re-encode them
// (json.Unmarshal into `any`, so float64). Nested objects are re-encoded by
// encoding/json on both sides, which sorts their keys — the other legs hold
// them in a plain map[string]any inside the ordered top level, so this matches
// there too.
//
// Text that is not a JSON object has no keys to preserve and is kept as the
// single-key "_raw" object, the shape the client leg and this bridge's own
// non-stream path both keep it in (a fragment the model was still writing is
// hashed as the text it is, wrapped the same way).
func canonicalCallArgs(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return "{}"
	}
	if trimmed == "null" {
		// The JSON literal null is an argument text with no keys, and an ordered
		// map parses it into an EMPTY one — but the value it re-encodes to is
		// still `null`, and that is the string both other legs hash: the local
		// converter marshals its own ordered map (a null argument text marshals
		// back to `null`, an empty one to `{}`) and the client leg's parser
		// marshals the map it unmarshalled into. Folding the two together here
		// minted `{}`'s id for a call the other two legs number from `null`, so
		// one upstream turn produced two ids depending on which leg answered it
		// — the block's own INPUT is the empty object on all three legs (a
		// tool_use input is an object; round 42/43), which is what made the
		// fold look right (2026-09-27 audit, round 49, correcting round 48's
		// C-F5, whose id half was pinned without checking the sibling legs).
		return "null"
	}
	if !strings.HasPrefix(trimmed, "{") {
		return rawFallbackArgs(trimmed)
	}
	dec := json.NewDecoder(strings.NewReader(trimmed))
	tok, err := dec.Token()
	if err != nil {
		return rawFallbackArgs(trimmed)
	}
	if d, ok := tok.(json.Delim); !ok || d != '{' {
		return rawFallbackArgs(trimmed)
	}
	// Keys in the order they were first written, each with the LAST value the
	// object gave it: that is what an ordered map does with a repeated key
	// (orderedmap.Set replaces the value in place), and it is what the client
	// leg hashes — {"a":1,"a":2} is {"a":2} there, so hashing both entries here
	// minted a different id for one call (2026-09-27 audit, round 48, B-F3).
	var keys []string
	vals := map[string]any{}
	for dec.More() {
		kt, err := dec.Token()
		if err != nil {
			return rawFallbackArgs(trimmed)
		}
		key, ok := kt.(string)
		if !ok {
			return rawFallbackArgs(trimmed)
		}
		var val any
		if err := dec.Decode(&val); err != nil {
			return rawFallbackArgs(trimmed)
		}
		if _, seen := vals[key]; !seen {
			keys = append(keys, key)
		}
		vals[key] = val
	}
	// The closing brace, and then nothing: a second value in the same text is
	// not this object and the text is hashed as the text it is instead.
	if _, err := dec.Token(); err != nil {
		return rawFallbackArgs(trimmed)
	}
	if dec.More() {
		return rawFallbackArgs(trimmed)
	}
	var b strings.Builder
	b.WriteByte('{')
	for i, key := range keys {
		kb, err := json.Marshal(key)
		if err != nil {
			return rawFallbackArgs(trimmed)
		}
		vb, err := json.Marshal(vals[key])
		if err != nil {
			return rawFallbackArgs(trimmed)
		}
		if i > 0 {
			b.WriteByte(',')
		}
		b.Write(kb)
		b.WriteByte(':')
		b.Write(vb)
	}
	b.WriteByte('}')
	return b.String()
}

// jsonNumber reads a value as a number, in either of the two shapes this
// process handles: the float64 a JSON number decodes into, and the plain int
// types a body this process built itself carries (the same pair the max_tokens
// reader accepts, round 42). Anything else is not a number and has no place on
// a field a backend's schema types as one.
func jsonNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, true
	case int:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// rawFallbackArgs is the argument text this bridge keeps for a call whose
// arguments are not a JSON object, in the shape the client leg keeps it:
// a single-key map under "_raw" (anthropic_openai_proxy.go, args.Set("_raw",
// raw)), which is also the input this gateway's own non-stream path hands the
// client for the same call (callInput). Hashing the bare text instead made one
// call mint a different id here than through the client leg, and a different id
// than the very block this bridge was about to emit — the block's input is the
// _raw object (2026-09-27 audit, round 48, B-F3).
func rawFallbackArgs(s string) string {
	b, err := json.Marshal(map[string]any{"_raw": s})
	if err != nil {
		return "{}"
	}
	return string(b)
}

func (b *anthropicBridge) toolDelta(upIdx *int, id, name, args string) {
	key := b.toolKey(upIdx, id, name, args)
	tb, ok := b.toolBlocks[key]
	if !ok {
		tb = &toolBlock{key: key, index: -1}
		b.toolBlocks[key] = tb
		b.toolOrder = append(b.toolOrder, tb)
		if name != "" {
			// This fragment introduces a call, so the calls before it will not
			// state another argument: any of them still waiting to be numbered
			// is numbered now, and takes its index before this one
			// (releaseHeldToolBlocks).
			b.releaseHeldToolBlocks(tb)
		}
	}
	if name != "" && tb.name == "" {
		tb.name = name
	}
	if args != "" {
		tb.args.WriteString(args)
	}
	if tb.id == "" {
		// The id is decided after the fragment's name and arguments are in, so
		// the fallback below has the call's own identity to work from.
		if id != "" && !strings.HasPrefix(tb.key, "?") {
			tb.id = id
			tb.statedID = true
			// The upstream's own id, stated for this very call (the ordinary
			// OpenAI order states the name first and the id on a later
			// fragment): nothing is left to mint.
			tb.needsMint = false
			b.grantedIDs[id] = true
		} else {
			// A block the split rule minted a key for: the fragment repeated an
			// id another block already carries, so this block must NOT inherit
			// it. Claude Code answers a tool_use by its id — two blocks sharing
			// one id get one answer for two calls, and the OpenAI wire cannot
			// key two tool_results by one tool_call_id — so the id is derived
			// from a hash of the call itself, which is what both of the other
			// legs mint for a call the upstream never named an id for
			// (anthropic.ToolCallIDFor over name and arguments), and what they
			// mint for a split too. It is NOT derived from the synthetic key's
			// counter: "call_" + the sequence number collides with the upstream
			// ids this wire actually carries — an upstream that reuses "call_1"
			// is exactly the wire the split rule exists for, and the block the
			// split minted then came out carrying the very id it was split from
			// (2026-09-27 audit, round 44, B44-1; round 39, B-F8 for the split).
			//
			// The mint itself waits for the block to open. It hashes the call
			// its name and its arguments — and on the ordinary OpenAI order the
			// name arrives on one fragment and the arguments on the next, so a
			// mint taken here hashed the EMPTY prefix, the id of an
			// argument-less call: this bridge's own non-stream path, the client
			// leg and the local converter all mint from the finished arguments,
			// so one call reached the client under two different ids depending
			// on the path that answered it (2026-09-27 audit, round 48, B-F2 and
			// A-F3).
			tb.needsMint = true
		}
	}
	if tb.merged {
		// A restatement of a call another block already carries: this block is
		// not a call, so its fragments are not arguments the client is owed
		// (see startToolBlock). Dropped silently — the call they repeat is
		// already on the wire, complete.
		return
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
		if tb.needsMint && !argsAreFinished(tb.args.String()) {
			// The block has to be numbered by this bridge, and the arguments it
			// is numbered FROM are not all in: opening now would mint the id of
			// a half-written call — on the ordinary OpenAI order the name
			// arrives on one fragment and the arguments on the next, and on
			// that wire the mint hashed the empty prefix. Held until the
			// object closes (the arguments stated so far follow in one delta
			// below), until a fragment introduces the NEXT call (releaseHeldToolBlocks),
			// or until the turn ends (finishStream).
			return
		}
		b.closeOpen()
		b.flushHeldText()
		b.closeOpen() // the held text took a block of its own; the call follows it
		if !b.startToolBlock(tb) {
			return // a restatement of a call already delivered (see startToolBlock)
		}
	}
	b.flushToolArgs(tb)
}

// flushToolArgs emits the argument text a block has accumulated but not yet
// delivered. A block is held while the arguments that would number it are still
// arriving (see toolDelta) and while it has no name to open under, so the
// fragments that arrived in the meantime are delivered the moment it opens, as
// one delta, and no argument is lost to the delay.
func (b *anthropicBridge) flushToolArgs(tb *toolBlock) {
	if tb.emitted >= tb.args.Len() {
		return
	}
	// The whole call arrived in one fragment, so the text is the block's input
	// entire: if it is not a JSON object — freeform arguments — it is delivered
	// as the single-key "_raw" object that both other legs deliver it as, and
	// that this bridge's own non-stream path puts in the same call's block. A
	// text already delivered in pieces cannot be re-wrapped, and stays as the
	// model wrote it (the freeform call the upstream split across fragments).
	first := tb.emitted == 0
	pending := tb.args.String()[tb.emitted:]
	tb.emitted = tb.args.Len()
	if first {
		if wrapped, ok := rawArgsObject(pending); ok {
			pending = wrapped
		}
	}
	b.emit("content_block_delta", map[string]any{
		"type": "content_block_delta", "index": tb.index,
		"delta": map[string]any{"type": "input_json_delta", "partial_json": pending},
	})
}

// rawArgsObject returns the object an argument text that is not a JSON object
// has to be delivered as, and whether it was one of those: the empty object for
// null (what both other legs parse it into), and the single-key "_raw" encoding
// for anything else that is not an object — the freeform shape the client leg
// keeps whole rather than dropping (anthropic_openai_proxy.go, callInput here).
// An object still being written is not this case: it is delivered as it stands,
// and a tool the model gave no arguments at all needs nothing.
func rawArgsObject(s string) (string, bool) {
	if argsAreMidObject(s) {
		return "", false
	}
	t := strings.TrimSpace(s)
	if t == "" || strings.HasPrefix(t, "{") {
		return "", false
	}
	if t == "null" {
		return "{}", true
	}
	return rawFallbackArgs(t), true
}

// argsAreMidObject reports whether the argument text is an object the upstream
// has not finished writing — started with "{" and not yet parseable. Text that
// does not begin an object is not half of one (a tool that takes freeform
// arguments, or a complete JSON value of another kind), and an empty argument
// list is the empty object, which is a complete list for a tool that takes
// none.
func argsAreMidObject(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" || !strings.HasPrefix(s, "{") {
		return false
	}
	return !json.Valid([]byte(s))
}

// argsAreFinished reports whether a call's argument text is finished — a
// complete JSON object, which is the one spelling the wire has for a call that
// states its arguments — or is freeform text rather than the beginning of an
// object. It is the test a mint waits for (see toolDelta): an id-less call is
// numbered from its own arguments, and the number has to be the one both other
// legs produce for the same call.
//
// An empty argument list is NOT finished: a fragment that names a call states
// no arguments either, so an argument-less call and a call whose arguments have
// not arrived yet look the same here. The call is numbered once nothing more
// can arrive for it — the next call's fragment, or the end of the turn — and
// its id is then the mint over no arguments, which is the id both other legs
// mint for a call that states none.
func argsAreFinished(raw string) bool {
	s := strings.TrimSpace(raw)
	if s == "" {
		return false
	}
	if !strings.HasPrefix(s, "{") {
		return true
	}
	return json.Valid([]byte(s))
}

// releaseHeldToolBlocks opens every block that was waiting to be numbered, ahead
// of the call whose fragment has just arrived. A fragment that NAMES a call
// introduces it, so any earlier call that had not stated a finished argument
// list will not state one — its arguments are whatever it wrote — and its block
// takes the index the wire gives it, before the new call's rather than after
// (the blocks are opened in the order the calls appear, as both other legs
// deliver them).
func (b *anthropicBridge) releaseHeldToolBlocks(except *toolBlock) {
	for _, tb := range b.toolOrder {
		if tb == except || tb.started || tb.merged || tb.name == "" {
			continue
		}
		if !tb.needsMint {
			continue
		}
		b.closeOpen()
		b.flushHeldText()
		b.closeOpen()
		if b.startToolBlock(tb) {
			b.flushToolArgs(tb)
		}
	}
}

// startToolBlock emits the content_block_start for one tool call, once, and
// reports whether the block was opened. The caller has closed whatever was
// open.
func (b *anthropicBridge) startToolBlock(tb *toolBlock) bool {
	if tb.started || tb.merged {
		return false
	}
	if other := b.blockCarrying(tb.id, tb); other != nil {
		// The id this call is about to carry is one another block already
		// carries, and the client can answer a tool_use only once: two blocks
		// under one id get one tool_result for two calls. What that makes this
		// block depends on whether the two are the same CALL.
		//
		// The check sits here, at the one point an id is handed to the client,
		// rather than at the fragment that stated it: an id can be stated before
		// the fragment that names its call (the usual OpenAI order is id first),
		// and only at the start is the call's identity — the name the block will
		// carry and the arguments accumulated under it — known.
		if mine, ok := settledIdentity(tb); ok {
			// `other.statedID` is the whole difference between a restatement and
			// two calls. A block whose id this bridge MINTED has been numbered
			// from its own name and arguments, and the upstream stating that
			// string for a separately-indexed entry is naming a SECOND call
			// (`call_` + a hash of the call is reproducible, so an upstream that
			// wants to state it can) — the non-stream list answers that wire
			// with two blocks, and so does the client leg's parser. Only when
			// the id came from the upstream is a repeat of the same name and
			// arguments under it the one call restated, which both other legs
			// answer with ONE block (2026-09-27 audit, round 49, A-F3).
			if theirs, ok := settledIdentity(other); ok && theirs == mine && other.statedID {
				// A restatement of the SAME call under its own id, which both
				// other legs answer with the one block (the client leg and the
				// local converter both drop the repeat; anthropic.go's
				// seenStatedID, the proxy's dedupKey). This block is that call
				// again, so it is not a call: it never opens, and the fragments
				// that follow it are the arguments of a call already delivered
				// (2026-09-27 audit, round 48, B-F1, A-F1 and C-F1).
				tb.merged = true
				tb.closed = true
				return false
			}
		}
		// A different call that states an id already handed out — the index
		// path reaches this directly, since a fragment's index names its call.
		// It is numbered from its own identity, the value both other legs mint
		// for a call the upstream never gave a usable id, bumped until it is one
		// no call of this turn holds (mintedToolCallID).
		tb.id = b.mintedToolCallID(tb.name, tb.args.String())
		tb.needsMint = false
	} else if tb.needsMint {
		// A block this bridge has to number itself is numbered here, from the
		// FINISHED arguments — the value the other paths and legs answer with.
		// The fragment that named it could not number it, because on the
		// ordinary OpenAI order the arguments arrive after the name
		// (2026-09-27 audit, round 48, B-F2 and A-F3).
		tb.id = b.mintedToolCallID(tb.name, tb.args.String())
		tb.needsMint = false
	}
	tb.started = true
	tb.index = b.takeIdx()
	b.emit("content_block_start", map[string]any{
		"type": "content_block_start", "index": tb.index,
		"content_block": map[string]any{"type": "tool_use", "id": tb.id, "name": tb.name, "input": map[string]any{}},
	})
	b.cur = &openBlock{index: tb.index, tool: tb}
	return true
}

// countedToolBlocks counts the tool blocks this turn carries for the client.
// A finish_reason of tool_calls over zero tool blocks claims the model asked for
// a tool it never named (2026-09-27 audit, round 39, B-F8).
//
// Started is not the whole question: the count is what decides stop_reason, and
// a block opened for a fragment whose arguments never parsed on a turn the
// upstream truncated is not a call — the client leg drops that fragment on both
// of its paths, and the non-stream path here drops it too, so a turn carrying
// only fragments reports max_tokens instead of promising a call the client
// would execute with input the model never wrote. The block is already out by
// then; the stream cannot un-send it, so the verdict is the part left to get
// right (2026-09-27 audit, round 44, C44-1; round 43, C43-4).
func (b *anthropicBridge) countedToolBlocks() int {
	n := 0
	for _, tb := range b.toolOrder {
		if !tb.started {
			continue
		}
		if _, ok := callInput(tb.args.String(), b.sse.stopMsg == "length"); !ok {
			continue
		}
		n++
	}
	return n
}

// cacheHit is the hit the upstream stated for this turn, UNCLAMPED and resolved
// the way the ledger's recorder resolves the same two fields (details over
// sibling, each keeping its own last positive statement). Unclamped because
// every site that reports it raises the prompt to it instead of clamping it
// down (round 40's rule, and the reason a whole-document answer and the same
// usage over frames must not disagree — 2026-09-27 audit, round 41, B41-1 and
// B41-3).
func (b *anthropicBridge) cacheHit() int {
	if b.sse.cacheDetails > 0 {
		return b.sse.cacheDetails
	}
	if b.sse.cacheSibling > 0 {
		return b.sse.cacheSibling
	}
	return 0
}

// stateCacheHit records one usage object's cache hit in both spellings it can
// arrive in, keeping each spelling's last POSITIVE statement. A chunk that
// states only the sibling spelling must not erase a details count an earlier
// chunk stated, which is what one last-writer-wins slot did (2026-09-27 audit,
// round 41, B41-3).
func (b *anthropicBridge) stateCacheHit(details, sibling int) {
	if details > 0 {
		b.sse.cacheDetails = details
	}
	if sibling > 0 {
		b.sse.cacheSibling = sibling
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
	cached = b.cacheHit()
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
		// known of the prompt and the raise below reports it as it stands.
		prompt = b.promptEstimate
	}
	// The hit is evidence about the prompt: tokens the upstream served from its
	// prefix cache are tokens OF this prompt, so a stated hit larger than this
	// gateway's own reading of the body proves the reading is short. Clamping
	// the hit DOWN to the reading discarded the only first-hand measure of the
	// turn's prompt that either record had, and both records discarded it the
	// same way — the client was told input_tokens=0/cache_read_input_tokens=8
	// for a turn the upstream had just said 900 of its prompt came from cache,
	// and the ledger row agreed, so no reconciliation between them could see
	// it (2026-09-27 audit, round 40, A40-3). The prompt is raised to the hit
	// instead, so the two halves still sum to the prompt and neither is
	// negative.
	if prompt < cached {
		prompt = cached
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
	// A call this bridge had to number, whose arguments were an object the
	// upstream never closed, opens here — the same block it would have opened on
	// the fragment that named it before the mint waited for the arguments (a
	// truncated turn: the model was still writing the call when it stopped).
	// Started before the nameless arm below, which is for calls that HAVE no
	// name and so can never open a block at all.
	for _, tb := range b.toolOrder {
		if tb.started || tb.merged || tb.name == "" {
			continue
		}
		b.closeOpen()
		b.flushHeldText()
		b.closeOpen()
		if b.startToolBlock(tb) {
			b.flushToolArgs(tb)
			// The block is OPENED here and no fragment is left to close it:
			// the stream is over (this is finishStream), so the client kept an
			// unterminated tool_use block for the turn — a call no client-side
			// accumulator can finish, since nothing told it the arguments had
			// stopped arriving (2026-09-27 audit, round 50, B-F2).
			b.closeOpen()
		}
	}
	// A call the upstream never named is not a call the client can make: its
	// content_block_start could only go out with an empty name (the event that
	// carries the name has no second chance), so the block is never opened and
	// whatever arguments arrived are delivered as TEXT — the model's raw output,
	// which is at least readable — rather than as a tool_use Claude Code would
	// report as pending and could never run (2026-09-27 audit, round 39, B-F8).
	for _, tb := range b.toolOrder {
		// A MERGED block is not a call this bridge failed to name: it is the
		// restatement of one the client already holds, and its arguments were
		// the arguments of that delivered call. Relaying them as text — the
		// fate of a call that has no name — put the call's own JSON on the wire
		// as assistant prose, so the client read `{"a":1}` as something the
		// model said (2026-09-27 audit, round 49, A-F1).
		if tb.started || tb.merged || tb.args.Len() == 0 {
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
	// The blocks this stream actually opened decide, exactly as they do on the
	// non-stream path and on leg 1: a call reached the client means tool_use,
	// whatever the upstream's finish_reason said, and no call means not
	// tool_use — an upstream stopping to call a tool whose fragments were all
	// unnamed said nothing the client can act on, and reporting tool_use there
	// told Claude Code to wait for a call it will never receive, on a turn that
	// is over (2026-09-27 audit, round 39, B-F8; round 43, C43-3 and C43-4).
	stop := stopReason(b.sse.stopMsg, b.countedToolBlocks())
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
	if !ok || !documentSaysSomething(resp) {
		// A whole completion that says nothing — no text, no reasoning, no call
		// — is not an answer to adopt. Adopting it opened a message_start for a
		// turn the bridge then declared empty, so the client read a start and an
		// error for a body that was never an answer, and the ledger row written
		// before this ran read 200 for it (2026-09-27 audit, round 43, B43-1).
		return false
	}
	b.sse.startSent = true
	// The document IS the turn's completion marker: it is the whole completion,
	// so nothing is missing from it (round 45, B45-2's rule).
	b.sse.finished = true
	b.emit("message_start", map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"id": respID(resp.ID), "type": "message", "role": "assistant",
			"model": b.model, "content": []any{},
			"usage": map[string]any{"input_tokens": 0, "output_tokens": 0},
		},
	})
	// The bytes relayed are counted here exactly as relayDelta counts them: this
	// path writes the turn directly, and relayDelta is the only other writer of
	// sse.outBytes — so an adopted answer was relayed in full and then closed
	// with output_tokens:0, and the ledger row recorded completion=0 for it
	// (2026-09-27 audit, round 40, B40-3).
	choice := resp.Choices[0]
	// Counted through documentRelayedBytes, the same terms (and the same
	// helper) the ledger's documentOutputEstimate uses, so the client's
	// output_tokens and the row's completion_tokens for one adopted answer
	// cannot drift (2026-09-27 audit, round 41, B41-2).
	b.sse.outBytes += documentRelayedBytes(choice.Message)
	// Reasoning FIRST, then the answer — the order relayDelta emits them in,
	// and the order the Anthropic wire puts a thinking block in. This path
	// hand-writes its deltas rather than going through relayDelta, and writing
	// the answer first meant one upstream document produced two different
	// client-visible block orders depending on whether it arrived as frames or
	// as a single body (2026-09-27 audit, round 42, B42-2).
	if r := firstNonEmpty(choice.Message.Reasoning, choice.Message.ReasoningContent); r != "" {
		b.textDelta(r)
	}
	if choice.Message.Content != nil && *choice.Message.Content != "" {
		b.textDelta(*choice.Message.Content)
	}
	// A truncated fragment is not a call on this path either. A call the model
	// was still writing when it hit the token limit is dropped by the
	// non-stream path (finalize) and by the client leg on both of its paths;
	// adoption relayed it, so one identical document told the client to run a
	// tool whose input_json_delta is not JSON — arguments the model never
	// finished, handed to an agent that would parse them as a call
	// (2026-09-27 audit, round 45, B45-13). A truncated turn that carries a
	// COMPLETE call is still a call: the test is the arguments' parseability,
	// exactly as callInput's other callers ask it.
	truncated := choice.FinishReason == "length"
	for i, tc := range choice.Message.ToolCalls {
		if _, ok := callInput(firstNonEmptyStr(tc.Function.Arguments, tc.Arguments), truncated); !ok {
			continue
		}
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
	// The UNCLAMPED hit, exactly as the frame path stores it: the clamped one
	// made one upstream usage object report 1000 when the answer arrived as
	// frames and 1000 when it arrived as one whole document, and 5000 the other
	// way round -- the two framings of one answer disagreeing about the hit
	// (2026-09-27 audit, round 41, B41-1).
	b.stateCacheHit(u.detailsCachedTokens(), u.PromptCacheHitTokens)
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

// oaStreamChunk is one parsed `data:` frame of the upstream's stream. Nothing
// here is translated until a frame is in hand (see writeStream): the frame
// decides whether the stream is committed at all.
type oaStreamChunk struct {
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

// frameRelaysSomething reports whether one frame of the upstream's stream holds
// anything the client will be shown: text, reasoning, or a tool-call fragment
// that states a name (the block opens on its name) or arguments (a call the
// upstream never names has its arguments relayed as TEXT by finishStream, so
// they are shown too — round 39, B-F8). It is the question message_start's
// commitment turns on (see writeStream).
//
// Not documentSaysSomething: that predicate answers for a whole completion this
// bridge HOLDS, where a nameless call relays nothing, and it is asked by the
// ledger before the turn is written. This one answers for one frame mid-stream,
// where a nameless call's arguments do reach the client.
func frameRelaysSomething(chunk oaStreamChunk) bool {
	for _, ch := range chunk.Choices {
		if ch.Message != nil && deltaRelaysSomething(*ch.Message) {
			return true
		}
		if deltaRelaysSomething(ch.Delta) {
			return true
		}
	}
	return false
}

// deltaRelaysSomething is frameRelaysSomething's question for one delta, in the
// fields relayDelta reads.
func deltaRelaysSomething(d oaDelta) bool {
	if firstNonEmpty(d.Reasoning, d.ReasoningContent) != "" {
		return true
	}
	if d.Content != nil && *d.Content != "" {
		return true
	}
	for _, tc := range d.ToolCalls {
		if firstNonEmptyStr(tc.Function.Name, tc.Name) != "" ||
			firstNonEmptyStr(tc.Function.Arguments, tc.Arguments) != "" {
			return true
		}
	}
	return false
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

// documentRelayedBytes is every answer byte ADOPTING this document relays, in
// the same three terms adoptWholeStream counts: content, reasoning, and each
// tool call's argument JSON. One function for both, because the two must be the
// same number — the ledger's estimate read only content (falling back to
// reasoning when content was empty), so a tool-call answer and a
// content-beside-reasoning answer were booked with completion_tokens=0 and 2
// while the client was told 13 for the same turn (2026-09-27 audit, round 41,
// B41-2).
func documentRelayedBytes(msg oaDelta) int {
	bytes := 0
	if msg.Content != nil {
		bytes += len(*msg.Content)
	}
	bytes += len(firstNonEmpty(msg.Reasoning, msg.ReasoningContent))
	for _, tc := range msg.ToolCalls {
		bytes += len(firstNonEmptyStr(tc.Function.Arguments, tc.Arguments))
	}
	return bytes
}

// documentSaysSomething reports whether a whole completion holds anything a
// turn would relay: text, reasoning, or a call the client could run. It is
// nothingRelayed's question asked of a document the bridge is still holding
// rather than of the stream, and every caller must ask it together — the
// ledger's status before finalize, adoptWholeStream itself, and the non-stream
// path, which answers the same body — or two readers of one turn see it two
// ways (2026-09-27 audit, round 43, B43-1; round 44, B44-3).
//
// A call counts only when it NAMES itself. content_block_start is the only
// event that carries a tool name, so the stream path holds a nameless fragment
// and opens no block for it, relaying its arguments as text at most; counting
// such a call here said "this document relays something" about one that relays
// nothing, which is the state round 42's empty-stream arm exists to refuse —
// the client was handed a 200 success whose stream carried no block at all,
// while the non-stream path answered the identical document with a nameless
// tool_use block (2026-09-27 audit, round 44, B44-5). The note this replaces,
// that a call counts even with no arguments, still holds for a call that is
// NAMED: an argument-less call is one the client can make.
//
// Text is measured the way the block that reaches the client is built — a
// non-empty string — and not by documentRelayedBytes. Not with TrimSpace:
// relayDelta relays a content of whitespace as a text block, so a document
// holding one is a document the stream would carry a block for, and trimming
// here refused a turn the other two legs answer, on a body whose text the
// upstream did write (2026-09-27 audit, round 44).
func documentSaysSomething(doc openAICompletion) bool {
	if len(doc.Choices) == 0 {
		return false
	}
	m := doc.Choices[0].Message
	if m.Content != nil && *m.Content != "" {
		return true
	}
	if firstNonEmpty(m.Reasoning, m.ReasoningContent) != "" {
		return true
	}
	for _, tc := range m.ToolCalls {
		if strings.TrimSpace(firstNonEmptyStr(tc.Function.Name, tc.Name)) != "" {
			return true
		}
	}
	return false
}

// documentOutputEstimate is outputEstimate's arm for a document the bridge is
// still holding: the same number finalize() will put in the client's
// output_tokens for it, worked out from the document's own text rather than
// from a counter that is only written later (2026-09-27 audit, round 40,
// B40-2). Empty text is no answer to measure, and reports 0 — finalize()'s
// outBytes is the same sum, so the two agree there too.
func (b *anthropicBridge) documentOutputEstimate(doc openAICompletion) int {
	if len(doc.Choices) == 0 {
		return 0
	}
	bytes := documentRelayedBytes(doc.Choices[0].Message)
	if bytes <= 0 {
		return 0
	}
	return bytes/4 + 1
}

// nothingRelayed reports whether the stream has said nothing yet — no block
// opened and no text held — which is the condition for adopting a whole
// completion that arrives inside a frame as the turn itself.
//
// A call counts only when it NAMES itself, which is the rule the document twin
// states outright (documentSaysSomething): a fragment that states arguments
// without a name is held until a name arrives, because content_block_start is
// the only event that carries one and a start without it can never be
// corrected (startToolBlock) — so counting the block made a stream that relayed
// NOTHING read as one that had said something. It set `finished` on an empty
// `message` frame carrying such a fragment (so an unterminated stream was
// relayed as the model's complete answer), and it defeated the `[DONE]` and
// empty-stream arms, which then booked and metered a turn with no block, no
// text and no call — the byte-identical outcome with no frames at all is a 502
// (2026-09-27 audit, round 47, G47-3).
func (b *anthropicBridge) nothingRelayed() bool {
	return b.nextIdx == 0 && b.heldText.Len() == 0 && b.callsCarryingOutput() == 0
}

// callsCarryingOutput counts the tool calls that will reach the client as
// something — the blocks that NAME themselves, and the unnamed ones whose
// arguments are relayed as text at the end of the turn (finishStream).
//
// A call with neither a name nor arguments is not one of them: content_block_start
// is the only event that carries a name and a start without one can never be
// corrected (startToolBlock holds the block instead), so such a fragment relays
// nothing at all — and counting the BLOCK rather than its content made a stream
// that said nothing read as one that had said something. It set `finished` on an
// EMPTY `message` frame that carried such a fragment, so a stream that then
// ended with no finish_reason and no [DONE] was relayed as the model's complete
// answer; and it defeated the `[DONE]` and empty-stream arms, which booked and
// metered a turn with no block, no text and no call — while the byte-identical
// outcome with no frames at all is a 502. The arguments case is why this counts
// content and not names alone: round 39's B-F8 requires the raw arguments of a
// call the upstream never named to stay readable to the client, so that turn is
// NOT empty (2026-09-27 audit, round 47, G47-3).
func (b *anthropicBridge) callsCarryingOutput() int {
	n := 0
	for _, tb := range b.toolOrder {
		if tb.name != "" || tb.args.Len() > 0 {
			n++
		}
	}
	return n
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

// callInput resolves one stated call's arguments into the object the client
// executes, on the rule the client leg applies to the same wire — and the rule
// answers two questions at once: what the block's input is, and whether the
// fragment is a call at all.
//
//   - Arguments that parse into an object are that object.
//   - Arguments that do not parse on a turn the upstream TRUNCATED are not a
//     call: the model was still writing when it hit the token limit, and
//     emitting it would hand the agent a complete, executable tool_use whose
//     input is a key the model never wrote — while the turn reports tool_use
//     and hides the "length" the upstream stated. The client leg drops it for
//     exactly this reason, on both of its paths
//     (cmd/launch/anthropic_openai_proxy.go, rounds 16 and 17), and this leg
//     used to answer the same fragment with an empty object and tool_use
//     (2026-09-27 audit, round 44, C44-1 / B44-2).
//   - Arguments that do not parse otherwise are freeform text from a model that
//     does not emit JSON, kept under "_raw" so the call is not lost — the
//     sibling leg's fallback, which this leg did not have: it emitted {} and
//     dropped what the model wrote (2026-09-27 audit, round 44, C44-5).
//
// A call with no arguments at all is a call: the empty string is a complete
// argument list for a tool that takes none.
func callInput(raw string, truncated bool) (map[string]any, bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		s = "{}"
	}
	var in map[string]any
	if err := json.Unmarshal([]byte(s), &in); err != nil {
		if truncated {
			return nil, false
		}
		return map[string]any{"_raw": s}, true
	}
	if in == nil {
		// The text is the JSON literal null, which unmarshals into a nil map
		// without an error. The block's input is a required object on this
		// wire, and the id this bridge minted for the call says so already
		// (canonicalCallArgs folds null into the empty object, as both other
		// legs do): emitting "input":null contradicted the very id beside it
		// (2026-09-27 audit, round 48, C-F5).
		in = map[string]any{}
	}
	return in, true
}

// mintedToolCallID returns the id for a call this bridge has to number itself,
// keyed on the call's own identity — its name and its arguments — and distinct
// for the second and later call that states the same identity.
//
// The identity alone is not enough, and round 44's fix for the block the split
// rule mints rested on it: a model that runs one command twice sends two calls
// with the same name and the same arguments, which hashed to one id — two
// tool_use blocks the client can answer only once, and an OpenAI wire that
// cannot key two tool_results by one tool_call_id. This is the shape round 39's
// B-F9 rule is about (a bare repeat is a second call), so the pair is kept as
// two blocks and the second takes the identity of its ordinal, exactly as the
// local leg's converter does for a list that names the same id-less call twice
// (anthropic.go, seenInCall: "\x00"+name+"\x00"+args, then that with "#"+n)
// (2026-09-27 audit, round 45, B45-4 and C45-2).
func (b *anthropicBridge) mintedToolCallID(name, args string) string {
	args = canonicalCallArgs(args)
	base := "\x00" + name + "\x00" + args
	n := b.mintedIDs[base]
	b.mintedIDs[base] = n + 1
	seed := args
	if n > 0 {
		seed = args + "#" + strconv.Itoa(n)
	}
	id := gatewayToolCallIDFor(name, seed)
	// A minted id must not be one a call already carries. The "#n" suffix the
	// repeat rule appends is part of the CALL's own text as far as this hash can
	// tell, so an upstream whose second call really states the arguments
	// "x#1" and an upstream that merely repeats "x" both hash to the same id —
	// two blocks the client can answer only once. The suffix is bumped until
	// the id is one no call in this turn holds (2026-09-27 audit, round 46,
	// G45-5).
	//
	// The bump seed is spelled "\x00#k", the shape the local and client legs
	// bump with (anthropic.go, anthropic_openai_proxy.go), because
	// gatewayToolCallIDFor promises the same id for the same call whichever leg
	// mints it — a tool_result written against one leg's answer stays valid if
	// the retry goes through another (2026-09-27 audit, round 47, G47-4).
	// An id the wire STATES is reserved before any mint is taken (reservedIDs,
	// filled by the non-stream list from the calls it is about to walk), so an
	// id-less call EARLY in the list is not numbered with the id a LATER call
	// states: this mint walks in list order, so without the reservation the
	// first call took the second call's own id and the second call was
	// renumbered, while the client leg and the local converter both keep the
	// stated id and renumber the minted one — the answer depended on which call
	// the upstream happened to write first (2026-09-27 audit, round 48, B-F4).
	for k := 0; b.grantedIDs[id] || b.reservedIDs[id]; k++ {
		id = gatewayToolCallIDFor(name, seed+"\x00#"+strconv.Itoa(k))
	}
	b.grantedIDs[id] = true
	return id
}

// gatewayToolCallIDFor mints an id for a tool call from the call's own identity
// — its name and its arguments.
//
// It is the same function the local and client legs mint with
// (anthropic.ToolCallIDFor, FNV-1a over "\x00"+name+"\x00"+arguments), copied
// rather than imported because this gateway is its own module. Keeping the
// shape identical matters: the same call reaching the client through either leg
// carries the same id, so a tool_result written against one leg's answer is
// still a valid id if the retry goes through the other (2026-09-27 audit,
// round 44, B44-1).
//
// Two calls sharing one identity are distinguished by the caller, not here:
// this function is a pure function of what it is given, which is what makes it
// reproducible, and mintedToolCallID is the rule that keeps two identical calls
// apart.
func gatewayToolCallIDFor(name, argsJSON string) string {
	var h uint32 = 2166136261
	for _, c := range []byte("\x00" + name + "\x00" + argsJSON) {
		h ^= uint32(c)
		h *= 16777619
	}
	return "call_" + strconv.FormatUint(uint64(h), 16)
}

// stopReason maps an OpenAI finish_reason to an Anthropic stop_reason, in the
// terms the turn's own blocks decide — leg 1's mapStopReason, which is the rule
// the local leg answers with and which the client-side proxy mirrors.
//
// A turn that carries a call is tool_use whatever the upstream said, including
// "length": the call is in the body the client just read, so it can execute it,
// and the two legs Claude Code talks to say tool_use for exactly this body
// (2026-09-27 audit, round 43, C43-4). A turn carrying none is NOT tool_use even
// when the upstream stopped to call one: "tool_use" promises the client a
// tool_use content block, and an agent that reads it waits for a call that is
// not in the message — the stalled turn the stream path has guarded against
// since round 39 (B-F8), and which this arm answered anyway on the non-stream
// path (2026-09-27 audit, round 43, C43-3).
func stopReason(finishReason string, toolBlocks int) string {
	if toolBlocks > 0 {
		return "tool_use"
	}
	switch finishReason {
	case "length":
		return "max_tokens"
	default:
		// "tool_calls", "function_call", "stop", "" and anything unknown: the
		// turn ended. "stop_sequence" is never produced — nothing in this pipe
		// records the sequence that matched.
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
		// Message is the streaming reader's own type for one assistant turn
		// (oaDelta), not a second anonymous struct with the same fields: the
		// wire states a whole completion's message and a streamed delta with
		// the same names and both spellings of a tool call, and one type is
		// what keeps the two readings from drifting -- the ledger's estimate of
		// an adopted answer has to be the same number the client is told
		// (2026-09-27 audit, round 41, B41-2).
		Message oaDelta `json:"message"`
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
