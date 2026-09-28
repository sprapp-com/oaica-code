package launch

// round88_error_framing_and_held_call_identity_integrity_test.go — leg 2,
// F88-L2-1, F88-L2-2, F88-L2-3 and F88-L2-4 (2026-09-29 audit, round 88).
//
// Four ways the same upstream body was read differently by the spellings this
// leg accepts, all measured 2026-09-29 at HEAD `5cfdd74ea`:
//
// F88-L2-1 — a named event. The stream reader joins every line of an event into
// the payload it hands `upstreamErrorMessage`, and that recognises an error only
// in a string that starts `{`. A field line — `event: error`, an `id:`, a
// `retry:`, a `:` comment — put its own name in front, so a 200 whose body was a
// named event carrying the upstream's error object, or the same object pretty
// printed over several `data:` lines under a named event, matched nothing and
// the client was told "upstream stream ended before the response was complete":
// a cause that did not happen (the stream ended exactly where the upstream ended
// it) in place of the upstream's own reason. The field line also went into the
// whole-body fallback buffer, which is why the unframed spelling of the same
// event was corrupted too.
//
// F88-L2-2 — the id a call is stated under. `heldCall` asked only name and
// argument bytes, so a whole-completion frame stating a call the turn already
// held under a DIFFERENT id was folded away as a restatement: two calls the wire
// gave two ids reached the client as one, where the same two statements written
// as fragments — and the same two entries in a whole-list body — reached it as
// two. The frame spelling lost a call the client had been told to run.
//
// F88-L2-3 — the emptiness sentence. The [DONE] gate asked `len(toolAccums) > 0`
// while the empty-turn guard it feeds asks `relaysSomething`: a fragment that
// relays no block at all (an opener with neither a name nor arguments) left the
// gate saying the stream completed and the guard saying it had said nothing, so
// `[opener][DONE]` reached the client as "upstream returned an empty completion"
// where `[DONE]` alone reached it as "upstream stream ended before the response
// was complete".
//
// F88-L2-4 — the two no-answer sentences. The buffered arm refused a body with
// no choices as "upstream returned no completion choices" while the streaming
// arm refused the same bytes as "upstream returned an empty completion".

import (
	"strings"
	"testing"
)

// r88Frame is a whole-completion frame stating one call, under `id` when one is
// given — the shape F88-L2-2 turns on.
func r88Frame(id, name, args string) string {
	idPart := ""
	if id != "" {
		idPart = `"id":"` + id + `",`
	}
	return `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{` + idPart +
		`"type":"function","function":{"name":"` + name + `","arguments":"` + args + `"}}]},"finish_reason":"tool_calls"}]}`
}

// r88Frag is the same call stated as a delta fragment.
func r88Frag(id, name, args string) string {
	idPart := ""
	if id != "" {
		idPart = `"id":"` + id + `",`
	}
	return `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,` + idPart +
		`"type":"function","function":{"name":"` + name + `","arguments":"` + args + `"}}]}}]}`
}

