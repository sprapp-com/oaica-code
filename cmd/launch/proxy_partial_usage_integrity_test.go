package launch

// proxy_partial_usage_integrity_test.go — a usage object that states ONE count
// was read as a statement about BOTH (2026-09-26 audit, thirteenth round).
//
// The round-twelfth fix taught the non-streaming path not to trust a usage
// object that states nothing. It asked the question with an OR over the two
// fields, so an object stating only completion_tokens — a shape a build that
// counts outputs but not inputs emits — counted as "the upstream stated usage"
// and suppressed the prompt fallback entirely:
//
//	{"choices":[{"finish_reason":"stop","message":{"content":"hi"}}],
//	 "usage":{"prompt_tokens":0,"completion_tokens":7}}
//
// reached the client as input_tokens 0. That is the same failure the fallback
// exists to prevent (a session whose context never appears to grow, so
// auto-compaction never fires until the request hits the wall), and the
// streaming path never had it: it gates each field separately
// (statedPrompt/statedCompletion). The rule is per field, and one count is not
// a statement about the other.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestPartialUsageStatingOnlyCompletionDoesNotZeroTheInput(t *testing.T) {
	up := jsonUpstream(t, `{"id":"x","model":"kat-awq","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":0,"completion_tokens":7,"total_tokens":7}}`)
	defer up.Close()
	proxy := startCalibProxy(t, up.URL, "sess-partial-usage")

	resp, err := http.Post(proxy+"/v1/messages", "application/json", strings.NewReader(string(calibMessagesBody(t, 32, 64, false))))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	var got struct {
		Usage struct {
			InputTokens  int `json:"input_tokens"`
			OutputTokens int `json:"output_tokens"`
		} `json:"usage"`
	}
	if err := json.Unmarshal(raw, &got); err != nil {
		t.Fatalf("decode: %v\n%s", err, raw)
	}
	if got.Usage.InputTokens <= 0 {
		t.Errorf("input_tokens = %d for a turn whose prompt was real: the upstream stated only completion_tokens, and the fallback was gated on the OBJECT (an OR of the two fields) instead of on the field it was placing — the client reads a context that never grows, so auto-compaction never fires\n%s", got.Usage.InputTokens, raw)
	}
	if got.Usage.OutputTokens != 7 {
		t.Errorf("output_tokens = %d, want the stated 7 — the per-field rule must not override a count the upstream actually gave\n%s", got.Usage.OutputTokens, raw)
	}
}
