package launch

// round36_search_result_and_estimator_integrity_test.go — a content-bearing
// block the client converter drops, the same drop one level down inside a tool
// result, an estimator that charges neither, and every JSON escape the unit
// still billed as transport (2026-09-27 audit, round 36, A-F1/A-F2/A-F3/A-F5).
//
//   - A-F1: `search_result` is a documented Anthropic block that CARRIES the
//     passages the model is asked about (content:[{type:"text"}], plus a title
//     and a source). `convertMessage` has no case for it: the passages are
//     dropped, the turn is answered 200, and the model is asked a question
//     about text it never received — the class round 35 closed for `document`.
//     The gateway leg refuses the identical body, so the two legs disagree.
//   - A-F2: a document nested inside a `tool_result` is skipped by
//     `convertToolResultContent`, which handles only text and image blocks.
//     The gateway leg DESCRIBES the same nested block (a text source by its
//     text), so the same body yields a different prompt per leg and the
//     attachment vanishes on this one with a 200.
//   - A-F3: `countContentBlock` charges nothing for a document or a
//     search_result, although the converter writes their text into the prompt.
//     EstimateInputTokens is the fallback `middleware/anthropic.go` seeds the
//     stream converter's input_tokens with when the upstream states no usage,
//     so a session whose newest turn is a large attachment is told ~6 input
//     tokens and its context meter never grows.
//   - A-F5: `markupEscapeOverhead` undoes only the three HTML escapes, so
//     `"`, `\`, `\n`, `\t` (two bytes each) and every control character (six)
//     were still charged to the prompt. The unit has to describe the DECODED
//     prompt, so every escape json.Marshal writes has to be undone.

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/ollama/ollama/anthropic"
)

// TestASearchResultBlockIsCarriedIntoThePrompt is A-F1.
func TestASearchResultBlockIsCarriedIntoThePrompt(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const passage = "The study found that tab indentation correlates with nothing at all."
	body := round34MessagesBody(t, "user", []map[string]any{
		{"type": "text", "text": "Answer using the search results."},
		{"type": "search_result", "source": "https://example.com/study", "title": "Study",
			"content": []map[string]any{{"type": "text", "text": passage}}},
	}, 4096)

	var anthReq anthropic.MessagesRequest
	if err := json.Unmarshal(body, &anthReq); err != nil {
		t.Fatalf("a search_result block makes the whole request unparseable: %v — the client's request is rejected outright rather than answered", err)
	}
	chatReq, err := anthropic.FromMessagesRequest(anthReq)
	if err != nil {
		t.Fatalf("converting a turn carrying search results failed: %v", err)
	}
	converted, err := json.Marshal(chatRequestToOpenAI(chatReq, anthReq, anthReq.Model))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(converted, []byte("correlates with nothing")) {
		t.Errorf("the passages the client asked about are not in the prompt: %s\nthe model answers a question about search results it was never shown, and the client reads a 200", converted)
	}
}

// TestADocumentInsideAToolResultIsNotDroppedByTheClientLeg is A-F2.
func TestADocumentInsideAToolResultIsNotDroppedByTheClientLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const fileText = "the attached style guide says: use tabs."
	body := round34MessagesBody(t, "user", []map[string]any{
		{"type": "tool_result", "tool_use_id": "toolu_1", "content": []map[string]any{
			{"type": "text", "text": "read the file"},
			{"type": "document", "source": map[string]any{
				"type": "text", "media_type": "text/plain", "data": fileText}},
		}},
	}, 4096)

	var anthReq anthropic.MessagesRequest
	if err := json.Unmarshal(body, &anthReq); err != nil {
		t.Fatal(err)
	}
	chatReq, err := anthropic.FromMessagesRequest(anthReq)
	if err != nil {
		t.Fatalf("a tool result carrying a document failed to convert: %v", err)
	}
	converted, err := json.Marshal(chatRequestToOpenAI(chatReq, anthReq, anthReq.Model))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(converted, []byte("use tabs")) {
		t.Errorf("the tool returned a text file and its content never reached the prompt: %s\nthe gateway leg describes the same nested block by its text, so the same body is answered differently depending on which leg serves it", converted)
	}

	// A binary source cannot be carried, and must not be pasted in.
	pdf := strings.Repeat("JVBERi0xLjQK", 2000)
	binBody := round34MessagesBody(t, "user", []map[string]any{
		{"type": "tool_result", "tool_use_id": "toolu_1", "content": []map[string]any{
			{"type": "document", "source": map[string]any{
				"type": "base64", "media_type": "application/pdf", "data": pdf}},
		}},
	}, 4096)
	if err := json.Unmarshal(binBody, &anthReq); err != nil {
		t.Fatal(err)
	}
	chatReq, err = anthropic.FromMessagesRequest(anthReq)
	if err != nil {
		t.Fatalf("a tool result carrying a binary document failed to convert: %v", err)
	}
	converted, err = json.Marshal(chatRequestToOpenAI(chatReq, anthReq, anthReq.Model))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(converted, []byte("JVBERi0xLjQKJVBERi0xLjQK")) {
		t.Errorf("the tool result's base64 was pasted into the prompt: %s", converted[:400])
	}
	if !bytes.Contains(converted, []byte("application/pdf")) {
		t.Errorf("the model was told nothing about what the tool returned: %s", converted)
	}
}

