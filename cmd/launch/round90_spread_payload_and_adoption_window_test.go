package launch

// round90_spread_payload_and_adoption_window_test.go — leg 2 (2026-09-29
// audit, round 90). Four defects, one doctrine: one upstream body is one
// answer, whatever spelling carries it and whatever the client asked for.
//
//   - F90-L2-1: a turn whose payload is SPREAD over several `data:` lines was
//     not read as a turn. The proxy joined the payload lines of one event only
//     when it was answering an error, and the frame reader looked at one line
//     at a time, so `data: {` / `data: "id":…` / `data: }` was three malformed
//     frames and the body was served as a truncated stream.
//
//   - F90-L2-2: one choice stating the same content in BOTH `message` and
//     `delta` had it relayed twice — the two shapes are read by one struct and
//     the adopted whole-completion branch did not stop the adopted frame's
//     deltas from being relayed on top of the blocks it had just written.
//
//   - F90-L2-4: a whole completion that arrives BARE (no `data:` prefix) and is
//     then closed by a framed finish chunk was not adopted at all: the tail
//     adoption was gated on the turn never having completed, and the framed
//     chunk had already completed it. The same document framed as a chunk WAS
//     adopted, so one document had two answers.
//
//   - F90-L2-3: a body that is not JSON was refused with two different
//     sentences depending on whether the client asked for a stream — the
//     streamed arm said "upstream stream ended before the response was
//     complete" where the buffered arm named the decode failure. One body, one
//     cause.
//
// RECORDED, not fixed:
//
//   - R90-L2-3 residual: a FRAMED body that is not JSON still reads "upstream
//     stream ended before the response was complete" on the streamed arm. Its
//     bytes have no buffered twin — the client leg never buffers a frame body
//     it read frame by frame, and the frame's payload is what failed, not the
//     document — so the sentence is this arm's own reading of its own wire.
//
//   - R90-L2-5: a FRAMED upstream body answered to a NON-STREAMING client is
//     decoded as a raw document and refused with a decode sentence
//     ("… invalid character 'd' looking for beginning of value"), where the
//     streamed client reads "upstream returned an empty completion". No live
//     producer: the proxy mirrors the client's flag upstream (`Stream:
//     anthropicReq.Stream`), so a client that asks for a buffered answer gets a
//     buffered upstream request and never meets an SSE body. Recorded with the
//     measurement in the pin below, like F68-L1-1 and F90-L1-2, so that a later
//     attempt to close it must confront the measurement rather than the note.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r90Arm relays one fixed upstream body through the real proxy and reports the
// verdict plus what the client was handed: the block order, the joined text,
// and the usage the client was told (or the refusal sentence).
func r90Arm(t *testing.T, ct, upstream string, clientStream bool) (int, string, string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstream)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", clientStream)

	var order []string
	var text, usage, errMsg string
	if clientStream {
		for _, line := range strings.Split(body, "\n") {
			raw, ok := strings.CutPrefix(line, "data: ")
			if !ok {
				continue
			}
			var ev struct {
				Type         string `json:"type"`
				ContentBlock struct {
					Type string `json:"type"`
				} `json:"content_block"`
				Delta struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"delta"`
				Usage json.RawMessage `json:"usage"`
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if json.Unmarshal([]byte(raw), &ev) != nil {
				continue
			}
			switch ev.Type {
			case "content_block_start":
				order = append(order, ev.ContentBlock.Type)
			case "content_block_delta":
				if ev.Delta.Type == "text_delta" {
					text += ev.Delta.Text
				}
			case "message_delta":
				usage = string(ev.Usage)
			case "error":
				errMsg = ev.Error.Message
			}
		}
	} else {
		var msg struct {
			Type    string `json:"type"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
			Usage json.RawMessage `json:"usage"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		_ = json.Unmarshal([]byte(body), &msg)
		if msg.Type == "error" {
			errMsg = msg.Error.Message
		}
		for _, b := range msg.Content {
			order = append(order, b.Type)
			if b.Type == "text" {
				text += b.Text
			}
		}
		usage = string(msg.Usage)
	}
	if errMsg == "" {
		var env struct {
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(body), &env) == nil {
			errMsg = env.Error.Message
		}
	}
	summary := `blocks=` + strings.Join(order, ",") + ` text="` + text + `"`
	if usage != "" {
		summary += ` usage=` + usage
	}
	if errMsg != "" {
		summary += ` err=` + errMsg
	}
	return code, summary, body
}

// oneTurn is the whole document this file's tests vary the SPELLING of: the
// prose "hi" and one call, finished, with the upstream's usage.
const r90OneTurn = `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi","tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":8,"completion_tokens":3}}`

func TestASpreadPayloadIsOneTurn(t *testing.T) {
	spread := "data: {\ndata: \"id\":\"x\",\"choices\":[{\"index\":0,\"message\":{\"role\":\"assistant\",\"content\":\"hi\"},\"finish_reason\":\"stop\"}]\ndata: }"
	one := `data: {"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`

	_, first, _ := r90Arm(t, "text/event-stream", one+"\n\n"+"data: [DONE]\n\n", true)
	_, second, _ := r90Arm(t, "text/event-stream", spread+"\n\n"+"data: [DONE]\n\n", true)
	if first != second {
		t.Errorf("one event, two spellings, two answers: one line %s, spread %s — an SSE event's payload is the join of its `data:` lines, and a body written that way is the same turn (2026-09-29 audit, round 90, F90-L2-1)", first, second)
	}
	if !strings.Contains(first, `text="hi"`) {
		t.Errorf("a whole completion on one `data:` line is %s: the turn was not served (2026-09-29 audit, round 90, F90-L2-1)", first)
	}

	// The same body with no framing at all, which is how an upstream that
	// ignores `stream` answers: one line, a whole document.
	_, bare, _ := r90Arm(t, "application/json", r90OneTurn, true)
	_, barePlain, _ := r90Arm(t, "application/json", r90OneTurn, false)
	if bare != barePlain {
		t.Errorf("one bare document, two client arms, two answers: %s and %s", bare, barePlain)
	}
	if !strings.Contains(bare, `blocks=text,tool_use`) {
		t.Errorf("a bare whole completion is %s, want its prose and its call (2026-09-29 audit, round 90, F90-L2-1)", bare)
	}
}

func TestAContentStatedTwiceIsRelayedOnce(t *testing.T) {
	msgOnly := `data: {"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`
	deltaOnly := `data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"finish_reason":"stop"}]}`
	both := `data: {"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"delta":{"content":"hi"},"finish_reason":"stop"}]}`

	_, a, _ := r90Arm(t, "text/event-stream", msgOnly+"\n\n"+"data: [DONE]\n\n", true)
	_, b, _ := r90Arm(t, "text/event-stream", deltaOnly+"\n\n"+"data: [DONE]\n\n", true)
	_, c, _ := r90Arm(t, "text/event-stream", both+"\n\n"+"data: [DONE]\n\n", true)
	for _, arm := range []struct{ name, got string }{{"message only", a}, {"delta only", b}, {"both", c}} {
		if !strings.Contains(arm.got, `text="hi"`) {
			t.Errorf("%s is %s: a choice may state its content in `message` or in `delta`, and the two are one shape read by one struct — a choice that states it in both stated it ONCE (2026-09-29 audit, round 90, F90-L2-2)", arm.name, arm.got)
		}
	}
	if b != c {
		t.Errorf("the same content on the delta-only wire is %s and on the message+delta wire %s — the extra field is the same content written twice, not a second copy of the turn (2026-09-29 audit, round 90, F90-L2-2)", b, c)
	}

	// The same doubled choice as a bare document: the arm the client did not ask
	// to stream reads it too, and one document cannot be two texts.
	_, d, _ := r90Arm(t, "application/json", `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"delta":{"content":"hi"},"finish_reason":"stop"}]}`, true)
	if !strings.Contains(d, `text="hi"`) || strings.Contains(d, "hihi") {
		t.Errorf("a bare doubled choice is %s, want the content once (2026-09-29 audit, round 90, F90-L2-2)", d)
	}
}

func TestABareDocumentClosedByAFramedFinishIsStillAdopted(t *testing.T) {
	fin := `data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	want := `blocks=text,tool_use text="hi" usage={"input_tokens":8,"output_tokens":3}`

	// The framing of each half is not part of the turn: the document is the
	// answer and the finish chunk only names when it stopped.
	for _, tc := range []struct{ name, body string }{
		{"doc bare, then framed finish", r90OneTurn + "\n\n" + fin + "\n\n"},
		{"doc framed, then framed finish", "data: " + r90OneTurn + "\n\n" + fin + "\n\n"},
		{"doc bare, then [DONE]", r90OneTurn + "\n\n" + "data: [DONE]\n\n"},
		{"doc bare alone", r90OneTurn + "\n"},
		{"framed finish, then doc bare", fin + "\n\n" + r90OneTurn + "\n\n"},
	} {
		code, got, _ := r90Arm(t, "text/event-stream", tc.body, true)
		if code != http.StatusOK {
			t.Errorf("%s: answered %d, want 200 — a whole completion this bridge can read is the turn (2026-09-29 audit, round 90, F90-L2-4)\n%s", tc.name, code, got)
			continue
		}
		if !strings.Contains(got, want) {
			t.Errorf("%s: %s\nwant %s — the tail adoption asked whether the turn had not completed, and a framed finish chunk had already completed it, so one document answered two ways depending on whether it arrived bare or framed (2026-09-29 audit, round 90, F90-L2-4)", tc.name, got, want)
		}
	}
}

func TestANonAnswerStatesOneCauseOnEveryArm(t *testing.T) {
	// A body with no choices states the completion cause on all three arms.
	for _, tc := range []struct{ name, ct, body string }{
		{"framed, streamed client", "text/event-stream", `data: {"id":"x","choices":[]}` + "\n\n" + "data: [DONE]\n\n"},
		{"bare, streamed client", "text/event-stream", `{"id":"x","choices":[]}`},
		{"bare, buffered client", "application/json", `{"id":"x","choices":[]}`},
	} {
		streamed := strings.Contains(tc.name, "streamed")
		code, got, _ := r90Arm(t, tc.ct, tc.body, streamed)
		if code != http.StatusBadGateway {
			t.Errorf("%s: answered %d, want 502", tc.name, code)
		}
		if !strings.Contains(got, "upstream returned an empty completion") {
			t.Errorf("%s: %s\none body with no choices states one cause, whichever arm reads it (2026-09-29 audit, round 90, F90-L2-3)", tc.name, got)
		}
	}

	// A body that is not JSON at all: the two client arms read the same bytes
	// and must name the same failure.
	_, streamed, _ := r90Arm(t, "text/event-stream", "not json at all", true)
	_, buffered, _ := r90Arm(t, "application/json", "not json at all", false)
	if !strings.Contains(streamed, "decode upstream response:") || !strings.Contains(buffered, "decode upstream response:") {
		t.Errorf("a body that is not JSON: the streamed client read %s and the buffered one %s — both hold the same bytes, so both name the decode failure that refused them (2026-09-29 audit, round 90, F90-L2-3)", streamed, buffered)
	}

	// RECORDED, held here so a later fix has to confront the measurement:
	//
	//   * a FRAMED body that is not JSON has no buffered twin — the sentence is
	//     the streamed arm's own reading of its own wire (R90-L2-3).
	_, framedBad, _ := r90Arm(t, "text/event-stream", "data: not json at all\n\n", true)
	if !strings.Contains(framedBad, "upstream stream ended before the response was complete") {
		t.Errorf("a framed non-JSON body reads %s, want the stream arm's own truncation sentence: this is R90-L2-3, recorded — if it changed, the note in this file must be rewritten rather than the assertion loosened", framedBad)
	}
	//   * a FRAMED body answered to a NON-streaming client is decoded as a raw
	//     document. No live producer: the proxy mirrors the client's flag
	//     upstream, so this pairing is unreachable from this code (R90-L2-5).
	_, framedPlain, _ := r90Arm(t, "text/event-stream", `data: {"id":"x","choices":[]}`+"\n\n"+"data: [DONE]\n\n", false)
	if !strings.Contains(framedPlain, "decode upstream response: invalid character 'd'") {
		t.Errorf("a framed body answered to a non-streaming client reads %s, want the recorded decode sentence of R90-L2-5 — the proxy mirrors the client's `stream` flag upstream, so this pairing has no live producer and the measurement is what is held here", framedPlain)
	}
}
