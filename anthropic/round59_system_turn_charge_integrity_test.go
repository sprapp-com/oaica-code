package anthropic

// round59_system_turn_charge_integrity_test.go — round 59's two findings on the
// local leg's estimate (F59-L1-1, F59-L1-2).
//
// Both are the same question round 58 asked, taken one step further: the
// estimate charges the conversation that SURVIVES the rewrite, and the walk that
// models it (messageShapeBytes) asked the client's block types what the message
// carries rather than what the converter writes for it.
//
//   - F59-L1-1: the system arm was the one place the early "does this message
//     carry anything" return was not taken, so a system message the converter
//     writes in FULL — one holding a document, an image, a search_result — was
//     charged the role of a turn and nothing of its content. The prompt carries
//     the payload verbatim (the converted message holds the document's text),
//     and the estimate billed a 4034-byte prompt the same 1 token a wholly
//     blank conversation is billed.
//   - F59-L1-2: the same arm asked the CLIENT's block types whether a system
//     message was text-only, so a system message holding nothing the converter
//     writes — the empty search_result is the shape here — was read as carrying
//     content, which suppressed the rewrite that DELETES it. The deleted message
//     was charged 3 tokens the prompt does not contain, on a body whose
//     converted prompt is byte-identical to the same conversation without it.
//
// Both seed the client-visible input_tokens whenever the upstream states no
// usage, so a wrong charge here is the session's meter and its auto-compaction
// threshold reading a prompt the model was never sent. Each case is fail-first:
// RED against the tree before this round's fix.

import (
	"encoding/json"
	"testing"
)

// r59Charge converts one body and returns the estimate and the prompt it
// converts to — the prompt is the premise every case here rests on.
func r59Charge(t *testing.T, body string) (int, string) {
	t.Helper()
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("decode %s: %v", body, err)
	}
	conv, err := FromMessagesRequest(req)
	if err != nil {
		t.Fatalf("convert %s: %v", body, err)
	}
	prompt, err := json.Marshal(conv.Messages)
	if err != nil {
		t.Fatalf("marshal prompt: %v", err)
	}
	return EstimateInputTokens(req), string(prompt)
}

// TestASystemOnlyTurnIsChargedWhatItCarries is F59-L1-1. A body whose only
// message is a system message holding a document converts to a prompt that
// carries the document's thousand-character payload, and the estimate must
// charge that prompt rather than the empty conversation.
func TestASystemOnlyTurnIsChargedWhatItCarries(t *testing.T) {
	payload := ""
	for i := 0; i < 1000; i++ {
		payload += "0123456789"
	}

	docBody := `{"model":"m","messages":[{"role":"system","content":[` +
		`{"type":"document","source":{"type":"text","data":"` + payload + `"}}]}]}`
	got, prompt := r59Charge(t, docBody)
	if len(prompt) < 4000 {
		t.Fatalf("premise: the system-only document converted to a %d-byte prompt:\n%s", len(prompt), prompt)
	}
	if got < 500 {
		t.Errorf("a system message the converter wrote a %d-byte document for was charged %d tokens:\n%s\n"+
			"the system arm asked whether the message carried content and never took the answer, so the payload the prompt holds was billed as nothing — the 1 token a wholly blank conversation is charged (2026-09-28 audit, round 59, F59-L1-1)",
			len(prompt), got, prompt)
	}

	// The control: the same payload stated as text is charged the same prompt,
	// so the two spellings of one conversation cannot be billed differently.
	textBody := `{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"` + payload + `"}]}]}`
	asText, textPrompt := r59Charge(t, textBody)
	if got != asText {
		t.Errorf("the same %d-byte conversation is charged %d as a document and %d as text:\n%s\n%s",
			len(prompt), got, asText, prompt, textPrompt)
	}

	// The other control: a system message holding a blank text block is deleted
	// by the hoist and the body falls back to one user turn — one token, which is
	// what makes the charge above a charge of CONTENT rather than of the role.
	blank, blankPrompt := r59Charge(t, `{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"  "}]}]}`)
	if blank != 1 {
		t.Errorf("a system message holding only blanks was charged %d, want 1 (the one user turn the rewrite writes):\n%s", blank, blankPrompt)
	}

	// An image in the system position is the same shape one block type over.
	imgBody := `{"model":"m","messages":[{"role":"system","content":[` +
		`{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + payload + `"}}]}]}`
	if img, imgPrompt := r59Charge(t, imgBody); img < 500 {
		t.Errorf("a system message holding a %d-byte image was charged %d tokens:\n%s", len(imgPrompt), img, imgPrompt)
	}
}

// TestAnEmptySearchResultSystemMessageIsNotCharged is F59-L1-2. The converter
// writes nothing for an empty search_result block, the hoist deletes a system
// message it wrote nothing for, and the estimate must charge the conversation
// that is left — which is byte-identical to the same body without the message.
func TestAnEmptySearchResultSystemMessageIsNotCharged(t *testing.T) {
	withIt := `{"model":"m","messages":[` +
		`{"role":"system","content":[{"type":"search_result"}]},` +
		`{"role":"user","content":[{"type":"text","text":"x y"}]}]}`
	without := `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"x y"}]}]}`

	a, aPrompt := r59Charge(t, withIt)
	b, bPrompt := r59Charge(t, without)
	if aPrompt != bPrompt {
		t.Fatalf("premise: the two bodies convert to different prompts:\n%s\n%s", aPrompt, bPrompt)
	}
	if a != b {
		t.Errorf("an empty search_result system message was charged %d against %d without it:\n%s\n"+
			"the message is deleted by the rewrite (the converter writes nothing for the block), so the charge must be the surviving conversation's — the deleted turn's role was billed for a prompt that does not contain it (2026-09-28 audit, round 59, F59-L1-2)",
			a, b, aPrompt)
	}

	// The control: a system message holding a search_result that DOES write text
	// survives the rewrite and is charged for what it writes.
	full := `{"model":"m","messages":[` +
		`{"role":"system","content":[{"type":"search_result","title":"t",` +
		`"content":[{"type":"text","text":"a search result the converter writes"}]}]},` +
		`{"role":"user","content":[{"type":"text","text":"x y"}]}]}`
	c, cPrompt := r59Charge(t, full)
	if c <= b {
		t.Errorf("a search_result carrying text was charged %d against %d without it:\n%s", c, b, cPrompt)
	}
}