// TestAnAttachmentIsChargedByTheTokenEstimate is A-F3.
func TestAnAttachmentIsChargedByTheTokenEstimate(t *testing.T) {
	const fileText = "invoice line items and totals. " // repeated below
	attachment := strings.Repeat(fileText, 30000)      // ~900 KB

	asText := anthropic.MessagesRequest{
		Model:     "kat-awq",
		MaxTokens: 4096,
		Messages: []anthropic.MessageParam{{Role: "user", Content: []anthropic.ContentBlock{
			{Type: "text", Text: strPtrR36(attachment)},
		}}},
	}
	asDocument := anthropic.MessagesRequest{
		Model:     "kat-awq",
		MaxTokens: 4096,
		Messages: []anthropic.MessageParam{{Role: "user", Content: []anthropic.ContentBlock{
			{Type: "document", Source: &anthropic.ImageSource{Type: "text", MediaType: "text/plain", Data: attachment}},
		}}},
	}
	textTokens := anthropic.EstimateInputTokens(asText)
	docTokens := anthropic.EstimateInputTokens(asDocument)
	if docTokens < textTokens/2 {
		t.Errorf("the same %d bytes are estimated at %d tokens as text and %d tokens as an attachment: the estimate is what middleware/anthropic.go seeds the stream converter's input_tokens with when the upstream states no usage, so a session whose newest turn is an attachment reports a prompt that never grew — and its context meter and auto-compaction never fire",
			len(attachment), textTokens, docTokens)
	}

	asSearch := anthropic.MessagesRequest{
		Model:     "kat-awq",
		MaxTokens: 4096,
		Messages: []anthropic.MessageParam{{Role: "user", Content: []anthropic.ContentBlock{
			{Type: "search_result", Content: []anthropic.ContentBlock{
				{Type: "text", Text: strPtrR36(attachment)},
			}},
		}}},
	}
	if got := anthropic.EstimateInputTokens(asSearch); got < textTokens/2 {
		t.Errorf("a turn carrying %d bytes of search results is estimated at %d tokens against %d for the same bytes as text", len(attachment), got, textTokens)
	}
}

// TestTheUnitDoesNotChargeAnyJSONEscaping is A-F5.
func TestTheUnitDoesNotChargeAnyJSONEscaping(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const n = 100000
	plain := round34MessagesBody(t, "user", []map[string]any{
		{"type": "text", "text": strings.Repeat("a", n)},
	}, 4096)
	baseline := clientPromptBytes(plain)

	for name, ch := range map[string]string{
		"quote":        `"`,
		"backslash":    `\`,
		"newline":      "\n",
		"tab":          "\t",
		"control-0x01": "\x01",
		"line-sep":     "\u2028",
	} {
		body := round34MessagesBody(t, "user", []map[string]any{
			{"type": "text", "text": strings.Repeat(ch, n)},
		}, 4096)
		got := clientPromptBytes(body)
		if got > baseline+64 || got < baseline-64 {
			t.Errorf("%d %s characters measure %d bytes against %d for the same number of plain characters: the upstream JSON-decodes the body before tokenizing anything, so a two- or six-byte escape for a one-byte character is the transport's cost and not the prompt's — and the same unit feeds the calibration ratio and the estInputTokens fallback",
				n, name, got, baseline)
		}
	}
}

// TestAnEmptyTextSourceDocumentIsRefusedForTheRightReason is a lead the round-36
// auditor raised: the refusal names "text" as unrepresentable when text is the
// one source type this wire CAN carry.
func TestAnEmptyTextSourceDocumentIsRefusedForTheRightReason(t *testing.T) {
	msg := anthropic.MessageParam{
		Role: "user",
		Content: []anthropic.ContentBlock{
			{Type: "document", Source: &anthropic.ImageSource{Type: "text", MediaType: "text/plain", Data: ""}},
		},
	}
	_, err := anthropic.FromMessagesRequest(anthropic.MessagesRequest{
		Model: "kat-awq", MaxTokens: 4096, Messages: []anthropic.MessageParam{msg},
	})
	if err == nil {
		t.Fatal("a document whose text source is empty converted with no error: the block contributes nothing to the prompt while the client reads a 200")
	}
	if strings.Contains(err.Error(), `"text" cannot be represented`) {
		t.Errorf("the refusal says a text source cannot be represented, which is the case it exists to carry: %v", err)
	}
}

func strPtrR36(s string) *string { return &s }
