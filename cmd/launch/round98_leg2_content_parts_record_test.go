package launch

// round98_leg2_content_parts_record_test.go — leg 2, round 98 (2026-09-29
// audit), RECORD for F98-L3-3's cross-leg half.
//
// An upstream that states content as an ARRAY OF PARTS is refused by every arm
// of both legs, and each spelling of a body agrees with itself:
//
//	body: a document whose message states content as an array of parts
//	  buffered              502 decode upstream response: json: cannot unmarshal
//	                            array into Go struct field .choices.message.content
//	                            of type string
//	  a whole completion in a frame   502 the same decode detail
//	  the gateway's three arms        502 unparseable upstream response
//
//	body: a choice whose `delta` states content as an array of parts
//	  framed (delta frames) 502 upstream returned an empty completion
//	  the same bytes as a document    502 upstream returned an empty completion
//
// The second body's document spelling states no turn at all — the document
// reader knows no `delta` field — so the emptiness sentence is the one round
// 97's F97-L2-2 pinned for exactly this shape (a payload this reader cannot read
// as a chunk and the document reader accepts). No producer of the array
// spelling exists in this tree: this leg writes content as a string on its own
// request path, and the catalogue's providers are described by the shapes they
// are known to state. RECORDED rather than fixed, so the day a producer appears
// the refusal is a stated decision and not a discovery.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestTheContentPartsSpellingIsRefusedOnEveryArm records the readings above.
func TestTheContentPartsSpellingIsRefusedOnEveryArm(t *testing.T) {
	doc := `{"id":"chatcmpl-r98","model":"glm-5.3","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
	deltaFrame := `{"id":"c","choices":[{"index":0,"delta":{"content":[{"type":"text","text":"hi"}]},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
	const detail = "decode upstream response: json: cannot unmarshal array into Go struct field .choices.message.content of type string"

	for _, arm := range []struct {
		name, ct, body, want string
		stream               bool
	}{
		{"a document stating parts", "application/json", doc, detail, false},
		{"a whole completion in a frame", "text/event-stream", "data: " + doc + "\n\n", detail, true},
		{"delta frames stating parts", "text/event-stream", "data: " + deltaFrame + "\n\ndata: [DONE]\n\n", "upstream returned an empty completion", true},
		{"those delta frames as a document", "application/json", deltaFrame, "upstream returned an empty completion", false},
	} {
		code, body := r98PartsAsk(t, arm.ct, arm.body, arm.stream)
		if code != http.StatusBadGateway {
			t.Errorf("%s answered %d, want the 502 every arm of this leg states for a body it cannot serve (2026-09-29 audit, round 98, F98-L3-3, recorded):\n%s",
				arm.name, code, body)
			continue
		}
		if !strings.Contains(body, arm.want) {
			t.Errorf("%s named a cause other than %q (2026-09-29 audit, round 98, F98-L3-3 — one body, one cause):\n%s",
				arm.name, arm.want, body)
		}
	}
}

// r98PartsAsk runs one fixed upstream body down this leg's buffered or streamed
// arm and reports the status and the client's body.
func r98PartsAsk(t *testing.T, ct, upstreamBody string, stream bool) (int, string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", ct)
		_, _ = io.WriteString(w, upstreamBody)
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	return round54PostMessage(t, proxy, "glm-5.3", stream)
}
