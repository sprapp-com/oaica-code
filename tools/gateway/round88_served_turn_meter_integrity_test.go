package main

// round88_served_turn_meter_integrity_test.go — leg 3, F88-L3-1, F88-L3-2,
// F88-L3-3 and F88-L3-4 (2026-09-29 audit, round 88).
//
// The meter (usageRecorder) reads the UPSTREAM's own bytes and the bridge reads
// the same bytes to decide what the client is served. Round 87 made the second
// reader ask the first (DocumentServed) for the turn; these four are the same
// two readers disagreeing about where one turn ENDS and what the last statement
// of a body is.
//
// F88-L3-1. The scanner consumes whole LINES, so a stream that ended without a
// final newline left its last line in the partial-line buffer, and the document
// read glued it to the body: `{...document...}data: {...}` is not JSON. A turn
// whose client WAS served the document (7/3) was metered from the usage frame
// that preceded it — 9000/500 — and, where the pending line was the only thing
// left, was metered as no usage at all. Two spellings of one body, differing by
// one trailing newline, and the same defect seen from both sides.
//
// F88-L3-2. The meter's document read ASSIGNED the document's usage over the
// scan's, so a cache hit an earlier frame had stated was erased: the client,
// told the frame's hit, read input=2 with cache=5 while the row recorded 7/3
// and cache=0 — the one field that decides whether the prompt paid the cached
// rate.
//
// F88-L3-3. A document with usage and no choices is what the bridge REFUSES
// (502, nothing served). The meter's document read accepted it anyway, so the
// buffered arm booked the refused turn's usage while the streamed arm booked
// nothing for the same bytes.
//
// F88-L3-4. The two arms bounded the body they would read at different sizes
// (4 MiB buffered, 8 MiB of frames), so a 5 MiB completion document was metered
// with usage_seen=false when it arrived buffered and usage_seen=true when the
// same bytes arrived as frames.

import (
	"strconv"
	"strings"
	"testing"
)

// r88Leg3Doc is a whole completion document that states its own usage, with a
// cache hit when one is given.
func r88Leg3Doc(text string, prompt, completion, cached int) string {
	details := ""
	if cached > 0 {
		details = `,"prompt_tokens_details":{"cached_tokens":` + strconv.Itoa(cached) + `}`
	}
	return `{"id":"chatcmpl-r88","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"` + text +
		`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":` + strconv.Itoa(prompt) +
		`,"completion_tokens":` + strconv.Itoa(completion) + `,"total_tokens":` + strconv.Itoa(prompt+completion) + details + `}}`
}

// r88Leg3UsageFrame is a frame that relays nothing and states usage only.
func r88Leg3UsageFrame(prompt, completion, cached int) string {
	details := ""
	if cached > 0 {
		details = `,"prompt_tokens_details":{"cached_tokens":` + strconv.Itoa(cached) + `}`
	}
	return `data: {"id":"chatcmpl-r88","choices":[],"usage":{"prompt_tokens":` + strconv.Itoa(prompt) +
		`,"completion_tokens":` + strconv.Itoa(completion) + `,"total_tokens":` + strconv.Itoa(prompt+completion) + details + `}}` + "\n\n"
}

// r88Leg3FinFrame ends a stream the way an upstream that relays a document ends
// it.
const r88Leg3FinFrame = `data: {"choices":[{"index":0,"finish_reason":"stop"}]}`

// sameTurn reads one row against one client reading: the row states the prompt
// the client was billed for (its input plus whatever of it was a cache hit) and
// the same hit.
func sameTurn(t *testing.T, name string, row ledgerEntry, in, cache, out int, finding string) {
	t.Helper()
	if row.CachedTokens != cache || row.PromptTokens-row.CachedTokens != in || row.CompletionTokens != out {
		t.Errorf("%s: the client read input=%d cache=%d output=%d and the ledger row booked prompt=%d cache=%d completion=%d — one turn has two records and both are of the turn that was served (2026-09-29 audit, round 88, %s)",
			name, in, cache, out, row.PromptTokens, row.CachedTokens, row.CompletionTokens, finding)
	}
}

// TestOneTurnTwoRecordsIsTheServedSpelling is the F88-L3-1 and F88-L3-2 pin: a
// document that is the turn is metered as the turn, whether the frame that ends
// the stream kept its newline or not, and a cache hit the body stated survives
// the document's own usage.
func TestOneTurnTwoRecordsIsTheServedSpelling(t *testing.T) {
	for _, tc := range []struct {
		name           string
		body           string
		in, cache, out int
		pendingFrame   string
		finding        string
	}{
		{
			name:         "a usage frame, the document, an unterminated finish frame",
			body:         r88Leg3UsageFrame(9000, 500, 0) + r88Leg3Doc("WORLD", 7, 3, 0) + "\n" + r88Leg3FinFrame,
			in:           7,
			out:          3,
			pendingFrame: "finish frame",
			finding:      "F88-L3-1",
		},
		{
			name:         "the same body, ended by an unterminated [DONE]",
			body:         r88Leg3UsageFrame(9000, 500, 0) + r88Leg3Doc("WORLD", 7, 3, 0) + "\n" + "data: [DONE]",
			in:           7,
			out:          3,
			pendingFrame: "[DONE] sentinel",
			finding:      "F88-L3-1",
		},
		{
			name: "a frame stating a cache hit, then the adopted document",
			body: r88Leg3UsageFrame(7, 3, 5) + r88Leg3Doc("WORLD", 7, 3, 0) + "\n",
			in:   2, cache: 5, out: 3,
			finding: "F88-L3-2",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, row := r86Leg3Turn(t, "text/event-stream", tc.body, round44AskStream)
			if status != 200 {
				t.Fatalf("the turn answered %d, want a served 200:\n%s", status, body)
			}
			in, cache, out := round40Delta(t, body)
			if in != tc.in || cache != tc.cache || out != tc.out {
				t.Fatalf("premise: the client was told input=%d cache=%d output=%d, want %d/%d/%d — the document this body adopts states those",
					in, cache, out, tc.in, tc.cache, tc.out)
			}
			sameTurn(t, tc.name, row, in, cache, out, tc.finding)
		})
	}
}

