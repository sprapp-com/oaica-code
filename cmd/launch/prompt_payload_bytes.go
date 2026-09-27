package launch

// The prompt-size unit the client proxy's context-fit clamp and its
// calibration both use. tools/gateway measures the same thing server-side
// (messagesBytes / promptPayloadBytes): the serialized prompt with any inline
// image's transport encoding replaced by a fixed per-image allowance.
//
// Kept as one function so the clamp, the two record call sites and the
// estInputTokens fallback cannot drift from each other -- a calibrated ratio
// is only meaningful if every site that produces or consumes one counts the
// same bytes.

import "encoding/json"

// clientPromptBytes is what the client proxy charges a request body to the
// prompt-size estimate: the serialized body, with any inline image's base64
// payload replaced by inlineImageByteAllowance.
//
// Charging the raw length billed a screenshot as its own transport encoding —
// a 1 MB PNG is ~1.4 MB of base64, ~350k "tokens", against a leg that accepts
// images and publishes 262144. The clamp then refused the turn locally with
// Anthropic's exact "prompt is too long" wording, which is the string Claude
// Code pattern-matches to its compaction recovery path, and every later turn
// of that session failed the same way. Calibration could not rescue it either:
// estimate() scales by the same inflated byte count, and an image body's real
// ratio (~0.0002 tok/byte) is below calibMinRatio, so the ground truth was
// discarded as bogus (2026-09-27 audit, round 33, A-F1 — the round-32 gateway
// fix, on the other clamp).
func clientPromptBytes(body []byte) int {
	payload, images := decodedPromptBytes(body)
	return len(body) - payload + images*inlineImageByteAllowance
}

// inlineImageByteAllowance matches tools/gateway's imagePartByteAllowance: an
// inline image is charged as an image, not as its base64.
const inlineImageByteAllowance = 4096

// inlineImagePayloadBytes reports the total inline base64 payload length found
// in a decoded request body and how many images it holds. Same walk as the
// gateway's: an OpenAI part (`{"type":"image_url","image_url":{"url":"data:…"}}`,
// which the proxy writes for Claude Code's pasted and Read-tool screenshots)
// and an Anthropic block (`{"type":"image","source":{"data":…}}`) both count;
// a remote URL carries no base64 and is left alone.
func inlineImagePayloadBytes(v any) (payload, images int) {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			p, n := inlineImagePayloadBytes(e)
			payload += p
			images += n
		}
	case map[string]any:
		for k, e := range t {
			if k == "image_url" {
				if m, ok := e.(map[string]any); ok {
					if u, ok := m["url"].(string); ok && len(u) > 5 && u[:5] == "data:" {
						payload += len(u)
						images++
						continue
					}
				}
			}
			if k == "source" {
				if m, ok := e.(map[string]any); ok {
					if d, ok := m["data"].(string); ok && d != "" {
						payload += len(d)
						images++
						continue
					}
				}
			}
			p, n := inlineImagePayloadBytes(e)
			payload += p
			images += n
		}
	}
	return payload, images
}

// decodedPromptBytes parses a request body and returns the payload and image
// count of the images it carries. A body that does not parse carries no image
// this estimate can find, which is 0/0 -- the caller falls back to the raw
// length.
func decodedPromptBytes(body []byte) (payload, images int) {
	var doc any
	if err := json.Unmarshal(body, &doc); err != nil {
		return 0, 0
	}
	return inlineImagePayloadBytes(doc)
}
