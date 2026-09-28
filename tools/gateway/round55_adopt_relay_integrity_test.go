package main

// round55_adopt_relay_integrity_test.go — round 55's findings on the metered
// gateway's ADOPTED document (leg 3), the arm that answers a stream request with
// an upstream's single application/json completion.
//
// Neither finding is about the frame path or the non-stream path: both of those
// were already right (round 53's F1 drops a truncated named call, F3 relays a
// nameless fragment's arguments as text). Adoption is the third arm of the same
// document, and it asked neither question in the same terms.
//
//   - G1: an adopted document whose ONLY payload is a call the token limit cut
//     short relays nothing at all — the truncated call is dropped (round 45,
//     B45-13) — and round 42's empty-stream arm then read that silence as a
//     failed turn: the client got an `error` event while the ledger row, which
//     asks documentSaysSomething of the same document, booked a metered 200 for
//     it. The sibling legs serve the identical turn as 200 + max_tokens: a
//     document the upstream finished, whose finish_reason closes the turn, is
//     not a stream that said nothing.
//
//   - G2: a fragment the upstream never NAMED, carrying arguments, was dropped
//     by adoption even though every other arm relays those bytes as text —
//     finishStream hands them over at the end of the frame turn and finalize
//     appends them after its call blocks on the non-stream turn. A nameless
//     fragment is never an executable call, so the rule that drops a truncated
//     NAMED call has nothing to drop here, and making the relay depend on
//     truncation meant one document produced text on two arms and nothing on
//     the third.

import (
	"net/http"
	"strings"
	"testing"
)

// r55Adopted answers one stream request with one whole completion served as
// application/json, and reports what the client read, what the row booked.
func r55Adopted(t *testing.T, doc string) (int, string, ledgerEntry) {
	t.Helper()
	up := round45Upstream(t, "application/json", doc)
	srv, ledger := round39Gateway(t, up, nil)
	status, body := round45Ask(t, srv, round45AskStream)
	rows := waitLedger(t, ledger, 1)
	if len(rows) != 1 {
		t.Fatalf("the turn booked %d ledger rows, want 1", len(rows))
	}
	return status, body, rows[0]
}

// TestAnAdoptedDocumentCutShortIsNotAnEmptyStream is G1.
func TestAnAdoptedDocumentCutShortIsNotAnEmptyStream(t *testing.T) {
	// The model was still writing this call when the token limit stopped it, so
	// there is no executable call in the document — and no text either. The
	// document still states its own finish_reason, which is the difference
	// between an answer that stops short and a stream that died.
	doc := `{"id":"c1","choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Bash","arguments":"echo hi"}}]}}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`

	status, body, row := r55Adopted(t, doc)

	if status != http.StatusOK {
		t.Fatalf("an adopted document the upstream finished was answered %d:\n%s", status, body)
	}
	if strings.Contains(body, `"type":"error"`) || strings.Contains(body, "upstream returned an empty stream") {
		t.Errorf("the client read this turn as a failure:\n%s\nthe document's only payload is a call the token limit cut short, which cannot be run — but the document is still the upstream's whole answer, with its own finish_reason, and the frame path of this leg, its non-stream path and the client leg all serve the identical turn as 200 + max_tokens (round 53, F1). Reading adoption's silence as an empty STREAM gave one document two readings: an `error` event to the client, a metered 200 to the row (2026-09-28 audit, round 55)", body)
	}
	if sr := r53StopReason(t, "true", body); sr != "max_tokens" {
		t.Errorf("the adopted turn reports stop_reason %q, want max_tokens — the finish_reason the document itself states\n%s", sr, body)
	}
	if row.Status != http.StatusOK {
		t.Errorf("the ledger booked %d for a turn the client read as %d: the row and the client are two readers of one turn and may not disagree, and this row asks documentSaysSomething of the very document adoption relayed (2026-09-28 audit, round 55)",
			row.Status, status)
	}

	// The control: a document that says NOTHING has no finish_reason to close it
	// and is still refused — adoption must not become "any document is a turn".
	// This is round 43's B43-1, unchanged by round 55.
	//
	// The SENTENCE is round 90's F90-L3-2: this body holds no `data:` frame at
	// all, so the refusal is the one the document arms state, not the one the
	// frame arms state — the same bytes answered `stream:false` say "upstream
	// returned an empty completion", and one body states one cause whichever
	// spelling the client asked for.
	empty := `{"id":"c2","choices":[{"finish_reason":"stop","message":{"role":"assistant","content":""}}]}`
	es, ebody, erow := r55Adopted(t, empty)
	if !strings.Contains(ebody, "upstream returned an empty completion") {
		t.Errorf("a completion with no content, no reasoning and no call was served as a turn (status %d):\n%s\na client that reads a completed empty turn never retries", es, ebody)
	}
	if erow.Status < 500 {
		t.Errorf("the ledger booked %d for a turn the client read as a failure:\n%+v", erow.Status, erow)
	}
}

// TestAnAdoptedDocumentsNamelessFragmentIsRelayed is G2.
func TestAnAdoptedDocumentsNamelessFragmentIsRelayed(t *testing.T) {
	// The fragment has no name (so no block can be opened for it) and is not
	// finished JSON (so it is not a call even if it had one): its bytes are the
	// model's raw output, and this is the turn's ONLY payload.
	doc := `{"id":"c1","choices":[{"finish_reason":"length","message":{"role":"assistant","content":"","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"arguments":"{\"a\":"}}]}}],"usage":{"prompt_tokens":8,"completion_tokens":2}}`

	status, body, row := r55Adopted(t, doc)

	if status != http.StatusOK {
		t.Fatalf("the turn was answered %d:\n%s", status, body)
	}
	if got := r53BlockText(t, body); got != `{"a":` {
		t.Errorf("the adopted turn carried %q as text, want the fragment's own bytes\nthe frame path (finishStream) and the non-stream path (finalize) both relay a nameless fragment's arguments as text, so this is the model's output being dropped on one arm of three, and on a turn whose output it is (2026-09-28 audit, round 55)\n%s",
			got, body)
	}
	if strings.Contains(body, `"type":"tool_use"`) {
		t.Errorf("the fragment reached the client as an executable tool_use block:\n%s\nno event after content_block_start can name a call, and these arguments are not JSON", body)
	}
	if row.Status != http.StatusOK {
		t.Errorf("the ledger booked %d for the turn the client was served:\n%+v", row.Status, row)
	}
}
