package anthropic

// round61_estimate_system_merge_integrity_test.go — round 61's findings on the
// local leg's input estimate (F61-L1-1 .. F61-L1-4).
//
// The estimate's system arm asks two questions about every system message: does
// the converter write anything but text for it (in which case the hoist leaves
// the conversation as it arrived), and what does the turn charge. Both were
// answered by reading the client's own block TYPES rather than what
// convertMessage writes, and the ordinary block spellings answered both wrong.
//
//   - F61-L1-1: `{"type":"text"}` with no `text` key writes no run (the
//     converter's text arm writes `block.Text != nil`), so the converted system
//     message is blank and the hoist DELETES it — exactly as a `"text":""` block
//     leaves it. Read as "not text", it refused the merge, the as-is branch kept
//     and billed the blank message beside it, and the same converted 32-byte
//     prompt cost 25004 tokens against 1.
//   - F61-L1-2: a tool_result in a system turn becomes a role-"tool" message of
//     its own — never part of the merged system message — so it cannot be why
//     the merge is refused. Read as "not text" it refused the merge, and the
//     same prompt cost 25014 against 13. Charging the joined branch's total also
//     dropped that role-"tool" message from the bill entirely, so the fix
//     charges it: the turn's text is what the merge carries, and the results are
//     what the hoist keeps beside it.
//   - F61-L1-3: the two spellings of one block disagreed. A decoded
//     web_search_tool_result was charged its whole JSON (encrypted_content
//     included), and a decoded search_result charged its source's `data`, where
//     the typed arms charge the converter's own hits and reference.
//   - F61-L1-4: the typed tool_result frame stated `"tool_use_id":""` for a body
//     that never wrote one; the message the block becomes writes its
//     tool_call_id with omitempty.

import (
	"strings"
	"testing"
)

// TestANilTextSystemBlockIsChargedLikeAnEmptyOne is F61-L1-1. The two blocks
// convert to the same prompt — the hoist deletes the blank message either way —
// so the estimate must answer them alike.
func TestANilTextSystemBlockIsChargedLikeAnEmptyOne(t *testing.T) {
	spaces := strings.Repeat(" ", 100000)
	nilText := `{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"text"}]},` +
		`{"role":"system","content":[{"type":"text","text":"` + spaces + `"}]}]}`
	empty := `{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"text","text":""}]},` +
		`{"role":"system","content":[{"type":"text","text":"` + spaces + `"}]}]}`
	nA, pA := r60Charge(t, nilText)
	nB, pB := r60Charge(t, empty)
	if pA != pB {
		t.Fatalf("PREMISE: the two bodies convert differently: %s vs %s", pA, pB)
	}
	if nA != nB {
		t.Errorf("F61-L1-1: the same prompt (%d bytes) is charged %d with a text block that has no text key and %d with an empty one: a block the converter writes no run for is not why a system message cannot be merged, and normalizeSystemFirst deletes the blank one either way (2026-09-28 audit, round 61)", len(pA), nA, nB)
	}
}

// TestASystemToolResultIsChargedLikeTheSameTurnWithoutABlankSystem is F61-L1-2:
// the blank system message beside a tool result is deleted by the hoist, so the
// two bodies reach the model as the same prompt and must be charged alike.
func TestASystemToolResultIsChargedLikeTheSameTurnWithoutABlankSystem(t *testing.T) {
	spaces := strings.Repeat(" ", 100000)
	withTrailing := `{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]},` +
		`{"role":"system","content":[{"type":"text","text":"` + spaces + `"}]}]}`
	alone := `{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`
	nA, pA := r60Charge(t, withTrailing)
	nB, pB := r60Charge(t, alone)
	if pA != pB {
		t.Fatalf("PREMISE: the two bodies convert differently: %s vs %s", pA, pB)
	}
	if nA != nB {
		t.Errorf("F61-L1-2: the same prompt is charged %d with a blank system message beside the result and %d without one: the blank message is deleted by the hoist either way, and the tool message the result becomes is what the charge is for (2026-09-28 audit, round 61)", nA, nB)
	}
}

