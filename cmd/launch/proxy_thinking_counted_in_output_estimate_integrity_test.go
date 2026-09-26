package launch

// proxy_thinking_counted_in_output_estimate_integrity_test.go — the fallback
// output count left reasoning content out on two of the three paths that
// compute it (2026-09-27 audit, round 17).
//
// When the upstream states no completion_tokens, the proxy estimates the output
// count from the text it relayed, because a client that is told the turn was
// empty never grows its context estimate and auto-compaction never fires. The
// adopted-completion path counted content AND thinking
// (adoptNonSSECompletion); the streaming delta path and the plain non-streaming
// path counted content only — and a reasoning model's thinking is the larger
// half of what it produced, all of it relayed to the client as thinking deltas
// and all of it billed as output upstream.

import (
	"encoding/json"
	"strings"
	"testing"
)

// A stream that sends reasoning and content, states no usage at all, and ends
// with a finish_reason: the turn's output count has to account for both.
func TestProxyStream_ThinkingCountsTowardTheOutputEstimate(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"reasoning_content":"let me think about this for a while"}}]}`,
		"",
		`data: {"choices":[{"delta":{"content":"ok"}}]}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"), true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-thinking-output-estimate")
	body, status := postMessagesStream(t, proxy)
	if status != 200 {
		t.Fatalf("status %d\nbody:\n%s", status, body)
	}

	got := streamedOutputTokens(t, body)
	// The content alone is 2 characters, so a content-only count lands on 1;
	// the thinking is 34, which puts the estimate at 9.
	if got < 5 {
		t.Errorf("usage.output_tokens = %d, want the reasoning content counted too — the client sizes its context from this, and a reasoning turn that looks nearly empty never triggers compaction:\n%s", got, body)
	}
}

// streamedOutputTokens reads the cumulative output_tokens from the last
// message_delta event of an Anthropic SSE stream.
func streamedOutputTokens(t *testing.T, body string) int {
	t.Helper()
	best := -1
	for _, frame := range strings.Split(body, "\n") {
		line := strings.TrimSpace(frame)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event struct {
			Type  string `json:"type"`
			Usage struct {
				OutputTokens int `json:"output_tokens"`
			} `json:"usage"`
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			continue
		}
		if event.Type == "message_delta" {
			best = event.Usage.OutputTokens
		}
	}
	if best < 0 {
		t.Fatalf("no message_delta event carried usage:\n%s", body)
	}
	return best
}

// The non-streaming path has the same shape of gap: a response that carries
// thinking and no usage must not report an output count for its content alone.
func TestProxyNonStream_ThinkingCountsTowardTheOutputEstimate(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"m","choices":[{"index":0,`+
		`"message":{"role":"assistant","content":"ok","reasoning_content":"let me think about this for a while"},`+
		`"finish_reason":"stop"}]}`)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-thinking-output-estimate-nonstream")
	body, status := postMessagesRaw(t, proxy, false)
	if status != 200 {
		t.Fatalf("status %d\nbody:\n%s", status, body)
	}

	var resp struct {
		Usage struct {
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("the non-streaming response is not JSON (%v):\n%s", err, body)
	}
	if resp.Usage.OutputTokens < 5 {
		t.Errorf("usage.output_tokens = %d, want the reasoning content counted too:\n%s", resp.Usage.OutputTokens, body)
	}
}
