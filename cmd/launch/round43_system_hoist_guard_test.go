package launch

// round43_system_hoist_guard_test.go — round 43's finding on the client leg,
// A43-1, the twin of the local leg's.
//
// normalizeSystemFirst asks systemMessageIsTextOnly before it will merge a
// system message into the single leading string, and that guard used to be
// reachable only for the system messages the scan walked before it stopped: a
// first late system message that carried text alone ended the scan, and a
// SECOND one carrying an image was then merged away by the rewrite — the image
// dropped, and the estimate still charging it its 4096 bytes. The guard now
// ranges over the whole conversation.

import "testing"

// TestACarrierBehindATextOnlyLateSystemMessageIsNotMerged is A43-1 on this leg.
func TestACarrierBehindATextOnlyLateSystemMessageIsNotMerged(t *testing.T) {
	in := []openAIMessage{
		{Role: "user", Content: "hi"},
		{Role: "system", Content: "late note"},
		{Role: "system", Content: "and the picture", Images: []openAIImageBlock{{DataURL: "data:image/png;base64,QUJD"}}},
	}

	got := normalizeSystemFirst(in)

	if len(got) != 3 {
		t.Fatalf("normalizeSystemFirst() = %+v, want the three messages as the client sent them: the third carries an image, and merging it into the leading system string drops what it carries", got)
	}
	if got[0].Role != "user" || got[0].Content != "hi" {
		t.Errorf("message 0 = %+v, want the user turn untouched", got[0])
	}
	for i := 1; i < 3; i++ {
		if got[i].Role != in[i].Role || got[i].Content != in[i].Content || len(got[i].Images) != len(in[i].Images) || len(got[i].ToolCalls) != len(in[i].ToolCalls) {
			t.Errorf("message %d = %+v, want %+v: a conversation holding a system message that carries more than text is returned exactly as it arrived, whichever position its text-only sibling sits in", i, got[i], in[i])
		}
	}
}

// TestACarrierAloneIsStillNotMerged is the control the round-42 suite already
// pinned; it is repeated here so this file fails for its own reason if the
// whole-slice guard is ever narrowed back to a prefix.
func TestACarrierAloneIsStillNotMerged(t *testing.T) {
	in := []openAIMessage{
		{Role: "user", Content: "go"},
		{Role: "system", Content: "late", Images: []openAIImageBlock{{DataURL: "data:image/png;base64,QUJD"}}},
	}
	if got := normalizeSystemFirst(in); len(got) != 2 {
		t.Errorf("normalizeSystemFirst() = %+v, want the two messages untouched", got)
	}
}
