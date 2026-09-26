package launch

// proxy_nonstream_truncated_tool_call_integrity_test.go — round 16 taught the
// DELTA path that a tool call cut off at the token limit is not executable; the
// other three paths kept handing the client the same fabricated call
// (2026-09-27 audit, round 17).
//
// The upstream stops at max_tokens in the middle of a tool call's arguments:
//
//	{"function":{"name":"Write","arguments":"{\"file_path\":\"/tmp/x\","}}
//	{"finish_reason":"length"}
//
// parseOpenAIToolCalls has no view of the finish_reason, so unparseable
// arguments took the {"_raw": <fragment>} fallback — a tool_use block whose
// input is a key the model never wrote — and mapStopReason then reported
// "tool_use" because a call was present, discarding the "length" the upstream
// stated. The client is an agent: it executes the fabricated call and never
// learns the answer was cut off.
//
// These two cases reach it without going through the delta accumulator:
//
//   - a plain stream:false response (handleNonStreamResponse), and
//   - a whole completion delivered as the answer to a stream request
//     (adoptNonSSECompletion) — the shape that must NOT be reported as a 502,
//     which is why it is adopted rather than refused.
//
// The streaming sibling is
// proxy_stream_truncated_tool_call_integrity_test.go, and the control below is
// the shape this must NOT break: unparseable arguments with no truncation
// signal (finish_reason "stop") are a model emitting freeform arguments, which
// the `_raw` fallback exists for.

import (
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

const truncatedToolCallBody = `{"id":"x","model":"m","choices":[{"index":0,` +
	`"message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function",` +
	`"function":{"name":"Write","arguments":"{\"file_path\":\"/tmp/x\","}}]},` +
	`"finish_reason":"length"}],"usage":{"prompt_tokens":10,"completion_tokens":5}}`

func postMessagesRaw(t *testing.T, proxyURL string, stream bool) (string, int) {
	t.Helper()
	body := calibMessagesBody(t, 32, 64, stream)
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", strings.NewReader(string(body)))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read proxy response: %v", err)
	}
	return string(b), resp.StatusCode
}

func TestProxyNonStream_ATruncatedToolCallIsNotAnExecutableToolUse(t *testing.T) {
	up := jsonUpstream(t, truncatedToolCallBody)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-truncated-tool-call-nonstream")
	body, status := postMessagesRaw(t, proxy, false)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody:\n%s", status, body)
	}

	var resp map[string]any
	if err := json.Unmarshal([]byte(body), &resp); err != nil {
		t.Fatalf("the non-streaming response is not JSON (%v):\n%s", err, body)
	}
	blocks, _ := resp["content"].([]any)
	for _, raw := range blocks {
		b, _ := raw.(map[string]any)
		if b["type"] != "tool_use" {
			continue
		}
		input, _ := b["input"].(map[string]any)
		if _, bad := input["_raw"]; bad {
			t.Errorf("the client was handed tool_use %v with input %v — the arguments are an unterminated fragment the model was still writing, and `_raw` is a key it never produced:\n%s",
				b["name"], input, body)
		}
	}
	if got, _ := resp["stop_reason"].(string); got != "max_tokens" {
		t.Errorf("stop_reason = %q, want max_tokens: the upstream said finish_reason \"length\", and an agent that reads tool_use here runs a call that was never made:\n%s", got, body)
	}
}

func TestProxyAdoptedCompletion_ATruncatedToolCallIsNotAnExecutableToolUse(t *testing.T) {
	up := jsonUpstream(t, truncatedToolCallBody)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-truncated-tool-call-adopted")
	body, status := postMessagesRaw(t, proxy, true)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200 — a whole body answering a stream request is adopted, not failed\nbody:\n%s", status, body)
	}

	for _, b := range sseToolUseBlocks(t, body) {
		input, _ := b["input"].(map[string]any)
		if _, bad := input["_raw"]; bad {
			t.Errorf("the client was handed tool_use %v with input %v — same fabrication as the non-streaming path, adopted through the stream tail:\n%s",
				b["name"], input, body)
		}
	}
	if !strings.Contains(body, `"max_tokens"`) {
		t.Errorf("stop_reason did not report the truncation for an adopted whole completion:\n%s", body)
	}
	if strings.Contains(body, `"tool_use"`) {
		t.Errorf("stop_reason said tool_use for a turn the upstream truncated:\n%s", body)
	}
}

// The control: the same unparseable arguments, WITHOUT a truncation signal.
// Freeform (non-JSON) arguments are a real shape, and `_raw` is how oaica
// keeps such a call from being dropped entirely — the drop above is scoped to
// the turn the upstream said it had cut off.
func TestProxyNonStream_FreeformArgumentsKeepTheirRawForm(t *testing.T) {
	up := jsonUpstream(t, strings.Replace(truncatedToolCallBody, `"finish_reason":"length"`, `"finish_reason":"stop"`, 1))
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-freeform-tool-call")
	body, status := postMessagesRaw(t, proxy, false)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody:\n%s", status, body)
	}
	if !strings.Contains(body, `"_raw"`) {
		t.Errorf("a freeform-argument tool call (no truncation signal) was dropped — the `_raw` fallback exists so such a call is not lost:\n%s", body)
	}
}
