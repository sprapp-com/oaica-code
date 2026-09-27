package main

// round42_bridge_integrity_test.go — round 42's findings on the gateway's
// /v1/messages bridge (B42-1..B42-8, C42-1, C42-3, C42-4, C42-5, C42-6), one
// test per finding:
//
//   - B42-1: an answer adopted from a whole completion sized its output_tokens
//     by the content alone, so a tool-only turn told the client 0 where the
//     row recorded the call's real size.
//   - B42-2: the adopted path hand-wrote its deltas answer-first where
//     relayDelta writes reasoning first, so one upstream document produced two
//     client-visible block orders.
//   - B42-3: a stream whose frames relayed nothing at all was booked and
//     relayed as a successful empty turn.
//   - B42-4: tool_choice was matched case-sensitively, so `"NONE"` forwarded
//     the whole tool surface the client had just refused.
//   - B42-5/C42-6: a message content that is neither a string nor an array was
//     answered with content:"" — an empty user turn the client read as its own
//     prompt, where the sibling refuses the body.
//   - B42-6: max_tokens was accepted as null or as a quoted number.
//   - B42-7/C42-3: a tool_result text block or a search passage whose text is
//     not a string was marshalled into the prompt as though the tool or the
//     search had returned that JSON.
//   - B42-8: a non-object element in the tool list was skipped, serving a turn
//     with a tool the client did send missing.
//   - C42-1: the image walk charged a body the bridge built itself (a concrete
//     []map[string]any, not []any) its whole base64 — a 1 MB screenshot read as
//     250 023 prompt tokens on this leg and ~1 050 on the other two.
//   - C42-4: an image source this wire cannot express (base64 with no data, or
//     an unknown type) was answered with a placeholder the model read as the
//     tool's answer.
//   - C42-5: a mid-conversation system message reached the backend in place,
//     where the template refuses it and the client leg hoists it.
//   - C42-6: a tool_use input that is not a JSON object was re-marshalled as
//     the raw string the client leg refuses.

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round42Ask builds one /v1/messages body around a message list.
func round42Ask(messages string) string {
	return `{"model":"kat-awq","max_tokens":64,"messages":[` + messages + `]}`
}

// round42AskStream is round42Ask for the findings that live on the STREAM path:
// the adopted whole completion and the frame relay are what those pins are
// about, and a non-stream request never reaches either.
func round42AskStream(messages string) string {
	return `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[` + messages + `]}`
}

// round42BodyWithImages is round41Body against a gateway whose model accepts
// images, so the image-carrying arms of the converter run at all — the ones
// C42-4's two refusals live behind.
func round42BodyWithImages(t *testing.T, body, reply string) (map[string]any, int) {
	t.Helper()
	sent := make(chan map[string]any, 1)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req map[string]any
		if json.Unmarshal(raw, &req) == nil {
			select {
			case sent <- req:
			default:
			}
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, reply)
	}))
	defer upstream.Close()

	srv, _ := round39Gateway(t, upstream, []string{"text", "image"})
	status, _ := round40Post(t, srv, body)
	select {
	case req := <-sent:
		return req, status
	default:
		return nil, status
	}
}

// TestAToolOnlyAdoptedAnswerIsSizedLikeItsRow is B42-1. The client's
// output_tokens and the row's completion_tokens are two readings of one answer,
// and the answer's size is what the wire carries — a tool call, not just text.
func TestAToolOnlyAdoptedAnswerIsSizedLikeItsRow(t *testing.T) {
	up := round40JSONReply(`{"id":"c1","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/x\"}"}}]}}],"usage":{"prompt_tokens":9,"completion_tokens":0}}`)
	defer up.Close()
	srv, ledger := round39Gateway(t, up, nil)

	status, stream := round40Post(t, srv, round42AskStream(`{"role":"user","content":"go"}`))
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\n%s", status, stream)
	}
	_, _, out := round40Delta(t, stream)
	rows := waitLedger(t, ledger, 1)
	if len(rows) == 0 {
		t.Fatal("no ledger row")
	}
	if out <= 0 {
		t.Errorf("a tool-only answer was relayed with output_tokens=%d: the call is the answer's size, and a session that reads 0 never compacts (%d bytes of it reached the client)", out, rows[0].CompletionTokens)
	}
	if out != rows[0].CompletionTokens {
		t.Errorf("the client was told output_tokens=%d and the row for the same turn records %d: one answer, and the same terms must size both", out, rows[0].CompletionTokens)
	}
}