// TestAPendingFrameIsStillMetered is the other half of F88-L3-1: a body's last
// statement is a statement whether or not the upstream put a newline after it.
// The frame that was still pending is the one that stated the turn's usage, and
// the row that read the stream to its end read it.
func TestAPendingFrameIsStillMetered(t *testing.T) {
	const relayed = "data: {\"id\":\"chatcmpl-r88\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		r88Leg3FinFrame + "\n\n"
	for _, tc := range []struct{ name, body string }{
		{"the usage frame without its newline", relayed + strings.TrimSuffix(r88Leg3UsageFrame(9000, 500, 0), "\n\n")},
		{"the usage frame with it", relayed + r88Leg3UsageFrame(9000, 500, 0)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body, row := r86Leg3Turn(t, "text/event-stream", tc.body, round44AskStream)
			if status != 200 {
				t.Fatalf("the turn answered %d, want a served 200:\n%s", status, body)
			}
			in, _, out := round40Delta(t, body)
			sameTurn(t, tc.name, row, in, 0, out, "F88-L3-1")
			if !row.UsageSeen {
				t.Errorf("%s: the row recorded usage_seen=false for a turn the upstream stated usage for. The last line of a stream is read where the upstream left it, not where the next newline happens to be (2026-09-29 audit, round 88, F88-L3-1):\n%s",
					tc.name, tc.body)
			}
		})
	}
}

// TestAChoiceslessDocumentIsNotATurnToMeter is the F88-L3-3 pin: the meter must
// not book usage for a body the bridge refuses as no answer, and the two arms
// must refuse it in the same breath — one body, one row.
func TestAChoiceslessDocumentIsNotATurnToMeter(t *testing.T) {
	const refused = `{"choices":[],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	for _, arm := range []struct {
		name, ct, ask string
	}{
		{"streamed", "text/event-stream", round44AskStream},
		{"buffered", "application/json", round44AskPlain},
	} {
		t.Run(arm.name, func(t *testing.T) {
			status, body, row := r86Leg3Turn(t, arm.ct, refused, arm.ask)
			if status != 502 {
				t.Errorf("a document with no choices answered %d, want 502 — it is not a turn (2026-09-29 audit, round 88, F88-L3-3):\n%s", status, body)
			}
			if row.PromptTokens != 0 || row.CompletionTokens != 0 || row.UsageSeen {
				t.Errorf("the %s arm booked prompt=%d completion=%d usage_seen=%v for a turn it refused and never served. The usage of a refused body is not a turn's usage, and which arm met the same bytes must not decide whether it is booked (2026-09-29 audit, round 88, F88-L3-3)",
					arm.name, row.PromptTokens, row.CompletionTokens, row.UsageSeen)
			}
			if row.CostUSD != 0 {
				t.Errorf("the %s arm charged %g for a refused turn (2026-09-29 audit, round 88, F88-L3-3)", arm.name, row.CostUSD)
			}
		})
	}
}

// TestBothArmsTrustTheSameSize is the F88-L3-4 pin: a document past the
// buffered arm's old 4 MiB gate is the same turn as the one the streamed arm
// read, and both arms meter it as a document they saw.
func TestBothArmsTrustTheSameSize(t *testing.T) {
	const big = `{"choices":[{"index":0,"message":{"role":"assistant","content":"` + "A" + `"},"finish_reason":"stop"}],"usage":{"prompt_tokens":7,"completion_tokens":3,"total_tokens":10}}`
	pad := strings.Repeat("A", 5<<20)
	body := strings.Replace(big, `"A"`, `"`+pad+`"`, 1)

	_, _, streamed := r86Leg3Turn(t, "text/event-stream", body, round44AskStream)
	_, _, buffered := r86Leg3Turn(t, "application/json", body, round44AskPlain)
	if buffered.PromptTokens != streamed.PromptTokens || buffered.CompletionTokens != streamed.CompletionTokens || buffered.UsageSeen != streamed.UsageSeen {
		t.Errorf("one five-MiB document metered two ways: streamed prompt=%d completion=%d usage_seen=%v, buffered prompt=%d completion=%d usage_seen=%v. One turn reaches the client as a document or as frames; the size the meter will read must not depend on which arm carried it (2026-09-29 audit, round 88, F88-L3-4)",
			streamed.PromptTokens, streamed.CompletionTokens, streamed.UsageSeen,
			buffered.PromptTokens, buffered.CompletionTokens, buffered.UsageSeen)
	}
}
