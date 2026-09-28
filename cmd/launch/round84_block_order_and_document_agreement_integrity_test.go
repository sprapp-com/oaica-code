package launch

// round84_block_order_and_document_agreement_integrity_test.go — leg 2,
// R84-L2-1 (2026-09-28 audit, round 84).
//
// One turn — a finished call, then the prose that came after it — written five
// ways: as deltas, as one whole frame carrying both the prose and the call, as
// a call fragment followed by a whole frame carrying the prose, as the same
// with two call fragments, and as a non-stream body. Round 83's flush made the
// first of those the odd one out; every spelling answers in the document arm's
// order, prose first, and this file is what says so.
//
// The order is the converter's fixed thinking→text→tool_use order, recorded as
// deliberate at anthropic/anthropic.go's F68-L1-1 note. The one spelling that
// does NOT follow it is a whole frame whose content is EMPTY and whose call is
// stated in the same frame: there is no prose to order against, the call block
// opens where the frame states it, and the prose that streams afterwards opens a
// text block behind it. That divergence is F68-L1-1's, and round 83's flush was
// an attempt to move the ordinary turn onto that special case.

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// r84Order renders a whole-message answer as the block order the client sees.
func r84Order(t *testing.T, body string) string {
	t.Helper()
	var msg struct {
		Content []struct {
			Type string `json:"type"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &msg); err != nil {
		t.Fatalf("the relayed turn is not a message: %v\n%s", err, body)
	}
	var order []string
	for _, b := range msg.Content {
		order = append(order, b.Type)
	}
	return strings.Join(order, ",")
}

// r84WholeBody is the same turn as a non-stream body: one completion carrying
// the prose and the call, which is also the shape a vendor streams as one frame.
func r84WholeBody(content string) string {
	return `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"` + content +
		`","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3,"total_tokens":13}}`
}

// TestOneFinishedCallThenProseHasOneOrderOnEverySpelling is R84-L2-1. The turn
// is a call c1 Bash {"a":1} and the prose "hi"; the four streamed spellings and
// the non-stream body must hand the client the same message.
func TestOneFinishedCallThenProseHasOneOrderOnEverySpelling(t *testing.T) {
	callDelta := `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`
	textDelta := `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	fin := `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`
	whole := `data: ` + r84WholeBody("hi")

	orders := map[string]string{}
	for _, arm := range []struct {
		name   string
		frames []string
	}{
		{"deltas", []string{callDelta, textDelta, fin, r81Done}},
		{"one whole frame", []string{whole, fin, r81Done}},
		{"fragment then whole frame", []string{callDelta, whole, r81Done}},
	} {
		_, got, _ := r83OrderArm(t, arm.frames)
		orders[arm.name] = strings.TrimPrefix(got, `blocks=`)
	}

	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, r84WholeBody("hi"))
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK {
		t.Fatalf("the non-stream arm answered %d: %s", code, body)
	}
	orders["non-stream"] = r84Order(t, body)

	want := "text,tool_use"
	for _, name := range []string{"deltas", "one whole frame", "fragment then whole frame", "non-stream"} {
		if !strings.HasPrefix(orders[name], want) {
			t.Errorf("the same turn (%s) as %s is %q, want %q — a call the wire finished before it wrote the prose is held to the turn's end, the way the converter's whole-message order and this leg's non-stream arm already put it; round 83's flush emitted the call as the prose arrived and split one body across two orders (2026-09-28 audit, round 84, R84-L2-1)",
				`call c1 Bash {"a":1}, then "hi"`, name, orders[name], want)
		}
	}
}

// TestAWholeFrameWithNoProseKeepsTheFramesOwnOrder is the recorded exception,
// F68-L1-1. The frame states an EMPTY content and a call in one object: there
// is no prose for the call's block to be ordered against, so the call block
// opens where the frame states it and the prose that streams afterwards opens a
// text block behind it. This pins the shape so that a future attempt to make
// every spelling agree must confront it explicitly rather than by accident.
func TestAWholeFrameWithNoProseKeepsTheFramesOwnOrder(t *testing.T) {
	wholeCallOnly := `data: {"id":"c","choices":[{"index":0,"message":{"role":"assistant","content":"","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":"{\"a\":1}"}}]},"finish_reason":"tool_calls"}]}`
	textDelta := `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"content":"hi"}}]}`
	fin := `data: {"id":"c","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`

	_, got, _ := r83OrderArm(t, []string{wholeCallOnly, textDelta, fin, r81Done})
	if !strings.HasPrefix(strings.TrimPrefix(got, `blocks=`), "tool_use,text") {
		t.Errorf("a whole frame with no prose, then a prose delta, is %q — the frame is written in its own order (the call block where the frame states it) and the prose behind it opens a text block, which is the divergence anthropic/anthropic.go's F68-L1-1 note records as deliberate (2026-09-28 audit, round 84, R84-L2-1)", got)
	}
}
