package anthropic

// round60_estimate_written_block_integrity_test.go — round 60's three findings
// on the local leg's estimate (F60-L1-1, F60-L1-2, F60-L1-3).
//
// One question runs through all three, and it is the question rounds 57 to 59
// kept asking one arm at a time: the estimate charges what the converter
// WRITES, and every arm that answered it had been taught only the block types
// the walk happened to know. Round 59 taught two arms that a document and a
// search_result are written into the run beside the text; what it left behind is
// the three cases below.
//
//   - F60-L1-1: systemContentIsTextOnly — the question the rewrite asks before
//     merging a system message into the single leading system string — still
//     asked for "text" blocks alone, so a system message the converter writes in
//     full because it holds a text-sourced DOCUMENT was read as not text-only.
//     The rewrite then refused to merge the conversation (normalizeSystemFirst
//     leaves a list holding a non-text system message exactly as it arrived),
//     the blank system message beside it survived instead of being deleted, and
//     the same conversation was billed 25003 tokens spelled as a document
//     against 2 spelled as text.
//   - F60-L1-2: the same predicate's other direction, and a regression round 59
//     introduced. A system message whose document the converter writes but the
//     hoist then DELETES — a whitespace-only payload, which the hoist trims to
//     blank — is charged its payload by joinedMessageText while the prompt it
//     was dropped from holds one bare user turn: 25001 tokens for a 41-byte
//     prompt. (Round 58's tree charged 1, so this is a fix that grew a new hole.)
//   - F60-L1-3: a conversation whose only turn converts to a message carrying
//     nothing — a bare tool_result block, with no call id and no content — is
//     replaced by FromMessagesRequest's fallback, and the charge must follow the
//     replacement. The walk counted every tool_result as a message that states
//     something (toolResults > 0), so the fallback never fired in the estimate:
//     10 tokens for the one-word prompt the model is actually sent.
//
// All three seed the client-visible input_tokens whenever the upstream states no
// usage, and size auto-compaction. Each case is fail-first: RED against the tree
// before this round's fix.

import (
	"encoding/json"
	"strings"
	"testing"
)

