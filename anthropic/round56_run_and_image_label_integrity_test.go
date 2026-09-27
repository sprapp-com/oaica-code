package anthropic

// round56_run_and_image_label_integrity_test.go — round 56's cross-leg findings
// on the shape of a turn that holds text AND images (F56-4, F56-5).
//
// Both are the same kind of question: what does one client body become on each
// leg? This leg (and the client proxy, which converts through this same
// converter) writes a turn as an ollama api.Message — a content STRING and an
// images LIST (api.ImageData is []byte, with no media type). That shape has no
// place to put an image BETWEEN two texts, and none to state what an image's
// bytes are, so:
//
//   - [text A, image X, text B] reaches the model as "A\n\nB" with X attached
//     after it, while the metered gateway leg writes the three parts in the
//     order the client wrote them (tools/gateway/messages.go, tested there).
//   - the client's media_type is dropped with the label: the bytes travel and
//     whoever reads them sniffs (or, at the OpenAI door this leg feeds,
//     cmd/launch's imageDataURL labels them by magic bytes and calls anything
//     it does not know jpeg, while the gateway leg prefers the client's own
//     label for those).
//
// Both divergences are REJECTED as findings rather than fixed (2026-09-28 audit,
// round 56, rejections):
//
//   - Folding the gateway's parts to match — one text part joined with "\n\n",
//     images after it — would import this side's invented separator and destroy
//     an order the client wrote and the other leg can carry, for a leg whose
//     upstream wire has room for it. Round 52 fixed this same leg the other way
//     round: it was taught to keep the client's block order rather than hoist
//     text past the results it followed, because "one client body was two
//     different conversations". The gateway's arrival order is the reference.
//   - Splitting the run into several messages (one per image) to preserve order
//     is worse than either: it changes the conversation's structure — the turn
//     becomes two or three user messages — and consecutive same-role messages
//     are refused outright by templates that require alternation, so a body
//     that is served everywhere today would 400 on some backends.
//   - Labelling an image cannot be unified either: this wire carries no label
//     at all, so the two sides' labels are invented rather than translated. The
//     gateway's rule (the bytes' own magic, else the client's stated type, else
//     jpeg) is the one that keeps the most of what the client said; the door's
//     blanket jpeg for payloads it cannot sniff is a guess this side has no
//     room to improve on. Forcing the gateway to that guess would hand a
//     backend a WRONG label for a picture the client labelled correctly.
//
// Both renderings are pinned below so a later round sees this decision rather
// than rediscovering the difference, and so any change to either side trips a
// test that names the trade.

import (
	"encoding/json"
	"testing"
)

// r56RunBody is one user turn holding text, an image and more text.
const r56RunBody = `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
	`{"type":"text","text":"A"},` +
	`{"type":"image","source":{"type":"base64","media_type":"image/bmp","data":"QUJD"}},` +
	`{"type":"text","text":"B"}]}]}`

func TestARunOfTextAndImagesIsFoldedByTheApiMessageShape(t *testing.T) {
	var req MessagesRequest
	if err := json.Unmarshal([]byte(r56RunBody), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(conv.Messages) != 1 {
		t.Fatalf("the turn became %d messages: %+v", len(conv.Messages), conv.Messages)
	}
	m := conv.Messages[0]
	if m.Content != "A\n\nB" || len(m.Images) != 1 {
		t.Errorf("the turn reached the model as content=%q images=%d, want \"A\\n\\nB\" and 1 image\n"+
			"api.Message is a content string plus an images list, so the image cannot sit between the two texts: the metered gateway leg writes the parts in the order the client wrote them, and this side folds them (round 56, F56-4, rejected — see the file comment)", m.Content, len(m.Images))
	}
}

// The other direction of the same shape: a turn that OPENS with an image and
// follows it with text puts the text first on this wire.
func TestAnImageBeforeTextFollowsItOnThisWire(t *testing.T) {
	body := `{"model":"m","max_tokens":64,"messages":[{"role":"user","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"QUJD"}},` +
		`{"type":"text","text":"what is this"}]}]}`
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	m := conv.Messages[0]
	if m.Content != "what is this" || len(m.Images) != 1 {
		t.Errorf("the turn reached the model as content=%q images=%d, want the text and the image beside it", m.Content, len(m.Images))
	}
}

// The label the client stated does not survive this wire: api.ImageData is
// bytes alone, so the media_type is dropped here and invented again downstream.
func TestAnImageKeepsNoLabelOnThisWire(t *testing.T) {
	var req MessagesRequest
	if err := json.Unmarshal([]byte(r56RunBody), &req); err != nil {
		t.Fatalf("decode: %v", err)
	}
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if got := string(conv.Messages[0].Images[0]); got != "ABC" {
		t.Errorf("the image reached the model as %q, want the payload's bytes: the label is what this wire has no room for", got)
	}
}
