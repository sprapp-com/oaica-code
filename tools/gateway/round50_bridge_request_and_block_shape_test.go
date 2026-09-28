package main

// round50_bridge_request_and_block_shape_test.go — round 50's findings on the
// gateway leg. Every one was reproduced on the unmodified tree before it was
// fixed, and each is stated as the verdict or the upstream request the two
// sibling legs (anthropic/anthropic.go, cmd/launch/anthropic_openai_proxy.go)
// give the same body.
//
// B-F1: an assistant turn whose content mixes text and an image, and then
// carries more text after a tool_use, lost everything but the last text. The
// rebuild in putContent read the message's existing content back with a type
// assertion the writer never produces (`[]map[string]any`, where the writer
// stores `[]any`), so the parts already accumulated were dropped and the new
// string was appended to an empty array as a BARE STRING — not a text part.
//
// B-F2: a truncated named call — the model stopped mid-arguments, so the block
// never opened on a fragment and finishStream opened it — had no
// content_block_stop emitted. The client held an unterminated tool_use block
// for every such turn, and this is the shape a client-side accumulator needs to
// finish a call.
//
// C50-2: a message with no content key at all was served with content:"". Both
// sibling legs refuse it (their decode of an absent content fails), so one body
// got a 200 here and a 400 there. An absent key is not a statement; `null` is,
// and stays the empty content both siblings read it as.
//
// C50-3: `thinking` and `tool_choice` of the wrong shape (a bool, a string, a
// number) were ignored in silence, so the body was served with the control
// dropped. Both sibling legs decode these into typed structs and 400.

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"testing"
)

// round50BlockEvents reports, per content block index, whether the stream
// opened and closed it. A client-side accumulator cannot finish a block it was
// never told ended.
func round50BlockEvents(t *testing.T, stream string) (opened, closed map[int]bool) {
	t.Helper()
	opened, closed = map[int]bool{}, map[int]bool{}
	for _, line := range strings.Split(stream, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Index int    `json:"index"`
		}
		if json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &ev) != nil {
			continue
		}
		switch ev.Type {
		case "content_block_start":
			opened[ev.Index] = true
		case "content_block_stop":
			closed[ev.Index] = true
		}
	}
	return opened, closed
}

// TestAssistantPartsSurviveALaterTextBlock is B-F1. An assistant turn is one
// message carrying its text and its image parts; a later text block appends to
// that message and must not discard what is already on it.
func TestAssistantPartsSurviveALaterTextBlock(t *testing.T) {
	out, err := contentBlocksToOpenAI("assistant", []any{
		map[string]any{"type": "text", "text": "before"},
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "media_type": "image/png", "data": "aGk=",
		}},
		map[string]any{"type": "tool_use", "id": "call_1", "name": "a", "input": map[string]any{}},
		map[string]any{"type": "text", "text": "after"},
	}, true)
	if err != "" {
		t.Fatalf("the turn must convert: %s", err)
	}
	if len(out) != 1 {
		t.Fatalf("the turn converted to %d messages, want 1 — an assistant's text, images and calls are one message:\n%#v", len(out), out)
	}
	parts, ok := out[0]["content"].([]any)
	if !ok {
		t.Fatalf("content is %#v, want the parts array of a mixed turn", out[0]["content"])
	}
	var kinds []string
	for _, p := range parts {
		pm, _ := p.(map[string]any)
		kind, _ := pm["type"].(string)
		if kind == "" {
			t.Fatalf("a part of the array is not an object with a type: %#v (content %#v)", p, out[0]["content"])
		}
		kinds = append(kinds, kind)
	}
	want := []string{"text", "image_url", "text"}
	if strings.Join(kinds, ",") != strings.Join(want, ",") {
		t.Fatalf("the parts are %v, want %v — a text block after the call discarded the parts already on the message", kinds, want)
	}
	first, _ := parts[0].(map[string]any)
	if first["text"] != "before" {
		t.Errorf(`parts[0].text = %#v, want "before"`, first["text"])
	}
	last, _ := parts[2].(map[string]any)
	if last["text"] != "after" {
		t.Errorf(`parts[2].text = %#v, want "after"`, last["text"])
	}
	if tcs, ok := out[0]["tool_calls"].([]map[string]any); !ok || len(tcs) != 1 {
		t.Errorf("the call was lost: tool_calls = %#v", out[0]["tool_calls"])
	}
}

