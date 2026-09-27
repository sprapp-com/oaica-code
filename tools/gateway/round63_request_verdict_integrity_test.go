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
//   - F63-L3-6: a conversation with no turns at all. Round 63 read the sibling's
//     CONVERTER (which writes one message carrying the client's role and no
//     content) rather than its handler, served the turn here, and so made the
//     same body a 400 on two legs and a 200 on this one. Round 64's F64-L3-1
//     reversed it; the test below is the reversed pin, and round 64's own file
//     carries the finding.
//
// F63-L3-5 is fail-first: RED against the tree before this round's fix.

import (
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

// TestATurnlessConversationIsRefusedWhateverTheKeySays is F63-L3-6 REVERSED by
// round 64's F64-L3-1. Round 63 read one sibling's CONVERTER — which does write
// one message carrying the client's role and no content for a turn-less body —
// and served the turn here, on the reading that the other two legs served it
// too. They do not: both HANDLERS refuse an empty turn list with this same 400
// before their converters run (middleware/anthropic.go's `len(req.Messages) ==
// 0`, cmd/launch/anthropic_openai_proxy.go's `len(anthReq.Messages) == 0`, each
// citing this leg), so the round-63 change made one body a 400 on two legs and
// a metered 200 here — and served a `messages` that is not an array at all,
// which both siblings fail to decode.
func TestATurnlessConversationIsRefusedWhateverTheKeySays(t *testing.T) {
	for _, tc := range []struct{ name, body string }{
		{"empty-array", `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":[]}`},
		{"absent", `{"model":"kat-awq","max_tokens":64,"stream":true}`},
		{"null", `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":null}`},
		{"string", `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":"oops"}`},
		{"number", `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":7}`},
		{"object", `{"model":"kat-awq","max_tokens":64,"stream":true,"messages":{"a":1}}`},
	} {
		code, out := r63Ask(t, tc.body)
		if code != 400 {
			t.Errorf("a body whose messages is %s is answered %d, want 400:\n%s\nthe sibling hands refuse an empty turn list with this same 400 and neither decodes a messages that is not an array, so serving it here makes one body a 400 on two legs and a metered 200 on this one (2026-09-28 audit, round 64, F64-L3-1)", tc.name, code, out)
		}
	}
}
