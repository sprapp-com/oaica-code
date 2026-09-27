package anthropic

// round43_system_hoist_guard_test.go — round 43's two findings on the local leg.
//
// A43-1: normalizeSystemFirst's non-text guard sat behind the scan's early exit,
// so a conversation whose FIRST late system message was text-only never reached
// the guard for a LATER one that carried an image. The rewrite merged both into
// one bare string, the image was dropped, and EstimateInputTokens still charged
// it its 4096-byte allowance — the client was billed for a picture the model
// never saw. The guard now ranges over the whole conversation, which is the rule
// the function's own comment states.
//
// C43-1: a request whose messages all convert to nothing (an empty content
// array, or an assistant turn holding only a redacted thinking block) left
// `messages` empty and marshalled as "messages":null. The server answers that
// body with a synthetic 200 and no generation (server/routes.go), so the client
// read a successful turn it was charged for while no model ran — and the other
// two legs both keep a turn here.

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/api"
)

// round43Carrier asks with a text-only late system message followed by one that
// carries an image: the shape whose first half ended the scan before the guard
// could see the second.
const round43Carrier = `{"model":"m","max_tokens":10,"messages":[
	{"role":"user","content":"hi"},
	{"role":"system","content":[{"type":"text","text":"late note"}]},
	{"role":"system","content":[{"type":"text","text":"and the picture"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}}]}]}`

// TestAnImageOnALateSystemMessageIsNotMergedAway is A43-1 through the request
// path the middleware actually uses.
func TestAnImageOnALateSystemMessageIsNotMergedAway(t *testing.T) {
	var req MessagesRequest
	if err := json.Unmarshal([]byte(round43Carrier), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("FromMessagesRequest: %v", err)
	}

	images := 0
	for _, m := range conv.Messages {
		images += len(m.Images)
	}
	if images != 1 {
		t.Errorf("the image the client sent converted to %d image(s) over %d messages: %+v\nthe non-text guard must be asked of every system message, not only of those the scan walks before it stops — a first late system message that is text-only used to end the scan, and the rewrite then merged the message that carried the image into one bare string", images, len(conv.Messages), conv.Messages)
	}

	// The carrier's own text is not rewritten either: the conversation is
	// returned exactly as it arrived.
	for _, m := range conv.Messages {
		if m.Role == "system" && strings.Contains(m.Content, "and the picture") && m.Content != "and the picture" {
			t.Errorf("the system message's text was rewritten to %q: a conversation holding a system message that carries more than text is returned as it arrived, not re-rendered", m.Content)
		}
	}
}

// TestALateSystemMessageCarryingOnlyTextIsStillHoisted is the control: the
// bail-out above is about what a message carries, not about rewriting at all.
func TestALateSystemMessageCarryingOnlyTextIsStillHoisted(t *testing.T) {
	got := normalizeSystemFirst([]api.Message{
		{Role: "user", Content: "go"},
		{Role: "system", Content: "late"},
	})
	if len(got) != 2 || got[0].Role != "system" || got[0].Content != "late" || got[1].Role != "user" {
		t.Errorf("normalizeSystemFirst() = %+v, want the system message first and the user turn after it: a strict chat template raises on a system message that arrives after a user turn", got)
	}
}

// TestTheCarrierGuardRangesOverTheWholeConversation is A43-1 at the function
// itself, where the early exit lived.
func TestTheCarrierGuardRangesOverTheWholeConversation(t *testing.T) {
	in := []api.Message{
		{Role: "user", Content: "go"},
		{Role: "system", Content: "late note"},
		{Role: "system", Content: "and the picture", Images: []api.ImageData{{1, 2, 3}}},
	}
	got := normalizeSystemFirst(in)
	if len(got) != 3 {
		t.Fatalf("normalizeSystemFirst() = %+v, want the three messages untouched: the second system message carries an image, and no rewrite may drop it", got)
	}
	for i := range in {
		if got[i].Role != in[i].Role || got[i].Content != in[i].Content || len(got[i].Images) != len(in[i].Images) {
			t.Errorf("message %d = %+v, want %+v: a conversation holding a system message that carries more than text is returned exactly as it arrived", i, got[i], in[i])
		}
	}
}

// TestAConversationThatConvertsToNothingIsStillATurn is C43-1. The body reaches
// this package: the middleware's own guard tests the RAW messages array, which
// is non-empty in both of these.
func TestAConversationThatConvertsToNothingIsStillATurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
	}{
		{"an empty content array", `{"model":"m","max_tokens":10,"messages":[{"role":"user","content":[]}]}`},
		{"a replayed redacted thinking block", `{"model":"m","max_tokens":10,"messages":[{"role":"assistant","content":[{"type":"redacted_thinking","data":"AAAA"}]}]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var req MessagesRequest
			if err := json.Unmarshal([]byte(tc.body), &req); err != nil {
				t.Fatalf("unmarshal: %v", err)
			}
			conv, err := FromMessagesRequest(req)
			if err != nil {
				t.Fatalf("FromMessagesRequest: %v", err)
			}
			if len(conv.Messages) == 0 {
				t.Fatalf("the request converted to zero messages: %s\nmessages:null is answered by a synthetic 200 with no generation, so the client reads a successful turn it was charged for while no model ran", tc.body)
			}
			wire, err := json.Marshal(conv)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			if strings.Contains(string(wire), `"messages":null`) {
				t.Errorf("the converted request marshalled as %s: the empty conversation must carry a turn, as it does on the other two legs", wire)
			}
		})
	}
}