// r60Charge converts one body and returns the estimate and the prompt it
// converts to — the prompt is the premise every case here rests on.
func r60Charge(t *testing.T, body string) (int, string) {
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

// r60Spaces is a payload of n spaces: a block the converter writes, and the one
// shape the hoist then throws away.
func r60Spaces(n int) string {
	return strings.Repeat(" ", n)
}

// TestABlankSystemMessageBesideADocumentIsMerged is F60-L1-1. The rewrite asks
// whether a system message is text-only before it merges the conversation, and
// the answer must be the converter's: a text-sourced document IS written as
// text, so a system message holding one neither blocks the merge nor survives
// as a turn of its own beside the blank one the hoist deletes.
func TestABlankSystemMessageBesideADocumentIsMerged(t *testing.T) {
	blank := r60Spaces(100000)
	asDocument := `{"model":"m","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"` + blank + `"}]},` +
		`{"role":"system","content":[{"type":"document","source":{"type":"text","data":"D"}}]}]}`
	asText := `{"model":"m","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"` + blank + `"}]},` +
		`{"role":"system","content":[{"type":"text","text":"D\n"}]}]}`

	got, prompt := r60Charge(t, asDocument)
	want, wantPrompt := r60Charge(t, asText)
	if prompt != wantPrompt {
		t.Fatalf("premise: the two bodies convert to different prompts:\n%s\n%s", prompt, wantPrompt)
	}
	if got != want {
		t.Errorf("the same %d-byte conversation is charged %d spelled as a document and %d spelled as text:\n%s\n"+
			"systemContentIsTextOnly asked for \"text\" blocks alone, so a system message the converter writes as the document's own text was read as not text-only: the rewrite refused to merge, the blank message beside it survived instead of being deleted, and its 100000 spaces were billed as a turn the model is not sent (2026-09-28 audit, round 60, F60-L1-1)",
			len(prompt), got, want, prompt)
	}

	// The control: the blank text message alone still leaves the conversation
	// exactly as it arrived — nothing here merges a message that is not text.
	_, onlyDocument := r60Charge(t, `{"model":"m","messages":[{"role":"system","content":[{"type":"document","source":{"type":"text","data":"D"}}]}]}`)
	if !strings.Contains(onlyDocument, `"content":"D\n"`) {
		t.Errorf("control: a system message holding a text document converted to %s, want the document's own text:\n%s", onlyDocument, onlyDocument)
	}
}

// TestAWhitespaceDocumentSystemMessageIsChargedTheRewrite is F60-L1-2. A system
// message the hoist deletes — because the text the converter writes for it is
// blank — is not in the prompt, and the estimate must charge the prompt, which
// is the one bare user turn the fallback writes.
func TestAWhitespaceDocumentSystemMessageIsChargedTheRewrite(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"system","content":[` +
		`{"type":"document","source":{"type":"text","data":"` + r60Spaces(100000) + `"}}]}]}`
	got, prompt := r60Charge(t, body)
	if len(prompt) > 60 {
		t.Fatalf("premise: the prompt is %d bytes, expected the contentless fallback:\n%s", len(prompt), prompt)
	}
	if got > 2 {
		t.Errorf("a system message the rewrite deleted converted to %s (%d bytes) and was charged %d tokens:\n"+
			"the hoist trims the document's text to blank and drops the message, and joinedMessageText billed the payload it dropped — 25001 tokens for a prompt that holds one bare user turn, against the 1 the same conversation without the message is charged (2026-09-28 audit, round 60, F60-L1-2)",
			prompt, len(prompt), got)
	}

	// The control: a document whose text SURVIVES the hoist is charged for it.
	live := `{"model":"m","messages":[{"role":"system","content":[` +
		`{"type":"document","source":{"type":"text","data":"a document the converter keeps"}}]}]}`
	if c, cp := r60Charge(t, live); c <= 2 {
		t.Errorf("control: a surviving system document (%d-byte prompt) was charged %d:\n%s", len(cp), c, cp)
	}
}

// TestAContentlessToolResultTurnIsChargedTheRewrite is F60-L1-3. A tool_result
// block that states nothing — no call id, no content — converts to a tool
// message carrying nothing, so the conversation is replaced by the fallback and
// the charge must be the fallback's, not the role of the message it replaced.
func TestAContentlessToolResultTurnIsChargedTheRewrite(t *testing.T) {
	body := `{"model":"m","messages":[{"role":"assistant","content":[{"type":"tool_result"}]}]}`
	got, prompt := r60Charge(t, body)
	if len(prompt) > 60 {
		t.Fatalf("premise: the prompt is %d bytes, expected the contentless fallback:\n%s", len(prompt), prompt)
	}
	if got > 1 {
		t.Errorf("a turn that converts to %s (%d bytes) was charged %d tokens:\n"+
			"the walk counted every tool_result as a message that STATES something, so the rewrite that replaced this one never fired in the estimate: the tool message carries no content, no images and no call id (anyMessageCarriesContent), and the prompt the model is sent is one bare user turn (2026-09-28 audit, round 60, F60-L1-3)",
			prompt, len(prompt), got)
	}

	// The control: the same block WITH a call id states something, survives the
	// rewrite, and is charged for it.
	stated := `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1",` +
		`"content":[{"type":"text","text":"a result the converter writes"}]}]}]}`
	if c, cp := r60Charge(t, stated); c <= 1 {
		t.Errorf("control: a tool_result carrying a call id and text (%d-byte prompt) was charged %d:\n%s", len(cp), c, cp)
	}

	// The other control: the id alone is enough — the tool message states which
	// call it answers even when it carries no content.
	byID := `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"call_1"}]}]}`
	if c, cp := r60Charge(t, byID); c <= 1 {
		t.Errorf("control: a tool_result stating only its call id (%d-byte prompt) was charged %d:\n%s", len(cp), c, cp)
	}
}
