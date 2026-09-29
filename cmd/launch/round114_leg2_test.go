package launch

// round114_leg2_test.go — leg 2, round 114 (2026-09-29 audit), F114-L2-2 and F114-L2-3.

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// F114-L2-2: on the translated (openai-wire) arm an upstream stream that resets after the first
// frame ends with an `event: error` frame ("upstream stream failed: ..."). The anthropic-wire
// passthrough relays bytes and on a non-EOF read error only set delivered=false and returned, so the
// client got a 200 whose body ended after message_start with a normally terminated chunked body: no
// error frame and no message_stop, a truncated stream it cannot attribute. The same upstream failure
// now reads the same on both wires.
func TestMine114AMidStreamResetOnTheAnthropicWireIsAnErrorFrame(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = w.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"m\",\"content\":[],\"usage\":{\"input_tokens\":1,\"output_tokens\":0}}}\n\n"))
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetLinger(0) // RST, not a clean FIN
		}
		conn.Close()
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "m", "anthropic")
	url := r108Serve(t, proxyRouteTable{Default: route, ByModel: map[string]proxyRoute{"m": route}})
	code, body := r108Message(t, url, 16)
	if code != 200 {
		t.Fatalf("premise: the reset turn answered %d %s", code, body)
	}
	if !strings.Contains(body, "message_start") {
		t.Fatalf("premise: the first frame was not relayed: %q", body)
	}
	// The frame is valid JSON, whatever the transport error's text holds.
	if i := strings.Index(body, "event: error\ndata: "); i >= 0 {
		line := strings.SplitN(body[i+len("event: error\ndata: "):], "\n", 2)[0]
		var v struct {
			Type  string `json:"type"`
			Error struct {
				Type    string `json:"type"`
				Message string `json:"message"`
			} `json:"error"`
		}
		if err := json.Unmarshal([]byte(line), &v); err != nil || v.Type != "error" || v.Error.Type != "api_error" {
			t.Errorf("the error frame is not valid Anthropic error JSON: %q (%v) (2026-09-29 audit, round 114, F114-L2-2)", line, err)
		}
	}
	if !strings.Contains(body, "event: error") || !strings.Contains(body, "upstream stream failed") {
		t.Errorf("the relayed stream ends %q — a mid-stream reset must be stated to the client as an error frame, as the translated arm states it (2026-09-29 audit, round 114, F114-L2-2)", body[max(0, len(body)-160):])
	}
}

// A stream that ends cleanly is not given an error frame.
func TestMine114ACleanAnthropicStreamGetsNoErrorFrame(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		// Chunked, not a Content-Length body: an extra frame written after a short fixed-length body
		// is refused by net/http and could never be observed.
		half := len(r106AnthropicOKStream) / 2
		_, _ = w.Write([]byte(r106AnthropicOKStream[:half]))
		w.(http.Flusher).Flush()
		time.Sleep(30 * time.Millisecond)
		_, _ = w.Write([]byte(r106AnthropicOKStream[half:]))
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "m", "anthropic")
	url := r108Serve(t, proxyRouteTable{Default: route, ByModel: map[string]proxyRoute{"m": route}})
	code, body := r108Message(t, url, 16)
	if code != 200 || strings.Contains(body, "event: error") {
		t.Errorf("a clean stream answered %d %q (2026-09-29 audit, round 114, F114-L2-2)", code, body)
	}
}

// The frame belongs to an event stream only: a reset in the middle of a JSON body is not given SSE text.
func TestMine114AResetInsideAJSONBodyGetsNoEventFrame(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(200)
		_, _ = w.Write([]byte(`{"id":"m","type":"message"`))
		w.(http.Flusher).Flush()
		time.Sleep(50 * time.Millisecond)
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		if tc, ok := conn.(*net.TCPConn); ok {
			tc.SetLinger(0)
		}
		conn.Close()
	}))
	t.Cleanup(up.Close)
	route := round54Route(up.URL, "m", "anthropic")
	url := r108Serve(t, proxyRouteTable{Default: route, ByModel: map[string]proxyRoute{"m": route}})
	_, body := r108Message(t, url, 16)
	if strings.Contains(body, "event: error") {
		t.Errorf("SSE text was written into a JSON body: %q (2026-09-29 audit, round 114, F114-L2-2)", body)
	}
}
