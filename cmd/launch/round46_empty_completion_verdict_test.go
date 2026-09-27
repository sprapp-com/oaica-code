package launch

// round46_empty_completion_verdict_test.go — round 46's two findings on this
// leg's emptiness verdicts, A46-2 and A46-3.
//
// A46-2: the non-stream refusal of a completion that "says nothing" read the
// content with strings.TrimSpace, so an upstream that answered with a space or
// a newline was refused with 502 — while the gateway leg's documentSays
// something, this leg's own streaming path (relayDelta relays a content of
// whitespace as a text block) and the local server all answer the same body
// with a turn carrying that text. A text is content whatever it says.
//
// A46-3: the framed path adopted a whole completion that arrived inside a
// `data:` frame, and an EMPTY one fell through to the delta loop, whose
// finish_reason completed an empty 200 turn — while the unframed twin of the
// same document and the gateway leg both refuse it with 502. The verdict must
// not depend on which shape the upstream chose.

import (
	"net/http"
	"strings"
	"testing"
)

// TestAWhitespaceCompletionIsNotAnEmptyOne is A46-2.
func TestAWhitespaceCompletionIsNotAnEmptyOne(t *testing.T) {
	for _, tc := range []struct{ label, content string }{
		{"space", " "},
		{"newline", `\n`},
	} {
		up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"`+tc.content+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":1,"total_tokens":12}}`)
		proxy := startCalibProxy(t, up.URL, "sess-r46-ws-completion")
		body, status := postMessagesRaw(t, proxy, false)
		up.Close()
		if status != http.StatusOK {
			t.Errorf("%s: status %d, want 200 — a text is content whatever it says, and the gateway leg, the local server and this leg's own stream path all relay it: %s", tc.label, status, body)
		}
	}
}

// TestAFramedEmptyCompletionIsRefusedLikeTheUnframedOne is A46-3.
func TestAFramedEmptyCompletionIsRefusedLikeTheUnframedOne(t *testing.T) {
	frames := "data: " + `{"choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":""}}]}` + "\n\ndata: [DONE]\n\n"
	up := streamUpstream(t, frames, true)
	proxy := startCalibProxy(t, up.URL, "sess-r46-framed-empty")
	body, status := postMessagesStream(t, proxy)
	up.Close()

	if status == http.StatusOK && !strings.Contains(body, `"error"`) {
		t.Errorf("a whole completion that arrived in a data: frame and says nothing was relayed as an empty 200 turn; the unframed twin and the gateway leg both refuse it with 502:\n%s", body)
	}
	for _, want := range []string{`"tool_use"`, `"text"`} {
		if strings.Contains(body, want) {
			t.Errorf("the refused turn carried %s anyway:\n%s", want, body)
		}
	}
}
