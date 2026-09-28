package main

// round68_choice_finish_reason_integrity_test.go — leg 3, which choice's
// finish_reason the document arm reads.
//
// The fragment arm scans every entry and keeps the last reason stated, and says
// why: which of an upstream's choices carried the reason is not this bridge's
// to decide. The document arm read Choices[0] alone, so ONE body — the same two
// choices, the reason on the second — answered end_turn as a document and
// max_tokens streamed, and the verdict depended on which entry carried the
// field and on the `stream` flag (2026-09-28 audit, round 68, F68-L3-2).
//
// The content stays the first entry's on both arms; only the verdict moved.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// r68Reason pulls the stop_reason out of an Anthropic-shaped answer, whichever
// of this bridge's two shapes the turn was asked with.
func r68Reason(t *testing.T, body string) string {
	t.Helper()
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		var msg struct {
			StopReason string `json:"stop_reason"`
		}
		if err := json.Unmarshal([]byte(body), &msg); err != nil {
			t.Fatalf("unmarshal: %v\n%s", err, body)
		}
		return msg.StopReason
	}
	stop := ""
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(strings.TrimSpace(line), "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(raw), &ev) != nil {
			continue
		}
		if ev.Type == "message_delta" {
			stop = ev.Delta.StopReason
		}
	}
	return stop
}

// r68TwoChoiceDoc is one completion whose first entry ends normally and whose
// second states the token limit.
const r68TwoChoiceDoc = `{"id":"x","model":"m","choices":[` +
	`{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"},` +
	`{"index":1,"message":{"role":"assistant","content":""},"finish_reason":"length"}],` +
	`"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`

// TestTheDocumentArmReadsTheReasonAnyChoiceStated is the finding.
func TestTheDocumentArmReadsTheReasonAnyChoiceStated(t *testing.T) {
	status, body, _ := round45OneRow(t, "application/json", r68TwoChoiceDoc, round45AskPlain)
	if status != http.StatusOK {
		t.Fatalf("status %d, body:\n%s", status, body)
	}
	if got := r68Reason(t, body); got != "max_tokens" {
		t.Errorf("the document arm answered stop_reason %q for a completion whose second choice stated the token limit, want max_tokens: the fragment arm of this same bridge reads the LAST reason any entry stated, so the verdict depended on which choice carried the field and on the `stream` flag (2026-09-28 audit, round 68, F68-L3-2)\n%s",
			got, body)
	}
}

// TestTheFragmentArmReadsTheSameReason is the control in the other direction:
// the arm that was already right must not move.
func TestTheFragmentArmReadsTheSameReason(t *testing.T) {
	up := round45Frames(t,
		`data: {"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`)
	srv, _ := round39Gateway(t, up, nil)
	_, body := round45Ask(t, srv, round45AskStream)
	if got := r68Reason(t, body); got != "max_tokens" {
		t.Errorf("the fragment arm answered stop_reason %q, want max_tokens from the last choice that stated one\n%s", got, body)
	}
}
