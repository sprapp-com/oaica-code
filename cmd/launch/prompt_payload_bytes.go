package launch

import (
	"bytes"
	"encoding/json"

	"github.com/ollama/ollama/anthropic"
)

// The prompt-size unit the client proxy's context-fit clamp and its
// calibration both use. tools/gateway measures the same thing server-side
// (messagesBytes / promptPayloadBytes): the serialized OpenAI-wire prompt with
// any inline image's transport encoding replaced by a fixed per-image
// allowance.
//
// Kept as one function so the clamp, the two record call sites and the
// estInputTokens fallback cannot drift from each other -- a calibrated ratio
// is only meaningful if every site that produces or consumes one counts the
// same bytes.
//
// The unit is taken from the CONVERTED request, not from the Anthropic body
// the client sent, because the two differ by every block the conversion does
// not forward. Measuring the client's body charged the prompt for content the
// upstream never tokenizes, in both directions (2026-09-27 audit, round 34,
// A-F1/A-F2):
//
//   - Over-charged: a session with extended thinking echoes its own reasoning
//     back, and the OpenAI wire has no thinking block at all
//     (api.Message.Thinking is never emitted by chatRequestToOpenAI, and a
//     redacted_thinking payload is opaque). ~800 KB of reasoning was charged as
//     ~200k prompt tokens of a 262144-token leg, so the clamp refused a healthy
//     turn LOCALLY with "prompt is too long" -- the string Claude Code matches
//     to its compaction recovery path. Compaction cannot help (the reasoning is
//     in the newest turns), and a locally refused request reports no usage, so
//     the session can never calibrate out of it.
//   - Under-charged: the walk discounted any map keyed "source" holding a
//     "data" string, so a document block with a TEXT source had its whole text
//     replaced by the 4096-byte image allowance although the converter writes
//     that text into the prompt. A 900 KB attachment was charged as 4 KB, and
//     the clamp forwarded a request the upstream must reject.
//
// Deriving the unit from the conversion itself means the question "what does
// the upstream actually receive?" cannot go out of date as the converter gains
// or loses block types; round 33 fixed the same unit's image over-charge, and
// each enumeration of "blocks that are dropped" was one block short.
func clientPromptBytes(body []byte) int {
	serialized, imagePayload, images, ok := convertedPromptBody(body)
	if !ok {
		// A body this proxy cannot convert has no prompt unit to measure. The
		// raw length is the coarse fallback, and it is only ever used for a
		// request that is about to be refused as unconvertible anyway.
		return len(body)
	}
	return serialized - imagePayload + images*inlineImageByteAllowance
}

// inlineImageByteAllowance matches tools/gateway's imagePartByteAllowance: an
// inline image is charged as an image, not as its base64.
const inlineImageByteAllowance = 4096

// convertedPromptBody serializes the OpenAI-wire request an Anthropic body
// becomes -- the translation the proxy sends upstream, in the unescaped form
// the upstream decodes it to (see marshalPrompt) -- and reports the total
// inline-image data-URI length that serialization carries and how many images
// there are. The model field is the request's own id: the resolved upstream id
// is not known here and the two differ by a few bytes against a prompt
// measured in kilobytes and up.
//
// ok is false when the body is not an Anthropic Messages request, which is the
// same input the proxy itself refuses.
func convertedPromptBody(body []byte) (serialized, imagePayload, images int, ok bool) {
	var anthReq anthropic.MessagesRequest
	if err := json.Unmarshal(body, &anthReq); err != nil {
		return 0, 0, 0, false
	}
	chatReq, err := anthropic.FromMessagesRequest(anthReq)
	if err != nil {
		return 0, 0, 0, false
	}
	oai := chatRequestToOpenAI(chatReq, anthReq, anthReq.Model)

	// The payload the wire carries is the data URI openAIMessage.MarshalJSON
	// writes, so it is read from the same field that marshaller reads.
	for _, m := range oai.Messages {
		for _, img := range m.Images {
			images++
			imagePayload += len(img.DataURL)
		}
	}

	n, err := marshalPrompt(oai)
	if err != nil {
		return 0, 0, 0, false
	}
	return n, imagePayload, images, true
}

// marshalPrompt returns the length of the converted request as the upstream
// READS it, which is not the length of the bytes that carry it. encoding/json
// writes `<`, `>` and `&` as their six-character `\u` escapes, so a markup
// prompt (HTML, XML, JSX, SVG, `2>&1`, a tool result holding a file) was
// charged up to six bytes per byte — 6x its real size — and the clamp refused
// a healthy turn locally with "prompt is too long", the compaction-trigger
// wording, which cannot help because the markup is in the newest turn
// (2026-09-27 audit, round 35, A-F1). The upstream JSON-decodes the body
// before tokenizing anything, so the escaping describes the transport, not the
// prompt; the image base64 is discounted for the same reason.
//
// The escaping is undone by counting it rather than by re-encoding: the
// conversion's own marshaller (openAIMessage.MarshalJSON) re-marshals each
// message internally, so a top-level json.Encoder with SetEscapeHTML(false)
// never reaches the text it would have to leave alone, and the request is
// still SENT escaped — a valid encoding of the same string.
func marshalPrompt(v any) (int, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, err
	}
	return len(b) - markupEscapeOverhead(b), nil
}

// markupEscapeOverhead counts the bytes json.Marshal spends writing `<`, `>`
// and `&` as `<`, `>` and `&`: five extra bytes each, and the
// only escapes it writes that a JSON reader turns back into one byte.
//
// The scan follows backslashes the way a JSON reader does, so a prompt holding
// the literal characters `<` (escaped as `\\u003c`) is not counted: its
// backslash is consumed as the two-character escape it is, and the `u003c`
// after it is ordinary text.
func markupEscapeOverhead(b []byte) int {
	extra := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			continue
		}
		switch b[i+1] {
		case '\\', '"', '/':
			i++ // an escaped backslash, quote or solidus: no markup behind it
		case 'u':
			if i+5 < len(b) && isEscapedMarkup(b[i+2:i+6]) {
				extra += 5
				i += 5
			}
		}
	}
	return extra
}

// isEscapedMarkup reports whether a four-character `\u` payload is one of the
// three characters Go escapes for HTML embedding: `<` (003c), `>` (003e) and
// `&` (0026), written in the lowercase hex encoding/json emits.
func isEscapedMarkup(hex []byte) bool {
	return bytes.Equal(hex, []byte("003c")) ||
		bytes.Equal(hex, []byte("003e")) ||
		bytes.Equal(hex, []byte("0026"))
}
