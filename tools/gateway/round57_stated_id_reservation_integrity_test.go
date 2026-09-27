package main

// round57_stated_id_reservation_integrity_test.go — round 57's finding on the
// id a whole list states (F57-L3-1).
//
// A minted id is a pure function of the call's own name and arguments
// (gatewayToolCallIDFor), so an id-less call numbered early in a list can be
// numbered with the very string a LATER call of that list states. Round 48
// settled which side gives way: the stated id is the upstream's own name for
// its call and is kept, and the mint is renumbered (reservedIDs). finalize does
// that for the non-stream list, and so do both sibling translators of a whole
// list — anthropic.StreamConverter.Process (a reservation pass before its
// block loop) and the client leg's parseOpenAIToolCalls (usedIDs filled from
// the list before any mint). Two of this leg's own translators were missing
// that pass: adoptWholeStream, which holds the same whole completion finalize
// is handed, and relayDelta, which holds one fragment's whole call list. Both
// numbered the id-less call with the stated id and renumbered the call that
// STATED one, so one upstream answer reached the client under two different ids
// depending on the arm that relayed it — the arm was visible in the ids.
//
// The one shape no translator can fix is the fragment that states its id AFTER
// an earlier call's fragment was already relayed and numbered: a frame arm sees
// fragments in arrival order, and an id already written into a
// content_block_start cannot be recalled. That residual divergence is pinned
// below so a later round reads the reason rather than rediscovering it; the
// harm it can do is bounded, since the two calls still carry DISTINCT ids and
// the client answers with the ids it was handed.

import (
	"strings"
	"testing"
)

// r57StatedIDDoc is one completion with two calls of the same identity: the
// first id-less, the second stating exactly the id this bridge mints for that
// identity.
func r57StatedIDDoc(minted string) string {
	return `{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":null,` +
		`"tool_calls":[{"type":"function","function":{"name":"a","arguments":"{}"}},` +
		`{"id":"` + minted + `","type":"function","function":{"name":"a","arguments":"{}"}}]},` +
		`"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":2,"total_tokens":13}}`
}

// r57StatedIDFrames is the same two calls delivered as SSE frames, each call in
// its own fragment.
func r57StatedIDFrames(minted string) []string {
	return []string{
		`data: {"choices":[{"delta":{"tool_calls":[{"function":{"name":"a","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{"tool_calls":[{"id":"` + minted + `","function":{"name":"a","arguments":"{}"}}]}}]}`,
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}
}