// TestANamedEventCarriesTheUpstreamsOwnReason is the F88-L2-1 pin: the SSE
// field lines that frame an event are framing, not payload, so the upstream's
// own reason reaches the client whichever fields the sender used — and the
// comment-only body, which carries no object at all, is not an error either way.
func TestANamedEventCarriesTheUpstreamsOwnReason(t *testing.T) {
	const (
		want    = "upstream error: boom"
		flat    = `{"error":{"message":"boom","type":"server_error"}}`
		spread  = "{\n  \"error\": {\n    \"message\": \"boom\",\n    \"type\": \"server_error\"\n  }\n}"
		spreadD = "data: {\ndata:   \"error\": {\"message\": \"boom\", \"type\": \"server_error\"}\ndata: }"
	)
	for _, tc := range []struct{ name, body string }{
		{"one data frame", "data: " + flat + "\n\ndata: [DONE]"},
		{"spread over data lines", spreadD + "\n\ndata: [DONE]"},
		{"named event, one data frame", "event: error\ndata: " + flat + "\n\ndata: [DONE]"},
		{"named event, spread over data lines", "event: error\n" + spreadD + "\n\ndata: [DONE]"},
		{"id line, spread over data lines", "id: 3\n" + spreadD + "\n\ndata: [DONE]"},
		{"retry line, spread over data lines", "retry: 100\n" + spreadD + "\n\ndata: [DONE]"},
		{"comment line, spread over data lines", ": keep-alive\n" + spreadD + "\n\ndata: [DONE]"},
		{"named event, unframed object", "event: error\n" + spread},
		{"named event, one-line object", "event: error\ndata: " + flat},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := r87ErrorBody(t, "text/event-stream", tc.body, true)
			if status != 502 {
				t.Errorf("the %s spelling of one upstream error object answered HTTP %d, want 502 — an error object is not a turn (2026-09-29 audit, round 88, F88-L2-1)", tc.name, status)
			}
			if got := r87ErrMessage(t, body); got != want {
				t.Errorf("the %s spelling reached the client as %q, want %q — a field line frames the event, it is not part of what the event carries (2026-09-29 audit, round 88, F88-L2-1):\n%s",
					tc.name, got, want, body)
			}
		})
	}
}

// TestAFieldLineDoesNotCorruptAnUnframedCompletion is the other half of
// F88-L2-1: the whole-body fallback reads the lines that are not `data:` as one
// document, so a field line left in that buffer is not framing but garbage — the
// document stops parsing and a turn that exists is reported as one that never
// finished. Measured at HEAD with only the payload join fixed: an unframed whole
// completion under an `id:` line answered 502 and lost its text.
func TestAFieldLineDoesNotCorruptAnUnframedCompletion(t *testing.T) {
	const whole = `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":2}}`
	for _, tc := range []struct{ name, body string }{
		{"no field line at all", whole},
		{"under an id line", "id: 5\n" + whole},
		{"under an event line", "event: message\n" + whole},
		{"under a comment line", ": keep-alive\n" + whole},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := r87ErrorBody(t, "text/event-stream", tc.body, true)
			if status != 200 {
				t.Errorf("an unframed whole completion %s answered HTTP %d, want 200 — a field line frames an event, it is not part of the document the body carries (2026-09-29 audit, round 88, F88-L2-1):\n%s",
					tc.name, status, body)
			}
			if !strings.Contains(body, "hi") {
				t.Errorf("an unframed whole completion %s lost its text (2026-09-29 audit, round 88, F88-L2-1):\n%s", tc.name, body)
			}
		})
	}
}

// TestASecondStatementUnderASecondIdIsASecondCall is the F88-L2-2 pin: two calls
// the upstream stated under two ids are two calls on every spelling, and the
// restatements that reach the same client today — the same id, no id at all, the
// same id in the other spacing — stay one.
func TestASecondStatementUnderASecondIdIsASecondCall(t *testing.T) {
	const a1 = `{\"a\":1}`
	for _, tc := range []struct {
		name   string
		frames []string
		calls  int
	}{
		{"a fragment, then a fragment under a second id", []string{r88Frag("c1", "Bash", a1), r88Frag("c2", "Bash", a1)}, 2},
		{"a frame, then a frame under a second id", []string{r88Frame("c1", "Bash", a1), r88Frame("c2", "Bash", a1)}, 2},
		{"a fragment, then a frame under a second id", []string{r88Frag("c1", "Bash", a1), r88Frame("c2", "Bash", a1)}, 2},
		{"a frame, then a fragment under a second id", []string{r88Frame("c1", "Bash", a1), r88Frag("c2", "Bash", a1)}, 2},
		{"a frame, then the frame that restates it", []string{r88Frame("c1", "Bash", a1), r88Frame("c1", "Bash", a1)}, 1},
		{"a frame, then the same frame with no id", []string{r88Frame("", "Bash", a1), r88Frame("", "Bash", a1)}, 1},
		{"a frame, then it again in the other spacing", []string{r88Frame("c1", "Bash", a1), r88Frame("c1", "Bash", `{\"a\": 1}`)}, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			order, calls := r85Blocks(t, r85Body(t, append(tc.frames, r85CallsFin, r81Done)))
			if len(calls) != tc.calls {
				t.Errorf("%s handed the client %d calls %v, want %d — two ids are two calls, one id stated twice is one (2026-09-29 audit, round 88, F88-L2-2)",
					tc.name, len(calls), calls, tc.calls)
			}
			if want := "tool_use" + strings.Repeat(",tool_use", tc.calls-1); order != want {
				t.Errorf("%s reached the client as %q, want %q (2026-09-29 audit, round 88, F88-L2-2)", tc.name, order, want)
			}
		})
	}
}