// TestReasoningPrecedesTheAnswerOnBothPaths is B42-2: a document that arrives
// as one body and the same document arriving as frames are one answer, so the
// client must read one block order.
func TestReasoningPrecedesTheAnswerOnBothPaths(t *testing.T) {
	pos := func(stream, sub string) int { return strings.Index(stream, `"text":"`+sub+`"`) }

	adopted := round40JSONReply(`{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ANSWER","reasoning":"THINK"}}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`)
	defer adopted.Close()
	srvA, _ := round39Gateway(t, adopted, nil)
	_, streamA := round40Post(t, srvA, round42AskStream(`{"role":"user","content":"go"}`))

	frames := round40JSONReply(`{"id":"c1","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ANSWER"}}],"usage":{"prompt_tokens":9,"completion_tokens":2}}`)
	defer frames.Close()
	srvF, _ := round39Gateway(t, frames, nil)
	_, streamF := round40Post(t, srvF, round42AskStream(`{"role":"user","content":"go"}`))

	a, th := pos(streamA, "ANSWER"), pos(streamA, "THINK")
	if th < 0 {
		t.Fatalf("the adopted answer's reasoning never reached the client:\n%s", streamA)
	}
	if a < th {
		t.Errorf("the adopted answer relayed ANSWER at %d and THINK at %d — answer first, where the frame path relays reasoning first (ANSWER@%d THINK@%d): one document, two client-visible block orders", a, th, pos(streamF, "ANSWER"), pos(streamF, "THINK"))
	}
}

// TestAStreamThatRelaysNothingIsNotASuccess is B42-3. Frames arrived and said
// nothing: no block opened, no text held, no call named. Booked as a 200 with a
// closing message_stop, the client reads a completed empty turn and never
// retries — which is what the 502 for the byte-identical `[DONE]`-only body is
// for.
func TestAStreamThatRelaysNothingIsNotASuccess(t *testing.T) {
	// The upstream answers with SSE frames that carry no chunk at all: the
	// first `data:` line opens the message, and nothing after it says anything.
	srv, _ := round39Gateway(t, garbageFrameUpstream(t), nil)
	status, stream := round40Post(t, srv, round42AskStream(`{"role":"user","content":"go"}`))

	if status == http.StatusOK && !strings.Contains(stream, `"type":"error"`) {
		t.Errorf("a stream whose frames relayed nothing was answered as a SUCCESS (status %d): the client reads a completed empty turn\n%s", status, stream)
	}
	if status == http.StatusOK && strings.Contains(stream, "message_stop") {
		t.Errorf("the empty stream was closed with message_stop as though it were a turn:\n%s", stream)
	}
}

// TestToolChoiceNoneIsMatchedInAnySpelling is B42-4: the type is normalized the
// way the sibling normalizes it, case- and space-insensitively, before
// anything is read from it.
func TestToolChoiceNoneIsMatchedInAnySpelling(t *testing.T) {
	tools := `"tools":[{"name":"Bash","description":"run","input_schema":{"type":"object"}}],`
	for _, spelling := range []string{`"none"`, `"NONE"`, `" none "`, `"None"`} {
		t.Run(spelling, func(t *testing.T) {
			req, status := round41Body(t, `{"model":"kat-awq","max_tokens":64,`+tools+`"tool_choice":{"type":`+spelling+`},"messages":[{"role":"user","content":"go"}]}`, round41Reply)
			if status != http.StatusOK {
				t.Fatalf("premise: refused (%d)", status)
			}
			if v, ok := req["tools"]; ok {
				t.Errorf("tool_choice %s still sent the tool definitions (%v): the model can call a tool the client ruled out", spelling, v)
			}
		})
	}
}

