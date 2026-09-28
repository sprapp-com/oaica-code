package main

// round87_served_document_and_meter_integrity_test.go — leg 3, F87-L3-1 and
// F87-L3-2 (2026-09-29 audit, round 87).
//
// One upstream body can wear both spellings at once: frames that relay the turn,
// and a whole completion document that also states a usage object. Which of the
// two the client is served is settled by adoptWholeStream — frames that relayed
// something are the turn and the document is declined; nothing relayed and a
// document that says something is the turn. The METER read the other way: the
// usageRecorder scans the upstream's own bytes, and once it had seen a usage
// object it stopped looking, so its row was built from whichever statement it
// met first rather than from the turn the client was handed.
//
// F87-L3-1. Frames relayed the turn (client read text "hi", usage 8/1) and the
// document they were followed by — declined, never served — stated 9000/500.
// The row booked 9000/500 plus this gateway's own estimate for the fields the
// scan left empty: the ledger charged counts the client was never told,
// unbounded by anything but docBufferLimit.
//
// F87-L3-2. The mirror: a usage-only frame stated 9000/500, then the document
// that WAS the turn stated 7/3 and was adopted — the client read 7/3 while the
// row kept the frame's earlier statement.
//
// Both are the same defect seen from either side: the row and the client are two
// records of one turn and each must state the turn that was served. The
// question is asked of the writer that decided (DocumentServed), from the same
// terms adoptWholeStream judges it by, so the two readers cannot drift.
//
// Measured on 2026-09-29 before the fix, one stream request each: frames+declined
// document → client 8/1, row 9000/500 (cost 0.00051); usage frame+adopted
// document → client 7/3, row 9000/500 (cost 0.00051); document alone → client
// 7/3, row 8/0.

import (
	"strconv"
	"testing"
)

// r87Leg3Doc is a whole completion document that says something and states its
// own usage — the spelling the arms below either relay or adopt.
func r87Leg3Doc(text string, prompt, completion int) string {
	return `{"id":"chatcmpl-r87","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"` + text + `"},"finish_reason":"stop"}],` +
		`"usage":{"prompt_tokens":` + strconv.Itoa(prompt) + `,"completion_tokens":` + strconv.Itoa(completion) +
		`,"total_tokens":` + strconv.Itoa(prompt+completion) + `}}`
}

// r87Leg3UsageFrame is a frame that relays nothing and states usage only — the
// shape round 39 (B-F4/C-F4) and round 47 (C-F7) both read.
func r87Leg3UsageFrame(prompt, completion int) string {
	return `data: {"id":"chatcmpl-r87","choices":[],"usage":{"prompt_tokens":` + strconv.Itoa(prompt) +
		`,"completion_tokens":` + strconv.Itoa(completion) + `,"total_tokens":` + strconv.Itoa(prompt+completion) + `}}` + "\n\n"
}

// TestAMeteredTurnIsTheTurnTheClientWasServed is the F87-L3-1 and F87-L3-2 pin.
// The client's own closing usage and the ledger row are the two records of one
// turn; they must state the same counts, and those counts are the ones the
// served spelling carried.
func TestAMeteredTurnIsTheTurnTheClientWasServed(t *testing.T) {
	// The frame that relays the turn: content, and a usage object of its own.
	relayed := "data: {\"id\":\"chatcmpl-r87\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]," +
		"\"usage\":{\"prompt_tokens\":8,\"completion_tokens\":1,\"total_tokens\":9}}\n\n" +
		"data: {\"choices\":[{\"index\":0,\"finish_reason\":\"stop\"}]}\n\n"

	for _, tc := range []struct {
		name        string
		body        string
		prompt, out int
	}{
		{
			name:   "frames relay the turn, the document after them is declined",
			body:   relayed + r87Leg3Doc("WORLD", 9000, 500) + "\n\ndata: [DONE]\n\n",
			prompt: 8,
			out:    1,
		},
		{
			name:   "a usage frame states 9000/500, the adopted document is the turn",
			body:   r87Leg3UsageFrame(9000, 500) + r87Leg3Doc("WORLD", 7, 3) + "\n",
			prompt: 7,
			out:    3,
		},
		{
			name:   "the document alone",
			body:   r87Leg3Doc("WORLD", 7, 3) + "\n",
			prompt: 7,
			out:    3,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, row := r86Leg3Turn(t, "text/event-stream", tc.body, round44AskStream)
			if status != 200 {
				t.Fatalf("the turn answered %d, want a served 200:\n%s", status, body)
			}
			in, _, out := round40Delta(t, body)
			if in != tc.prompt || out != tc.out {
				t.Fatalf("premise: the client was told input=%d output=%d, want %d/%d — the served spelling of this body states those",
					in, out, tc.prompt, tc.out)
			}
			if row.PromptTokens != in || row.CompletionTokens != out {
				t.Errorf("one turn, two records: the client read input=%d output=%d and the ledger row booked prompt=%d completion=%d. The meter reads the upstream's own bytes and must book the turn the client was SERVED, not the first usage statement the body happens to carry (2026-09-29 audit, round 87, F87-L3-1/F87-L3-2)",
					in, out, row.PromptTokens, row.CompletionTokens)
			}
		})
	}
}
