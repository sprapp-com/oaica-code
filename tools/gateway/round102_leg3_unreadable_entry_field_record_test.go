package main

// round102_leg3_unreadable_entry_field_record_test.go — leg 3, round 102
// (2026-09-29 audit), F102-L3-1. RECORDED, not changed.
//
// One tool-call ENTRY field the typed decode cannot read — a slot spelled as a
// whole float (`0.0`, `0e0`), and, measured the same way, a fractional slot, a
// numeric `id`/`name`/`type`, or a `function` that is not an object — is refused
// by `oaToolCall.UnmarshalJSON` (`messages.go`, `oaIndexValue` reads a slot only
// as an integer or a numeric STRING). The refusal lands on the whole body on the
// document arms, and the framed arm drops the whole chunk in silence
// (`messages.go`'s chunk decode: `if json.Unmarshal(...) != nil { continue }`,
// no cause stated), so the turn's own ending goes with it:
//
//	{"index":0.0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"cmd\":\"ls\"}"}}
//	  plain    502 {"error":{"message":"unparseable upstream response",...}}
//	  adopted  502 same
//	  framed   200 text "hi", NO call, stop_reason end_turn
//
// and when that entry shares its chunk with the turn's ending, the stated
// `finish_reason` and the stated `usage` are dropped with it — a later fragment
// stating them is what keeps the framed arm's counts, not the turn's own. The
// harm is the one round 100's F100-L3-1 fixed for the shapes the decoder CAN read
// ("the call reached the client on no arm, and a streaming client was told the
// turn ended normally, so an agent loop stops without running the tool"), reached
// through a field spelling this decoder still refuses.
//
// Recorded rather than changed, on exactly the ground round 100's F100-L3-2 was
// recorded one field over: no producer in or out of tree states these spellings.
// This leg's fixtures state integer slots and string ids; this tree's own writer
// (`openai`'s structs) marshals an int index and a string id, and a slot that is
// a whole float is what a JSON writer that held the index as a float emits
// (Python's `json.dumps(0.0)`, not Go's `json.Marshal(float64(0))`, which spells
// it `0`). Rank c by the round's own report.
//
// What a later round should know before "fixing" it: the first half is the same
// tolerance round 100 already put in `oaIndexValue` for the numeric string — a
// slot whose JSON number is a whole number IS the integer it spells, and the
// reading belongs there alone. The second half needs a decision the bridge does
// not currently make: an unreadable entry costs its whole CHUNK, so teaching the
// framed arm to state the document reader's cause (round 96's F96-L2-1 doctrine,
// narrowed here by `nothingRelayed()`) is a different change from widening the
// decode, and only the second leaves this record intact. Reverting either half
// alone is behavioural RED against this pin.

import (
	"strings"
	"testing"
)

// r102FloatSlotEntry is the entry whose slot is a whole float, and r102IntSlotEntry
// is the same call spelled the way this tree's own writer spells it — the control.
const r102FloatSlotEntry = `{"index":0.0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"cmd\":\"ls\"}"}}`
const r102IntSlotEntry = `{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"cmd\":\"ls\"}"}}`

// r102EntryArms spells one entry down all three arms and answers them unjudged.
func r102EntryArms(t *testing.T, entry string) ([3][]string, [3]string) {
	t.Helper()
	return r100RunArms(t, round98Doc("tool_calls", `"hi"`, entry), []string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		round98Frame(entry),
		round98Tail("tool_calls"),
	})
}

// TestMine102ASlotThatIsAWholeFloatIsRefusedWhereTheIntegerIsRead is F102-L3-1's
// record: the unreadable spelling against the readable one, on the same call.
func TestMine102ASlotThatIsAWholeFloatIsRefusedWhereTheIntegerIsRead(t *testing.T) {
	// The control first: the integer spelling is the call on every arm.
	arms, _ := r102EntryArms(t, r102IntSlotEntry)
	for i, arm := range [3]string{"plain", "adopted", "framed"} {
		if got := strings.Join(arms[i], " "); got != `text:hi call:call_1|Read|{"cmd":"ls"}` {
			t.Fatalf("the %s arm reads %q for the INTEGER slot, want the whole turn — this pin's control must hold before its record means anything (2026-09-29 audit, round 102, F102-L3-1)", arm, got)
		}
	}

	// The record: the same call, slot spelled as a whole float.
	arms, bodies := r102EntryArms(t, r102FloatSlotEntry)
	for i, name := range [2]string{"plain", "adopted"} {
		if len(arms[i]) != 0 || !strings.Contains(bodies[i], "unparseable upstream response") {
			t.Errorf("the %s arm now reads %v / %q — if the document arms have been taught to read a slot that is a whole float, this record is spent and the fix it describes has been made (2026-09-29 audit, round 102, F102-L3-1)", name, arms[i], strings.TrimSpace(bodies[i]))
		}
	}
	if got := strings.Join(arms[2], " "); got != "text:hi" {
		t.Errorf("the framed arm reads %q, want the text alone — this record states that an unreadable entry costs the framed arm its call AND silently ends the turn; if it now states the call, or states the document reader's cause instead of dropping the chunk, this record is spent (2026-09-29 audit, round 102, F102-L3-1)", got)
	}
	if body := bodies[2]; !strings.Contains(body, `"stop_reason":"end_turn"`) || strings.Contains(body, `"error"`) {
		t.Errorf("the framed arm answered %q, want the end_turn-with-no-call this record states (2026-09-29 audit, round 102, F102-L3-1)", strings.TrimSpace(body))
	}
}

// TestMine102TheUnreadableEntryCostsItsChunkTheTurnsOwnEnding pins the sharper
// half: when the entry shares its chunk with the turn's ending, the stated
// `finish_reason` and `usage` go with the dropped chunk.
func TestMine102TheUnreadableEntryCostsItsChunkTheTurnsOwnEnding(t *testing.T) {
	entry := r102FloatSlotEntry
	doc := round98Doc("tool_calls", `"hi"`, entry)
	// One chunk states the entry, the finish, and the usage; nothing after it
	// states them again, so nothing else can carry them.
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		`data: {"choices":[{"index":0,"delta":{"tool_calls":[` + entry + `]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":11,"completion_tokens":4}}`,
	}
	arms, bodies := r100RunArms(t, doc, frames)
	if got := strings.Join(arms[2], " "); got != "text:hi" {
		t.Errorf("the framed arm reads %q, want the text alone (2026-09-29 audit, round 102, F102-L3-1)", got)
	}
	// The chunk that stated 11/4 was dropped whole, so the turn's own counts are
	// gone with it — the bridge states its own reading instead.
	if strings.Contains(bodies[2], `"input_tokens":11`) {
		t.Errorf("the framed arm states the turn's own counts — if a chunk carrying an unreadable entry no longer costs the client its stated usage, this record is spent (2026-09-29 audit, round 102, F102-L3-1)")
	}
	for i, name := range [2]string{"plain", "adopted"} {
		if !strings.Contains(bodies[i], "unparseable upstream response") {
			t.Errorf("the %s arm answered %q, want the 502 this record states (2026-09-29 audit, round 102, F102-L3-1)", name, strings.TrimSpace(bodies[i]))
		}
	}
}
