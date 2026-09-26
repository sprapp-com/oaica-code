package launch

// proxy_zeroed_usage_integrity_test.go — a usage object that is PRESENT but
// zero defeats both no-usage fallbacks (2026-09-26 audit, twelfth round).
//
// The neighbours of this file fixed the no-usage shapes: an upstream that
// never sends the usage chunk, and one that sends no usage object at all,
// both now get the prompt estimate instead of a hard zero, so a client that
// sizes its context accounting on the reported usage (Claude Code always
// streams, and compacts on that number) sees the session grow. The guard that
// decides "did the upstream state usage?" was the POINTER, not the value —
// and a build that always emits the object, populated only when it has
// something to say, sends
//
//	data: {"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0}}
//
// which is not a statement of zero, it is an empty statement. Read as fact it
// is worse than the shape the fallback exists for: the session never appears
// to grow, so auto-compaction never fires, until the request hits the 262k
// wall — the exact 2026-08-30 failure this proxy's usage accounting was
// fixed for, reached through a different door.
//
// The calibration path already gates on the VALUE (`chunk.Usage.PromptTokens > 0`,
// anthropic_openai_proxy.go:2142) before recording a sample, for this same
// reason: record() refuses a non-positive count. The reporting path has to
// agree with it — a zeroed object must fall through to the estimate per
// FIELD, not be trusted because it was present.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

// The streaming shape: the usage-only final chunk is present and zeroed.
func TestProxyStream_ZeroedUsageChunkStillReportsAUsableCount(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"), true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-zeroed-usage")
	body, status := postMessagesStream(t, proxy)
	if status != http.StatusOK {
		t.Fatalf("status = %d, want 200\n%s", status, body)
	}
	u := deltaUsage(t, body)
	if in := usageInt(u, "input_tokens"); in <= 0 {
		t.Errorf("input_tokens = %d for a turn whose prompt was real: the upstream sent a usage object that states no counts, and the proxy read \"present\" as \"zero\" instead of falling back to its estimate — the client reads that as an empty context that never grows, so auto-compaction never fires\n%s", in, body)
	}
	if out := usageInt(u, "output_tokens"); out <= 0 {
		t.Errorf("output_tokens = %d for a turn that streamed text; the zeroed usage object suppressed the streamed-text fallback too\n%s", out, body)
	}
}

// The non-streaming shape: the usage object is present and zeroed.
func TestProxyNonStream_ZeroedUsageObjectStillReportsAUsableCount(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":0,"total_tokens":0}}`)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-nostream-zeroed-usage")
	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var got struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	if got.Usage.InputTokens <= 0 {
		t.Errorf("usage.input_tokens = %d for a turn whose prompt was real; a usage object that states nothing must fall back to the estimate, not be believed as zero\n%s", got.Usage.InputTokens, b)
	}
}

// Control: a usage object that DOES state counts must still be believed
// verbatim — the fallback must not paper over a real measurement. This is the
// shape the fleet's vLLM and gateway send.
func TestProxyNonStream_StatedUsageIsStillBelieved(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":4321,"completion_tokens":7,"total_tokens":4328}}`)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-stated-usage")
	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	var got struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(b, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, b)
	}
	if got.Usage.InputTokens != 4321 {
		t.Errorf("usage.input_tokens = %d, want the stated 4321 — the estimate must not override a real measurement\n%s", got.Usage.InputTokens, b)
	}
	if got.Usage.OutputTokens != 7 {
		t.Errorf("usage.output_tokens = %d, want the stated 7\n%s", got.Usage.OutputTokens, b)
	}
}
