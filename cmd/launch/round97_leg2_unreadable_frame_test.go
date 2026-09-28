package launch

// round97_leg2_unreadable_frame_test.go — leg 2, round 97 (2026-09-29 audit).
//
// F97-L2-1 — a frame stating a whole completion in a spelling this leg's
// DOCUMENT decoder rejects, beside a `delta` the chunk reader accepts, was
// served as an ordinary delta frame. Same bytes:
//
//	buffered (application/json)  502 "decode upstream response: json: cannot
//	                             unmarshal array into Go struct field
//	                             .choices.message.content of type string"
//	framed (data: …)             200 with the delta's prose — or with the delta's
//	                             tool call as a RUNNABLE tool_use
//
// `frameCarriesWholeCompletion` asked the same decoder that had just rejected
// the message, so the frame was not a whole-completion frame at all: the
// delta-blanking below never applied (round 93's F93-L2-1a) and the delta loop
// relayed the `delta` beside the unreadable `message`. A frame that states a
// `message` the document decoder cannot read IS that document — the shape round
// 96's F96-L2-1 taught this leg to name — so it is now a whole-completion frame,
// its delta is not this arm's to relay, and every spelling of the body answers
// the buffered arm's decode detail.
//
// F97-L2-2 — a frame the chunk reader rejects while the DOCUMENT reader accepts
// it stated a cause that did not happen:
//
//	buffered                      502 "upstream returned an empty completion"
//	unframed, stream=true         502 "upstream returned an empty completion"
//	framed                        502 "upstream stream ended before the response
//	                                 was complete"
//
// The chunk-decode failure branch leaves the frame to the tail, and the tail's
// last word asks `unreadFrameCause` — "does this payload decode as a document?"
// — which for these bytes says yes and returns "". The upstream closed the
// stream; nothing ended early. The frame is now recorded as what it is: bytes
// this reader cannot read as a chunk and the document reader reads as a choice
// with no turn in it, the buffered arm's own reading of the same body.

import (
	"strings"
	"testing"
)

// One body whose `message` the document decoder rejects, spelled inside a frame
// beside a delta the chunk reader accepts: every spelling of it states the
// decode detail, on both arms, and the delta is never relayed.
func TestAWholeCompletionThisDecoderRejectsIsNotRelayedAsItsDelta(t *testing.T) {
	for _, tc := range []struct {
		name string
		doc  string
		leak string // a fragment the framed arm must NOT serve
	}{
		{
			name: "the delta beside it carries prose",
			doc:  `{"id":"x","choices":[{"index":0,"delta":{"content":"hi"},"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"finish_reason":"stop"}]}`,
			leak: `"text":"hi"`,
		},
		{
			name: "the delta beside it carries a tool call",
			doc:  `{"id":"x","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]},"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"finish_reason":"tool_calls"}]}`,
			leak: `"name":"Bash"`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bc, bb := r96Arm(t, "application/json", tc.doc, false)
			fc, fb := r96Arm(t, "text/event-stream", "data: "+tc.doc+"\n\ndata: [DONE]\n\n", true)
			if bc != 502 || fc != 502 {
				t.Fatalf("one unreadable body: buffered %d, framed %d, want 502 on both\n  buffered %s\n  framed   %s",
					bc, fc, bb, fb)
			}
			bMsg, fMsg := r96ErrorOf(t, bb), r96ErrorOf(t, fb)
			if !strings.Contains(bMsg, "message.content") {
				t.Errorf("the buffered arm states %q, want the decode detail naming message.content", bMsg)
			}
			if fMsg != bMsg {
				t.Errorf("one body, two causes (2026-09-29 audit, round 97, F97-L2-1):\n  buffered %q\n  framed   %q", bMsg, fMsg)
			}
			if strings.Contains(fb, tc.leak) {
				t.Errorf("the framed arm relayed what the document arm refuses to read (2026-09-29 audit, round 97, F97-L2-1): the frame's delta reached the client as %s:\n%s",
					tc.leak, fb)
			}
			t.Logf("buffered %d | framed %d, both %q", bc, fc, fMsg)
		})
	}
}

// One body this reader cannot read as a chunk and the document reader reads as
// an empty turn states the buffered arm's cause, whichever spelling it arrives
// in.
func TestAFrameTheChunkReaderRejectsStatesTheBufferedArmsCause(t *testing.T) {
	doc := `{"id":"x","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hi"}]},"finish_reason":"stop"}]}`
	const bufferedSays = "upstream returned an empty completion"

	bc, bb := r96Arm(t, "application/json", doc, false)
	jc, jb := r96Arm(t, "application/json", doc, true)
	fc, fb := r96Arm(t, "text/event-stream", "data: "+doc+"\n\ndata: [DONE]\n\n", true)
	if bc != 502 || jc != 502 || fc != 502 {
		t.Fatalf("one unreadable-as-a-chunk body: buffered %d, unframed streamed %d, framed %d, want 502 on all three", bc, jc, fc)
	}
	if got := r96ErrorOf(t, bb); got != bufferedSays {
		t.Errorf("the buffered arm states %q, want %q — the premise moved", got, bufferedSays)
	}
	for _, arm := range []struct {
		name string
		body string
	}{{"unframed, streamed", jb}, {"framed", fb}} {
		if got := r96ErrorOf(t, arm.body); got != bufferedSays {
			t.Errorf("%s states %q, want the buffered arm's %q (2026-09-29 audit, round 97, F97-L2-2):\n%s",
				arm.name, got, bufferedSays, arm.body)
		}
	}
	t.Logf("buffered %q | unframed %q | framed %q", r96ErrorOf(t, bb), r96ErrorOf(t, jb), r96ErrorOf(t, fb))
}

// The control: an ordinary delta stream is still an ordinary delta stream. The
// gate this round widened asks about a `message` the document decoder rejects,
// and a delta frame states none.
func TestAnOrdinaryDeltaStreamIsStillServed(t *testing.T) {
	stream := "data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n" +
		"data: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\" there\"},\"finish_reason\":\"stop\"}]}\n\n" +
		"data: [DONE]\n\n"
	if c, body := r96Arm(t, "text/event-stream", stream, true); c != 200 ||
		!strings.Contains(body, `"text":"hi"`) || !strings.Contains(body, `"text":" there"`) {
		t.Errorf("an ordinary delta stream answered %d and %q, want 200 with both deltas of the turn (2026-09-29 audit, round 97, F97-L2-1 control):\n%s",
			c, body, body)
	}
}
