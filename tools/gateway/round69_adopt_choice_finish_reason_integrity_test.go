package main

// round69_adopt_choice_finish_reason_integrity_test.go — leg 3's ADOPT arm and
// which choice's finish_reason it reads.
//
// Round 68 taught the document arm (finalize) to read the last reason any
// choice stated, and left the two lines that read the same field eight lines
// above — and the adopt arm, which hand-writes the same turn for a stream
// request answered with a whole completion — reading Choices[0] alone. So one
// body answered a different verdict, and relayed a different set of calls,
// depending on the framing the upstream chose: the truncation gate itself
// (callInput's truncated flag) read the first entry while the verdict read the
// last, and the adopt arm disagreed with both.

import (
	"encoding/json"
	"testing"
)

// r69JSONToolUseIDs returns the tool_use ids of a whole-document answer (the
// non-stream shape), which is JSON rather than frames.
func r69JSONToolUseIDs(t *testing.T, body string) []string {
	t.Helper()
	var msg struct {
		Content []struct {
			Type string `json:"type"`
			ID   string `json:"id"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	var ids []string
	for _, b := range msg.Content {
		if b.Type == "tool_use" {
			ids = append(ids, b.ID)
		}
	}
	return ids
}

// r69TwoChoiceCallDoc is one completion whose FIRST entry carries a call the
// model was still writing and whose SECOND entry states the token limit.
const r69TwoChoiceCallDoc = `{"id":"x","model":"m","choices":[` +
	`{"index":0,"message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":"}}]},"finish_reason":"stop"},` +
	`{"index":1,"message":{"role":"assistant","content":""},"finish_reason":"length"}],` +
	`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

// r69TwoChoiceCompleteCallDoc is the mirror: the first entry states the token
// limit and carries a COMPLETE call, the second ends the turn normally.
const r69TwoChoiceCompleteCallDoc = `{"id":"x","model":"m","choices":[` +
	`{"index":0,"message":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},"finish_reason":"length"},` +
	`{"index":1,"message":{"role":"assistant","content":""},"finish_reason":"tool_calls"}],` +
	`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

// TestTheAdoptArmReadsTheReasonAnyChoiceStated is the verdict half: a stream
// request the upstream answered with a whole completion whose second entry
// states the limit must report max_tokens, as the document arm does.
func TestTheAdoptArmReadsTheReasonAnyChoiceStated(t *testing.T) {
	up := round45Upstream(t, "application/json", r69TwoChoiceCallDoc)
	srv, _ := round39Gateway(t, up, nil)
	_, body := round45Ask(t, srv, round45AskStream)

	if got := r68Reason(t, body); got != "max_tokens" {
		t.Errorf("the adopt arm answered stop_reason %q for a completion whose second choice stated the token limit, want max_tokens: this arm hand-writes the same turn the document arm does, and round 68 taught that arm to read the LAST reason any entry stated (2026-09-28 audit, round 69)\n%s", got, body)
	}
}

// TestTheAdoptArmDropsATruncatedCallTheLastChoiceStated is the other
// consequence: the truncation gate feeds callInput, so reading Choices[0] here
// relays a call the model never finished — an executable tool_use whose input
// is not JSON — while the document arm drops it.
func TestTheAdoptArmDropsATruncatedCallTheLastChoiceStated(t *testing.T) {
	up := round45Upstream(t, "application/json", r69TwoChoiceCallDoc)
	srv, _ := round39Gateway(t, up, nil)
	_, body := round45Ask(t, srv, round45AskStream)

	if ids := round46ToolUseIDs(t, body); len(ids) != 0 {
		t.Errorf("the adopt arm relayed %v as tool_use for a turn the last choice states was cut short at the token limit: the call is dropped by the document arm, by the frame arm, and by the client leg, and an agent that runs it executes a Bash call whose input is half an object (2026-09-28 audit, round 69)\n%s", ids, body)
	}
	if got := r68Reason(t, body); got != "max_tokens" {
		t.Errorf("the adopt arm answered stop_reason %q, want max_tokens\n%s", got, body)
	}
}

// TestBothDocumentArmsKeepACompleteCallTheLastChoiceAsksFor is the control: a
// complete call is a call even when the first entry states the token limit, and
// reading the LAST stated reason must not turn it into a dropped one.
func TestBothDocumentArmsKeepACompleteCallTheLastChoiceAsksFor(t *testing.T) {
	up := round45Upstream(t, "application/json", r69TwoChoiceCompleteCallDoc)
	srv, _ := round39Gateway(t, up, nil)
	_, streamed := round45Ask(t, srv, round45AskStream)
	if ids := round46ToolUseIDs(t, streamed); len(ids) != 1 || ids[0] != "call_1" {
		t.Errorf("the adopt arm answered ids %v for a completion whose last choice asks for the call, want [call_1]\n%s", ids, streamed)
	}
	if got := r68Reason(t, streamed); got != "tool_use" {
		t.Errorf("the adopt arm answered stop_reason %q, want tool_use from the last choice that stated one\n%s", got, streamed)
	}

	up2 := round45Upstream(t, "application/json", r69TwoChoiceCompleteCallDoc)
	srv2, _ := round39Gateway(t, up2, nil)
	_, plain := round45Ask(t, srv2, round45AskPlain)
	if ids := r69JSONToolUseIDs(t, plain); len(ids) != 1 || ids[0] != "call_1" {
		t.Errorf("the document arm answered ids %v for the same completion, want [call_1]: the two arms must agree\n%s", ids, plain)
	}
}
