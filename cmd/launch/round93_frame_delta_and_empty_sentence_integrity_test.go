package launch

// round93_frame_delta_and_empty_sentence_integrity_test.go — leg 2, F93-L2-1a
// and F93-L2-1b (2026-09-29 audit, round 93).
//
// One root cause, two measured faces. A frame that `frameCarriesWholeCompletion`
// accepts is a WHOLE COMPLETION, and this leg's unframed arm reads a whole
// completion for its message and nothing else — `openAIResponseToChatCompletion`
// decodes `message` and never looks at a `delta` field (the same reading round
// 90's F90-L2-2 pinned for the adoption branch). A frame that carried a
// `message` and a `delta` and was taken by NEITHER the fold nor the adoption —
// a message that states nothing, with the turn sitting in the delta — fell
// through to the delta reader and was relayed, while every other spelling of the
// same body was refused.
//
//   - F93-L2-1a: the frame answered 200 with the delta's prose (or its call),
//     where the byte-identical document buffered, the same body sent whole to a
//     streaming client, and the same body with no `data:` prefix were all 502
//     "upstream returned an empty completion". Measured on this leg before the
//     fix: framed `[data: {"message":{"role":"assistant"},"delta":{"content":
//     "hi"}}][data: [DONE]]` → 200 text="hi"; all four other spellings → 502.
//     The frame's delta is not this arm's to relay, so a whole-completion frame
//     neither branch took has its delta blanked before the reader below — which
//     still reads the finish_reason the frame states, a stream-level fact that
//     must end the turn.
//   - F93-L2-1b: the sentence. A frame that parsed as a whole completion and
//     stated nothing, cleanly terminated by `[DONE]`, was answered "upstream
//     stream ended before the response was complete" — the cause of a stream
//     that DIED, not of a body that was empty — while the same document
//     buffered, whole-bodied and unframed all said "upstream returned an empty
//     completion". One cause, one sentence, whichever arm met it.
//
// Two halves, each reverted on its own and each went behaviourally red: the
// blanking (framed arm 200 again for both the prose and the call spelling) and
// the sentence rescue (framed arm "ended before the response was complete"
// again). The `frameReadAsDelta` flag that keeps the fold's own construction
// from being blanked, and the hoist of `frameCarriesWholeCompletion` into
// `wholeCompletionFrame`, measure inert on their own — they exist only to let
// the blanking ask its question once; they are dropped with it, so they are not
// separate halves.
//
// F93-L2-1a has no live producer in tree: `openai.ChunkChoice` has no `Message`
// field, so this repo's own stream producer cannot emit a choice carrying
// `message` at all, and the only whole-completion emitter is the non-stream path
// — exactly the shape every non-framed arm reads consistently. A third-party
// upstream mirroring its message into a streamed chunk produces the POPULATED
// form, which round 90's F90-L2-2 already settled. The finding is fixed rather
// than recorded because the divergence is on the arm invariant itself: one body,
// two spellings, two verdicts.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r93Arm drives one upstream body under one arm: the client asking to buffer
// with a `application/json` body, the client asking to stream with the same
// whole body (framed or not), or an SSE script.
func r93Arm(t *testing.T, body, contentType string, clientStream bool) (int, string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", contentType)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, body)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	return round54PostMessage(t, proxy, "glm-5.3", clientStream)
}

// r93Frame wraps a document as one SSE data frame.
func r93Frame(doc string) string { return "data: " + doc }

// r93Sentence is the error sentence a refusal named.
func r93Sentence(t *testing.T, body string) string {
	t.Helper()
	var doc struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("the refusal was not a JSON error: %q", body)
	}
	return doc.Error.Message
}