// TestTheEmptinessSentenceDoesNotDependOnTheSpelling is the F88-L2-3 and
// F88-L2-4 pin. A stream that says nothing is refused by the same sentence
// whichever spelling carried the nothing, and the sentence turns on what the
// upstream STATED — a finish_reason is a statement, a [DONE] sentinel is not —
// not on whether a map happened to hold an entry.
func TestTheEmptinessSentenceDoesNotDependOnTheSpelling(t *testing.T) {
	const (
		nothingFrag = `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"","arguments":""}}]}}]}`
		stopFrame   = `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`
	)
	const (
		ended = "upstream stream ended before the response was complete"
		empty = "upstream returned an empty completion"
	)
	for _, tc := range []struct{ name, body, want string }{
		{"[DONE] alone", "data: [DONE]", ended},
		{"one opener that relays nothing, then [DONE]", nothingFrag + "\n\ndata: [DONE]", ended},
		{"two openers that relay nothing, then [DONE]", nothingFrag + "\n\n" + nothingFrag + "\n\ndata: [DONE]", ended},
		{"a finish_reason that carries no answer", stopFrame, empty},
		{"an opener that relays nothing, then that finish_reason", nothingFrag + "\n\n" + stopFrame, empty},
	} {
		t.Run(tc.name, func(t *testing.T) {
			status, body := r87ErrorBody(t, "text/event-stream", tc.body, true)
			if status != 502 {
				t.Errorf("%s answered HTTP %d, want 502 — a stream that says nothing is not a turn (2026-09-29 audit, round 88, F88-L2-3)", tc.name, status)
			}
			if got := r87ErrMessage(t, body); got != tc.want {
				t.Errorf("%s reached the client as %q, want %q — the sentence turns on what the upstream stated, not on the spelling that carried the silence (2026-09-29 audit, round 88, F88-L2-3):\n%s",
					tc.name, got, tc.want, body)
			}
		})
	}

	// F88-L2-4: the body with no choices at all, refused by both arms in the
	// same words.
	for name, upstream := range map[string]string{
		"a body with no choices":      `{"detail":"Not Found"}`,
		"a bare object":               `{}`,
		"a health payload":            `{"status":"ok"}`,
		"an empty choices array":      `{"id":"x","choices":[],"usage":{"prompt_tokens":123}}`,
		"a choice with nothing in it": `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			for _, arm := range []struct {
				name, ct string
				stream   bool
			}{{"buffered", "application/json", false}, {"streamed", "text/event-stream", true}} {
				status, body := r87ErrorBody(t, arm.ct, upstream, arm.stream)
				if status != 502 {
					t.Errorf("the %s arm answered the %s %d, want 502 (2026-09-29 audit, round 88, F88-L2-4)", arm.name, name, status)
				}
				if got := r87ErrMessage(t, body); got != empty {
					t.Errorf("the %s arm refused the %s as %q, want %q — one body that carries no answer is one sentence, and which arm met it must not change the words (2026-09-29 audit, round 88, F88-L2-4):\n%s",
						arm.name, name, got, empty, body)
				}
			}
		})
	}
}
