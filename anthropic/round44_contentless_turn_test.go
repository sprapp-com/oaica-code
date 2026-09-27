package anthropic

// round44_contentless_turn_test.go — round 44's finding on how a turn with no
// content travels the local leg, C44-2 and C44-3.
//
// C44-2: convertMessage drops a message whose content array converts to no
// block at all — an empty content array, or an assistant turn holding only a
// redacted thinking block (a shape the client replays on the next request). The
// gateway leg pads the same turn and keeps it; this leg deleted it, so the two
// legs sent different conversations for the same body and a strict template
// could reject a turn sequence this leg produced.
//
// C44-3: the padding is not enough on its own. The turn has to keep ITS OWN
// role. Rebuilding every contentless turn as a user turn turns an assistant
// turn holding only a redacted thinking block into a second user turn in a row
// — a shape the Anthropic wire refuses outright. The collapse to a single user
// turn is the last resort, and only for a conversation where nothing at all
// carries content.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// round44Convert is the request path the middleware uses, for a body given
// inline.
func round44Convert(t *testing.T, body string) []api.Message {
	t.Helper()
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("FromMessagesRequest: %v", err)
	}
	return conv.Messages
}

// TestAContentlessTurnIsKeptWithItsOwnRole is C44-2 and C44-3 together: the
// turn survives, and it survives as the role it arrived with.
func TestAContentlessTurnIsKeptWithItsOwnRole(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want []string // role of every message the leg must produce, in order
	}{
		{
			name: "an empty content array keeps its user role",
			body: `{"model":"m","max_tokens":10,"messages":[
				{"role":"user","content":"a"},
				{"role":"user","content":[]},
				{"role":"user","content":"b"}]}`,
			want: []string{"user", "user", "user"},
		},
		{
			name: "a replayed redacted thinking turn keeps its assistant role",
			body: `{"model":"m","max_tokens":10,"messages":[
				{"role":"assistant","content":[{"type":"redacted_thinking","data":"AAAA"}]},
				{"role":"user","content":"question"}]}`,
			want: []string{"assistant", "user"},
		},
		{
			name: "a replayed redacted thinking turn mid-conversation keeps it too",
			body: `{"model":"m","max_tokens":10,"messages":[
				{"role":"user","content":"a"},
				{"role":"assistant","content":[{"type":"redacted_thinking","data":"AAAA"}]},
				{"role":"user","content":"b"}]}`,
			want: []string{"user", "assistant", "user"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := round44Convert(t, tc.body)
			if len(got) != len(tc.want) {
				t.Fatalf("the body converted to %d message(s) %+v, want %d: a turn whose content array converts to no block is a turn the client sent and the model has to be shown, so it is kept — the gateway leg pads it rather than deleting it", len(got), got, len(tc.want))
			}
			for i, role := range tc.want {
				if got[i].Role != role {
					t.Errorf("message %d has role %q, want %q (whole conversation: %+v): the padded turn keeps the role it arrived with — rebuilding it as a user turn puts two user turns in a row, which the Anthropic wire refuses", i, got[i].Role, role, got)
				}
			}
		})
	}
}

// TestAConversationWithNothingInItCollapsesToATurn is C44-3's second half: the
// fallback is for a conversation where not one message carries anything, and
// there the turn is a single user turn.
func TestAConversationWithNothingInItCollapsesToATurn(t *testing.T) {
	got := round44Convert(t, `{"model":"m","max_tokens":10,"messages":[
		{"role":"assistant","content":[{"type":"redacted_thinking","data":"AAAA"}]}]}`)
	if len(got) != 1 || got[0].Role != "user" {
		t.Fatalf("the body converted to %+v, want one user turn: an assistant turn holding only a redacted thinking block carries nothing the model can read, and a lone assistant turn is not a conversation", got)
	}
}

// TestAConversationThatCarriesTextIsNotCollapsed is the control: the collapse
// above is the last resort for a conversation with nothing in it, and a
// conversation that carries text reaches the model unchanged.
func TestAConversationThatCarriesTextIsNotCollapsed(t *testing.T) {
	got := round44Convert(t, `{"model":"m","max_tokens":10,"messages":[
		{"role":"user","content":"a"},
		{"role":"user","content":"b"}]}`)
	if len(got) != 2 {
		t.Fatalf("the body converted to %+v, want the two turns as they arrived", got)
	}
}

// TestAWhitespacePromptIsStillAPrompt is the edge the collapse must not catch:
// a text is content whatever it says. Trimming it away sent the upstream a bare
// empty user turn where the client had written a prompt, and the calibration
// unit measured 66 bytes for a hundred-kilobyte one — caught by the round-36
// estimator test when this leg's guard was written with TrimSpace.
func TestAWhitespacePromptIsStillAPrompt(t *testing.T) {
	const n = 100000
	text := strings.Repeat("\n", n)
	got := round44Convert(t, `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":`+
		string(mustJSON(t, text))+`}]}`)
	if len(got) != 1 {
		t.Fatalf("the body converted to %+v, want one turn", got)
	}
	if got[0].Content != text {
		t.Errorf("the turn's text is %d bytes, want the %d the client sent: whitespace is not nothing on this wire — the model can be asked a prompt made of it, and the estimate charges it",
			len(got[0].Content), len(text))
	}
}

// mustJSON renders s as a JSON string literal.
func mustJSON(t *testing.T, s string) []byte {
	t.Helper()
	b, err := json.Marshal(s)
	if err != nil {
		t.Fatal(err)
	}
	return b
}
