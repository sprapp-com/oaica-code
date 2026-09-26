package launch

// round24_streamed_output_integrity_test.go — the output estimate's rule is
// "count what was relayed to the client", and one site counted what was not
// (2026-09-27 audit, round 24).
//
// Round 23 added tool-call argument bytes to the streaming estimate so a
// tool-only turn stopped reporting output_tokens: 0. It did so where the
// fragments ACCUMULATE — before the code decides whether the fragment is a
// call at all. flushToolCalls drops a truncated fragment whose argument JSON
// never parsed (round 16: emitting it as {"_raw": …} handed the agent an
// executable tool_use whose input the model never wrote), so a turn cut off at
// the token limit with a half-written call relayed no tool_use block, no text
// and no thinking — and still reported output tokens for it. The adopt path
// and the non-streaming path both measure the EMITTED calls
// (toolCallArgumentsSize); only the streaming path measured the received ones.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TestStreamedOutputEstimateExcludesADroppedToolCall: one tool call whose
// arguments never parse, and a finish_reason of "length" — the model was still
// writing the call when the token limit hit. flushToolCalls drops it, so the
// client receives no content, no thinking and no tool_use: there is nothing
// whose tokens could be counted, and output_tokens must be 0 rather than the
// size of the fragment that was thrown away.
func TestStreamedOutputEstimateExcludesADroppedToolCall(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for _, frame := range []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Write","arguments":"{\"path\":\"/tmp/x.txt\",\"content\":\"unterminated"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, frame+"\n\n")
			f.Flush()
		}
	}))
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-r24-dropped-toolcall")
	resp, err := http.Post(proxy+"/v1/messages", "application/json", bytes.NewReader(calibMessagesBody(t, 400, 64, true)))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	// The premise: the fragment really was dropped. If a tool_use block ever
	// reaches the client here, the estimate is right to count it and this test
	// is asking for the wrong number.
	if strings.Contains(body, `"tool_use"`) {
		t.Fatalf("premise: the truncated fragment was relayed as a tool_use block, so this test is not about a dropped call:\n%s", body)
	}
	if got := usageInt(deltaUsage(t, body), "output_tokens"); got != 0 {
		t.Errorf("output_tokens = %d for a turn that relayed no block at all: the estimate was counting the argument bytes of the call it discarded rather than the calls it emitted", got)
	}
}

// TestStreamedOutputEstimateCountsOnlyTheEmittedCall is the control for the
// same rule read the other way: when one call parses and a second is a
// truncated fragment, the emitted call's arguments are still counted — the fix
// moved WHERE the count happens, it did not stop counting tool arguments.
func TestStreamedOutputEstimateCountsOnlyTheEmittedCall(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		f := w.(http.Flusher)
		for _, frame := range []string{
			`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"path\":\"/tmp/x.txt\"}"}}]}}]}`,
			`data: {"choices":[{"delta":{"tool_calls":[{"index":1,"id":"call_2","type":"function","function":{"name":"Write","arguments":"{\"path\":\"/tmp/y.txt\",\"content\":\"unterminated"}}]}}]}`,
			`data: {"choices":[{"delta":{},"finish_reason":"length"}]}`,
			`data: [DONE]`,
		} {
			_, _ = io.WriteString(w, frame+"\n\n")
			f.Flush()
		}
	}))
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-r24-mixed-toolcalls")
	resp, err := http.Post(proxy+"/v1/messages", "application/json", bytes.NewReader(calibMessagesBody(t, 400, 64, true)))
	if err != nil {
		t.Fatalf("POST /v1/messages: %v", err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	body := string(raw)

	if !strings.Contains(body, `"tool_use"`) {
		t.Fatalf("premise: the parseable call did not reach the client:\n%s", body)
	}
	// The estimate is chars/4 + 1, so the bound below is the emitted call's own
	// arguments and nothing else. It has to be a bound, not just "> 0": the
	// defect this control sits beside counted the DROPPED fragment too, and both
	// readings are positive — so a bare "> 0" would have passed with the fix
	// reverted and pinned nothing (2026-09-27 audit, round 25, F5).
	emitted := len(`{"path":"/tmp/x.txt"}`)
	got := usageInt(deltaUsage(t, body), "output_tokens")
	if got <= 0 {
		t.Errorf("output_tokens = %d for a turn that relayed an executable tool_use block: moving the count to the emitted calls must not stop counting them", got)
	} else if limit := emitted/4 + 1; got > limit {
		t.Errorf("output_tokens = %d for a turn whose only emitted call has %d bytes of arguments (at most %d by the chars/4+1 estimate): the truncated fragment that never reached the client is being counted as output again", got, emitted, limit)
	}
}
