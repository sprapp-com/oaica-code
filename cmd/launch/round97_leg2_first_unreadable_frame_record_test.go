package launch

// round97_leg2_first_unreadable_frame_record_test.go — leg 2, F97-L2-3,
// RECORDED (2026-09-29 audit, round 97).
//
// The streamed arm remembers the FIRST frame it could not read as a chunk, and
// asks that one frame for a cause at the end. A frame can be unreadable as a
// chunk while the DOCUMENT decoder accepts it, and such a frame can name no
// cause (unreadFrameCause asks the document decoder, which returns "" for it) —
// so a later frame whose bytes the document decoder really does reject is not
// asked, and the stream states the earlier, emptier reading. Measured:
//
//	[f1 {"delta":{"content":[1]}}, f2 message.content an array]  -> 502
//	    "upstream returned an empty completion"      (f1's reading)
//	[f2, f1]                                                     -> 502
//	    "decode upstream response: … .choices.message.content …"
//	the buffered f2 alone                                        -> 502 decode detail
//
// It is recorded, not fixed:
//
//   - no live producer. Two frames are needed, the first unreadable as a chunk
//     with the document reader accepting it, the second a whole completion the
//     document reader rejects; and the first must come first, since the
//     reversed stream (also measured above) already states the decode detail.
//     Neither round-97 auditor nor this pin could build an upstream that emits
//     the pair; this pin states the frames by hand.
//   - the reading this round CHANGED is the sentence, not the divergence: with
//     F97-L2-2's fix the composite stream now states the buffered arm's reading
//     of its first frame ("upstream returned an empty completion") where it
//     used to say the stream "ended before the response was complete" — a cause
//     that happened in neither frame.
//   - whether the LAST unreadable frame's cause should win is a decision about
//     what a stream of unreadable frames IS, not a decoding defect: the
//     first-frame rule is deliberate (it holds the frame the round-96 fix
//     exists for), and a later round picks a different frame only by saying so.
//
// The pin is the reading itself: if a later round decides the later frame's
// cause must win, this test fails and the decision must be taken deliberately.

import (
	"strings"
	"testing"
)

func TestTheCompositeUnreadableStreamStatesItsFirstFramesReading(t *testing.T) {
	f1 := `{"id":"x","choices":[{"index":0,"delta":{"content":[1]},"finish_reason":null}]}`
	f2 := `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"finish_reason":"stop"}]}`

	const firstFramesReading = "upstream returned an empty completion"
	const decodeDetail = "decode upstream response"

	for _, tc := range []struct {
		name   string
		stream string
		want   string
	}{
		{
			name:   "the unreadable-as-a-chunk frame first",
			stream: "data: " + f1 + "\n\ndata: " + f2 + "\n\ndata: [DONE]\n\n",
			want:   firstFramesReading,
		},
		{
			name:   "the nameable frame first",
			stream: "data: " + f2 + "\n\ndata: " + f1 + "\n\ndata: [DONE]\n\n",
			want:   decodeDetail,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, body := r96Arm(t, "text/event-stream", tc.stream, true)
			if c != 502 {
				t.Fatalf("answered %d, want 502 (%q)", c, body)
			}
			got := r96ErrorOf(t, body)
			if !strings.Contains(got, tc.want) {
				t.Errorf("states %q, want it to state %q (2026-09-29 audit, round 97, F97-L2-3 — recorded, see this file's header)",
					got, tc.want)
			}
			t.Logf("%s -> %q", tc.name, got)
		})
	}
}
