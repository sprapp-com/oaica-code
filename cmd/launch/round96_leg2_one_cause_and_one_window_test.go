package launch

// round96_leg2_one_cause_and_one_window_test.go — leg 2, round 96 (2026-09-29
// audit).
//
// Two findings, one file, because both are the same rule stated twice: a body
// means one thing, and the arm that meets it must say so.
//
// F96-L2-1 — the streamed arm named a cause that did not happen for a body its
// own decoders reject, while the buffered arm named the decode detail. One
// upstream body reached a streaming client as "upstream stream ended before the
// response was complete" (the stream ended fine; the body did not decode) or as
// "upstream returned an empty completion" (the frame did state content, in a
// spelling this arm cannot read), where the byte-identical document, buffered,
// was refused with "decode upstream response: json: cannot unmarshal …". The
// buffered detail is the actionable half, exactly as round 90's F90-L2-3 argued
// for the unprefixed spelling of the same wire.
//
// F96-L2-2 — the context-window probe decoded a remote's whole /v1/models list
// into one typed struct, so a single retyped field ANYWHERE in the list failed
// the decode and the route's window became 0: the entry the route named was
// well-formed and lost its window to an unrelated sibling, and a length the
// wire spelled as a string ("262144") or a float (262144.0) read as no window
// at all. Zero here is not a small window: it is "unknown", which drops the
// CLAUDE_CODE_MAX_CONTEXT_TOKENS hint and the context-fit clamp ceiling.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r96ErrorOf reads the sentence an arm states for a refusal: both arms answer a
// refusal as the same one-document error shape.
func r96ErrorOf(t *testing.T, body string) string {
	t.Helper()
	var doc struct {
		Type  string `json:"type"`
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(body), &doc); err != nil {
		t.Fatalf("refusal body is not a document: %v (%q)", err, body)
	}
	if doc.Type != "error" {
		t.Fatalf("refusal body states %q, want an error document (%q)", doc.Type, body)
	}
	return doc.Error.Message
}

// r96Arm asks one upstream spelling through one client arm of the real proxy.
// The upstream server answers every request with the same fixed bytes, so the
// caller hands the arm the spelling its client would actually meet: a document
// for the buffered arm, a frame for the streamed one.
func r96Arm(t *testing.T, ct, upstream string, stream bool) (int, string) {
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
	return round54PostMessage(t, proxy, "glm-5.3", stream)
}

// One upstream body the document decoder rejects states ONE cause, whichever arm
// met it — and that cause is the decode detail, not a sentence about a stream or
// an emptiness that did not happen.
func TestOneBodyTheDocumentDecoderRejectsStatesOneCauseOnBothArms(t *testing.T) {
	msg := `{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}`
	for _, tc := range []struct {
		name string
		doc  string // what the buffered arm's client is served
		want string // a fragment of the decode detail both arms must state
	}{
		{
			name: "a usage field the wire spelled as a string",
			doc:  `{"id":"x","choices":[` + msg + `],"usage":{"prompt_tokens":"11","completion_tokens":4,"total_tokens":15}}`,
			want: "usage.prompt_tokens",
		},
		{
			name: "a message whose content is an array of parts",
			doc:  `{"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":[{"type":"text","text":"hi"}]},"finish_reason":"stop"}]}`,
			want: "message.content",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			bc, bb := r96Arm(t, "application/json", tc.doc, false)
			sc, sb := r96Arm(t, "text/event-stream", "data: "+tc.doc+"\n\ndata: [DONE]\n\n", true)
			if bc != http.StatusBadGateway || sc != http.StatusBadGateway {
				t.Fatalf("one unreadable body: buffered %d, streamed %d, want 502 on both", bc, sc)
			}
			bMsg, sMsg := r96ErrorOf(t, bb), r96ErrorOf(t, sb)
			if !strings.Contains(bMsg, tc.want) {
				t.Errorf("the buffered arm states %q, want the decode detail naming %s", bMsg, tc.want)
			}
			if sMsg != bMsg {
				t.Errorf("one body, two causes (2026-09-29 audit, round 96, F96-L2-1):\n  buffered %q\n  streamed %q", bMsg, sMsg)
			}
		})
	}
}

