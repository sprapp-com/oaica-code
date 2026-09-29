package main

// round106_leg3_error_object_document_record_test.go — leg 3, round 106
// (2026-09-29 audit), F106-L3-1. RECORDED, not changed.
//
// A top-level `error` object is authoritative on the framed arm — the frame path
// states the upstream's own sentence and fails the turn — and absent on the
// document arms: `openAICompletion` has no Error field, so plain and adopted
// never look at it. Error alone: all three arms answer 502, the framed arm with
// the upstream's sentence and the document arms with "returned no completion
// choices". Error beside a usable choice: plain and adopted serve a metered 200
// turn while framed refuses 502 — two verdicts for one object.
//
// Recorded on the ground round 102's F102-L3-1 was: it needs a backend that
// emulates streaming around a non-streaming engine and reports a failure ALONGSIDE
// a partial message, and no producer in or out of tree states it. The fix (an
// Error field on openAICompletion read by bufferedCompletion, LedgerStatus and
// finalize, stated through upstreamErrorSentence) is left unmade until a wire
// needs it. The pin states both readings and goes red if either moves.

import (
	"strings"
	"testing"
)

func TestMine106AnErrorObjectIsAuthoritativeOnlyOnTheFramedArm(t *testing.T) {
	errObj := `"error":{"message":"engine died","type":"server_error"}`
	choice := `"choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]`
	frame := func(body string) []string { return []string{`data: {` + body + `}`, `data: [DONE]`} }

	// Error beside a usable choice.
	_, bodies := r100RunArms(t, `{"id":"x","model":"m",`+errObj+`,`+choice+`}`, frame(errObj+`,`+choice))
	for i, arm := range [2]string{"plain", "adopted"} {
		if strings.Contains(bodies[i], `"error"`) || !strings.Contains(bodies[i], "hi") {
			t.Errorf("the %s arm answered %q, this record states a served turn — if the document arms now refuse it, the fix is made and this record is spent (2026-09-29 audit, round 106, F106-L3-1)", arm, strings.TrimSpace(bodies[i]))
		}
	}
	if !strings.Contains(bodies[2], "engine died") {
		t.Errorf("the framed arm answered %q, this record states the upstream's sentence (2026-09-29 audit, round 106, F106-L3-1)", strings.TrimSpace(bodies[2]))
	}

	// Error alone: the sentence is the framed arm's only.
	_, bodies = r100RunArms(t, `{"id":"x","model":"m",`+errObj+`}`, frame(errObj))
	for i, arm := range [2]string{"plain", "adopted"} {
		if !strings.Contains(bodies[i], "no completion choices") {
			t.Errorf("the %s arm answered %q, this record states the generic cause (2026-09-29 audit, round 106, F106-L3-1)", arm, strings.TrimSpace(bodies[i]))
		}
	}
	if !strings.Contains(bodies[2], "engine died") {
		t.Errorf("the framed arm answered %q, this record states the upstream's sentence (2026-09-29 audit, round 106, F106-L3-1)", strings.TrimSpace(bodies[2]))
	}
}
