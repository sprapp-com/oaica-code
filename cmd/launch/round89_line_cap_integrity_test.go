package launch

// round89_line_cap_integrity_test.go — leg 2, F89-L2-1 (2026-09-29 audit, round
// 89).
//
// The stream reader splits its upstream body into lines with a bufio.Scanner
// whose per-line cap was 8 MiB, on the reasoning that DeepSeek's reasoning runs
// can make a line long. The document arm of the same leg buffers the same body
// up to httpbody.DefaultMax, 64 MiB, as do the two buffers this reader fills
// from its own lines (eventBuf, nonSSE) — so a document that arrived as ONE
// line was read at 64 MiB by one arm and refused as a mid-stream reader failure
// by the other.
//
// Measured on the round-89 probe, one request per arm over the same body: a
// whole completion in a single `data:` frame carrying a tool call whose
// arguments are 9,437,384 bytes answered 502 `upstream stream failed:
// bufio.Scanner: token too long`, while the same bytes asked with stream:false
// answered 200 with the call, whole. The wire is a live shape — a whole
// completion inside one frame is how a non-streaming backend behind a streaming
// shim answers, which is the wire rounds 47 and 85 are about, and a tool call's
// arguments are one line however large the file it is writing.
//
// The pin holds the two arms to the same answer for one body, and holds the
// call's bytes whole: a cap that refuses a document must not be one the same
// arm's own buffers would have accepted.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// r89BigDoc is a whole completion of one line whose call arguments are
// `size` bytes — past the 8 MiB the reader used to refuse.
func r89BigDoc(size int) (doc string, args string) {
	args = `{"_raw":"` + strings.Repeat("a", size) + `"}`
	doc = `{"id":"c","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi","tool_calls":[{"id":"c1","type":"function","function":{"name":"Bash","arguments":` + r89Quote(args) + `}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":10,"completion_tokens":3}}`
	return doc, args
}

// r89Quote renders s as a JSON string literal, so the huge argument travels as
// one line of the document.
func r89Quote(s string) string {
	b, err := json.Marshal(s)
	if err != nil {
		panic(err)
	}
	return string(b)
}

// TestAWholeCompletionInOneFrameSurvivesTheLineCap is the F89-L2-1 pin. The
// same document, framed and unframed: both arms serve it, and the call's
// arguments reach the client whole.
func TestAWholeCompletionInOneFrameSurvivesTheLineCap(t *testing.T) {
	const size = 9_437_384
	doc, args := r89BigDoc(size)

	// The framed arm: the document is one line of one data: frame, so the
	// reader's per-line cap is what decides it.
	code, order, got := r83OrderArm(t, []string{"data: " + doc, r81Done})
	if code != http.StatusOK {
		t.Fatalf("a whole completion in one frame answered %d, want 200 — the same bytes unframed are read at 64 MiB by the document arm, and a reader's own line cap must not be the thing that refuses a document this arm's buffers would have accepted (2026-09-29 audit, round 89, F89-L2-1)", code)
	}
	if !strings.HasPrefix(order, "blocks=text,tool_use") {
		t.Errorf("the framed document relayed %s, want the text and the call (2026-09-29 audit, round 89, F89-L2-1)", order)
	}
	if !strings.Contains(got, args) {
		t.Errorf("the call reached the client with %d bytes of arguments, want the document's own %d — the answer this arm serves must be the whole one (2026-09-29 audit, round 89, F89-L2-1)", len(got), len(args))
	}

	// The sibling arm, over the same bytes: the bound it has always used.
	code, body := r87ErrorBody(t, "application/json", doc, false)
	if code != http.StatusOK || !strings.Contains(body, args) {
		t.Errorf("the same document unframed answered %d with the call's arguments %s (%d bytes of %d) — the two arms of one body must agree (2026-09-29 audit, round 89, F89-L2-1)",
			code, containsWord(strings.Contains(body, args)), len(body), len(args))
	}
}

func containsWord(ok bool) string {
	if ok {
		return "whole"
	}
	return "missing or truncated"
}