// The control: the same turn spelled the way the decoders want it answers 200
// with the turn on both arms, so the rule above narrows to unreadable bodies and
// does not colour a servable one.
func TestAReadableBodyStillAnswersItsTurnOnBothArms(t *testing.T) {
	msg := `{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}`
	doc := `{"id":"x","choices":[` + msg + `],"usage":{"prompt_tokens":11,"completion_tokens":4,"total_tokens":15}}`
	bc, bb := r96Arm(t, "application/json", doc, false)
	sc, sb := r96Arm(t, "text/event-stream", "data: "+doc+"\n\ndata: [DONE]\n\n", true)
	if bc != http.StatusOK || sc != http.StatusOK {
		t.Fatalf("a readable body: buffered %d, streamed %d, want 200 on both (%q / %q)", bc, sc, bb, sb)
	}
	for name, body := range map[string]string{"buffered": bb, "streamed": sb} {
		if !strings.Contains(body, `"hi"`) {
			t.Errorf("the %s arm does not state the turn's text: %q", name, body)
		}
	}
}

// One entry's spelling is not evidence about the entry the route named, and a
// length the wire spelled as a string or a float is the number it says.
func TestARetypedSiblingEntryDoesNotVoidTheContextWindow(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want int
	}{
		{
			name: "the plain list",
			body: `{"data":[{"id":"m","context_length":262144}]}`,
			want: 262144,
		},
		{
			name: "a sibling entry retyped as a string, after the named entry",
			body: `{"data":[{"id":"m","context_length":262144},{"id":"other","context_length":"1"}]}`,
			want: 262144,
		},
		{
			name: "a sibling entry retyped as a string, before the named entry",
			body: `{"data":[{"id":"other","context_length":"1"},{"id":"m","context_length":262144}]}`,
			want: 262144,
		},
		{
			name: "a sibling entry that is not a length at all",
			body: `{"data":[{"id":"m","context_length":262144},{"id":"other","context_length":"nope"}]}`,
			want: 262144,
		},
		{
			name: "a sibling that is not an entry at all",
			body: `{"data":[{"id":"m","context_length":262144},123]}`,
			want: 262144,
		},
		{
			name: "the named entry's own length as a float",
			body: `{"data":[{"id":"m","context_length":262144.0}]}`,
			want: 262144,
		},
		{
			name: "the named entry's own length as a string",
			body: `{"data":[{"id":"m","context_length":"262144"}]}`,
			want: 262144,
		},
		{
			name: "a vLLM spelling, max_model_len as a string",
			body: `{"data":[{"id":"m","max_model_len":"262144"}]}`,
			want: 262144,
		},
		{
			name: "a length that is not a number at all fails closed",
			body: `{"data":[{"id":"m","context_length":"not-a-number"}]}`,
			want: 0,
		},
		{
			name: "an entry that states no window fails closed",
			body: `{"data":[{"id":"m"}]}`,
			want: 0,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := modelsJSONUpstream(t, []byte(tc.body))
			got := defaultRemoteContextWindow(proxyRoute{BaseURL: srv.URL, UpstreamModel: "m"})
			if got != tc.want {
				t.Errorf("window = %d, want %d for %s (2026-09-29 audit, round 96, F96-L2-2):\n  %s", got, tc.want, tc.name, tc.body)
			}
		})
	}
}

// A list the route cannot find its own entry in, and cannot fall back from,
// stays unknown rather than guessing a neighbour's window.
func TestAnAmbiguousListStillStatesNoWindow(t *testing.T) {
	srv := modelsJSONUpstream(t, []byte(`{"data":[{"id":"a","context_length":4096},{"id":"b","context_length":8192}]}`))
	if got := defaultRemoteContextWindow(proxyRoute{BaseURL: srv.URL, UpstreamModel: "m"}); got != 0 {
		t.Errorf("window = %d for a list naming neither the route's model nor a single entry, want 0", got)
	}
}

// r96Named is a readability guard on the table above: every entry in it is a
// list the probe must READ (a document), not a bare model object.