// r93Agrees drives one document under all five spellings of the same request
// and reports the first arm that answered differently from the rest.
func r93Agrees(t *testing.T, doc string) (code int, sentence string) {
	t.Helper()
	arms := []struct {
		name        string
		body        string
		contentType string
		stream      bool
	}{
		{"buffered (stream:false)", doc, "application/json", false},
		{"whole body (stream:true)", doc, "application/json", true},
		{"one data: frame", r93Frame(doc) + "\n\ndata: [DONE]\n\n", "text/event-stream", true},
		{"one data: frame, no [DONE]", r93Frame(doc) + "\n\n", "text/event-stream", true},
		{"unframed, sse content-type", doc, "text/event-stream", true},
	}
	var wantCode int
	var wantSentence string
	for i, a := range arms {
		code, body := r93Arm(t, a.body, a.contentType, a.stream)
		if code == 200 {
			t.Errorf("%s: answered 200 with %q — the body says nothing an answer can be made of", a.name, r85Text(t, body))
			return code, ""
		}
		sentence := r93Sentence(t, body)
		if i == 0 {
			wantCode, wantSentence = code, sentence
			continue
		}
		if code != wantCode || sentence != wantSentence {
			t.Errorf("%s: %d %q, want the buffered arm's %d %q — one body, one cause, one sentence, whichever arm met it (2026-09-29 audit, round 93)", a.name, code, sentence, wantCode, wantSentence)
			return code, sentence
		}
	}
	return wantCode, wantSentence
}

// A frame whose message states nothing does not get its `delta` relayed: the
// turn is the empty completion every other spelling of the same body is.
func TestAWholeFrameStatesItsMessageNotItsDelta(t *testing.T) {
	prose := `{"id":"c","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant"},"delta":{"content":"hi"}}]}`
	call := `{"id":"c","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant"},"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`
	for _, tc := range []struct{ name, doc string }{
		{"the turn is in the frame's delta as prose", prose},
		{"the turn is in the frame's delta as a call", call},
	} {
		code, sentence := r93Agrees(t, tc.doc)
		if sentence != "upstream returned an empty completion" {
			t.Errorf("%s: refused %d %q, want \"upstream returned an empty completion\" (2026-09-29 audit, round 93, F93-L2-1a)", tc.name, code, sentence)
		}
	}
}

// A stream that stated nothing but whole completions is named the way the
// buffered arm names the same document — the `[DONE]` behind it does not turn
// an empty body into a stream that died.
func TestAnEmptyWholeFrameIsNamedAnEmptyCompletion(t *testing.T) {
	for _, tc := range []struct{ name, doc string }{
		{"a bare role", `{"id":"c","choices":[{"index":0,"message":{"role":"assistant"}}]}`},
		{"an empty content", `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":""}}]}`},
		{"an empty content and a stop", `{"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":""},"finish_reason":"stop"}]}`},
	} {
		code, sentence := r93Agrees(t, tc.doc)
		if sentence != "upstream returned an empty completion" {
			t.Errorf("%s: refused %d %q, want \"upstream returned an empty completion\" — the same document buffered says so (2026-09-29 audit, round 93, F93-L2-1b)", tc.name, code, sentence)
		}
	}
}

// And the frame that DOES state a message still reaches the client whole, with
// the delta beside it read as little as the unframed arm reads it.
func TestAFrameThatStatesAMessageStillEndsTheTurn(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
	}{
		{"a message beside a delta", `{"id":"c","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"hi"},"delta":{"content":"X"}}]}`},
		{"a message beside a call", `{"id":"c","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","content":"hi"},"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`},
	} {
		code, body := r93Arm(t, r93Frame(tc.doc)+"\n\ndata: [DONE]\n\n", "text/event-stream", true)
		if code != 200 {
			t.Errorf("%s: answered %d, want 200 — the frame states a message (2026-09-29 audit, round 93)", tc.name, code)
			continue
		}
		if got := r85Text(t, body); got != "hi" {
			t.Errorf("%s: the client was handed text=%q, want %q — the frame is read for its message, and the delta beside it is not this arm's", tc.name, got, "hi")
		}
	}
}

// And the round-85 property stands on the fold arm: a whole frame arriving
// after the stream has written still relays the prose and the call it states.
func TestTheFoldStillRelaysWhatTheFrameStates(t *testing.T) {
	frames := []string{r85TextDelta, r85WholeFrame("hi"), r85CallsFin, r81Done}
	order, calls := r85Blocks(t, r85Body(t, frames))
	if order != "text,tool_use" {
		t.Errorf("the turn relayed blocks %q, want %q — a whole frame still states the call it carries (round 85, R85-L2-1)", order, "text,tool_use")
	}
	if strings.Join(calls, " ") != "Read/c2" {
		t.Errorf("the turn relayed calls %v, want [Read/c2]", calls)
	}
	if got := r85Text(t, r85Body(t, frames)); got != "hi" {
		t.Errorf("the client was handed text=%q, want %q — the prose is said once", got, "hi")
	}
}
