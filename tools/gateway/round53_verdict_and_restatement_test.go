package main

// round53_verdict_and_restatement_test.go — round 53's findings on the metered
// gateway's bridge, all of them one rule: the two arms of this leg — the stream
// and the single completion — answer one upstream body the same way, and the
// verdict they state is about the call the CLIENT was handed.
//
//   - F1: the count of the turn's tool blocks asked its parse of the bytes the
//     client was handed, and a freeform argument text is handed over inside the
//     wrapper `{"_raw":…}` (flushToolArgs) — which always parses. A fragment the
//     model was still writing when the upstream stopped it for length therefore
//     reported tool_use, and the truncation disappeared with it, while the
//     non-stream path and the client leg drop that fragment and report
//     max_tokens for the same turn.
//
//   - F2/F4: a call the upstream listed twice under its own id — restated at the
//     same index, or with no index at all — was appended to the call already on
//     the wire, so the client accumulated `{"a":1}{"a":1}` (an input no client
//     can parse) under tool_use; with no index it was split into a SECOND block
//     under a minted id, and the model's one call was run twice. The non-stream
//     list answers both wires with one block (statedIDOwner).
//
//   - F3: a call the upstream never named, carrying arguments, was relayed as
//     text by the stream arm and refused with 502 by the non-stream arm — one
//     upstream body, two verdicts, and the 5xx invites a retry that re-bills the
//     whole prompt.
//
//   - F5: reasoning beside content was relayed by both streaming paths and
//     counted by every path, but this file's single-completion arm showed only
//     the content: the client read `answer` where the same document as frames
//     read `thoughtanswer`, and was charged for the bytes it was never shown.
//
// Every case below is one upstream body answered on both arms.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r53Answer posts one Anthropic body to a gateway in front of one upstream body
// and reports the status and the whole response.
func r53Answer(t *testing.T, ct, upstream, stream string) (int, string) {
	t.Helper()
	return round44Run(t, ct, upstream,
		`{"model":"kat-awq","max_tokens":64,"stream":`+stream+`,"messages":[{"role":"user","content":"go"}]}`)
}

// r53BlockText is every text_delta a stream carried, concatenated in order.
func r53BlockText(t *testing.T, stream string) string {
	t.Helper()
	var sb strings.Builder
	for _, line := range strings.Split(stream, "\n") {
		body, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(body), &ev) != nil || ev.Type != "content_block_delta" {
			continue
		}
		if ev.Delta.Type == "text_delta" {
			sb.WriteString(ev.Delta.Text)
		}
	}
	return sb.String()
}

// r53StopReason reads the stop_reason out of either shape of the answer.
func r53StopReason(t *testing.T, stream string, out string) string {
	t.Helper()
	if stream == "true" {
		for _, line := range strings.Split(out, "\n") {
			body, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
			if !ok {
				continue
			}
			var ev struct {
				Type  string `json:"type"`
				Delta struct {
					StopReason string `json:"stop_reason"`
				} `json:"delta"`
			}
			if json.Unmarshal([]byte(body), &ev) != nil || ev.Type != "message_delta" {
				continue
			}
			return ev.Delta.StopReason
		}
		return ""
	}
	var resp struct {
		StopReason string `json:"stop_reason"`
	}
	if json.Unmarshal([]byte(out), &resp) != nil {
		return ""
	}
	return resp.StopReason
}

