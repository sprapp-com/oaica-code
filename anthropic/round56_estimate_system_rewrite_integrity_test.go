package anthropic

// round56_estimate_system_rewrite_integrity_test.go — round 56's finding on the
// local leg (leg 1): the estimate charged the message list as the CLIENT sent
// it, while the prompt the backend reads is the one normalizeSystemFirst writes.
// The rewrite hoists a system message that arrived late, merges the top-level
// system with it into one message, and deletes a whitespace-only one — so two
// bodies that convert to the same conversation were charged different numbers,
// in both directions: a blank system message the rewrite deletes was billed for,
// and two system texts the rewrite merges into one were billed as two messages
// with two roles. The estimate seeds the client-visible input_tokens whenever
// the upstream states no usage, so the difference is what a session's
// auto-compaction is sized on (2026-09-28 audit, round 56, F56-1).

import (
	"encoding/json"
	"strings"
	"testing"
)

// r56Request decodes one /v1/messages body.
func r56Request(t *testing.T, body string) MessagesRequest {
	t.Helper()
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	return req
}

// r56Prompt is the conversation the converter writes — each message's role and
// content — so a case can show that its two bodies really do produce one prompt.
func r56Prompt(t *testing.T, req MessagesRequest) string {
	t.Helper()
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	var sb strings.Builder
	for _, m := range conv.Messages {
		sb.WriteString(m.Role)
		sb.WriteString(":")
		sb.WriteString(m.Content)
		sb.WriteString("|")
	}
	return sb.String()
}

func TestTheEstimateChargesTheRewrittenMessageList(t *testing.T) {
	for _, tc := range []struct {
		name string
		a, b string
	}{
		{
			name: "a system message that arrives after a user turn is hoisted",
			a:    `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"go"},{"role":"system","content":"SYS"}]}`,
			b:    `{"model":"m","max_tokens":64,"messages":[{"role":"system","content":"SYS"},{"role":"user","content":"go"}]}`,
		},
		{
			name: "a whitespace-only system message is deleted",
			a:    `{"model":"m","max_tokens":64,"messages":[{"role":"system","content":"   "},{"role":"user","content":"go"}]}`,
			b:    `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"go"}]}`,
		},
		{
			name: "the top-level system and a later one are merged",
			a:    `{"model":"m","max_tokens":64,"system":"TOP","messages":[{"role":"user","content":"go"},{"role":"system","content":"MID"}]}`,
			b:    `{"model":"m","max_tokens":64,"messages":[{"role":"system","content":"TOP\n\nMID"},{"role":"user","content":"go"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ra, rb := r56Request(t, tc.a), r56Request(t, tc.b)
			pa, pb := r56Prompt(t, ra), r56Prompt(t, rb)
			if pa != pb {
				t.Fatalf("premise: the two bodies do not convert to one prompt: %q vs %q", pa, pb)
			}
			ea, eb := EstimateInputTokens(ra), EstimateInputTokens(rb)
			if ea != eb {
				t.Errorf("two bodies the converter turns into the SAME prompt (%q) are charged %d and %d tokens: the estimate charges the message list as the client sent it, so a system message the rewrite moves, merges or deletes is still billed", pa, ea, eb)
			}
			// The control: the charge is still the prompt's own size, so the fix
			// cannot be "charge nothing". Four bytes to a token, and the blank
			// turn's body is the smaller of the two.
			if want := len(pa) / 4; ea < want-1 || ea > want+2 {
				t.Errorf("the prompt %q is charged %d tokens against its own %d bytes (%d tokens)", pa, ea, len(pa), want)
			}
		})
	}
}

// The conversation the rewrite leaves alone is charged exactly as it arrives:
// two leading system messages are two messages on the wire, and a system message
// that carries an image cannot be merged into the one system string, so
// normalizeSystemFirst returns it untouched and the charge must not move.
func TestAnUnrewrittenConversationIsChargedAsItArrives(t *testing.T) {
	leading := r56Request(t, `{"model":"m","max_tokens":64,"messages":[{"role":"system","content":"AAA"},{"role":"system","content":"BBB"},{"role":"user","content":"go"}]}`)
	conv, err := FromMessagesRequest(leading)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	systems := 0
	for _, m := range conv.Messages {
		if m.Role == "system" {
			systems++
		}
	}
	if systems != 2 {
		t.Fatalf("premise: the ordered conversation was rewritten into %d system messages", systems)
	}
	// Both roles are charged, as they are on the wire: 2*(6+3) + (4+2) = 24.
	if got := EstimateInputTokens(leading); got != 6 {
		t.Errorf("an already-ordered conversation of two system messages and a user turn is charged %d tokens, want 6 (each of its roles and contents is written)", got)
	}

	image := `{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}`
	withImage := r56Request(t, `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":"go"},{"role":"system","content":[`+image+`]}]}`)
	convImg, err := FromMessagesRequest(withImage)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	for _, m := range convImg.Messages {
		if m.Role == "system" && len(m.Images) == 0 {
			t.Errorf("premise: the image on the system message did not survive the conversion, so this case does not exercise the merge guard")
		}
	}
	// Unrewritten, so the image is still charged its allowance beside the text.
	if got := EstimateInputTokens(withImage); got < 1000 {
		t.Errorf("a conversation whose system message carries an image is charged %d tokens: it is returned untouched by the rewrite, image included", got)
	}
}
