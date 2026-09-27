package main

// round34_reasoning_and_document_blocks_integrity_test.go — what the block
// switch's new default case does to the block types it was never meant to
// cover (2026-09-27 audit, round 34).
//
// The default case was added this round so an unrepresentable block is refused
// in words instead of dropped (B-F2). Refusing what the bridge cannot
// represent is right for a block the answer depends on; it is wrong for the
// two block types the bridge deliberately DROPS:
//
//   - A Claude Code session with extended thinking echoes its own reasoning
//     back on every following turn — a `thinking` block (handled) beside a
//     `redacted_thinking` block (not handled, so it fell into the new default).
//     The API's own contract makes a redacted payload opaque ciphertext that
//     cannot be read or modified, and the sibling client-side converter drops
//     both (cmd/launch's block switch, anthropic.go): a 400 here fails a turn
//     this product answers on its other leg.
//   - A `document` block whose source is TEXT carries its content inline. The
//     client-side converter writes that text into the prompt, so refusing it
//     here answers a turn the other leg serves; only a binary source (a PDF the
//     bridge has no wire for) is genuinely unrepresentable.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// round34BlockPost posts an Anthropic request through the messages handler and
// returns the status, the body and what the upstream was handed.
func round34BlockPost(t *testing.T, g *gateway, seen *atomic.Pointer[map[string]any], body string) (int, string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer sk")
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	g.messagesHandler(w, req)
	out := ""
	if p := seen.Load(); p != nil {
		b, _ := json.Marshal(*p)
		out = string(b)
	}
	return w.Code, out
}

func round34BlockUpstream(t *testing.T, seen *atomic.Pointer[map[string]any]) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var body map[string]any
		_ = json.Unmarshal(b, &body)
		seen.Store(&body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"chatcmpl-abc","choices":[{"finish_reason":"stop","message":{"content":"ok"}}],"usage":{"prompt_tokens":10,"completion_tokens":1}}`)
	}))
}

// TestAReasoningBlockIsDroppedNotRefused: an extended-thinking session's echoed
// reasoning must not 400 the turn.
func TestAReasoningBlockIsDroppedNotRefused(t *testing.T) {
	var seen atomic.Pointer[map[string]any]
	upstream := round34BlockUpstream(t, &seen)
	defer upstream.Close()

	g := &gateway{}
	round34Gateway(t, g, upstream.URL, round34Model(), -1, 0)

	code, _ := round34BlockPost(t, g, &seen, `{"model":"kat-awq","max_tokens":64,"messages":[`+
		`{"role":"user","content":"hi"},`+
		`{"role":"assistant","content":[`+
		`{"type":"thinking","thinking":"Let me consider.","signature":"sig"},`+
		`{"type":"redacted_thinking","data":"QUJD"},`+
		`{"type":"text","text":"Here is the answer."}]}]}`)
	if code != http.StatusOK {
		t.Fatalf("a turn echoing its own reasoning was answered %d: Claude Code with extended thinking sends thinking and redacted_thinking back on every following turn, and the API's contract makes the redacted payload opaque ciphertext that cannot be read or modified — refusing it fails a session that was answered before the block switch gained a default case", code)
	}

	_, forwarded := round34BlockPost(t, g, &seen, `{"model":"kat-awq","max_tokens":64,"messages":[`+
		`{"role":"user","content":"hi"},`+
		`{"role":"assistant","content":[`+
		`{"type":"thinking","thinking":"Let me consider.","signature":"sig"},`+
		`{"type":"text","text":"Here is the answer."}]}]}`)
	if !strings.Contains(forwarded, "Here is the answer.") {
		t.Errorf("the reasoning turn reached the upstream without its text content: %s", forwarded)
	}
}

// TestADocumentTextSourceBecomesText: a text-source document is content the
// prompt can carry, so it is translated, not refused.
func TestADocumentTextSourceBecomesText(t *testing.T) {
	var seen atomic.Pointer[map[string]any]
	upstream := round34BlockUpstream(t, &seen)
	defer upstream.Close()

	g := &gateway{}
	round34Gateway(t, g, upstream.URL, round34Model(), -1, 0)

	const fileText = "the attached style guide says: use tabs."
	code, forwarded := round34BlockPost(t, g, &seen, `{"model":"kat-awq","max_tokens":64,"messages":[`+
		`{"role":"user","content":[`+
		`{"type":"text","text":"See the attached file."},`+
		`{"type":"document","source":{"type":"text","media_type":"text/plain","data":"`+fileText+`"}}]}]}`)
	if code != http.StatusOK {
		t.Fatalf("a document with a TEXT source was answered %d: its content is what the converter writes into the prompt on the client leg, so refusing it here answers a turn this product serves elsewhere", code)
	}
	if !strings.Contains(forwarded, fileText) {
		t.Errorf("the attached file's text never reached the upstream (answered %d): the model was asked about a document it never received — a silent wrong answer, the class the refusal exists to prevent", code)
	}
}
