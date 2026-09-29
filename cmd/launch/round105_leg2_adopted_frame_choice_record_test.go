package launch

// round105_leg2_adopted_frame_choice_record_test.go — leg 2, round 105
// (2026-09-29 audit), F105-L2-1. RECORDED, not changed.
//
// `primaryChoice` (round 103's F103-L2-1) is set inside the per-chunk choice
// loop. The whole-completion adoption branch ends in `continue` before that loop,
// so a stream whose first element is a whole `message` frame for choice 0 leaves
// `primaryChoice` unset and the NEXT delta chunk — for choice 1 — becomes the
// primary: "AB" streamed where the document states "A", and the alternative's
// tool call handed to the agent as a call to run.
//
// Recorded on the ground round 102's F102-L3-1 was: it needs an upstream that
// emulates streaming with a whole-message frame AND then streams a second choice
// as deltas, and no producer in or out of tree states that mix. The fix is one
// line (set `primaryChoice` from `chunk.Choices[0].Index` in the adoption
// branch, and where the mid-stream fold reads `doc.Choices[0]`); it is left
// unmade so that a later round can tie it to a wire that exists.

import "testing"

func TestMine105AnAdoptedFramesChoiceDoesNotFixThePrimary(t *testing.T) {
	streamed, whole := r67Texts(t, []string{
		`data: {"id":"c","object":"chat.completion","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":"A"},"finish_reason":null}]}`,
		r103Chunk(1, `{"content":"B"}`),
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, r67TwoChoiceWhole)
	if whole != "A" {
		t.Fatalf("premise: the document arm states %q, want \"A\"", whole)
	}
	if streamed != "AB" {
		t.Errorf("the fragment arm states %q — this record states \"AB\" (the alternative spliced in after an adopted frame); if it now states \"A\" the record is spent and the fix it describes has been made (2026-09-29 audit, round 105, F105-L2-1)", streamed)
	}
}