func TestAWholeDocumentDoesNotNumberACallWithTheIDItStates(t *testing.T) {
	minted := gatewayToolCallIDFor("a", "{}")

	// The reference: the non-stream arm, which has reserved the stated id since
	// round 48 — the first call is renumbered, the second keeps what it stated.
	up := round45Upstream(t, "application/json", r57StatedIDDoc(minted))
	srv, _ := round39Gateway(t, up, nil)
	_, plain := round45Ask(t, srv, round45AskPlain)
	want := round46BlockIDs(t, plain)
	if len(want) != 2 || want[0] == minted || want[1] != minted {
		t.Fatalf("non-stream arm = %v, want the id-less call renumbered and the stating call at %q — this test's reference moved", want, minted)
	}

	t.Run("adopted document", func(t *testing.T) {
		// A stream request the upstream answered with one whole document.
		up := round45Upstream(t, "application/json", r57StatedIDDoc(minted))
		srv, _ := round39Gateway(t, up, nil)
		_, body := round45Ask(t, srv, round45AskStream)
		got := round46ToolUseIDs(t, body)
		if len(got) != 2 {
			t.Fatalf("the adopted turn opened %d tool_use block(s), want 2:\n%s", len(got), body)
		}
		if got[1] != minted {
			t.Errorf("the adopted document numbered the calls %v against the non-stream arm's %v: the call that STATES %q was renumbered and the id-less call minted it — this path holds the same whole completion finalize is handed, and both sibling legs reserve the stated ids before numbering anything (2026-09-28 audit, round 57, F57-L3-1)", got, want, minted)
		}
	})

	t.Run("one fragment carrying both calls", func(t *testing.T) {
		up := round45Frames(t,
			`data: {"choices":[{"delta":{"tool_calls":[`+
				`{"function":{"name":"a","arguments":"{}"}},`+
				`{"id":"`+minted+`","function":{"name":"a","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		)
		srv, _ := round39Gateway(t, up, nil)
		_, body := round45Ask(t, srv, round45AskStream)
		got := round46ToolUseIDs(t, body)
		if len(got) != 2 {
			t.Fatalf("the turn opened %d tool_use block(s), want 2:\n%s", len(got), body)
		}
		if got[1] != minted {
			t.Errorf("one fragment naming both calls numbered them %v against the non-stream arm's %v: a fragment's whole call list is in hand before any of it is numbered, and the local leg's converter reserves exactly that list before its loop (2026-09-28 audit, round 57, F57-L3-1)", got, want)
		}
	})

	t.Run("both calls indexed in one fragment", func(t *testing.T) {
		up := round45Frames(t,
			`data: {"choices":[{"delta":{"tool_calls":[`+
				`{"index":0,"function":{"name":"a","arguments":"{}"}},`+
				`{"index":1,"id":"`+minted+`","function":{"name":"a","arguments":"{}"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
			`data: [DONE]`,
		)
		srv, _ := round39Gateway(t, up, nil)
		_, body := round45Ask(t, srv, round45AskStream)
		got := round46ToolUseIDs(t, body)
		if len(got) != 2 {
			t.Fatalf("the turn opened %d tool_use block(s), want 2:\n%s", len(got), body)
		}
		if got[1] != minted {
			t.Errorf("the indexed pair was numbered %v against the non-stream arm's %v: an index names the slot, not the id's owner (2026-09-28 audit, round 57, F57-L3-1)", got, want)
		}
	})
}

// TestAStatedIDArrivingAfterTheMintIsRenumberedOnTheFrameArm pins the residual
// divergence, deliberately: a fragment arm relays fragments as they arrive, so
// an id stated in a LATER fragment reaches the client after the earlier id-less
// call has already been numbered and its content_block_start written with that
// id. Nothing can recall it — the only cures are worse than the divergence
// (holding every mint-numbered block until the turn ends, which reorders the
// turn's blocks and delays every tool call's start; or numbering the stated
// call with the id of the call already on the wire, which the other two legs
// and this leg's own non-stream arm answer the other way). Both calls still
// carry DISTINCT ids, and the client answers with the ids it was handed, so the
// divergence is one an arm-parity reader can see and a client cannot.
func TestAStatedIDArrivingAfterTheMintIsRenumberedOnTheFrameArm(t *testing.T) {
	minted := gatewayToolCallIDFor("a", "{}")
	up := round45Frames(t, r57StatedIDFrames(minted)...)
	srv, _ := round39Gateway(t, up, nil)
	_, body := round45Ask(t, srv, round45AskStream)
	got := round46ToolUseIDs(t, body)
	if len(got) != 2 {
		t.Fatalf("the turn opened %d tool_use block(s), want 2:\n%s", len(got), body)
	}
	if got[0] == got[1] {
		t.Errorf("both calls reached the client under one id %q: one tool_result answers two calls (the frame path must still split a stated id another call already holds — round 46, G45-1):\n%s", got[0], body)
	}
	if got[0] != minted {
		t.Errorf("the frame arm numbered the first, id-less call %q instead of the mint %q — the arms agree here only because the mint was taken before the second frame stated the id; if this changed, re-read this file's comment (2026-09-28 audit, round 57, F57-L3-1, reasoned non-fix)", got[0], minted)
	}
	if !strings.Contains(body, `"content_block_stop"`) {
		t.Errorf("the pinned turn is not a well-formed stream:\n%s", body)
	}
}