// TestATruncatedCallClosesItsBlock is B-F2. Every block the stream opens is
// closed, including the one opened for a call whose arguments the upstream
// never finished.
func TestATruncatedCallClosesItsBlock(t *testing.T) {
	for _, finish := range []string{"tool_calls", "length", "stop"} {
		t.Run(finish, func(t *testing.T) {
			up := round45Frames(t,
				`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"a","arguments":"{\"x\":"}}]}}]}`,
				`data: {"choices":[{"delta":{},"finish_reason":"`+finish+`"}]}`,
				`data: [DONE]`,
			)
			srv, _ := round39Gateway(t, up, nil)
			status, body := round45Ask(t, srv, round45AskStream)
			if status != http.StatusOK {
				t.Fatalf("status %d\n%s", status, body)
			}
			opened, closed := round50BlockEvents(t, body)
			var idx []int
			for i := range opened {
				idx = append(idx, i)
			}
			sort.Ints(idx)
			if finish == "length" {
				// REVERSED 2026-09-28 (round 80, F80-L3-3). B-F2 was written when
				// this arm had no other answer: the block was opened for a call the
				// upstream cut off, and leaving it unterminated was the failure it
				// pinned. Both other arms of the same body DROP that call — the
				// non-stream path and the adoption arm through callInput, which
				// refuses an unparseable argument text on a truncated turn — and
				// answer the same turn with no tool block and a stop_reason of
				// max_tokens. Opening it here reached the client as half an object
				// under a stop_reason of tool_use, the very split rounds 55 and 74
				// closed for every other shape. The block is no longer opened, so
				// "every block that IS opened is closed" holds vacuously here; the
				// promise B-F2 makes is kept, by opening nothing.
				if len(idx) != 0 {
					t.Fatalf("the truncated call was opened as %d block(s); the document arms of this leg open none:\n%s", len(idx), body)
				}
				if !strings.Contains(body, `"stop_reason":"max_tokens"`) {
					t.Errorf("the truncated turn does not report max_tokens:\n%s", body)
				}
				return
			}
			if len(idx) == 0 {
				t.Fatalf("the stream opened no block at all for a call the upstream named:\n%s", body)
			}
			for _, i := range idx {
				if !closed[i] {
					t.Errorf("block %d was opened and never closed — a client-side accumulator cannot finish the call:\n%s", i, body)
				}
			}
			if !strings.Contains(body, `"tool_use"`) {
				t.Errorf("the named call did not reach the client as a tool_use block:\n%s", body)
			}
		})
	}
}

// TestAMessageWithoutAContentKeyIsRefused is C50-2. An absent content key is
// not the empty content; the sibling legs refuse the body, so this leg does
// too. `null` is the empty content and stays served.
func TestAMessageWithoutAContentKeyIsRefused(t *testing.T) {
	for _, c := range []struct {
		name     string
		messages string
		want     int
	}{
		{"absent", `[{"role":"user"}]`, http.StatusBadRequest},
		{"null", `[{"role":"user","content":null}]`, http.StatusOK},
		{"empty-string", `[{"role":"user","content":""}]`, http.StatusOK},
		{"empty-array", `[{"role":"user","content":[]}]`, http.StatusOK},
		{"string", `[{"role":"user","content":"hi"}]`, http.StatusOK},
		{"tools-only-assistant", `[{"role":"user","content":"hi"},{"role":"assistant","tool_calls":[]}]`, http.StatusBadRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got map[string]any
			g, _ := testGatewayForMessages(t, &got)
			var m map[string]any
			body := `{"model":"oaica-35b-a3b-vision","max_tokens":16,"messages":` + c.messages + `}`
			if err := json.Unmarshal([]byte(body), &m); err != nil {
				t.Fatalf("probe body: %v", err)
			}
			w := postMessages(t, g, "sk-test", m)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d — an absent content key is not the empty content, and both sibling legs refuse it:\n%s", w.Code, c.want, w.Body.String())
			}
			if c.want == http.StatusBadRequest && got != nil {
				t.Errorf("the refused body reached the upstream: %v", got)
			}
		})
	}
}

