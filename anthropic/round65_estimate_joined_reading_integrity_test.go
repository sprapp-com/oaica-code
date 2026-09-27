package anthropic

// round65_estimate_joined_reading_integrity_test.go — round 65's findings on the
// local leg's input estimate (F65-L1-1 … F65-L1-4).
//
// The estimate keeps two readings of a turn: the as-is charge for the messages
// the converter writes one per block, and the joined charge for the same turn
// after the hoist merges the system text it splits. Round 63 taught the joined
// reading to charge the blank lines the merge writes; round 64 taught both
// readings to carry the bytes a document or a search result writes beyond what
// the content chargers bill. This round compared the two readings against each
// other over randomly generated bodies — 3628 pairs of bodies that convert to
// ONE prompt — and found four places where the two readings still disagreed
// about the same prompt. Each is a pair below, and each pair fails first at
// HEAD.
//
//	the frame carried the block's own TYPE (F65-L1-1). A tool_result and a
//	web_search_tool_result convert to the SAME message — {"role":"tool",
//	"content":…,"tool_call_id":…} — and the frame wrote `"type":"tool_result",`
//	or `"type":"web_search_tool_result",` beside the id, so the two spellings of
//	one prompt were charged eleven bytes apart, and the id-less one charged
//	eighteen bytes it did not state (round 61's F61-L1-4 pinned the id's delta,
//	which was the fabricated comma in front of the id, not the id);
//
//	the joined reading was fed "the turn's text" as one string rather than the
//	RUNS the merge joins (F65-L1-2, F65-L1-3, F65-L1-4). A turn whose text a
//	result splits is [system text, tool, system text] in the prompt, and the
//	merge joins the surviving runs with a blank line; the flat reading could
//	not tell a run the merge keeps from one it drops, so
//	  - a result-split system turn written LAST in a request whose next turn is
//	    also a system turn lost the boundary between them (F65-L1-2),
//	  - a run written between two results was charged as if the merge kept it
//	    whole, without the boundary in front of it (F65-L1-3),
//	  - and a run the merge DROPS — a blank document or an all-whitespace text,
//	    which the hoist deletes from its system message — still counted, and
//	    lent its bytes to the merged string it is not part of (F65-L1-4).
//
// The estimate seeds the client-visible input_tokens whenever the upstream
// states no usage, so the session's meter read a prompt that was not the one
// sent, in both directions: Claude Code sizes auto-compaction on it
// (anthropic/anthropic.go's StreamConverter.Process).

import (
	"encoding/json"
	"testing"
)

// r65EstimateBytes is the estimate's own byte charge for a body, together with
// the prompt the converter writes for it — the premise every comparison below
// rests on. Two bodies that convert to the same prompt are the same question to
// the model, so they must be the same number to the meter.
func r65EstimateBytes(t *testing.T, body string) (int, string) {
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
	return conversationBytes(req.Messages, req.System), string(prompt)
}

// r65SamePromptCharge asserts the invariant: two spellings of ONE prompt are
// charged one amount. It reports the prompt itself when the two bodies are not
// in fact the same conversation, so a broken premise reads as a premise.
func r65SamePromptCharge(t *testing.T, split, written string) {
	t.Helper()
	nSplit, pSplit := r65EstimateBytes(t, split)
	nWritten, pWritten := r65EstimateBytes(t, written)
	if pSplit != pWritten {
		t.Fatalf("PREMISE: the two bodies convert to different prompts:\n%s\n%s", pSplit, pWritten)
	}
	if nSplit != nWritten {
		t.Errorf("one %d-byte prompt is charged %d with the first spelling and %d with the second:\n%s", len(pSplit), nSplit, nWritten, pSplit)
	}
}

// TestAResultAndASearchResultAreChargedTheSame is F65-L1-1. The two block types
// convert to one message with one content string, so the frame beside that
// content states the id and nothing else: a body that writes the search result
// and a body that writes the same text as a tool result are one prompt.
func TestAResultAndASearchResultAreChargedTheSame(t *testing.T) {
	r65SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"w1","content":"- t: http://u\n"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"web_search_tool_result","tool_use_id":"w1","content":[{"type":"web_search_result","title":"t","url":"http://u"}]}]}]}`)

	// The id-less twin: neither block states an id, so neither frame states the
	// key, and both bodies are charged the same — the search result's frame used
	// to write its own type name in place of the key.
	r65SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"tool_result","content":"- : \n"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":[{"type":"web_search_tool_result","content":[{"type":"web_search_result","title":"","url":""}]}]}]}`)
}

// TestASystemTurnAfterAResultSplitSystemTurnIsChargedTheMerge is F65-L1-2. The
// first turn's text is split by its own result — [system, tool, system] — so
// the hoist rewrites and the turn's surviving text ends up in the merged system
// message with the NEXT turn's text, joined by a blank line. The flat reading
// dropped that boundary.
func TestASystemTurnAfterAResultSplitSystemTurnIsChargedTheMerge(t *testing.T) {
	r65SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"}]},{"role":"system","content":[{"type":"text","text":"S2"}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S1\n\nS2"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)
}

// TestARunBetweenTwoResultsIsChargedTheBoundaryInFrontOfIt is F65-L1-3. The
// run sits between two results, so the merge writes a blank line in front of it
// and none after: the merged system message is the top-level system text, that
// boundary, and the run.
func TestARunBetweenTwoResultsIsChargedTheBoundaryInFrontOfIt(t *testing.T) {
	r65SamePromptCharge(t,
		`{"model":"m","system":"b","messages":[{"role":"system","content":[{"type":"tool_result","tool_use_id":"c1","content":"r1"},{"type":"text","text":"line."},{"type":"tool_result","tool_use_id":"c2","content":"r2"}]}]}`,
		`{"model":"m","system":"b\n\nline.","messages":[{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r1"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"r2"}]}]}`)
}

// TestARunTheHoistDeletesIsChargedNothing is F65-L1-4. A blank document and an
// all-whitespace text are messages the hoist DELETES from its merged system
// message — they are among the reasons it rewrites at all — so they write no
// bytes into the prompt and the charge must not move by them.
func TestARunTheHoistDeletesIsChargedNothing(t *testing.T) {
	r65SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"document","source":{"type":"text","data":"\n\n"}},{"type":"tool_result","tool_use_id":"c1","content":"r"},{"type":"text","text":"T"}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"T"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	r65SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"        "},{"type":"tool_result","tool_use_id":"c1","content":"r"},{"type":"text","text":"T"}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"T"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	// The same deleted run written LAST: the result ends the message the
	// converter writes for the turn, so the blank run writes nothing and the
	// turn's text is the run before the result.
	r65SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"T"},{"type":"tool_result","tool_use_id":"c1","content":"r"},{"type":"document","source":{"type":"text","data":"\n"}}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"T"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)
}

// The control: the readings this round did not move. A result-split system turn
// written after a user turn is charged by the merged spelling and by the split
// one alike (the case round 64's F64-L1-3 pinned), the tool result written as a
// merged turn is the same prompt as the split one, and a document whose data
// already ends in a newline writes no extra byte.
func TestTheReadingsRound64FixedAreUnchanged(t *testing.T) {
	r65SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"}]},{"role":"system","content":[{"type":"text","text":"S2"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[{"type":"text","text":"S1\n\nS2"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	r65SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"},{"type":"text","text":"S2"}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S1\n\nS2"}]},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	r65SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[{"type":"text","text":"S"},{"type":"tool_result","tool_use_id":"c1","content":"r"},{"type":"document","source":{"type":"text","data":"DDDD\n"}}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S\n\nDDDD\n"}]},{"role":"user","content":"u"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)
}