// TestAMessageContentThatIsNoShapeIsRefused is B42-5/C42-6. A content that is
// neither a string nor a block array is not a shape this wire carries; answered
// with content:"" the model was handed an empty user turn and the client read a
// successful answer to a prompt it never sent. A JSON null is the one
// non-string, non-array value the sibling accepts — it decodes to empty content
// there too — and it stays accepted.
func TestAMessageContentThatIsNoShapeIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, content string }{
		{"object", `{"a":1}`},
		{"number", `42`},
		{"boolean", `true`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, status := round41Body(t, round42Ask(`{"role":"user","content":`+tc.content+`}`), round41Reply)
			if status == http.StatusOK {
				t.Errorf("a message whose content is a %s was answered 200 (converted: %v): the model was handed a turn the client never wrote", tc.name, req)
			}
		})
	}

	req, status := round41Body(t, round42Ask(`{"role":"user","content":null}`), round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: a null content is the one shape the sibling accepts and it was refused (%d)", status)
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("a null content converted to %v, want the one empty user turn the sibling decodes it to", msgs)
	}
}

// TestMaxTokensMustBeAPositiveInteger is B42-6: the field is required and
// numeric on this wire, so a null (which the sibling decodes to 0 and refuses)
// and a quoted number (which does not decode at all) are refused here too —
// while a body this process built itself, whose max_tokens is a Go int rather
// than a JSON float, is a positive count and is served.
func TestMaxTokensMustBeAPositiveInteger(t *testing.T) {
	for _, tc := range []struct{ name, field string }{
		{"null", `"max_tokens":null`},
		{"quoted number", `"max_tokens":"64"`},
		{"zero", `"max_tokens":0`},
		{"negative", `"max_tokens":-5`},
		{"fractional", `"max_tokens":16.5`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, status := round41Body(t, `{"model":"kat-awq",`+tc.field+`,"messages":[{"role":"user","content":"go"}]}`, round41Reply); status == http.StatusOK {
				t.Errorf("max_tokens as %s was answered 200: the upstream reads it as an output cap the client never stated", tc.name)
			}
		})
	}

	// The in-process shape: a Go int, which is what the clamps in this file
	// write back into the request. It says exactly what this wire means.
	out, errStr := anthropicToOpenAI(map[string]any{
		"model": "kat-awq", "max_tokens": 64,
		"messages": []any{map[string]any{"role": "user", "content": "go"}},
	}, false)
	if errStr != "" {
		t.Fatalf("a Go int max_tokens was refused: %s — a body this process built itself carries an int, not a float64", errStr)
	}
	if got := out["max_tokens"]; got != 64 && got != float64(64) {
		t.Errorf("max_tokens reached the upstream as %#v, want 64", got)
	}
}

// TestANonStringTextIsNotAPassage is B42-7/C42-3: a tool_result text block and
// a search passage whose `text` is not a string carry no text — the sibling
// writes nothing for them in both places it reads one, where this leg
// marshalled the block into the prompt as though the tool or the search had
// returned that JSON.
func TestANonStringTextIsNotAPassage(t *testing.T) {
	for _, tc := range []struct{ name, messages string }{
		{
			"tool_result text block holding a number",
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"text","text":42}]}]}`,
		},
		{
			"search passage holding a number",
			`{"role":"user","content":[{"type":"search_result","title":"t","content":[{"type":"text","text":null}]}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, status := round41Body(t, round42Ask(tc.messages), round41Reply)
			if status != http.StatusOK {
				t.Fatalf("premise: refused (%d)", status)
			}
			raw, _ := json.Marshal(req["messages"])
			if strings.Contains(string(raw), `\":42`) || strings.Contains(string(raw), `"text":42`) ||
				strings.Contains(string(raw), `\":null`) || strings.Contains(string(raw), `"text":null`) {
				t.Errorf("the block's JSON was carried into the prompt as text the tool never returned:\n%s", raw)
			}
		})
	}

	// The control: a real passage still reaches the prompt.
	req, status := round41Body(t, round42Ask(`{"role":"user","content":[{"type":"search_result","title":"t","content":[{"type":"text","text":"real passage"}]}]}`), round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: refused (%d)", status)
	}
	raw, _ := json.Marshal(req["messages"])
	if !strings.Contains(string(raw), "real passage") {
		t.Errorf("the real passage was lost:\n%s", raw)
	}
}

