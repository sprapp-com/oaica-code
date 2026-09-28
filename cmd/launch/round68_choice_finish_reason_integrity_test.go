package launch

// round68_choice_finish_reason_integrity_test.go — leg 2, which choice's
// finish_reason the whole-document arm reads.
//
// The fragment arm scans every entry and keeps the last reason stated, and says
// why: "which choice carried it is not this arm's to decide". The whole-document
// arm read Choices[0] alone, so ONE body — the same two choices, the reason on
// the second — answered end_turn as a document and max_tokens streamed
// (2026-09-28 audit, round 68, F68-L2-2).
//
// The content stays the first entry's on both arms; only the verdict moved.

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

// r68TwoChoiceDoc is one completion whose first entry ends normally and whose
// second states the token limit.
func r68TwoChoiceDoc() string {
	return `{"id":"c","object":"chat.completion","choices":[` +
		`{"index":0,"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"},` +
		`{"index":1,"message":{"role":"assistant","content":""},"finish_reason":"length"}],` +
		`"usage":{"prompt_tokens":7,"completion_tokens":3}}`
}

// TestTheWholeDocumentReadsTheReasonAnyChoiceStated is the finding.
func TestTheWholeDocumentReadsTheReasonAnyChoiceStated(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	up := jsonUpstream(t, r68TwoChoiceDoc())
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	code, body := round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	var out struct {
		StopReason string `json:"stop_reason"`
		Content    []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("unmarshal: %v\n%s", err, body)
	}
	if out.StopReason != "max_tokens" {
		t.Errorf("the whole-document arm answered stop_reason %q for a completion whose second choice stated the token limit, want max_tokens: the fragment arm of this same leg reads the LAST reason any entry stated, so the verdict depended on which choice carried the field and on the `stream` flag (2026-09-28 audit, round 68, F68-L2-2)\n%s",
			out.StopReason, body)
	}
	if len(out.Content) != 1 || out.Content[0].Text != "hi" {
		t.Errorf("the content is %v, want the first entry's text: only the verdict follows the stated reason, the message stays the first choice's (2026-09-28 audit, round 68, F68-L2-2)", out.Content)
	}
}

// TestTheFragmentArmReadsTheSameReason is the control in the other direction:
// the arm that was already right must not move.
func TestTheFragmentArmReadsTheSameReason(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	up := streamUpstream(t,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":null}]}`+"\n\n"+
			`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"stop"},{"index":1,"delta":{},"finish_reason":"length"}]}`+"\n\n"+
			`data: [DONE]`+"\n\n", true)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	var stop string
	for _, line := range strings.Split(body, "\n") {
		line, ok := strings.CutPrefix(line, "data: ")
		if !ok {
			continue
		}
		var ev struct {
			Type  string `json:"type"`
			Delta struct {
				StopReason string `json:"stop_reason"`
			} `json:"delta"`
		}
		if json.Unmarshal([]byte(line), &ev) != nil {
			continue
		}
		if ev.Type == "message_delta" {
			stop = ev.Delta.StopReason
		}
	}
	if stop != "max_tokens" {
		t.Errorf("the fragment arm answered stop_reason %q, want max_tokens from the last choice that stated one\n%s", stop, body)
	}
}
