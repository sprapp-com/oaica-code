package launch

// proxy_stream_failure_test.go — what the translation proxy tells the caller
// when an upstream stream does not finish.
//
// The proxy's own retry-discipline comment promises it: "a mid-stream failure
// is NOT retried here ... The caller sees a clean upstream_error instead." It
// did not. The SSE loop ignored anything it could not decode, never consulted
// scanner.Err(), and never required a completion signal, then unconditionally
// flushed tool calls and emitted message_stop. So a decode crash, a dropped
// connection, or a JSON error body delivered over a 200 all reached Claude
// Code as a NORMAL end of turn — a truncated answer (or an empty one) that no
// layer above would retry, because nothing was reported as wrong.
//
// The worst shape was the tool call: partial argument JSON accumulated from
// the deltas was not valid JSON, so the flush invented {"_raw": "<partial>"}
// and emitted a complete, executable tool_use with stop_reason "tool_use" —
// turning a transport failure into a tool the agent then runs.

import (
	"bufio"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// streamUpstream serves one raw SSE script and then closes the connection.
// The script is written verbatim, so a test can end a stream mid-token, send
// an error object, or send no SSE at all.
func streamUpstream(t *testing.T, script string, complete bool) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, script)
		if fl, ok := w.(http.Flusher); ok {
			fl.Flush()
		}
	}))
}

// postMessagesStream sends an Anthropic Messages request through the proxy and
// returns the raw SSE response body and status.
func postMessagesStream(t *testing.T, proxyURL string) (string, int) {
	t.Helper()
	body := calibMessagesBody(t, 32, 64, true)
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

// sseEvents extracts the event names in order.
func sseEvents(body string) []string {
	var out []string
	sc := bufio.NewScanner(strings.NewReader(body))
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "event:") {
			out = append(out, strings.TrimSpace(strings.TrimPrefix(line, "event:")))
		}
	}
	return out
}

func hasEvent(names []string, want string) bool {
	for _, n := range names {
		if n == want {
			return true
		}
	}
	return false
}

// A decode crash reported mid-stream as an error object (how vLLM and
// gateways signal one) must reach the caller as a FAILURE, not as a clean end
// of turn with a truncated answer.
func TestProxyStream_UpstreamErrorObjectIsReportedAsError(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"partial ans"}}]}`,
		"",
		`data: {"error":{"message":"engine crashed mid-decode","type":"server_error","code":500}}`,
		"",
	}, "\n"), false)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-stream-err")
	body, _ := postMessagesStream(t, proxy)
	events := sseEvents(body)

	if !hasEvent(events, "error") {
		t.Errorf("mid-stream upstream error was reported as a clean turn (events: %v)\nbody:\n%s", events, body)
	}
	if hasEvent(events, "message_stop") {
		t.Errorf("a failed stream still ended with message_stop (events: %v)", events)
	}
}

// A connection that dies without a finish_reason, without usage, and without
// [DONE] is indistinguishable from a completed short answer once the proxy
// stops looking for a completion signal.
func TestProxyStream_TruncatedConnectionIsReportedAsError(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"truncated mid wor"}}]}`,
		"",
	}, "\n"), false)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-stream-cut")
	body, _ := postMessagesStream(t, proxy)
	events := sseEvents(body)

	if !hasEvent(events, "error") {
		t.Errorf("a stream that never completed was reported as a clean turn (events: %v)\nbody:\n%s", events, body)
	}
	if hasEvent(events, "message_stop") {
		t.Errorf("a truncated stream still ended with message_stop (events: %v)", events)
	}
}

// A 200 whose body is a plain JSON error — no SSE frames at all — must not be
// relayed as an empty successful message. Nothing has been streamed yet at
// that point, so the caller gets a real error status and can retry.
func TestProxyStream_JSONErrorBodyOverHTTP200IsNotAnEmptySuccess(t *testing.T) {
	up := streamUpstream(t, `{"error":{"message":"upstream overloaded"}}`, false)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-stream-jsonerr")
	body, status := postMessagesStream(t, proxy)

	if status == http.StatusOK {
		t.Errorf("a JSON error body over HTTP 200 was relayed as success (status %d)\nbody:\n%s", status, body)
	}
	if hasEvent(sseEvents(body), "message_stop") {
		t.Errorf("an error body was turned into a completed message:\n%s", body)
	}
}

// Partial tool-call arguments from a stream that then died must NOT become an
// executable tool_use. This is the sharpest consequence: the agent runs the
// fabricated call instead of retrying the turn.
func TestProxyStream_TruncatedToolArgumentsAreNotExecuted(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"{\"file_path\":\"/tmp/x"}}]}}]}`,
		"",
	}, "\n"), false)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-stream-toolcut")
	body, status := postMessagesStream(t, proxy)

	if strings.Contains(body, "tool_use") {
		t.Errorf("a truncated tool call was emitted as an executable tool_use:\n%s", body)
	}
	// Either shape is an honest failure the client can retry: an error status
	// (nothing had been streamed yet) or the protocol's error event (content
	// had already gone out).
	if status == http.StatusOK && !hasEvent(sseEvents(body), "error") {
		t.Errorf("the failed tool stream was reported as a clean turn (status %d):\n%s", status, body)
	}
}

// The deliberate fallback must survive for a stream that DID complete: a
// model that emits non-JSON arguments in a finished turn keeps its call
// (freeform tool formats), rather than being dropped or erroring.
func TestProxyStream_CompleteStreamWithNonJSONArgumentsKeepsTheCall(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"Read","arguments":"not json at all"}}]}}]}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"tool_calls"}]}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"), true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-stream-freeform")
	body, _ := postMessagesStream(t, proxy)

	if !strings.Contains(body, "tool_use") || !strings.Contains(body, "_raw") {
		t.Errorf("a completed freeform tool call was dropped (want tool_use with _raw):\n%s", body)
	}
	if hasEvent(sseEvents(body), "error") {
		t.Errorf("a completed stream was reported as an error:\n%s", body)
	}
}

// The ordinary path is unchanged: a finished stream still ends with
// message_delta (carrying stop_reason) then message_stop.
func TestProxyStream_CompleteStreamStillEndsCleanly(t *testing.T) {
	up := streamUpstream(t, strings.Join([]string{
		`data: {"choices":[{"delta":{"content":"hello"}}]}`,
		"",
		`data: {"choices":[{"delta":{},"finish_reason":"stop"}]}`,
		"",
		`data: {"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":2}}`,
		"",
		"data: [DONE]",
		"",
	}, "\n"), true)
	defer up.Close()

	proxy := startCalibProxy(t, up.URL, "sess-stream-ok")
	body, _ := postMessagesStream(t, proxy)
	events := sseEvents(body)

	if hasEvent(events, "error") {
		t.Errorf("a clean stream was reported as an error:\n%s", body)
	}
	if !hasEvent(events, "message_stop") {
		t.Errorf("a clean stream lost its message_stop (events: %v)\nbody:\n%s", events, body)
	}
	var delta map[string]any
	for _, line := range strings.Split(body, "\n") {
		line = strings.TrimSpace(line)
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		var ev map[string]any
		if err := json.Unmarshal([]byte(payload), &ev); err != nil {
			continue
		}
		if ev["type"] == "message_delta" {
			delta = ev
		}
	}
	if delta == nil {
		t.Fatalf("no message_delta in:\n%s", body)
	}
	d, _ := delta["delta"].(map[string]any)
	if d == nil || d["stop_reason"] != "end_turn" {
		t.Errorf("message_delta.stop_reason = %v, want \"end_turn\" (Anthropic's name for upstream \"stop\")", delta["delta"])
	}
}
