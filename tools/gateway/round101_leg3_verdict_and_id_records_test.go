package main

// round101_leg3_verdict_and_id_records_test.go — leg 3, round 101
// (2026-09-29 audit). Three readings RECORDED, none changed.
//
// F101-L3-1 and F101-L3-2 are the two directions of one measured consequence of
// the rule this leg has carried since round 82: a NAMED entry's mid-object and
// the entry that continues it SPLIT on the two document arms, while the same two
// fragments FOLD into one call on the framed arm. Round 82 pinned the fold
// itself, and the rounds since re-recorded it — what no test in the corpus
// pinned is the turn's VERDICT, which is where the two spellings part:
//
//	{"index":0,"id":"call_1","name":"Bash","arguments":"{\"a\":"},
//	{"index":2,"id":"call_1","name":"Bash","arguments":{"cmd":"ls"}}   finish length
//	  plain/adopted  [text:hi call:call_1|Bash|{"cmd":"ls"}]   tool_use
//	  framed         [text:hi]                                 max_tokens  (the call gone)
//
//	{"index":0,"id":"call_1","name":"Bash","arguments":"{\"cmd\":"},
//	{"index":0,"id":"call_1","name":"Bash","arguments":"\"ls\"}"}      finish length
//	  plain/adopted  [text:hi]                                 max_tokens  (no call)
//	  framed         [text:hi call:call_1|Bash|{"cmd":"ls"}]   tool_use  (a call invented)
//
// The fold joins the fragments' bytes into one object, so the framed arm's call
// either parses (and is delivered, where the documents drop both halves at the
// truncation) or does not (and is dropped, where the documents keep the second
// entry whole). Rounds 80, 81 and 98 closed exactly this class — one arm a
// runnable call, the other max_tokens with none — for a SINGLE entry; these two
// reach it through the split/fold rule itself, so closing them means deciding
// whether the two spellings still read one body two ways rather than a change to
// a verdict. Recorded, not changed: a later round that unifies the spellings
// spends this record, and one that only patches a verdict here leaves the fold
// intact and the other direction reachable.
//
// F101-L3-3 is a different failure, also recorded: the id a call wears when the
// call ABOVE it is dropped at the truncation. Same body, same bytes, same calls
// on every arm but one field:
//
//	{"index":0,"id":"call_1","name":"Bash","arguments":"echo hi"},
//	{"index":1,"id":"call_1","name":"Read","arguments":"{\"cmd\":\"ls\"}"}  finish length
//	  plain/adopted  call:call_1|Read|{"cmd":"ls"}
//	  framed         call:call_76c64b36|Read|{"cmd":"ls"}
//
// The Read block states an id a named block already carries, so the bridge mints
// it one of its own (round 44/59/71's rule, correct at the moment it is decided:
// the Bash call standing beside it is a call, freeform arguments and all). The
// Bash call is dropped only at finishStream, when the turn's truncation is
// known — after the Read block has been opened and its id written to the client,
// and the content the client has been sent cannot be un-sent (round 49's
// already-out argument, round 57's residual). Clearing the dropped block's claim
// at the drop was measured and moves nothing: the mint was decided while the
// block was still standing. So the client's correlation id depends on which arm
// answered — a rank-b wire that needs one id stated twice plus a truncated
// freeform first call — and closing it means deciding the id at a point the
// bridge does not yet have the truncation.

import (
	"strings"
	"testing"
)

// r101Record runs one upstream body three ways and answers each arm's blocks
// and body, judging nothing.
func r101Record(t *testing.T, doc string, frames []string) ([3][]string, [3]string) {
	t.Helper()
	return r100RunArms(t, doc, frames)
}

func r101RecordBlocks(t *testing.T, blocks []string) string {
	t.Helper()
	return strings.Join(blocks, " ")
}

// TestMine101TheSplitAndFoldRuleDecidesTheTurnVerdict is F101-L3-1 and
// F101-L3-2's record: both directions, spelled as the wire spells them.
func TestMine101TheSplitAndFoldRuleDecidesTheTurnVerdict(t *testing.T) {
	for _, c := range []struct {
		name   string
		finish string
		entry1 string
		entry2 string
		// want lists the two document arms' reading and the framed arm's.
		doc   string
		frame string
	}{
		{
			"the fold leaves nothing the documents keep",
			"length",
			`{"index":0,"id":"call_1","name":"Bash","arguments":"{\"a\":"}`,
			`{"index":2,"id":"call_1","name":"Bash","arguments":{"cmd":"ls"}}`,
			`text:hi call:call_1|Bash|{"cmd":"ls"}`,
			`text:hi`,
		},
		{
			"the fold invents the call the documents refuse",
			"length",
			`{"index":0,"id":"call_1","name":"Bash","arguments":"{\"cmd\":"}`,
			`{"index":0,"id":"call_1","name":"Bash","arguments":"\"ls\"}"}`,
			`text:hi`,
			`text:hi call:call_1|Bash|{"cmd":"ls"}`,
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			doc := round98Doc(c.finish, `"hi"`, c.entry1+`,`+c.entry2)
			frames := []string{
				`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
				round98Frame(c.entry1),
				round98Frame(c.entry2),
				round98Tail(c.finish),
			}
			arms, _ := r101Record(t, doc, frames)
			for i, arm := range [2]string{"plain", "adopted"} {
				if got := r101RecordBlocks(t, arms[i]); got != c.doc {
					t.Errorf("the %s arm reads %q, want %q — this record states the two document arms' reading of the split (2026-09-29 audit, round 101, F101-L3-1/F101-L3-2)", arm, got, c.doc)
				}
			}
			if got := r101RecordBlocks(t, arms[2]); got != c.frame {
				t.Errorf("the framed arm reads %q, want %q — this record states the fragment spelling's fold and the verdict it carries; if the spellings have been unified, this record is spent (2026-09-29 audit, round 101, F101-L3-1/F101-L3-2)", got, c.frame)
			}
		})
	}
}

// TestMine101TheIdOfTheCallAboveADroppedOne is F101-L3-3's record.
func TestMine101TheIdOfTheCallAboveADroppedOne(t *testing.T) {
	doc := round98Doc("length", `"hi"`,
		`{"index":0,"id":"call_1","name":"Bash","arguments":"echo hi"},`+
			`{"index":1,"id":"call_1","name":"Read","arguments":"{\"cmd\":\"ls\"}"}`)
	frames := []string{
		`data: {"choices":[{"index":0,"delta":{"content":"hi"}}]}`,
		round98Frame(`{"index":0,"id":"call_1","name":"Bash","arguments":"echo hi"}`),
		round98Frame(`{"index":1,"id":"call_1","name":"Read","arguments":"{\"cmd\":\"ls\"}"}`),
		round98Tail("length"),
	}
	arms, _ := r101Record(t, doc, frames)
	if got := r101RecordBlocks(t, arms[0]); got != `text:hi call:call_1|Read|{"cmd":"ls"}` {
		t.Errorf("the document arm reads %q, want the stated id — this record states the two readings (2026-09-29 audit, round 101, F101-L3-3)", got)
	}
	if got := r101RecordBlocks(t, arms[2]); got != `text:hi call:call_76c64b36|Read|{"cmd":"ls"}` {
		t.Errorf("the framed arm reads %q, want the minted id this record states; if a later round has taught the framed arm to state the dropped call's id here, the record is spent and the fix it describes has been made (2026-09-29 audit, round 101, F101-L3-3)", got)
	}
}
