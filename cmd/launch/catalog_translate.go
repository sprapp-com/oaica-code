package launch

// catalog_translate.go — two id rules that are invisible in the data.
//
// models.dev lists Anthropic models with dotted versions (claude-haiku-4.5);
// the Anthropic Messages API wants the dashed native slug (claude-haiku-4-5).
// Translating is lossless because no native Anthropic slug contains a dot —
// but applying the SAME substitution to an OpenAI id corrupts it, since OpenAI
// names legitimately keep dots (gpt-5.4). Hence: the wire decides.
//
// Aggregators proxy other vendors' models behind a "vendor/model" id. The
// leading openai/ or anthropic/ segment selects the wire for that model;
// anything else is OpenAI-compatible with the id passed through unchanged.

import "strings"

// translateModelID returns the id to send upstream for a model on the given
// wire. Any id we cannot translate is passed through: never dropped, never
// guessed at.
func translateModelID(wire, modelID string) string {
	if wire != "anthropic" {
		return modelID
	}
	return strings.ReplaceAll(modelID, ".", "-")
}

// splitVendorPrefix splits a leading wire-selecting vendor segment. ok is
// false for an id with no slash and for a vendor that does not name a wire —
// meta-llama/llama-4 is an ordinary model id, not a routing instruction.
func splitVendorPrefix(modelID string) (vendor, rest string, ok bool) {
	i := strings.IndexByte(modelID, '/')
	if i <= 0 || i == len(modelID)-1 {
		return "", modelID, false
	}
	vendor, rest = modelID[:i], modelID[i+1:]
	switch vendor {
	case "openai", "anthropic":
		return vendor, rest, true
	default:
		return "", modelID, false
	}
}

// aggregatorWire returns the wire a vendor prefix selects, falling back to the
// provider's own wire when the prefix does not name one.
func aggregatorWire(vendor, defaultWire string) string {
	switch vendor {
	case "anthropic":
		return "anthropic"
	case "openai":
		return "openai"
	default:
		return defaultWire
	}
}