// TestANonObjectToolIsRefused is B42-8. Skipping the element served a turn with
// one of the client's tools missing from the model's surface, where the
// sibling's typed list refuses the whole request on the same element.
func TestANonObjectToolIsRefused(t *testing.T) {
	body := round42Ask(`{"role":"user","content":"go"}`)
	body = strings.TrimSuffix(body, "}") + `,"tools":[{"name":"Bash","input_schema":{"type":"object"}},"stray-string"]}`
	if _, status := round41Body(t, body, round41Reply); status == http.StatusOK {
		t.Errorf("a tool list holding a non-object element was served: the model was given a tool surface the client did not send")
	}
}

// TestANonObjectToolInputIsRefused is C42-6. The sibling decodes this field
// into a JSON object and fails the whole request on anything else;
// re-marshalling it here wrote the raw string back as a JSON string, so a call
// the client leg refuses reached the model with arguments no tool can parse.
func TestANonObjectToolInputIsRefused(t *testing.T) {
	body := round42Ask(`{"role":"assistant","content":[{"type":"tool_use","id":"call_1","name":"Bash","input":"{\"cmd\":\"ls\"}"}]}`)
	if _, status := round41Body(t, body, round41Reply); status == http.StatusOK {
		t.Errorf("a tool_use whose input is a STRING was served: the model was handed a call whose arguments no tool can parse")
	}
}

