package launch

// round87_error_spelling_and_restated_call_integrity_test.go — leg 2,
// F87-L2-2 and F87-L2-3 (2026-09-29 audit, round 87).
//
// One upstream answer — a JSON error object over HTTP 200 — has three
// spellings: the whole body with `application/json`, the same bytes inside one
// `data:` frame, and the same bytes unframed with an SSE content type. Two
// things were read differently by the three:
//
// F87-L2-2. The object was recognised only when it arrived on ONE line. The
// stream reader walks the body line by line, and `upstreamErrorMessage` needs
// the whole object in one trimmed string starting `{`, so a pretty-printed
// object — the shape a vendor or a CDN that pretty-prints its error sends, and
// the shape an SSE sender makes when it spreads a payload over several `data:`
// lines, which the spec says to join — matched nothing. The client was then
// told "upstream stream ended before the response was complete" (framed) or
// "upstream returned an empty completion" (unframed, where the adoption in the
// tail refused the bytes it could not read as a turn) while the plain arm
// reported the upstream's own reason. The framed arm also asserted a cause that
// did not happen: the stream ended exactly where the upstream ended it.
//
// F87-L2-3. The whole-body arm says "upstream error: <reason>"; the two stream
// arms said the bare reason. Same object, same status, same type, two sentences.
//
// Measured on 2026-09-29 before the fix, upstream bytes
// `{\n  "error": {\n    "message": "boom",\n    "type": "server_error"\n  }\n}`
// over HTTP 200: plain → "upstream error: boom"; one data frame → "upstream
// stream ended before the response was complete"; unframed → "upstream returned
// an empty completion". One-line objects: plain → "upstream error: boom",
// framed and unframed → "boom".
//
// Live producer: the whole-body shape is live (round 54's pin), and so is a
// one-line error frame in a 200 stream (vLLM and this fleet's gateway both
// answer that way — the seventh round's pin). The MULTI-LINE shape is not
// observed in this fleet; it is fixed here because reading the event the way
// the SSE spec reads it is what makes the reader agree with the whole-body arm,
// and because a reader that cannot see the upstream's own reason invents one.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r87ErrorBody relays one fixed upstream body through the real proxy and
// returns the status and the body the client was handed.
func r87ErrorBody(t *testing.T, ct, upstream string, stream bool) (int, string) {
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

// r87ErrMessage is the message the client was handed, from the error event of a
// stream or from the error envelope of a plain body.
func r87ErrMessage(t *testing.T, body string) string {
	t.Helper()
	for _, line := range strings.Split(body, "\n") {
		raw, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Error struct {
				Message string `json:"message"`
			} `json:"error"`
		}
		if json.Unmarshal([]byte(raw), &ev) == nil && ev.Type == "error" {
			return ev.Error.Message
		}
	}
	var doc struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if json.Unmarshal([]byte(body), &doc) == nil && doc.Error.Message != "" {
		return doc.Error.Message
	}
	return "<none>"
}

// TestAnUpstreamErrorObjectIsTheSameSentenceOnEverySpelling is the F87-L2-2 and
// F87-L2-3 pin: the upstream's own reason, in one sentence, whichever spelling
// the same bytes arrived in.
func TestAnUpstreamErrorObjectIsTheSameSentenceOnEverySpelling(t *testing.T) {
	multi := "{\n  \"error\": {\n    \"message\": \"boom\",\n    \"type\": \"server_error\"\n  }\n}"
	one := `{"error":{"message":"boom","type":"server_error"}}`
	const want = "upstream error: boom"

	for _, shape := range []struct{ name, object string }{
		{"one line", one},
		{"pretty-printed over several lines", multi},
	} {
		t.Run(shape.name, func(t *testing.T) {
			for _, arm := range []struct {
				name, ct, body string
				stream         bool
			}{
				{"plain body", "application/json", shape.object, false},
				{"one data frame", "text/event-stream", "data: " + shape.object + "\n\ndata: [DONE]", true},
				{"unframed, sse content type", "text/event-stream", shape.object, true},
			} {
				status, body := r87ErrorBody(t, arm.ct, arm.body, arm.stream)
				if status != http.StatusBadGateway {
					t.Errorf("the %s spelling of an upstream error object answered HTTP %d, want 502 — an error object is not a turn (2026-09-29 audit, round 87)",
						arm.name, status)
				}
				if got := r87ErrMessage(t, body); got != want {
					t.Errorf("the %s spelling of one upstream error object reached the client as %q, want %q — the same body must give the same sentence on every arm, and the upstream's own reason is the one the whole-body arm has always reported (2026-09-29 audit, round 87, F87-L2-2/F87-L2-3):\n%s",
						arm.name, got, want, body)
				}
			}
		})
	}
}

// TestARestatedCallIsNotDoubledByTheSpellingItArrivesIn is the F87-L2-1 pin:
// when the second statement of one id-less call arrives as a whole-completion
// FRAME, it is a restatement and not a second call — whether the statement it
// restates arrived as a fragment or as a frame. A frame is this leg's
// restatement carrier (round 85's fold, round 86's canonical compare), so a
// call the ADOPTION wrote is held by the turn exactly as one the accumulators
// hold, and [frame][frame] must reach the client as the one call [frag][frame]
// does.
//
// [frag][frag] is deliberately NOT in this pin: two fragments are two
// statements of a call the wire never tied together, and A45-2 reads them as
// two calls. The frame spelling is the one that says "this is the same turn,
// stated again".
func TestARestatedCallIsNotDoubledByTheSpellingItArrivesIn(t *testing.T) {
	const frag = `data: {"id":"c","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`
	const frame = `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","tool_calls":[{"type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}]}`

	// The block ids are minted per request, so the arms are compared by the
	// call's NAME: the id says nothing about this reading.
	var reference string
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{"a fragment, then the frame that restates it", []string{frag, frame, r85CallsFin, r81Done}},
		{"a frame, then the frame that restates it", []string{frame, frame, r85CallsFin, r81Done}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := r85Body(t, tc.frames)
			order, calls := r85Blocks(t, body)
			if len(calls) != 1 {
				t.Errorf("%s handed the client %d calls %v — one call stated twice is one call, and a frame that only restates a call the turn already holds (here, one an adopted whole completion wrote) is not a new one (2026-09-29 audit, round 87, F87-L2-1)",
					tc.name, len(calls), calls)
			}
			if order != "tool_use" {
				t.Errorf("%s reached the client as %q, want one tool_use block (2026-09-29 audit, round 87, F87-L2-1)", tc.name, order)
			}
			names := make([]string, 0, len(calls))
			for _, c := range calls {
				name, _, _ := strings.Cut(c, "/")
				names = append(names, name)
			}
			got := strings.Join(names, " ")
			if reference == "" {
				reference = got
			} else if got != reference {
				t.Errorf("%s handed the client the call(s) %v where the other spelling of the same two statements handed it %v (2026-09-29 audit, round 87, F87-L2-1)",
					tc.name, names, reference)
			}
		})
	}
}
