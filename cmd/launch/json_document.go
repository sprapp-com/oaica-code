package launch

// json_document.go — reading a JSON OBJECT document that the user (or another
// program) also writes, without a nil map ever reaching a writer.
//
// `null` is a valid JSON document, and decoding it into a map yields a NIL map:
// reads on it are fine, and every WRITE to it panics. The file is one a
// hand-edit, an editor, or an interrupted writer can leave behind, and it holds
// no members to preserve — so it is read as the empty object it declares.
//
// Seven readers in this package decoded a document into a map they then wrote
// and every one of them panicked on it (2026-09-27 audit, round 19): pi's
// config, cline's providers, droid's settings, opencode's picker state, qwen's
// settings, OpenClaw's config, and opencode's auth.json. The rule lives here so
// the next reader inherits it rather than rediscovering it.

import (
	"bytes"
	"encoding/json"
)

// decodeJSONObject decodes data as a JSON object, preserving numbers as
// json.Number (every caller writes the document back whole, so a number this
// package cannot represent must survive the round trip), and never returns a
// nil map.
func decodeJSONObject(data []byte) (map[string]any, error) {
	// An EMPTY document is not a corrupt one: `Decode` reports io.EOF for a
	// zero-byte (or whitespace-only) store, which read as "not valid JSON" and
	// failed the launch, while storeDocumentMergeValue and every other writer
	// here treat emptiness as "nothing to preserve". No oaica writer produces
	// one (writes are atomic), so the trigger is another tool or an interrupted
	// editor (2026-09-27 audit, round 22).
	if len(bytes.TrimSpace(data)) == 0 {
		return map[string]any{}, nil
	}
	doc := map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, err
	}
	if doc == nil {
		doc = map[string]any{}
	}
	return doc, nil
}
