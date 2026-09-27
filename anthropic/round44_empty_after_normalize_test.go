package anthropic

// round44_empty_after_normalize_test.go — round 44's finding on the local leg,
// A44-1.
//
// Round 43 (C43-1) gave this leg a guard for a conversation that converts to
// nothing: every message becomes zero api.Message values, the list marshals as
// "messages":null, and server/routes.go answers that body with a synthetic 200
// — no generation — while the client is told a successful turn and charged the
// middleware's estimate.
//
// The guard was asked of the list BEFORE normalizeSystemFirst, but that step
// can empty a list that was not empty: an Anthropic MessageParam whose content
// array is empty converts to no message at all, so a body carrying any
// whitespace-only system message — a top-level "   ", a system array whose text
// is "\n", or an in-messages {"role":"system","content":" "} turn — arrives
// here as [system "…"] alone. That list is non-empty, the guard does not fire,
// and the rewrite's "all system messages were blank — drop them" arm then
// returns an empty rest, which marshals as "messages":[]. routes.go tests
// len(req.Messages)==0, not the wire spelling, so [] takes the same synthetic
// 200 as null: the class C43-1 claimed to close stayed open for every body with
// a blank system message, and the other two legs both keep a turn for these
// same bodies.
//
// The guard is now asked of the list AFTER normalization, which is the list
// that actually reaches the wire.

import (
	"encoding/json"
	"testing"

	"github.com/ollama/ollama/api"
)

// round44BlankSystemBodies are the bodies whose surviving content is a blank
// system message and nothing else. Each passes the middleware, which guards the
// RAW messages array only (middleware/anthropic.go), and each reaches this
// package with a non-empty converted list.
var round44BlankSystemBodies = []struct{ name, body string }{
	{"a blank system string",
		`{"model":"m","max_tokens":10,"system":"   ","messages":[{"role":"user","content":[]}]}`},
	{"a system array whose text is blank",
		`{"model":"m","max_tokens":10,"system":[{"type":"text","text":"\n"}],"messages":[{"role":"user","content":[]}]}`},
	{"a blank system turn inside messages",
		`{"model":"m","max_tokens":10,"messages":[{"role":"system","content":" "},{"role":"user","content":[]}]}`},
	{"a blank system beside a replayed redacted thinking block",
		`{"model":"m","max_tokens":10,"system":" ","messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"AAAA"}]}]}`},
	{"a blank system turn alone",
		`{"model":"m","max_tokens":10,"messages":[{"role":"system","content":" "}]}`},
}

// TestABlankSystemMessageDoesNotEmptyTheConversation is A44-1.
func TestABlankSystemMessageDoesNotEmptyTheConversation(t *testing.T) {
	for _, tc := range round44BlankSystemBodies {
		t.Run(tc.name, func(t *testing.T) {
			var r MessagesRequest
			if err := json.Unmarshal([]byte(tc.body), &r); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			conv, err := FromMessagesRequest(r)
			if err != nil {
				t.Fatalf("FromMessagesRequest() error = %v", err)
			}
			if len(conv.Messages) == 0 {
				t.Fatalf("the request converted to zero messages: %s\nan empty conversation is answered by a synthetic 200 with no generation (server/routes.go), so the client reads a successful turn it was charged for while no model ran. "+
					"The guard must be asked of the list normalizeSystemFirst returns — the one that reaches the wire — not of the list that enters it, because a blank system message is exactly what that step drops", tc.body)
			}
			// The wire spelling is what routes.go reads: api.ChatRequest.Messages
			// has no omitempty, so a nil slice marshals as "messages":null and an
			// empty one as "messages":[], and len(req.Messages)==0 answers both
			// with the synthetic 200. The length check above is this assertion;
			// re-asserting on the marshalled bytes would only restate it.
		})
	}
}

// TestAConversationThatEmptiesOnlyAfterNormalizationIsStillATurn pins the
// mechanism rather than the symptom: the guard's condition must hold for the
// list the rewrite produced, which for these inputs is empty even though the
// list entering the rewrite was not.
func TestAConversationThatEmptiesOnlyAfterNormalizationIsStillATurn(t *testing.T) {
	in := []api.Message{{Role: "system", Content: "   "}}
	if got := normalizeSystemFirst(in); len(got) != 0 {
		t.Fatalf("normalizeSystemFirst(%+v) = %+v, want it empty: this is the step that empties a non-empty list, and it is why the empty-conversation guard cannot sit before it", in, got)
	}
	if len(in) != 1 {
		t.Fatalf("the input list was %d messages, want 1: the guard would have been skipped", len(in))
	}
}

// TestABlankSystemIsStillDroppedWhenATurnRemains is the control. The rewrite is
// not wrong to drop a blank system message — it is wrong only to leave nothing
// behind. A body whose blank system message sits beside a real turn must still
// come out with the system message gone and the turn kept.
func TestABlankSystemIsStillDroppedWhenATurnRemains(t *testing.T) {
	body := `{"model":"m","max_tokens":10,"system":"   ","messages":[{"role":"user","content":"hello"}]}`
	var r MessagesRequest
	if err := json.Unmarshal([]byte(body), &r); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	conv, err := FromMessagesRequest(r)
	if err != nil {
		t.Fatalf("FromMessagesRequest() error = %v", err)
	}
	if len(conv.Messages) != 1 {
		t.Fatalf("converted to %d messages, want 1: %+v", len(conv.Messages), conv.Messages)
	}
	if conv.Messages[0].Role != "user" || conv.Messages[0].Content != "hello" {
		t.Errorf("message 0 = %+v, want the user turn with its text: a blank system message must still be dropped", conv.Messages[0])
	}
}