// r53ToolInput is the input a stream accumulated for its tool_use blocks, as the
// client's own accumulator builds it: the content_block_start input plus every
// input_json_delta of that index.
func r53ToolInput(t *testing.T, stream string) []string {
	t.Helper()
	inputs := map[int]string{}
	texts := []string{}
	ordered := []int{}
	for _, line := range strings.Split(stream, "\n") {
		body, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type         string `json:"type"`
			Index        int    `json:"index"`
			ContentBlock struct {
				Type string `json:"type"`
			} `json:"content_block"`
			Delta struct {
				Type        string `json:"type"`
				PartialJSON string `json:"partial_json"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(body), &ev) != nil {
			continue
		}
		switch {
		case ev.Type == "content_block_start" && ev.ContentBlock.Type == "tool_use":
			inputs[ev.Index] = ""
			ordered = append(ordered, ev.Index)
		case ev.Type == "content_block_delta" && ev.Delta.Type == "input_json_delta":
			inputs[ev.Index] += ev.Delta.PartialJSON
		}
	}
	for _, i := range ordered {
		texts = append(texts, inputs[i])
	}
	return texts
}

// TestATruncatedFreeformCallIsNotACallOnEitherArm is F1.
func TestATruncatedFreeformCallIsNotACallOnEitherArm(t *testing.T) {
	frames := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hi"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	doc := `{"id":"c1","choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hi"}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`

	_, stream := r53Answer(t, "text/event-stream", frames, "true")
	_, plain := r53Answer(t, "application/json", doc, "false")
	sr, pr := r53StopReason(t, "true", stream), r53StopReason(t, "false", plain)
	if sr != "max_tokens" {
		t.Errorf("the streamed turn reports stop_reason %q, want max_tokens\nthe argument text the model was still writing was wrapped as {\"_raw\":…}, which always parses, so the truncation vanished and the client was promised a call it would run with input the model never finished\n%s",
			sr, stream)
	}
	if pr != "max_tokens" {
		t.Errorf("the single completion reports stop_reason %q, want max_tokens", pr)
	}
	if sr != pr {
		t.Errorf("one upstream body reports %q streamed and %q as a whole completion", sr, pr)
	}
}

// TestACallRestatedAtItsOwnIndexIsOneCall is F2.
func TestACallRestatedAtItsOwnIndexIsOneCall(t *testing.T) {
	call := `{"index":0,"id":"call_1","type":"function","function":{"name":"A","arguments":"{\"a\":1}"}}`
	frames := `data: {"choices":[{"index":0,"delta":{"tool_calls":[` + call + `]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[` + call + `]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	doc := `{"id":"c1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[` + call + `,` + call + `]}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`

	_, stream := r53Answer(t, "text/event-stream", frames, "true")
	_, plain := r53Answer(t, "application/json", doc, "false")
	if ids := round44ToolUseIDs(t, stream); len(ids) != 1 {
		t.Errorf("the restated call reached the client as %d blocks (%v), want 1", len(ids), ids)
	}
	if ins := r53ToolInput(t, stream); len(ins) != 1 || ins[0] != `{"a":1}` {
		t.Errorf("the client accumulated %v as this call's input, want [{\"a\":1}]\nthe same call listed twice is not more of its own object: the concatenation is not JSON, so the client held an input it could not parse under a stop_reason of tool_use\n%s",
			ins, stream)
	}
	if !strings.Contains(plain, `"input":{"a":1}`) {
		t.Errorf("the single completion does not carry the call's input: %s", plain)
	}
	if strings.Count(plain, `"type":"tool_use"`) != 1 {
		t.Errorf("the single completion answered the restated call with %d blocks: %s", strings.Count(plain, `"type":"tool_use"`), plain)
	}
}

// TestACallRestatedWithoutAnIndexIsOneCall is F4.
func TestACallRestatedWithoutAnIndexIsOneCall(t *testing.T) {
	call := `{"id":"call_1","type":"function","function":{"name":"A","arguments":"{\"a\":1}"}}`
	frames := `data: {"choices":[{"index":0,"delta":{"tool_calls":[` + call + `]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[` + call + `]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	doc := `{"id":"c1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[` + call + `]}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`

	_, stream := r53Answer(t, "text/event-stream", frames, "true")
	_, plain := r53Answer(t, "application/json", doc, "false")
	if ids := round44ToolUseIDs(t, stream); len(ids) != 1 {
		t.Errorf("the model's one call reached the client as %d blocks (%v), want 1\nthe second fragment restates the call already carried under its own id: splitting it minted a second block, and an agent runs both\n%s",
			len(ids), ids, stream)
	}
	if ins := r53ToolInput(t, stream); len(ins) != 1 || ins[0] != `{"a":1}` {
		t.Errorf("the client accumulated %v as this call's input, want [{\"a\":1}]", ins)
	}
	if strings.Count(plain, `"type":"tool_use"`) != 1 {
		t.Errorf("the single completion answered it with %d blocks: %s", strings.Count(plain, `"type":"tool_use"`), plain)
	}
}

// TestANamelessFragmentReachesTheClientAsTextOnBothArms is F3.
func TestANamelessFragmentReachesTheClientAsTextOnBothArms(t *testing.T) {
	frames := `data: {"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"arguments":"{\"a\":1}"}}]}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	doc := `{"id":"c1","choices":[{"finish_reason":"tool_calls","message":{"role":"assistant","content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"arguments":"{\"a\":1}"}}]}}],"usage":{"prompt_tokens":8,"completion_tokens":2}}`

	ss, stream := r53Answer(t, "text/event-stream", frames, "true")
	ps, plain := r53Answer(t, "application/json", doc, "false")
	if ss != 200 || ps != 200 {
		t.Fatalf("one upstream body answered %d streamed and %d as a whole completion\n%s\n%s", ss, ps, stream, plain)
	}
	if got := r53BlockText(t, stream); got != `{"a":1}` {
		t.Errorf("the streamed turn carried %q as text, want the fragment's own bytes", got)
	}
	var resp struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(plain), &resp); err != nil {
		t.Fatalf("not Anthropic JSON: %v\n%s", err, plain)
	}
	if len(resp.Content) != 1 || resp.Content[0].Type != "text" || resp.Content[0].Text != `{"a":1}` {
		t.Errorf("the single completion answered %+v, want one text block holding the fragment", resp.Content)
	}
	if sr, pr := r53StopReason(t, "true", stream), r53StopReason(t, "false", plain); sr != pr || sr != "end_turn" {
		t.Errorf("stop_reason %q streamed and %q as a whole completion, want end_turn on both — a call the upstream never named is not a call, but the bytes it wrote are still output", sr, pr)
	}
}

// TestReasoningBesideContentIsRelayedByBothArms is F5.
func TestReasoningBesideContentIsRelayedByBothArms(t *testing.T) {
	frames := `data: {"choices":[{"index":0,"delta":{"role":"assistant","reasoning":"thought"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{"content":"answer"}}]}` + "\n\n" +
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}` + "\n\n" +
		`data: [DONE]` + "\n\n"
	doc := `{"id":"c1","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":"answer","reasoning":"thought"}}],"usage":{"prompt_tokens":8,"completion_tokens":4}}`

	_, stream := r53Answer(t, "text/event-stream", frames, "true")
	_, plain := r53Answer(t, "application/json", doc, "false")
	want := "thoughtanswer"
	if got := r53BlockText(t, stream); got != want {
		t.Errorf("the streamed turn carried %q, want %q", got, want)
	}
	var resp struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(plain), &resp); err != nil {
		t.Fatalf("not Anthropic JSON: %v\n%s", err, plain)
	}
	var got string
	for _, c := range resp.Content {
		got += c.Text
	}
	if got != want {
		t.Errorf("the single completion carried %q, want %q\nthis file relays reasoning first on both streaming paths and charges every path for its bytes: shown the content alone, the client read a different answer than the same document as frames, and paid for the text it was never shown\n%s",
			got, want, plain)
	}
}
