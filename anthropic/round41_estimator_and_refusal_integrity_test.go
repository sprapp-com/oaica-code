package anthropic

// round41_estimator_and_refusal_integrity_test.go — round 41's estimator
// findings on this leg (A41-2, A41-3, A41-4, A41-5, C41-9):
//
// The estimate seeds the client-visible input_tokens whenever the upstream
// states no usage, so it is not a cosmetic number: it is what a session's
// context meter and auto-compaction read. Each arm of it has to charge what the
// CONVERTER writes, not what the block carries and not what the transport
// spends carrying it — three separate errors, one per arm, all in the same
// direction (a prompt reported far larger than the model was sent).
//
// A41-5 is separate: a source DECLARED as a url whose text is not fetchable.
// Carried as bytes it reached the media sniffer as plain characters, which it
// labelled text/plain and forced to image/jpeg — the model was shown a picture
// of the address text.

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

// round41Estimate estimates the prompt for one /v1/messages body.
func round41Estimate(t *testing.T, body string) int {
	t.Helper()
	var req MessagesRequest
	if err := json.Unmarshal([]byte(body), &req); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	return EstimateInputTokens(req)
}

// round41PdfSource is one document block's source, carrying `data` as base64.
func round41PdfSource(data string) string {
	b, _ := json.Marshal(map[string]any{"type": "base64", "media_type": "application/pdf", "data": data})
	return string(b)
}

// TestANestedDocumentIsChargedWhatTheConverterWrites is A41-2. A binary the
// converter DESCRIBES in one line is charged that line: charging the source's
// data instead billed the transport encoding of a file the model is told about
// in sixty bytes, and since the estimate seeds the client-visible input_tokens
// a session carrying one attachment read as a prompt six figures long.
//
// The two sizes differ by exactly the digits in the notice ("4 bytes" against
// "400000 bytes"), so the charges have to agree to within a token or so — and
// both have to be nowhere near the payload's own cost.
func TestANestedDocumentIsChargedWhatTheConverterWrites(t *testing.T) {
	body := func(data string) string {
		return fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"search_result","title":"t","source":"https://e.test","content":[{"type":"text","text":"ok"},{"type":"document","source":%s}]}]}]}`, round41PdfSource(data))
	}
	small := round41Estimate(t, body("AAAA"))
	big := round41Estimate(t, body(strings.Repeat("A", 400000)))

	if d := big - small; d > 3 || d < 0 {
		t.Errorf("a 400 KB PDF nested in a search_result measured %d against %d for the same block with four bytes of base64: the converter describes both in one line, so the only difference the estimate may charge is the five digits of the size it names (%d charged)", big, small, d)
	}
	if big > 100 {
		t.Errorf("a 400 KB PDF nested in a search_result was charged %d estimated tokens: the model is told about it in one line, and its base64 is never carried (%d tokens would be the payload alone)", big, 400000/4)
	}
}

// TestADocumentBlockIsChargedWhatTheConverterWrites is A41-2 at the other shape
// the arm is reached from: a document as a direct element of a message's
// content array.
func TestADocumentBlockIsChargedWhatTheConverterWrites(t *testing.T) {
	body := func(data string) string {
		return fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"ok"},{"type":"document","source":%s}]}]}`, round41PdfSource(data))
	}
	small := round41Estimate(t, body("AAAA"))
	big := round41Estimate(t, body(strings.Repeat("A", 400000)))

	if d := big - small; d > 3 || d < 0 {
		t.Errorf("a 400 KB PDF measured %d against %d for the same block with four bytes of base64 (%d charged): the converter describes the source in one line, so the payload's size may not appear in the estimate", big, small, d)
	}
	if big > 100 {
		t.Errorf("a 400 KB PDF in a message's content array was charged %d estimated tokens (the payload alone would be %d)", big, 400000/4)
	}
}