// TestWrongShapeThinkingAndToolChoiceAreRefused is C50-3. A control of the
// wrong shape is refused rather than dropped in silence, the way the typed
// sibling legs answer it; the right shapes and an explicit null still pass.
func TestWrongShapeThinkingAndToolChoiceAreRefused(t *testing.T) {
	const head = `"model":"oaica-35b-a3b-vision","max_tokens":16,"messages":[{"role":"user","content":"hi"}]`
	for _, c := range []struct {
		name string
		body string
		want int
	}{
		{"thinking-bool", `{` + head + `,"thinking":true}`, http.StatusBadRequest},
		{"thinking-string", `{` + head + `,"thinking":"enabled"}`, http.StatusBadRequest},
		{"thinking-number", `{` + head + `,"thinking":5}`, http.StatusBadRequest},
		{"thinking-type-number", `{` + head + `,"thinking":{"type":5}}`, http.StatusBadRequest},
		{"thinking-null", `{` + head + `,"thinking":null}`, http.StatusOK},
		{"thinking-object", `{` + head + `,"thinking":{"type":"enabled"}}`, http.StatusOK},
		{"tool_choice-string", `{` + head + `,"tool_choice":"auto"}`, http.StatusBadRequest},
		{"tool_choice-number", `{` + head + `,"tool_choice":5}`, http.StatusBadRequest},
		{"tool_choice-array", `{` + head + `,"tool_choice":["auto"]}`, http.StatusBadRequest},
		{"tool_choice-type-number", `{` + head + `,"tool_choice":{"type":5}}`, http.StatusBadRequest},
		{"tool_choice-name-number", `{` + head + `,"tool_choice":{"type":"tool","name":5}}`, http.StatusBadRequest},
		{"tool_choice-null", `{` + head + `,"tool_choice":null}`, http.StatusOK},
		{"tool_choice-object", `{` + head + `,"tool_choice":{"type":"auto"}}`, http.StatusOK},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got map[string]any
			g, _ := testGatewayForMessages(t, &got)
			var m map[string]any
			if err := json.Unmarshal([]byte(c.body), &m); err != nil {
				t.Fatalf("probe body: %v", err)
			}
			w := postMessages(t, g, "sk-test", m)
			if w.Code != c.want {
				t.Fatalf("status %d, want %d — a control of the wrong shape is refused by both sibling legs' decode, not dropped:\n%s", w.Code, c.want, w.Body.String())
			}
			if c.want == http.StatusBadRequest && got != nil {
				t.Errorf("the refused body reached the upstream: %v", got)
			}
		})
	}

	// The shapes that pass still reach the upstream as the two sibling legs
	// send them: a stated thinking switch becomes the kwargs the fleet's
	// backends read, and "auto" becomes the tool_choice value.
	t.Run("forwarded", func(t *testing.T) {
		var got map[string]any
		g, _ := testGatewayForMessages(t, &got)
		var m map[string]any
		body := `{` + head + `,"thinking":{"type":"enabled"},"tool_choice":{"type":"auto"}}`
		if err := json.Unmarshal([]byte(body), &m); err != nil {
			t.Fatalf("probe body: %v", err)
		}
		if w := postMessages(t, g, "sk-test", m); w.Code != http.StatusOK {
			t.Fatalf("status %d\n%s", w.Code, w.Body.String())
		}
		kwargs, _ := got["chat_template_kwargs"].(map[string]any)
		if kwargs["enable_thinking"] != true {
			t.Errorf("chat_template_kwargs = %v, want enable_thinking true", got["chat_template_kwargs"])
		}
		if got["tool_choice"] != "auto" {
			t.Errorf("tool_choice = %#v, want auto", got["tool_choice"])
		}
	})
}
