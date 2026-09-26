package launch

// proxy_stream_truncated_tool_call_integrity_test.go — a tool call cut off at
// the token limit was handed to the client as a complete, executable tool_use
// (2026-09-26 audit, round 16).
//
// When the upstream stops the turn at max_tokens in the middle of a tool
// call's arguments, the stream still carries a finish_reason ("length"), so
// the turn counts as complete and flushToolCalls runs over argument JSON that
// never parsed. The fallback for unparseable arguments — {"_raw": <fragment>},
// which exists so a model emitting freeform arguments is never dropped
// entirely — then produced a tool_use block whose input is a key the model
// never wrote. stop_reason came out "tool_use" as well, because mapStopReason
// returns tool_use whenever ANY call is present, discarding the "length" the
// upstream stated.
//
// The client is an agent: it runs the fabricated call with those arguments and
// never learns the answer was truncated. The non-streaming sibling keeps its
// _raw fallback deliberately (freeform arguments are a real shape there, and
// there is no truncation signal to weigh it against), so this is the streaming
// path's rule.

import (
	"net/http"
	"strings"
	"testing"
)

// A turn cut at max_tokens mid-tool-call must not hand the client a fabricated,
// executable call, and must report the truncation.
func TestProxyStream_ATruncatedToolCallIsNotAnExecutableToolUse(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Write","arguments":"{\"file_path\":\"/tmp/x\","}}]}}]}`,
		"",
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"), true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-truncated-tool-call")
	body, status := postMessagesStream(t, proxy)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody:\n%s", status, body)
	}

	for _, b := range sseToolUseBlocks(t, body) {
		input, _ := b["input"].(map[string]any)
		if _, bad := input["_raw"]; bad {
			t.Errorf("the client was handed tool_use %v with input %v — the arguments the model wrote are an unterminated fragment, and `_raw` is a key the model never produced, so the agent executes a call that was never made:\n%s",
				b["name"], input, body)
		}
	}

	if !strings.Contains(body, `"max_tokens"`) {
		t.Errorf("stop_reason did not report the truncation — the upstream said finish_reason \"length\" and the client must be able to tell a cut-off turn from a chosen one:\n%s", body)
	}
	if strings.Contains(body, `"tool_use"`) {
		t.Errorf("stop_reason said tool_use for a turn the upstream truncated — an agent reads that as \"the model chose this call\" and runs it instead of continuing:\n%s", body)
	}
}
