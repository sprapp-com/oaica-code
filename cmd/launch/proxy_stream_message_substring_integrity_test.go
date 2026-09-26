package launch

// proxy_stream_message_substring_integrity_test.go — the whole-completion
// adoption gate looked for the characters "message" anywhere in an SSE frame,
// so an ordinary DELTA frame whose content contained that word was adopted as a
// whole completion: an empty message was emitted, the turn was marked complete,
// and the rest of the stream — the model's actual answer — was discarded. The
// client got HTTP 200, message_start with an empty content array, stop_reason
// end_turn and message_stop, and the leg was recorded healthy.
//
// The trigger is ordinary content, not an exotic upstream: a model writing JSON
// ({"message": …}), a config snippet, or a Write tool argument naming that key.
// Escaped \"message\" in the raw bytes matches too.
//
// 2026-09-26 audit, sixteenth round — the round-15 fix over-triggering, not the
// case it was written for (that one is pinned by
// proxy_stream_whole_frame_integrity_test.go and must keep passing).

import (
	"net/http"
	"strings"
	"testing"
)

func TestProxyStream_ADeltaFrameMentioningMessageIsNotAdopted(t *testing.T) {
	// The word appears in the model's own output, split across frames the way a
	// real stream carries it.
	up := streamUpstream(t, strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"content":"{\"message"},"finish_reason":null}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"content":": \"hello\"}"},"finish_reason":null}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`data: [DONE]`,
	}, "\n\n")+"\n\n", true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-message-substring")
	body, status := postMessagesStream(t, proxy)

	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody:\n%s", status, body)
	}
	if !strings.Contains(body, `\"message`) && !strings.Contains(body, `"message`) {
		t.Errorf("the streamed answer was discarded — an ordinary delta frame carrying the characters \"message\" was adopted as a whole completion:\n%s", body)
	}
	if !strings.Contains(body, "hello") {
		t.Errorf("the answer never reached the client:\n%s", body)
	}
	if hasEvent(sseEvents(body), "error") {
		t.Errorf("the turn was reported as an error:\n%s", body)
	}
}

// The same shape with the word in a TOOL-CALL delta: adopting it would drop the
// call and report end_turn, so the agent stops instead of running the tool.
func TestProxyStream_AToolCallDeltaMentioningMessageKeepsItsCall(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"role":"assistant"},"finish_reason":null}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Write","arguments":"{\"content\":\"{\\\"message\\\":"}}]},"finish_reason":null}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":" 1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"x","model":"m","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}, "\n\n")+"\n\n", true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-message-substring-tool")
	body, status := postMessagesStream(t, proxy)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody:\n%s", status, body)
	}

	blocks := sseToolUseBlocks(t, body)
	if len(blocks) != 1 {
		t.Fatalf("got %d tool_use block(s), want 1 — the call was dropped and the turn reported as a plain answer:\n%s", len(blocks), body)
	}
	if blocks[0]["name"] != "Write" {
		t.Errorf("tool_use name = %v, want Write\n%s", blocks[0]["name"], body)
	}
}

// Control: the shape the gate exists for still goes through it. A frame that
// really carries a whole completion has a populated `message` object and no
// delta, and must still be adopted.
func TestProxyStream_AWholeCompletionFrameIsStillAdoptedAfterTheShapeGate(t *testing.T) {
	const answer = "answer carried by message, not delta"
	up := streamUpstream(t, "data: "+
		`{"id":"x","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"`+answer+`"},"finish_reason":"stop"}],"usage":{"prompt_tokens":50000,"completion_tokens":7}}`+
		"\n\ndata: [DONE]\n\n", true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-message-substring-control")
	body, status := postMessagesStream(t, proxy)
	if status != http.StatusOK {
		t.Fatalf("status %d, want 200\nbody:\n%s", status, body)
	}
	if !strings.Contains(body, answer) {
		t.Errorf("a genuine whole-completion frame was no longer adopted — the gate is now too strict:\n%s", body)
	}
}
