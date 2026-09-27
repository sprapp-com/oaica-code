package launch

import (
	"encoding/json"
	"unicode/utf8"

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

	n, err := promptBytesOf(oai)
	if err != nil {
		return 0, 0, 0, false
	}
	return n, imagePayload, images, true
}

// promptBytesOf is the part of the converted request the upstream tokenizes: the
// messages and the tool schemas, each measured as its own JSON text, which is
// the unit the gateway leg's messagesBytes charges (its "messages" plus
// "tools"/"functions", each marshalled on its own).
//
// The ENVELOPE — the model id, `stream`, `stream_options`, the sampling options
// — is not prompt: the chat template renders none of it, so the upstream's
// prompt_tokens do not count it, and charging it made this leg's byte unit
// differ from the gateway leg's by a constant per request. The same prompt
// measured 12 tokens more with `stream: true` (and its include_usage
// instruction) than without, which is the flag Claude Code always sets: the
// calibrated tokens-per-byte ratio therefore carried a per-request offset, and
// the two legs' thresholds — a prompt the gateway admits against the client's
// refusal, or the reverse — disagreed by an amount no prompt can calibrate out
// (2026-09-27 audit, round 47, A-F2).
//
// A WRAPPER is an envelope too. The two fields were marshalled inside a
// synthetic {"messages":…,"tools":…} object, whose braces, key names and comma
// the gateway leg does not charge; measured against the same documents, this
// leg's unit carried 13 bytes the gateway's did not (22 with tools), which is a
// large share of a small prompt and a constant no calibration removes
// (2026-09-27 audit, round 48, C-F6).
//
// The messages keep the marshaller they had: openAIMessage.MarshalJSON is what
// writes the data-URI form the allowance below is measured against, and it runs
// for a slice element exactly as it did for the field of a request. A field the
// request does not carry is charged nothing, exactly as the gateway charges
// nothing for an absent key.
func promptBytesOf(oai openAIChatRequest) (int, error) {
	total := 0
	if len(oai.Messages) > 0 {
		n, err := marshalPrompt(oai.Messages)
		if err != nil {
			return 0, err
		}
		total += n
	}
	if len(oai.Tools) > 0 {
		n, err := marshalPrompt(oai.Tools)
		if err != nil {
			return 0, err
		}
		total += n
	}
	return total, nil
}

// marshalPrompt returns the length of the converted request as the upstream
// READS it, which is not the length of the bytes that carry it. encoding/json
// writes the characters it must escape as two- and six-character escapes, so a
// markup prompt (HTML, XML, JSX, SVG, `2>&1`, a tool result holding a file) was
// charged up to six bytes per byte — 6x its real size — and the clamp refused a
// healthy turn locally with "prompt is too long", the compaction-trigger
// wording, which cannot help because the markup is in the newest turn
// (2026-09-27 audit, round 35, A-F1; every escape, not only the HTML three,
// round 36, A-F5). The upstream JSON-decodes the body
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
	return len(b) - jsonEscapeOverhead(b), nil
}

// jsonEscapeOverhead counts the bytes json.Marshal spends writing a character
// as an escape sequence. Every escape it writes decodes back to fewer bytes
// than it occupies — one fewer for the two-character forms, and as many as the
// rune it stands for for a `\uXXXX`. Round 35 undid only the three HTML escapes
// (`<`, `>`, `&`), which left `"`, `\`, `\n`, `\t` — two bytes each, in every
// prompt holding a quote, a path, or a line break — and every control character
// charged to the prompt at up to six times their size (2026-09-27 audit, round
// 36, A-F5). The gateway's jsonEscapeOverhead is this function; the two legs of
// this product have to measure the same quantity.
//
// The scan follows backslashes the way a JSON reader does, so text holding the
// literal characters `\n` is not miscounted: its backslash is consumed as the
// two-character escape it is, and the `n` after it is ordinary text.
func jsonEscapeOverhead(b []byte) int {
	extra := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			continue
		}
		switch c := b[i+1]; c {
		case '\\', '"', '/', 'b', 'f', 'n', 'r', 't':
			extra++
			i++
		case 'u':
			if i+5 < len(b) && isHex4(b[i+2:i+6]) {
				extra += 6 - escapedRuneLen(b[i+2:i+6])
				i += 5
			}
		}
	}
	return extra
}

// escapedRuneLen is the UTF-8 length of what a `\uXXXX` escape decodes to. The
// escapes json.Marshal writes are for characters a JSON writer must escape —
// U+0000 to U+001F, U+2028 and U+2029 — and the last two are three bytes
// decoded: crediting every `\u` escape the one byte of a control escape
// measured a prompt made of line separators at a third of its size, so the fit
// clamp and the calibration ratio saw a prompt the upstream would have to
// refuse, or truncated the turn to fit a length it never had (2026-09-27
// audit, round 37, B-F6/A-F4). A lone surrogate decodes to U+FFFD, three bytes.
func escapedRuneLen(p []byte) int {
	var v rune
	for _, c := range p {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			v |= rune(c-'a') + 10
		default:
			v |= rune(c-'A') + 10
		}
	}
	if v >= 0xD800 && v <= 0xDFFF {
		return 3
	}
	if n := utf8.RuneLen(v); n > 0 {
		return n
	}
	return 3
}

// isHex4 reports whether a four-character `\u` payload is hex — every escape of
// that shape is a character json.Marshal escaped, whatever it is.
func isHex4(p []byte) bool {
	if len(p) != 4 {
		return false
	}
	for _, c := range p {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}
