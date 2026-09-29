package main

// round99_leg3_nameless_continuation_and_restated_slot_records_test.go — leg 3,
// round 99 (2026-09-29 audit). Two readings RECORDED, not changed.
//
// One upstream turn, three spellings: the whole document under stream:false, the
// whole document under stream:true, and the same entries one fragment per delta
// (round98Spellings).
//
// RECORD 1 — a continuation that restates the call's NAME mid-object.
//
// The canonical streaming shape for one call states the id and name on the delta
// that introduces it (often with an empty arguments string) and then carries the
// argument bytes in later deltas at the same index with the id and name omitted.
// That shape is one call on every arm, and the first pin below holds it. When a
// middle delta instead RESTATES the id and the name mid-object on the slot the
// argument-less statement opened, the fragment arm reads the call twice with the
// object split across the two and their order reversed:
//
//	doc arms   [call_2|Bash|{}] [call_83cf9330|Bash|{"cmd":"ls"}]
//	framed     [call_2|Bash|{"_raw":"\"ls\"}"}] [call_1315791b|Bash|{"_raw":"{\"cmd\":"}]
//
// The head of the object went to the second call and the tail to the first, and
// neither call can be run. A restating id alone, with no name, reads the turn as
// one call (third pin, M2c-3) — so it is the NAME at a slot whose block is
// already open that opens a second call, which is exactly what rounds 39
// (TestANameArrivingLateDoesNotStartAfterALaterBlock), 63 and 77 pin on purpose
// for the other wire. Recorded rather than changed: the fragment arm answers the
// spelling by the rule those rounds fixed, and the only whole-completion spelling
// of this turn is a document that states the call twice — which no producer of
// this leg's fixtures emits.
//
// RECORD 2 — the restated-entry fold and a chunk boundary.
//
// A whole completion that states the same call (same id, same name, same
// arguments) at two slots is folded into ONE call on every arm — the restated
// entry is not a second call (round 48's TestARestatedStatedCallIsOneBlock).
// Move a chunk boundary into the second entry's object and the fragment arm
// stops folding: it reads two calls, the second under a fresh mint, where both
// document arms still read the one. The same entries, read differently because
// of where the upstream put a newline.
//
//	whole   plain/adopted/framed   [call_1|Bash|{}]
//	split   plain/adopted          [call_1|Bash|{}]
//	framed                         [call_1|Bash|{}] [call_781541ff|Bash|{}]
//
// Recorded rather than changed: the fold is a whole-completion rule (a document
// states its entries at once), the fragment arm cannot know an entry is a
// restatement until its bytes have arrived, and the only spelling that reaches
// the fragment arm here is a wire that states the same call at two slots — which
// no producer of this leg's fixtures emits. Neither reading is a client's call
// lost or duplicated in a runnable form: the two blocks carry the same empty
// object.

import (
	"strings"
	"testing"
)

func r99L3Doc(entries string) string { return round98Doc("tool_calls", "null", entries) }

func r99L3Entry(slot int, id, name, args string) string {
	ix := string(rune('0' + slot))
	s := `{"index":` + ix
	if id != "" {
		s += `,"id":"` + id + `"`
	}
	return s + `,"type":"function","function":{"name":"` + name + `","arguments":"` + args + `"}}`
}

func r99L3Frame(slot int, id, name, args string) string {
	ix := string(rune('0' + slot))
	s := `{"index":` + ix
	if id != "" {
		s += `,"id":"` + id + `"`
	}
	if name != "" {
		s += `,"function":{"name":"` + name + `","arguments":"` + args + `"}`
	} else {
		s += `,"function":{"arguments":"` + args + `"}`
	}
	s += `}`
	return `data: {"choices":[{"index":0,"delta":{"tool_calls":[` + s + `]}}]}`
}

// r99L3Reads returns the three arms' readings joined, in plain/adopted/framed
// order.
func r99L3Reads(t *testing.T, doc string, frames []string) [3]string {
	t.Helper()
	var out [3]string
	for i, b := range round98Spellings(t, doc, frames) {
		out[i] = strings.Join(b, " ")
	}
	return out
}

// TestAStreamedCallWhoseContinuationsStateNoNameIsOneCallEverywhere is the good
// path the record below is measured against: the canonical shape — the call
// stated once, its argument bytes arriving in later deltas at the same index
// with no id and no name — is one runnable call on every arm.
func TestAStreamedCallWhoseContinuationsStateNoNameIsOneCallEverywhere(t *testing.T) {
	doc := r99L3Doc(r99L3Entry(0, "call_1", "Bash", `{\"cmd\":\"ls\"}`))
	frames := []string{
		r99L3Frame(0, "call_1", "Bash", ``),
		r99L3Frame(0, "", "", `{\"cmd\":`),
		r99L3Frame(0, "", "", `\"ls\"}`),
		round98Tail("tool_calls"),
	}
	want := `call:call_1|Bash|{"cmd":"ls"}`
	got := r99L3Reads(t, doc, frames)
	for i, name := range [3]string{"plain", "adopted", "framed"} {
		if got[i] != want {
			t.Errorf("%s reads %q, want %q — the canonical streaming shape is one whole call on every arm (2026-09-29 audit, round 99, F99-L3-1)", name, got[i], want)
		}
	}
}

