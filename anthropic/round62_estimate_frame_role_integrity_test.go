package anthropic

// round62_estimate_frame_role_integrity_test.go — round 62's findings on the
// local leg's input estimate (F62-L1-1, F62-L1-3).
//
//   - F62-L1-1: a web_search_tool_result is converted into a role-"tool" message
//     that carries its tool_call_id beside the hits, exactly as a tool_result
//     does. The hits arm charged the hits alone, so the estimate fell 20 bytes
//     behind the wire the moment the block stated an id — while its tool_result
//     twin beside it charged the frame. Both spellings of the block must be one
//     charge, from the one frame reader.
//   - F62-L1-3: the rewrite's joined branch charged a system turn's extra as
//     `bytes - text`, and that entry's bytes carry the turn's OWN role on top of
//     the role-"tool" messages it becomes. The merged system message is charged
//     one role below, so the turn's was billed twice: the same 114-byte prompt
//     cost 16 tokens with the system turn merged into the message after a
//     non-system one and 14 with the same conversation written system-first.
//
// Both cases compare two bodies that convert to the SAME prompt, so the
// disagreement cannot be a difference in what the model was sent. Each is
// fail-first: RED against the tree before this round's fix.

import (
	"strings"
	"testing"
)

// TestAWebSearchResultsFrameIsChargedLikeItsToolResultTwin is F62-L1-1: the id a
// block states moves the charge by the key the frame writes, and the typed and
// decoded spellings of one block are one charge.
func TestAWebSearchResultsFrameIsChargedLikeItsToolResultTwin(t *testing.T) {
	noID := countContentBlock(ContentBlock{
		Type: "web_search_tool_result",
		Content: []WebSearchResult{
			{Type: "web_search_result", Title: "t", URL: "http://x"},
		},
	})
	for _, id := range []string{"c1", "c1234567890"} {
		want := len(`"tool_use_id":"` + id + `"`)
		with := countContentBlock(ContentBlock{
			Type:      "web_search_tool_result",
			ToolUseID: id,
			Content: []WebSearchResult{
				{Type: "web_search_result", Title: "t", URL: "http://x"},
			},
		})
		if got := with - noID; got != want {
			t.Errorf("F62-L1-1: a %d-byte id adds %d bytes to a web_search_tool_result's charge, want the %d bytes the frame writes: the message the block becomes carries its tool_call_id exactly as a tool_result's does (2026-09-28 audit, round 62)", len(id), got, want)
		}
		decoded := countContentItem(map[string]any{
			"type":        "web_search_tool_result",
			"tool_use_id": id,
			"content": []any{
				map[string]any{"type": "web_search_result", "title": "t", "url": "http://x"},
			},
		})
		if decoded != with {
			t.Errorf("F62-L1-1: the same web_search_tool_result charges %d typed and %d decoded: the two spellings are one wire, and both charge the frame its tool_result twin is charged (2026-09-28 audit, round 62)", with, decoded)
		}
	}
}

// TestASystemTurnsResultsCostTheMergedTurnsRoleOnce is F62-L1-3: the same prompt
// with the system turn merged into the message after a non-system one, and with
// the same conversation written system-first.
func TestASystemTurnsResultsCostTheMergedTurnsRoleOnce(t *testing.T) {
	trailing := `{"model":"m","messages":[{"role":"user","content":"hi"},` +
		`{"role":"system","content":[{"type":"text","text":"S"},{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`
	leading := `{"model":"m","messages":[` +
		`{"role":"system","content":[{"type":"text","text":"S"}]},` +
		`{"role":"user","content":"hi"},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`
	nA, pA := r60Charge(t, trailing)
	nB, pB := r60Charge(t, leading)
	if len(pA) != len(pB) {
		t.Fatalf("PREMISE: the two bodies convert to different-length prompts: %d vs %d\n%s\n%s", len(pA), len(pB), pA, pB)
	}
	if nA != nB {
		t.Errorf("F62-L1-3: the same %d-byte prompt is charged %d with the system turn merged and %d with the same conversation written system-first: the joined branch's extra counts the merged turn's own %d-byte role, which the merged system message is already charged for (2026-09-28 audit, round 62)", len(pA), nA, nB, len("system"))
	}
}

// TestAToolResultOnlySystemTurnIsChargedLikeTheSameTurnAsUser holds the shape
// round 62's third leg-1 report named — the rewrite scan reading a system turn
// whose only content becomes role-"tool" messages. It is a REGRESSION PIN, not
// a fail-first case: the scan's answer was already the charge-neutral one, and
// the 16-against-15 the report carried is F62-L1-3 seen at the joined branch
// (the same conversation, its system turn placed after a non-system one). What
// the scan must keep is that the turn's role is not charged at all — the prompt
// the converter writes carries a role-"tool" message and no system message.
func TestAToolResultOnlySystemTurnIsChargedLikeTheSameTurnAsUser(t *testing.T) {
	asSystem := `{"model":"m","messages":[{"role":"system","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`
	asUser := `{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`
	nA, pA := r60Charge(t, asSystem)
	nB, pB := r60Charge(t, asUser)
	if pA != pB {
		t.Fatalf("PREMISE: the two bodies convert to different prompts:\n%s\n%s", pA, pB)
	}
	if nA != nB {
		t.Errorf("a tool-result-only turn is charged %d in a system turn and %d in a user turn for the same prompt: the block becomes a role-%q message either way, so no system message survives to be charged (2026-09-28 audit, round 62)", nA, nB, "tool")
	}
	if strings.Contains(pA, `"role":"system"`) {
		t.Fatalf("PREMISE: the prompt carries a system message: %s", pA)
	}
}
