package launch

// round106_leg2_passthrough_error_frame_test.go — leg 2, round 106 (2026-09-29
// audit), F106-L2-1.
//
// The translated arm reports a turn whose stream ended in an error frame as NOT
// delivered: a 502 request-log row, and a recordFail on the route's breaker and
// the session's escalation. The Anthropic-wire passthrough relays bytes verbatim
// and counted any body relayed to a clean EOF as delivered, so an upstream that
// answers 200, opens a message and ends it with `event: error`
// (overloaded_error — how Anthropic reports a mid-stream overload) had a perfect
// health record: a 200 row, recordOK, a breaker that never opened and an `auto`
// escalation that never fired, while the client saw the failure. The relay stays
// byte-for-byte; a bounded line watcher only READS it.

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

const r106AnthropicErrorStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"glm-5.3","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
	"event: error\n" +
	`data: {"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}` + "\n\n"

const r106AnthropicOKStream = "event: message_start\n" +
	`data: {"type":"message_start","message":{"id":"m","type":"message","role":"assistant","model":"glm-5.3","content":[],"usage":{"input_tokens":1,"output_tokens":0}}}` + "\n\n" +
	"event: message_stop\n" +
	`data: {"type":"message_stop"}` + "\n\n"

// r106Turns runs four streamed turns against an Anthropic-wire upstream that
// answers `raw`, and returns whether the breaker opened and the logged statuses.
func r106Turns(t *testing.T, raw string) (breakerOpen bool, statuses string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte(raw))
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "glm-5.3", "anthropic")
	br := &routeBreakers{}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() {
		_ = RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: route, ByModel: map[string]proxyRoute{"glm-5.3": route}, breakers: br})
	}()
	for i := 0; i < 4; i++ {
		round54PostMessage(t, "http://"+ln.Addr().String(), "glm-5.3", true)
	}
	lp, err := RequestLogPath()
	if err != nil {
		t.Fatal(err)
	}
	// The row is appended when the handler returns, which is after the client
	// has its response, so wait for all four rather than read a partial log.
	for deadline := time.Now().Add(3 * time.Second); ; {
		statuses = ""
		b, _ := os.ReadFile(lp)
		rows := 0
		for _, l := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			if i := strings.Index(l, `"status_code":`); i >= 0 {
				statuses += strings.SplitN(l[i+len(`"status_code":`):], ",", 2)[0] + " "
				rows++
			}
		}
		if rows >= 4 || time.Now().After(deadline) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	statuses = strings.TrimSpace(statuses)
	return br.forURL(route.BaseURL).open(), statuses
}

// TestMine106AnErrorFrameEndsThePassthroughTurnAsUndelivered is F106-L2-1's pin.
func TestMine106AnErrorFrameEndsThePassthroughTurnAsUndelivered(t *testing.T) {
	open, statuses := r106Turns(t, r106AnthropicErrorStream)
	if !open || statuses != "502 502 502 502" {
		t.Errorf("breakerOpen=%v rows=%q, want the breaker open and 502 rows — a stream that ends in an error frame is a dead turn the client saw, as on the translated arm (2026-09-29 audit, round 106, F106-L2-1)", open, statuses)
	}
}

// TestMine106ACleanPassthroughStreamStaysDelivered is the control: the watcher
// reads a stream that ends normally as delivered.
func TestMine106ACleanPassthroughStreamStaysDelivered(t *testing.T) {
	open, statuses := r106Turns(t, r106AnthropicOKStream)
	if open || statuses != "200 200 200 200" {
		t.Errorf("breakerOpen=%v rows=%q, want the breaker closed and 200 rows for a clean stream (2026-09-29 audit, round 106, F106-L2-1)", open, statuses)
	}
}

// TestMine106EitherSignalOfAnErrorFrameIsEnough pins each half of the watcher on
// its own: the `event: error` line with a payload that is not the error object,
// and the error object with no event line.
func TestMine106EitherSignalOfAnErrorFrameIsEnough(t *testing.T) {
	start := strings.SplitN(r106AnthropicErrorStream, "event: error", 2)[0]
	for name, raw := range map[string]string{
		"event line alone":   start + "event: error\ndata: {\"note\":\"x\"}\n\n",
		"data object alone":  start + "data: {\"type\": \"error\", \"error\": {\"type\":\"overloaded_error\"}}\n\n",
		"whole body, no SSE": `{"type":"error","error":{"type":"overloaded_error","message":"x"}}`,
	} {
		if open, statuses := r106Turns(t, raw); !open || statuses != "502 502 502 502" {
			t.Errorf("%s: breakerOpen=%v rows=%q, want open and 502 rows (2026-09-29 audit, round 106, F106-L2-1)", name, open, statuses)
		}
	}
}