// TestANameRestatedMidObjectOnTheAnnouncementSlotIsTheReadingThisTreeHas is
// RECORD 1: the reading as it is, on the fragment arm and on the document arms,
// with the reason it stands recorded in the file comment.
func TestANameRestatedMidObjectOnTheAnnouncementSlotIsTheReadingThisTreeHas(t *testing.T) {
	doc := r99L3Doc(r99L3Entry(0, "call_2", "Bash", ``) + "," + r99L3Entry(0, "call_2", "Bash", `{\"cmd\":\"ls\"}`))
	tail := round98Tail("tool_calls")
	nameRestated := []string{
		r99L3Frame(0, "call_2", "Bash", ``),
		r99L3Frame(0, "call_2", "Bash", `{\"cmd\":`),
		r99L3Frame(0, "", "", `\"ls\"}`),
		tail,
	}
	wantDoc := `call:call_2|Bash|{} call:call_83cf9330|Bash|{"cmd":"ls"}`
	wantFramed := `call:call_2|Bash|{"_raw":"\"ls\"}"} call:call_1315791b|Bash|{"_raw":"{\"cmd\":"}`
	got := r99L3Reads(t, doc, nameRestated)
	if got[0] != wantDoc || got[1] != wantDoc {
		t.Errorf("the document arms read %q | %q, want %q — a whole completion states this call twice and this leg mints the second (2026-09-29 audit, round 99, F99-L3-1)", got[0], got[1], wantDoc)
	}
	if got[2] != wantFramed {
		t.Errorf("the fragment arm reads %q, want %q — the name restated at a slot whose block is already open opens a second call there and the object splits across the two, head to the second and tail to the first (2026-09-29 audit, round 99, F99-L3-1)", got[2], wantFramed)
	}

	// The same wire with the restating chunk naming the call by its ID alone is
	// one whole call on every arm — so the second call above is the NAME, not
	// the id, and it is the rule rounds 39, 63 and 77 pin for that wire.
	idOnly := []string{
		r99L3Frame(0, "call_2", "Bash", ``),
		r99L3Frame(0, "call_2", "", `{\"cmd\":`),
		r99L3Frame(0, "", "", `\"ls\"}`),
		tail,
	}
	one := `call:call_2|Bash|{"cmd":"ls"}`
	if got := r99L3Reads(t, doc, idOnly); got[2] != one {
		t.Errorf("with the id restated and no name the fragment arm reads %q, want %q — the second call above is opened by the NAME (2026-09-29 audit, round 99, F99-L3-1)", got[2], one)
	}

	// And the document arms of the same wire keep their own reading: this turn
	// has no spelling that both spellings agree on, which is what makes it a
	// record rather than a fix.
	t.Logf("F99-L3-1: doc %s | framed %s", wantDoc, wantFramed)
}

// TestARestatedEntryFoldsUntilAChunkBoundaryEntersIt is RECORD 2: the restated
// entry is one call everywhere until the second entry's object crosses a chunk
// boundary, where the fragment arm stops folding it.
func TestARestatedEntryFoldsUntilAChunkBoundaryEntersIt(t *testing.T) {
	doc := r99L3Doc(r99L3Entry(0, "call_1", "Bash", `{}`) + "," + r99L3Entry(1, "call_1", "Bash", `{}`))
	tail := round98Tail("tool_calls")

	whole := []string{
		r99L3Frame(0, "call_1", "Bash", `{}`),
		r99L3Frame(1, "call_1", "Bash", `{}`),
		tail,
	}
	one := `call:call_1|Bash|{}`
	for i, name := range [3]string{"plain", "adopted", "framed"} {
		if got := r99L3Reads(t, doc, whole)[i]; got != one {
			t.Errorf("%s reads %q, want %q — a restated stated call is one block (round 48's TestARestatedStatedCallIsOneBlock) (2026-09-29 audit, round 99, F99-L3-2)", name, got, one)
		}
	}

	split := []string{
		r99L3Frame(0, "call_1", "Bash", `{}`),
		r99L3Frame(1, "call_1", "Bash", `{`),
		r99L3Frame(1, "", "", `}`),
		tail,
	}
	two := one + ` call:call_781541ff|Bash|{}`
	got := r99L3Reads(t, doc, split)
	if got[0] != one || got[1] != one {
		t.Errorf("the document arms read %q | %q, want %q — the fold is a whole-completion rule and both document arms answer it (2026-09-29 audit, round 99, F99-L3-2)", got[0], got[1], one)
	}
	if got[2] != two {
		t.Errorf("the fragment arm reads %q, want %q — one chunk boundary inside the second entry's object is enough to stop the fold, and the same entries then read as two calls (2026-09-29 audit, round 99, F99-L3-2)", got[2], two)
	}

	// The restated twin whose bytes are a freeform line rather than an object
	// reads the same way under the same boundary.
	docFF := r99L3Doc(r99L3Entry(0, "call_2", "Bash", `42`) + "," + r99L3Entry(1, "call_2", "Bash", `42`))
	wholeFF := []string{
		r99L3Frame(0, "call_2", "Bash", `42`),
		r99L3Frame(1, "call_2", "Bash", `42`),
		round98Tail("tool_calls"),
	}
	splitFF := []string{
		r99L3Frame(0, "call_2", "Bash", `42`),
		r99L3Frame(1, "call_2", "Bash", `4`),
		r99L3Frame(1, "", "", `2`),
		round98Tail("tool_calls"),
	}
	oneFF := `call:call_2|Bash|{"_raw":"42"}`
	if got := r99L3Reads(t, docFF, wholeFF)[2]; got != oneFF {
		t.Errorf("the freeform twin whole reads %q, want %q (2026-09-29 audit, round 99, F99-L3-2)", got, oneFF)
	}
	if got := r99L3Reads(t, docFF, splitFF)[2]; got != oneFF+` call:call_2e331e60|Bash|{"_raw":"42"}` {
		t.Errorf("the freeform twin split reads %q, want one call folded and the same call read as a second under a fresh mint (2026-09-29 audit, round 99, F99-L3-2)", got)
	}
	t.Logf("F99-L3-2: whole %s | split %s", one, two)
}
