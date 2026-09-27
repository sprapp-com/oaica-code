package anthropic

// round64_estimate_run_boundary_integrity_test.go — round 64's findings on the
// local leg's input estimate (F64-L1-1, F64-L1-2, F64-L1-3).
//
// The estimate has to charge a system turn the way the converter WRITES it. For
// a turn a tool result splits, that is [system text, tool, system text], which
// the hoist then merges back into one system message: the merged string is the
// surviving RUNS joined by the blank line the merge writes. Round 63 taught the
// joined branch to add those blank lines (F63-L1-1), and did it with a reading
// of the same walk that was wrong in three ways, all of them the same shape —
// the pair below converts to ONE prompt and is charged two amounts.
//
//	the extra a document or search_result writes beyond what the content
//	chargers bill (the newline its data lacks, the title and reference it
//	states) was in the per-run reading and not in the joined one, so the term
//	between them — which exists to measure the blank lines, not those bytes —
//	subtracted them, and every attachment in the turn shortened the charge
//	(F64-L1-1);
//
//	a run the merge DROPS still lent its bytes to the block after it: the walk
//	carried "the joined text holds something" across the result, so the empty
//	text that follows one was charged the blank line of a boundary the merge
//	never writes (F64-L1-2);
//
//	the branch itself was decided over the REQUEST's turns rather than the
//	shape the converter writes, so a result-split system turn written FIRST —
//	which the converter turns into [system, tool, system] — took the as-is
//	reading while its own converter rewrote it (F64-L1-3).
//
// The estimate seeds the client-visible input_tokens whenever the upstream
// states no usage, so the session's meter read a prompt that was not the one
// sent, in both directions: Claude Code sizes auto-compaction on it
// (anthropic/anthropic.go's StreamConverter.Process).

import (
	"encoding/json"
	"testing"
)

// r64EstimateBytes is the estimate's own byte charge for a body, together with
// the prompt the converter writes for it — the premise every comparison below
// rests on. Two bodies that convert to the same prompt are the same question to
// the model, so they must be the same number to the meter.
func r64EstimateBytes(t *testing.T, body string) (int, string) {
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

// r64SamePromptCharge asserts the invariant: two spellings of ONE prompt are
// charged one amount. It reports the prompt itself when the two bodies are not
// in fact the same conversation, so a broken premise reads as a premise.
func r64SamePromptCharge(t *testing.T, split, written string) {
	t.Helper()
	nSplit, pSplit := r64EstimateBytes(t, split)
	nWritten, pWritten := r64EstimateBytes(t, written)
	if pSplit != pWritten {
		t.Fatalf("PREMISE: the two bodies convert to different prompts:\n%s\n%s", pSplit, pWritten)
	}
	if nSplit != nWritten {
		t.Errorf("one %d-byte prompt is charged %d with the result splitting the turn's text and %d with the same conversation written with the text whole:\n%s", len(pSplit), nSplit, nWritten, pSplit)
	}
}

// TestAWriteOnlyExtraIsNotSubtractedFromTheMergeCount is F64-L1-1. The document
// and the search result write bytes the content chargers do not bill them, and
// the joined reading has to carry those bytes as well as the per-run one — the
// term between the two readings measures the BLANK LINES the merge writes, so
// anything else that differs between them is noise with a sign.
func TestAWriteOnlyExtraIsNotSubtractedFromTheMergeCount(t *testing.T) {
	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[`+
			`{"type":"text","text":"S"},{"type":"tool_result","tool_use_id":"c1","content":"r"},`+
			`{"type":"search_result","title":"TT","source":{"type":"text","ref":"RR"},"content":[{"type":"text","text":"P1"},{"type":"text","text":"P2"}]}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S\n\nTT\nRR\nP1\nP2\n"}]},`+
			`{"role":"user","content":"u"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[`+
			`{"type":"text","text":"S"},{"type":"tool_result","tool_use_id":"c1","content":"r"},`+
			`{"type":"document","source":{"type":"text","data":"DDDD"}}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S\n\nDDDD\n"}]},`+
			`{"role":"user","content":"u"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)
}

// TestARunTheMergeDropsLendsNoBlankLine is F64-L1-2. The empty text after a
// result states nothing, so the converter drops the run whole and the merge
// writes no boundary — while the walk, carrying "the joined text holds
// something" across the result, charged the block two bytes of a blank line the
// prompt does not contain.
func TestARunTheMergeDropsLendsNoBlankLine(t *testing.T) {
	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[`+
			`{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"},`+
			`{"type":"text","text":""}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"u"},`+
			`{"role":"system","content":[{"type":"text","text":"S1"}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	// Two of them, and one that says SOMETHING after a dropped one: the count is
	// per dropped boundary, not per turn.
	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[`+
			`{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"},`+
			`{"type":"text","text":""},{"type":"text","text":""}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"u"},`+
			`{"role":"system","content":[{"type":"text","text":"S1"}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[`+
			`{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"},`+
			`{"type":"text","text":""},{"type":"text","text":"S3"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"u"},`+
			`{"role":"system","content":[{"type":"text","text":"S1\n\nS3"}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)
}

// TestAResultSplitSystemTurnWrittenFirstIsChargedTheRewrite is F64-L1-3. The
// hoist decides whether to rewrite over the CONVERTED messages, and one system
// turn holding [text, result, text] converts to [system, tool, system] — a
// system message after one of another role — so the rewrite moves it whatever
// the request's own turn order says. The estimate asked the request instead, and
// charged that spelling the as-is reading while charging the same prompt written
// merged the joined one.
func TestAResultSplitSystemTurnWrittenFirstIsChargedTheRewrite(t *testing.T) {
	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"system","content":[`+
			`{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"},`+
			`{"type":"text","text":"S2"}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S1\n\nS2"}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	// The same turn written with its result LAST converts to [system, tool] —
	// ordered — and must keep the as-is reading: the clause above is about the
	// run the converter writes AFTER a result, not about results in general.
	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"system","content":[`+
			`{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S1"}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)
}

// The control: the case round 63 closed (a result splitting a system turn's text
// after a non-system turn) and the two other readings the walk feeds — a
// document whose data already ends in a newline, and three texts split by two
// results — all of which agreed before this round and must keep agreeing.
func TestTheCasesRound63ClosedAreUnchanged(t *testing.T) {
	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[`+
			`{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"},`+
			`{"type":"text","text":"S2"}]}]}`,
		`{"model":"m","messages":[{"role":"user","content":"u"},`+
			`{"role":"system","content":[{"type":"text","text":"S1\n\nS2"}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[`+
			`{"type":"text","text":"S"},{"type":"tool_result","tool_use_id":"c1","content":"r"},`+
			`{"type":"document","source":{"type":"text","data":"DDDD\n"}}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S\n\nDDDD\n"}]},`+
			`{"role":"user","content":"u"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`)

	r64SamePromptCharge(t,
		`{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[`+
			`{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"},`+
			`{"type":"text","text":"S2"},{"type":"tool_result","tool_use_id":"c2","content":"r2"},`+
			`{"type":"text","text":"S3"}]}]}`,
		`{"model":"m","messages":[{"role":"system","content":[{"type":"text","text":"S1\n\nS2\n\nS3"}]},`+
			`{"role":"user","content":"u"},{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]},`+
			`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c2","content":"r2"}]}]}`)
}
