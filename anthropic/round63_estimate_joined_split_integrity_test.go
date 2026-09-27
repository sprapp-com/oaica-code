package anthropic

// round63_estimate_joined_split_integrity_test.go — round 63's finding on the
// local leg's input estimate (F63-L1-1).
//
// A system turn whose text a tool_result splits is written by the rewrite as
// ONE system message holding both texts joined by the blank line the merge
// writes ("S1\n\nS2"), while the converter's own reading of the same turn ends
// its runs at the result: the second text opens a run of its own and takes no
// blank line in front of it. The joined branch subtracts len(e.text) — the
// merged text, blank line and all — from a charge the walk had left that blank
// line out of, so every result-split text was billed two bytes short, and the
// same conversation written with the result in a turn of its own was charged
// the full amount.
//
// The estimate seeds the client-visible input_tokens whenever the upstream
// states no usage, so the session's meter read a prompt smaller than the one
// that was sent. The case below is fail-first: RED against the tree before this
// round's fix, at the byte the two bodies disagree on rather than at the token
// the client sees (tokens quantize, and the same pair reads 15 tokens either
// way).

import (
	"encoding/json"
	"testing"
)

// r63EstimateBytes is the estimate's own byte charge for a body, together with
// the prompt the converter writes for it — the premise every comparison below
// rests on.
func r63EstimateBytes(t *testing.T, body string) (int, string) {
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

// TestAResultSplitTextIsChargedTheBlankLineTheMergeWrites is F63-L1-1. Both
// bodies convert to the SAME prompt: one system turn whose two texts a result
// splits, and the same conversation with the two texts already written as one
// block and the result in a turn of its own.
func TestAResultSplitTextIsChargedTheBlankLineTheMergeWrites(t *testing.T) {
	split := `{"model":"m","messages":[{"role":"user","content":"u"},{"role":"system","content":[` +
		`{"type":"text","text":"S1"},{"type":"tool_result","tool_use_id":"c1","content":"r"},{"type":"text","text":"S2"}]}]}`
	written := `{"model":"m","messages":[{"role":"user","content":"u"},` +
		`{"role":"system","content":[{"type":"text","text":"S1\n\nS2"}]},` +
		`{"role":"user","content":[{"type":"tool_result","tool_use_id":"c1","content":"r"}]}]}`
	nSplit, pSplit := r63EstimateBytes(t, split)
	nWritten, pWritten := r63EstimateBytes(t, written)
	if pSplit != pWritten {
		t.Fatalf("PREMISE: the two bodies convert to different prompts:\n%s\n%s", pSplit, pWritten)
	}
	if nSplit != nWritten {
		t.Errorf("the same %d-byte prompt is charged %d with the result splitting the turn's text and %d with the same conversation written with the text whole:\n%s\nthe merge joins the two texts with the blank line joinedMessageText writes, and the charge the joined branch subtracts that text from leaves the blank line out (2026-09-28 audit, round 63, F63-L1-1)", len(pSplit), nSplit, nWritten, pSplit)
	}
}
