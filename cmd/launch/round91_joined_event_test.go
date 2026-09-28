package launch

// round91_joined_event_test.go — leg 2, F91-L2-1 / F91-L2-2 / F91-L2-3
// (2026-09-29 audit, round 91).
//
// The SSE grammar joins an event's `data:` lines with newlines, so a JSON object
// may be written across several of them — an upstream that pretty-prints a long
// completion does exactly that. This reader took each line as a payload of its
// own, and no line of a spread object is a frame: a whole completion spelled
// that way was adopted by nothing, folded by nothing, and the client was told a
// cause that did not happen — "upstream returned an empty completion" for a
// document carrying content, or "upstream stream ended before the response was
// complete" when the event also had no trailing blank line. After the stream had
// already relayed text the same document was dropped silently instead.
//
// Measured (round 91, before the fix): one `data:` line carrying the document
// answered 200 text="hi" usage{8,3}; the same bytes over two `data:` lines
// answered 502, 200 with the answer dropped, or 502 with no trailing blank line.
// The fix reads the text the lines join to as the payload the moment it parses,
// so one rule reads both spellings. These tests pin that parity, and the
// controls keep it from becoming "any non-JSON line is held".

import (
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"
)

// r91Joined drives the real proxy with one fixed upstream body and returns the
// client's status and body.
func r91Joined(t *testing.T, upstream string) (int, string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, upstream)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	return code, body
}

var r91JoinedID = regexp.MustCompile(`"id":"msg_[0-9a-f]+"`)

func r91Fold(s string) string { return strings.Join(strings.Fields(s), " ") }

// r91SameVerdict asserts that one document spelled two ways is answered the same
// way: same status, same bytes apart from the minted message id.
func r91SameVerdict(t *testing.T, name, oneLine, acrossLines string) {
	t.Helper()
	rc, rb := r91Joined(t, oneLine)
	sc, sb := r91Joined(t, acrossLines)
	rb, sb = r91JoinedID.ReplaceAllString(rb, "msg_X"), r91JoinedID.ReplaceAllString(sb, "msg_X")
	if rc != sc || r91Fold(rb) != r91Fold(sb) {
		t.Errorf("%s: the same document answered two ways\n  one data: line: %d %s\n  across lines : %d %s\n"+
			"one event is its lines JOINED, so the rule that reads a frame must read the document they spell (2026-09-29 audit, round 91, F91-L2-1/F91-L2-2/F91-L2-3)",
			name, rc, r91Fold(rb), sc, r91Fold(sb))
	}
}

// r91WholeDoc is a whole, non-streamed completion. Written across two `data:`
// lines it is the same document.
const (
	r91DocLine1 = `data: {"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},`
	r91DocLine2 = `data: "finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":3}}`
	r91DocOne   = `data: {"id":"x","choices":[{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":8,"completion_tokens":3}}`
)

func TestAWholeCompletionSpelledAcrossLinesIsTheSameTurn(t *testing.T) {
	// F91-L2-1: nothing relayed yet, and a frame before it stated the turn's
	// end.
	r91SameVerdict(t, "a spread completion after a finish frame",
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+r91DocOne+"\n\n",
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n"+r91DocLine1+"\n"+r91DocLine2+"\n\n")

	// F91-L2-3: the event is never closed with a blank line.
	r91SameVerdict(t, "a spread completion with no trailing blank line",
		r91DocOne+"\n",
		r91DocLine1+"\n"+r91DocLine2+"\n")

	// F91-L2-2: the stream has already relayed text when the document arrives.
	r91SameVerdict(t, "a spread completion after a delta",
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi "}}]}`+"\n\n"+r91DocOne+"\n\n",
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi "}}]}`+"\n\n"+r91DocLine1+"\n"+r91DocLine2+"\n\n")
}

func TestALineThatIsNotJSONIsHeldOnlyUntilItsEventEnds(t *testing.T) {
	// The controls: text that never parses must not change a turn that is
	// otherwise whole. A garbage line inside an event is dropped at the event's
	// end, exactly as it was before this reading existed.
	code, body := r91Joined(t, `data: {"id":"x","choices":[{"index":0,"delta":{"content":"hi"}}]}`+"\n\n"+
		"data: {not json at all\n\ndata: {\"id\":\"x\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
	if code != http.StatusOK || !strings.Contains(body, "hi") {
		t.Errorf("a stream carrying a line that is not JSON was answered %d %q; the garbage is not this turn's payload", code, body)
	}

	// And two whole objects on two lines of ONE event are still two frames: a
	// line that IS JSON is never held.
	code, body = r91Joined(t, `data: {"id":"x","choices":[{"index":0,"delta":{"content":"a"}}]}`+"\n"+
		`data: {"id":"x","choices":[{"index":0,"delta":{"content":"b"}}]}`+"\n\n"+
		`data: {"id":"x","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")
	if code != http.StatusOK || strings.Index(body, `"text":"a"`) < 0 || strings.Index(body, `"text":"b"`) < strings.Index(body, `"text":"a"`) {
		t.Errorf("two delta frames written on two lines of one event were answered %d %q, want both deltas relayed in order", code, body)
	}
}