// TestATypedDocumentIsChargedItsTextAndNothingElse is A41-2 on the typed path:
// only a TEXT source is written into the prompt, and the converter refuses
// every other source.type outright — so a base64 document is charged nothing at
// all, and a text document is charged its text (not its text plus a `ref` the
// converter never reads).
func TestATypedDocumentIsChargedItsTextAndNothingElse(t *testing.T) {
	text := strings.Repeat("B", 5000)
	base := round41Estimate(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"ok"},{"type":"document","source":{"type":"text","data":"`+text+`"}}]}]}`)
	// The estimate is bytes/4, and the text is the only thing the converter
	// carries: a base64 source on the same body would add nothing to it.
	want := len(text) / 4
	if d := base - want; d > 2 || d < -2 {
		t.Errorf("a 5000-character text document estimated %d tokens (a body without it estimates ~0): the converter writes the text, so %d is the charge", base, want)
	}

	binary := round41Estimate(t, `{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"ok"},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"`+strings.Repeat("A", 400000)+`"}}]}]}`)
	if binary > 3 {
		t.Errorf("a typed document with a base64 source estimated %d tokens: the converter refuses that source.type, so no prompt carrying it ever reaches a model to have a size (%d tokens is the payload alone)", binary, 400000/4)
	}
}

// TestAWebSearchToolResultIsChargedItsHitsNotItsEncryptedContent is A41-3. The
// converter writes the hits — one line each — and nothing else, so serializing
// the block pasted back an opaque blob the wire never holds. The estimate is
// the client-visible prompt size, so a search whose results carry encrypted
// content read as a prompt hundreds of kilobytes long that the model was never
// sent, and the session compacted early.
func TestAWebSearchToolResultIsChargedItsHitsNotItsEncryptedContent(t *testing.T) {
	body := func(encrypted string) string {
		extra := ""
		if encrypted != "" {
			extra = `,"encrypted_content":"` + encrypted + `"`
		}
		return `{"model":"m","messages":[{"role":"user","content":[{"type":"web_search_tool_result","tool_use_id":"x","content":[{"type":"web_search_result","title":"t","url":"https://e.test"` + extra + `}]}]}]}`
	}
	plain := round41Estimate(t, body(""))
	blob := round41Estimate(t, body(strings.Repeat("A", 400000)))
	if blob != plain {
		t.Errorf("a search hit carrying 400 KB of encrypted_content measured %d against %d without it: the converter writes the title, the url and a newline per hit, and charges nothing for the blob (%d charged)", blob, plain, blob-plain)
	}
	if plain <= 0 {
		t.Errorf("a web_search_tool_result holding one hit was charged %d at all: its title and url reach the prompt", plain)
	}
}

// TestAToolResultsTextIsChargedItsCharactersNotItsEscapes is A41-4. The
// upstream JSON-decodes the body before anything is tokenized, so an escape
// describes the transport and not the prompt. Charging the escaped spelling
// billed every quote, backslash and newline in a tool result six bytes per
// character, where the product's other two measures charge the decoded text —
// one body, three sizes, and the one the session is shown was the largest.
func TestAToolResultsTextIsChargedItsCharactersNotItsEscapes(t *testing.T) {
	for _, tc := range []struct{ name, ch string }{
		{"quote", `"`}, {"backslash", `\`}, {"newline", "\n"}, {"tab", "\t"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := strings.Repeat(tc.ch, 1000)
			esc, _ := json.Marshal(s)            // the spelling a JSON writer sends
			inner := string(esc[1 : len(esc)-1]) // and the same text as it appears inside a body

			asText := round41Estimate(t, fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"text","text":"%s"}]}]}`, inner))
			asResult := round41Estimate(t, fmt.Sprintf(`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"x","content":"%s"}]}]}`, inner))

			// The tool_result is charged its whole JSON, so it carries the
			// envelope — the type, the id, the quotes — on top of the text.
			// What must not appear is the escape overhead, which for these
			// characters is a full thousand bytes.
			if asResult > asText+200 {
				t.Errorf("%s: the same 1000 characters cost %d as a text block and %d inside a tool_result: the difference (%d) is the escape overhead, which the JSON reader has already removed before anything is tokenized", tc.name, asText, asResult, asResult-asText)
			}
		})
	}
}

// TestABareToolResultObjectIsDescribedNotErased is C41-9. The spec asks for a
// one-element array; a client that sends the object alone had its whole tool
// answer dropped, and the model was told the tool returned nothing while the
// client read a 200. The gateway leg, given the identical block, puts its JSON
// in the prompt — so both legs now describe it the same way.
func TestABareToolResultObjectIsDescribedNotErased(t *testing.T) {
	got, images, err := convertToolResultContent(map[string]any{"type": "text", "text": "the tool's answer"})
	if err != nil {
		t.Fatalf("a bare object was refused: %v", err)
	}
	if len(images) != 0 {
		t.Errorf("a text-only block produced %d images", len(images))
	}
	if !strings.Contains(got, "the tool's answer") {
		t.Errorf("a bare object in a tool_result converted to %q: the tool's answer was erased and the model was told the tool returned nothing", got)
	}

	// A scalar cannot be a block; what must not happen is silence about it.
	if got, _, err := convertToolResultContent("a bare scalar answer"); err != nil || !strings.Contains(got, "a bare scalar answer") {
		t.Errorf("a scalar tool result converted to %q (err %v): the content the client sent has to reach the prompt", got, err)
	}
}

// TestASourceDeclaredAsAURLMustBeFetchable is A41-5. The only thing that makes
// a `url` source an image rather than a string is that a reader can fetch it.
// Text with no scheme used to be carried as bytes and forced through the media
// sniffer to image/jpeg, so the model was shown a picture of the address text
// while the picture itself was never requested.
func TestASourceDeclaredAsAURLMustBeFetchable(t *testing.T) {
	if _, err := resolveImageSource(&ImageSource{Type: "url", URL: "example.com/shot.png"}); err == nil {
		t.Errorf("a source declared as a url with scheme-less text was accepted: it is not fetchable, and carried as bytes the model is shown a picture of the address")
	}
	if _, err := resolveImageSource(&ImageSource{Type: "url", URL: ""}); err == nil {
		t.Errorf("a url source with no url was accepted")
	}

	img, err := resolveImageSource(&ImageSource{Type: "url", URL: "https://e.test/shot.png"})
	if err != nil {
		t.Fatalf("a fetchable url source was refused: %v", err)
	}
	if string(img) != "https://e.test/shot.png" {
		t.Errorf("a fetchable url source was carried as %q, want the address itself", string(img))
	}
}
