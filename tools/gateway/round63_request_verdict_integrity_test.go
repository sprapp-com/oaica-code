package main

// round63_request_verdict_integrity_test.go — round 63's two findings on the
// metered gateway's reading of the REQUEST (F63-L3-5, F63-L3-6).
//
// Both are bodies the two sibling legs answer one way and this leg answered
// another, which is the one thing a translation may not do: a client that
// switches between them cannot learn what this wire accepts.
//
//   - F63-L3-5: a call block whose `input` is not a JSON object. The sibling
//     decodes that field into an object whatever the block is CALLED, and fails
//     the whole request — but this leg asked the question only of tool_use, on
//     the reading that server_tool_use is carried "with whatever fields it
//     has". Its input reached the model as the bare scalar, arguments no tool
//     can parse, under a 200.
//   - F63-L3-6: a conversation with no turns at all. The sibling's converter
//     writes one message carrying the client's role and no content and serves
//     the turn; this leg refused the body "messages is required", so the same
//     request was a 200 on two legs and a 400 here.
//
// Each is fail-first: RED against the tree before this round's fix.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r63Ask posts one Anthropic body to this leg and returns the status and body.
func r63Ask(t *testing.T, body string) (int, string) {
	t.Helper()
	up := round45Frames(t, `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`, `data: [DONE]`)
	srv, _ := round39Gateway(t, up, nil)
	return round45Ask(t, srv, body)
}

// TestANonObjectInputIsRefusedWhateverTheBlockIsCalled is F63-L3-5: the same
// refused field on a tool_use and on a server_tool_use.
func TestANonObjectInputIsRefusedWhateverTheBlockIsCalled(t *testing.T) {
	for _, tc := range []struct{ name, block string }{
		{"tool_use", `{"type":"tool_use","id":"a","name":"Bash","input":5}`},
		{"server_tool_use", `{"type":"server_tool_use","id":"a","name":"web_search","input":5}`},
	} {
		body := `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[{"role":"user","content":[` + tc.block + `]}]}`
		code, out := r63Ask(t, body)
		if code != 400 {
			t.Errorf("a %s whose input is the number 5 is answered %d, want 400:\n%s\nthe sibling decodes that field into a JSON object whatever the block is called and fails the request, and the arguments this leg put on the wire instead were the bare scalar 5 — no tool parses that (2026-09-28 audit, round 63, F63-L3-5)", tc.name, code, out)
		}
		if !strings.Contains(out, "_use block 'input' must be a JSON object") {
			t.Errorf("the %s refusal reads %q, want it to name the block's input field", tc.name, out)
		}
	}
}

// TestAnEmptyConversationIsOneEmptyTurn is F63-L3-6: the messages key stated as
// an empty array, and left out entirely.
func TestAnEmptyConversationIsOneEmptyTurn(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty-array", `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[]}`},
		{"absent", `{"model":"kat-awq","max_tokens":64,"stream":true}`},
	} {
		code, out := r63Ask(t, tc.body)
		if code != 200 {
			t.Errorf("a conversation with no turns (%s) is answered %d, want 200:\n%s\nboth sibling legs write one message carrying the client's role and no content and serve the turn, so this body is a 400 on this leg alone (2026-09-28 audit, round 63, F63-L3-6)", tc.name, code, out)
		}
	}
}

// TestTheEmptyTurnReachesTheModelAsOneUserMessage holds the SHAPE the served
// turn takes: the message the sibling writes is a user message with no content,
// not an empty conversation the upstream would have to invent.
func TestTheEmptyTurnReachesTheModelAsOneUserMessage(t *testing.T) {
	cap := &round44Capture{}
	up := round44Upstream(t, cap, "text/event-stream", `data: {"choices":[{"delta":{"content":"hi"},"finish_reason":"stop"}]}`+"\n\n"+`data: [DONE]`+"\n\n")
	srv, _ := round39Gateway(t, up, nil)
	code, _ := round45Ask(t, srv, `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[]}`)
	if code != 200 {
		t.Fatalf("PREMISE: the empty conversation is answered %d, want 200", code)
	}
	var sent struct {
		Messages []map[string]any `json:"messages"`
	}
	if err := json.Unmarshal([]byte(cap.take()), &sent); err != nil {
		t.Fatalf("the upstream request is not JSON: %v", err)
	}
	if len(sent.Messages) != 1 || sent.Messages[0]["role"] != "user" {
		t.Errorf("the empty conversation reaches the model as %v, want one user message", sent.Messages)
	}
}