// TestAToolResultOnlyTurnIsChargedForItsMessage is F61-L1-2's other half: the
// turn's result becomes a role-"tool" message the prompt DOES carry, so the
// joined branch must charge it — it used to charge the turn as the empty
// fallback.
func TestAToolResultOnlyTurnIsChargedForItsMessage(t *testing.T) {
	n, p := r60Charge(t, `{"model":"m","messages":[{"role":"system","content":[{"type":"tool_result","tool_use_id":"c1","content":"out"}]}]}`)
	if !strings.Contains(p, "out") {
		t.Fatalf("PREMISE: the converted prompt does not carry the result: %s", p)
	}
	if n <= len("user") {
		t.Errorf("F61-L1-2: the prompt the client sends is %q but the estimate charges %d, the charge for a conversation that converts to nothing (2026-09-28 audit, round 61)", p, n)
	}
}

// TestAnIdlessToolResultFrameStatesNoId is F61-L1-4. The frame is what the
// message writes beside its content, and an id-less result writes no
// tool_call_id key at all — so the charge must move exactly by the key an id
// adds, and not by bytes no body wrote.
func TestAnIdlessToolResultFrameStatesNoId(t *testing.T) {
	if _, stated := toolResultFrame("tool_result", "")["tool_use_id"]; stated {
		t.Errorf("F61-L1-4: the frame states a tool_use_id for a body that wrote none: the message the block becomes writes its tool_call_id with omitempty, so the key is not in the prompt (2026-09-28 audit, round 61)")
	}
	noID := toolResultBytes(toolResultFrame("tool_result", ""), "out")
	for _, id := range []string{"c1", "c1234567890"} {
		want := len(`"tool_use_id":"` + id + `",`)
		if got := toolResultBytes(toolResultFrame("tool_result", id), "out") - noID; got != want {
			t.Errorf("F61-L1-4: a %d-byte id adds %d bytes to a tool_result's charge, want the %d bytes the key writes (2026-09-28 audit, round 61)", len(id), got, want)
		}
	}
}

// TestBothSpellingsOfOneBlockChargeTheSame is F61-L1-3: the typed block and the
// decoded map are two spellings of one wire, and every reader here treats them
// as one — which means charging what the converter WRITES for the block, not
// the JSON the client sent.
func TestBothSpellingsOfOneBlockChargeTheSame(t *testing.T) {
	blob := strings.Repeat("A", 40000)

	t.Run("a tool result with a stray key", func(t *testing.T) {
		typed := countContentBlock(ContentBlock{
			Type:      "tool_result",
			ToolUseID: "",
			Content:   "out",
		})
		decoded := countContentItem(map[string]any{
			"type":    "tool_result",
			"content": "out",
			"stray":   blob,
		})
		if typed != decoded {
			t.Errorf("the same tool_result charges %d typed and %d decoded: convertMessage reads the block's type, id and content and writes nothing else, so a key beside them is not prompt bytes (2026-09-28 audit, round 61, F61-L1-3)", typed, decoded)
		}
	})

	t.Run("a web search result's hits", func(t *testing.T) {
		typed := countContentBlock(ContentBlock{
			Type: "web_search_tool_result",
			Content: []WebSearchResult{
				{Type: "web_search_result", Title: "t", URL: "http://x"},
			},
		})
		decoded := countContentItem(map[string]any{
			"type":              "web_search_tool_result",
			"encrypted_content": blob,
			"content": []any{
				map[string]any{"type": "web_search_result", "title": "t", "url": "http://x"},
			},
		})
		if typed != decoded {
			t.Errorf("the same web_search_tool_result charges %d typed and %d decoded: the converter writes one line per hit, and a serialized block pastes its encrypted_content into the bill (2026-09-28 audit, round 61, F61-L1-3)", typed, decoded)
		}
	})

	t.Run("a search result's source", func(t *testing.T) {
		typed := countContentBlock(ContentBlock{
			Type:  "search_result",
			Title: "t",
			Source: &ImageSource{
				Type: "url",
				Ref:  "http://x",
			},
		})
		decoded := countContentItem(map[string]any{
			"type":  "search_result",
			"title": "t",
			"source": map[string]any{
				"type": "url",
				"ref":  "http://x",
				"data": blob,
			},
		})
		if typed != decoded {
			t.Errorf("the same search_result charges %d typed and %d decoded: the converter writes the source's reference, not a payload beside it (2026-09-28 audit, round 61, F61-L1-3)", typed, decoded)
		}
	})
}