// TestAnImageSourceThisWireCannotCarryIsRefused is C42-4. Both of these shapes
// are refused outright by the sibling, in words; answered here with a
// placeholder, the model read the placeholder as the tool's own answer.
func TestAnImageSourceThisWireCannotCarryIsRefused(t *testing.T) {
	for _, tc := range []struct{ name, source string }{
		{"base64 with no data", `{"type":"base64","media_type":"image/png"}`},
		{"unknown source type", `{"type":"weird","data":"QUJDRA=="}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := round42Ask(`{"role":"user","content":[{"type":"tool_result","tool_use_id":"t1","content":[{"type":"image","source":` + tc.source + `}]}]}`)
			if _, status := round42BodyWithImages(t, body, round41Reply); status == http.StatusOK {
				t.Errorf("an image source (%s) this wire cannot express was served: the model was told the tool returned the placeholder", tc.name)
			}
		})
	}
}

// TestAMidConversationSystemMessageIsHoisted is C42-5 here: the backend is
// handed one leading system message, the shape a strict chat template accepts
// and the shape the client leg sends for the same body.
func TestAMidConversationSystemMessageIsHoisted(t *testing.T) {
	body := `{"model":"kat-awq","max_tokens":64,"system":"top","messages":[{"role":"user","content":"go"},{"role":"system","content":"late"}]}`
	req, status := round41Body(t, body, round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: refused (%d)", status)
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 2 {
		t.Fatalf("converted messages = %v, want a system message then the user turn", msgs)
	}
	first, _ := msgs[0].(map[string]any)
	if first["role"] != "system" {
		t.Fatalf("the first message is %v, want the system message: a template that refuses a late system message answers this body with a 500", msgs)
	}
	if got, _ := first["content"].(string); got != "top\n\nlate" {
		t.Errorf("the system content reached the backend as %q, want both texts joined with a blank line", got)
	}
	if second, _ := msgs[1].(map[string]any); second["role"] != "user" {
		t.Errorf("the user turn did not follow the system message: %v", msgs)
	}
}

// An already-ordered conversation is untouched: several leading system messages
// stay several, because merging them re-renders a prompt the client sent.
func TestAnOrderedConversationIsNotReRenderedAtTheBridge(t *testing.T) {
	body := `{"model":"kat-awq","max_tokens":64,"system":"first","messages":[{"role":"system","content":"second"},{"role":"user","content":"go"}]}`
	req, status := round41Body(t, body, round41Reply)
	if status != http.StatusOK {
		t.Fatalf("premise: refused (%d)", status)
	}
	msgs, _ := req["messages"].([]any)
	if len(msgs) != 3 {
		t.Fatalf("converted messages = %v, want three (two system messages and the turn)", msgs)
	}
	for i, want := range []string{"first", "second"} {
		m, _ := msgs[i].(map[string]any)
		if got, _ := m["content"].(string); m["role"] != "system" || got != want {
			t.Errorf("message %d = %v, want a system message carrying %q", i, m, want)
		}
	}
}

// TestABridgeBuiltBodyChargesTheImageAllowance is C42-1, and the shape of the
// bug is in the container: the bridge hands the image walk a CONCRETE
// []map[string]any, which matches neither []any nor map[string]any, so every
// part under it fell through and a 1 MB screenshot was charged its whole
// base64 — 250 023 prompt tokens on this leg against ~1 050 on the other two,
// while the same body decoded from JSON (whose arrays ARE []any) measured
// correctly.
func TestABridgeBuiltBodyChargesTheImageAllowance(t *testing.T) {
	data := strings.Repeat("QUJDRA", 175000) // ~1 MB of base64
	messages := []map[string]any{{
		"role": "user",
		"content": []any{
			map[string]any{"type": "text", "text": "look"},
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + data}},
		},
	}}
	built := map[string]any{"model": "kat-awq", "max_tokens": 64, "messages": messages}

	// The same body as it arrives on the wire: decoded, its arrays ARE []any.
	raw, err := json.Marshal(built)
	if err != nil {
		t.Fatal(err)
	}
	var decoded map[string]any
	if err := json.Unmarshal(raw, &decoded); err != nil {
		t.Fatal(err)
	}

	fromBridge := messagesBytes(built)
	fromWire := messagesBytes(decoded)
	if d := fromBridge - fromWire; d > 8 || d < -8 {
		t.Errorf("the bridge's own body measures %d bytes and the same body decoded from JSON measures %d: the image walk charges the concrete []map[string]any a different price than the []any it becomes, so the bridge's clients are billed the base64\n", fromBridge, fromWire)
	}
	if fromBridge > 6000 {
		t.Errorf("a 1 MB inline screenshot measured %d prompt bytes through the bridge: the allowance for an inline image is %d, and charging the base64 read this turn as %d tokens", fromBridge, imagePartByteAllowance, fromBridge/4)
	}
}

// TestTheSameBodyMeasuresTheSameThroughBothShapes is C42-1's invariant stated
// the way the product states it: three measures, one body, one answer.
func TestTheSameBodyMeasuresTheSameThroughBothShapes(t *testing.T) {
	data := strings.Repeat("QUJDRA", 175000)
	msg := map[string]any{
		"role": "user",
		"content": []any{
			map[string]any{"type": "image_url", "image_url": map[string]any{"url": "data:image/png;base64," + data}},
		},
	}
	concrete := map[string]any{"model": "m", "messages": []map[string]any{msg}}
	generic := map[string]any{"model": "m", "messages": []any{msg}}

	// promptPayloadBytes takes the serialized size of the message list beside
	// the decoded value it discounts, so the two are measured the way the
	// admission gate measures them.
	nc, okC := jsonMeasuredBytes(concrete["messages"])
	ng, okG := jsonMeasuredBytes(generic["messages"])
	if !okC || !okG {
		t.Fatal("premise: the message lists are not measurable")
	}
	a, b := promptPayloadBytes(nc, concrete["messages"]), promptPayloadBytes(ng, generic["messages"])
	if a != b {
		t.Errorf("the concrete container is charged %d bytes and the []any %d: the walk must not depend on which Go type carries the same body", a, b)
	}
	// The allowance replaces the payload; the JSON envelope around the data URI
	// is still charged, and it is tens of bytes.
	if a < imagePartByteAllowance || a > imagePartByteAllowance+400 {
		t.Errorf("one inline image measured %d bytes, want the %d allowance plus the small envelope that carries it", a, imagePartByteAllowance)
	}
}

// garbageFrameUpstream answers with SSE frames that carry no chunk at all,
// ending at the sentinel.
func garbageFrameUpstream(t *testing.T) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		f := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"not\":\"a chunk\"}\n\n")
		f.Flush()
		fmt.Fprint(w, "data: {oops not json\n\n")
		f.Flush()
		fmt.Fprint(w, "data: [DONE]\n\n")
		f.Flush()
	}))
}
