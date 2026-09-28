package main

// round98_leg3_parts_and_delta_frame_records_test.go — leg 3, round 98
// (2026-09-29 audit), RECORDS. Neither finding is fixed: each states what every
// arm of this bridge already answers, so a later round meets the reading as a
// decision rather than as a discovery.
//
// F98-L3-3. An upstream that states `message.content` as an ARRAY OF PARTS —
// OpenAI's own newer spelling of a text turn — is refused by every arm of this
// bridge, with one sentence: 502 `unparseable upstream response`. The document
// arms refuse it because this bridge's decoder holds `content` as a string; the
// framed arm refuses it because the frame that carries such a message is one the
// bridge cannot read, and round 96's F96-L2-1 taught the tail to state the
// document reader's cause rather than fabricate an empty completion. Measured
// here on all three arms; the same body on the other leg (leg 2) is refused 502
// on every arm too, naming the decode detail where the frame states a `message`
// and an emptiness sentence where it states only a `delta` — and there its
// document spelling of that second body answers the same emptiness sentence, so
// neither leg has a spelling that serves what another refuses. No producer of
// the shape exists in this tree: the catalogue's providers are read for the
// shapes they are known to state, and this bridge's own request path never
// writes content as parts (it writes a string). RECORDED rather than fixed, and
// pinned so the day a producer appears the refusal is a stated decision.
//
// F98-L3-4. The id a streamed turn is announced under: a frame carrying a whole
// completion states the upstream's id and it is adopted (round 86's F86-L3-2,
// `messages.go` `startSent`); a frame that only carries a `delta` has no
// document to take an id from and the turn keeps the minted id. Round 98's
// auditor read the second half as a divergence between the framed spelling and
// the document spelling of the same bytes. It is not: the minted id is the
// decision round 86 pinned, and the document spelling of a body whose choice
// carries `delta` states no turn at all (this bridge's document decoder knows no
// `delta` field) and is refused as an empty completion — the doc-vs-frame split
// rounds 82 and 97 record, not an id this leg invented. Both readings are stated
// below as they are.

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestAContentPartsBodyIsRefusedWithOneCauseOnEveryArm is F98-L3-3's record: the
// array-of-parts spelling is refused by all three arms of this bridge, with the
// one sentence the bridge states for a body it cannot decode.
func TestAContentPartsBodyIsRefusedWithOneCauseOnEveryArm(t *testing.T) {
	doc := `{"id":"chatcmpl-r98","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
	frame := `data: {"id":"c","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hi"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}` + "\n\n"

	const want = "unparseable upstream response"
	for _, arm := range []struct {
		name, ct, ask string
		frames        []string
	}{
		{name: "plain", ct: "application/json", ask: round44AskPlain},
		{name: "adopted", ct: "text/event-stream", ask: round44AskStream},
		{name: "framed", ct: "text/event-stream", ask: round44AskStream, frames: []string{frame, "data: [DONE]\n\n"}},
	} {
		var up *httptest.Server
		if arm.frames != nil {
			up = round45Frames(t, arm.frames...)
		} else {
			up = round45Upstream(t, arm.ct, doc)
		}
		srv, _ := round39Gateway(t, up, nil)
		status, body := round45Ask(t, srv, arm.ask)
		if status != http.StatusBadGateway {
			t.Errorf("the %s arm answered a content-as-parts body %d, want the 502 every arm of this bridge states for a body it cannot decode (2026-09-29 audit, round 98, F98-L3-3 — recorded, no producer in tree):\n%s",
				arm.name, status, body)
			continue
		}
		if !strings.Contains(body, want) {
			t.Errorf("the %s arm named %q for a content-as-parts body, where the other arms name the bridge's one sentence %q — one body states one cause (2026-09-29 audit, round 98, F98-L3-3):\n%s",
				arm.name, body, want, body)
		}
	}
}

// TestATurnOpenedByADeltaFrameKeepsTheMintedID is F98-L3-4's record: the id of a
// turn a delta frame opened is this bridge's own, and the document spelling of
// those same bytes states no turn at all.
func TestATurnOpenedByADeltaFrameKeepsTheMintedID(t *testing.T) {
	body := `{"id":"chatcmpl-abc","model":"kat-awq","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`

	for _, arm := range []struct{ name, ct, ask string }{
		{"plain", "application/json", round44AskPlain},
		{"adopted", "text/event-stream", round44AskStream},
	} {
		srv, _ := round39Gateway(t, round45Upstream(t, arm.ct, body), nil)
		status, resp := round45Ask(t, srv, arm.ask)
		if status != http.StatusBadGateway {
			t.Errorf("the %s arm of a body whose choice carries `delta` answered %d — this bridge's document decoder knows no `delta` field, so that body states no turn and is refused as an empty completion (round 90's F90-L3-2; 2026-09-29 audit, round 98, F98-L3-4, recorded):\n%s",
				arm.name, status, resp)
		}
	}

	srv, _ := round39Gateway(t, round45Frames(t,
		"data: "+body+"\n\n", "data: [DONE]\n\n"), nil)
	status, resp := round45Ask(t, srv, round44AskStream)
	if status != http.StatusOK {
		t.Fatalf("the framed arm of that body answered %d — it is the spelling that states the turn:\n%s", status, resp)
	}
	id := r86Leg3StartID(t, resp)
	if !strings.HasPrefix(id, "msg_") || strings.Contains(id, "abc") {
		t.Errorf("the framed arm announced the turn as %q — a frame that only carries a `delta` states no id to adopt, so the turn keeps the minted id (round 86's F86-L3-2; 2026-09-29 audit, round 98, F98-L3-4, recorded rather than changed):\n%s",
			id, resp)
	}
	if !strings.Contains(resp, `"text":"hi"`) {
		t.Errorf("the framed arm served no prose for the turn it opened: %s", resp)
	}
}
