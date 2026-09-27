package anthropic

// round42_system_hoist_integrity_test.go — C42-5 on this leg.
//
// A client can send a top-level `system` beside a mid-conversation system
// message, which reaches the backend as [system, user, system]; a strict chat
// template raises on that (KAT-Coder's apex GGUF answers "System message must
// be at the beginning"). The client-side proxy repaired that shape and this
// leg did not, so one body was answered by a 500 here and by an answer there,
// and the two legs handed their backends different prompts for one request.
//
// The repair has two bounds that are as much a part of it as the hoist:
//
//   - an ALREADY-ORDERED conversation is returned byte for byte, because
//     merging its several leading system messages changes a prompt the client
//     sent and the previous turn's, defeating any prefix cache keyed on the
//     rendered text;
//   - a system message carrying anything but text cannot be merged into the one
//     leading string without dropping what it carries, so a conversation
//     holding one is left exactly as it arrived.

import (
	"encoding/json"
	"strings"
	"testing"
)

// round42Convert converts one /v1/messages body and returns the messages the
// backend would be handed.
func round42Convert(t *testing.T, body string) []string {
	t.Helper()
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	out, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("conversion refused the body: %v", err)
	}
	rendered := make([]string, 0, len(out.Messages))
	for _, m := range out.Messages {
		rendered = append(rendered, m.Role+"="+m.Content)
	}
	return rendered
}

// TestALateSystemMessageIsHoistedToTheFront is C42-5 itself: the body the
// templates refuse is repaired into the body they accept, with both texts
// preserved and in order.
func TestALateSystemMessageIsHoistedToTheFront(t *testing.T) {
	got := round42Convert(t, `{"model":"m","system":"top","messages":[{"role":"user","content":"go"},{"role":"system","content":"late"}]}`)
	want := []string{"system=top\n\nlate", "user=go"}
	eq := len(got) == len(want)
	if eq {
		for i := range got {
			if got[i] != want[i] {
				eq = false
			}
		}
	}
	if !eq {
		t.Errorf("converted messages = %q, want %q: a system message after a user turn is hoisted, and a strict template that refuses it (\"System message must be at the beginning\") answers the same body the proxy serves", got, want)
	}
}

// The control: an already-ordered conversation is NOT re-rendered. Two leading
// system messages stay two, byte for byte — merging them would change the
// prompt the client sent (and the previous turn's, defeating a prefix cache
// keyed on the rendered text).
func TestAnOrderedConversationIsNotReRendered(t *testing.T) {
	got := round42Convert(t, `{"model":"m","system":"first","messages":[{"role":"system","content":"second"},{"role":"user","content":"go"}]}`)
	want := []string{"system=first", "system=second", "user=go"}
	eq := len(got) == len(want)
	if eq {
		for i := range got {
			if got[i] != want[i] {
				eq = false
			}
		}
	}
	if !eq {
		t.Errorf("converted messages = %q, want %q: this conversation is already in the shape the templates accept, so the rewrite must be a no-op", got, want)
	}
}

// A blank leading system message is dropped by the rewrite — it is no
// instruction — while the real one it was hiding behind is carried.
func TestABlankSystemMessageIsDroppedByTheRewrite(t *testing.T) {
	got := round42Convert(t, `{"model":"m","system":"   ","messages":[{"role":"user","content":"go"},{"role":"system","content":"late"}]}`)
	want := []string{"system=late", "user=go"}
	eq := len(got) == len(want)
	if eq {
		for i := range got {
			if got[i] != want[i] {
				eq = false
			}
		}
	}
	if !eq {
		t.Errorf("converted messages = %q, want %q: a blank system message is no instruction, and the one that IS an instruction must survive the rewrite", got, want)
	}
}

// A system message carrying an image cannot be merged into the single leading
// string without dropping the image, so the conversation is left exactly as it
// arrived — the shape the client sent, and not one with a silent omission in
// it.
func TestASystemMessageCarryingAnImageStopsTheRewrite(t *testing.T) {
	got := round42Convert(t, `{"model":"m","messages":[{"role":"user","content":"go"},{"role":"system","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJDRA=="}}]}]}`)
	if len(got) != 2 || !strings.HasPrefix(got[1], "system=") {
		t.Fatalf("converted messages = %q, want the user turn then the system message exactly where the client put it: an image-carrying system message cannot be merged into a leading string without dropping the image", got)
	}
	if len(got) != 2 || got[0] != "user=go" {
		t.Errorf("converted messages = %q: the user turn must be untouched", got)
	}
}
