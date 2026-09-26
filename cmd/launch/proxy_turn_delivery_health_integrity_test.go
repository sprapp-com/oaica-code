package launch

// proxy_turn_delivery_health_integrity_test.go — a leg that never delivered a
// turn was recorded as a HEALTHY one (2026-09-26 audit).
//
// The translated path fed the circuit breaker straight off the upstream's
// STATUS (anthropic_openai_proxy.go's "case resp.StatusCode < 300"), before a
// byte of the body was looked at. But a 2xx is not a turn: this proxy already
// recognises two shapes where the leg answers 200 and the turn is dead anyway
// — a plain JSON error object over HTTP 200 (the shape upstreamErrorMessage
// exists for), and a stream that ends after the headers without ever saying the
// answer was complete. Both were relayed to the client as a failure and
// recorded against the leg as a success, so a leg that can never finish a turn
// kept a perfect health record: its circuit never opened and, under `auto`, the
// session was never escalated off it. The user sees the same broken turn
// forever while `oaica usage` shows a clean session.
//
// The client-hangup case is the opposite conclusion (see
// route_health_client_cancel_integrity_test.go) and is deliberately excluded.

import (
	"bufio"
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// postStreamMessage posts a /v1/messages turn with stream set as asked and
// returns the status plus the body.
func postStreamMessage(t *testing.T, proxy, model string, stream bool) (int, string) {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": model, "max_tokens": 16, "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(raw)
}

// translatedLegProxy points a translated leg at an arbitrary upstream handler
// and returns the table and the proxy URL.
func translatedLegProxy(t *testing.T, sessionID string, h http.HandlerFunc) (proxyRouteTable, string) {
	t.Helper()
	up := httptest.NewServer(h)
	t.Cleanup(up.Close)
	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai", BaseURL: up.URL + "/v1", Token: "sk", UpstreamModel: "glm-5.3", Wire: "openai",
	}})
	table := proxyRouteTable{
		Default: route, Policy: RouteAuto, SessionID: sessionID,
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
	return table, startProxyWithOversize(t, table)
}

// The two shapes of "200, and no turn": a JSON error object over HTTP 200, and
// a stream that stops after a frame without a finish_reason or [DONE].
func TestALegThatNeverDeliversTheTurnIsNotRecordedAsHealthy(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	t.Run("an error object over HTTP 200", func(t *testing.T) {
		table, proxy := translatedLegProxy(t, "sess-http200-error", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, `{"error":{"message":"prefill failed","type":"internal_error"}}`)
		})

		code, _ := postStreamMessage(t, proxy, "glm-5.3", true)
		if code == http.StatusOK {
			t.Fatalf("premise: a 200 carrying an error object was relayed to the client as %d", code)
		}

		fails := breakerFails(table, table.Default.BaseURL)
		if fails == 0 {
			t.Errorf("a leg that answered HTTP 200 with an error object instead of a turn was recorded as a SUCCESS — %d of these (breakerFailsToOpen) never open its circuit, and under `auto` two never escalate the session off a leg that cannot complete a turn",
				breakerFailsToOpen)
		}
		if esc := escalationFails(table, table.SessionID); esc == 0 {
			t.Errorf("the same dead turn did not count toward the `auto` escalation of session %q (arms at %d)", table.SessionID, autoEscalateAfterFails)
		}
	})

	t.Run("a stream that ends mid-answer", func(t *testing.T) {
		table, proxy := translatedLegProxy(t, "sess-torn-stream", func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			w.WriteHeader(http.StatusOK)
			io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half an ans\"}}]}\n\n")
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			// The upstream dies here: no finish_reason, no [DONE], no error.
		})

		code, body := postStreamMessage(t, proxy, "glm-5.3", true)
		if code != http.StatusOK {
			t.Fatalf("premise: the torn stream's headers were a %d, so the client did not see the mid-stream failure this test is about", code)
		}
		if !strings.Contains(body, "half an ans") || !strings.Contains(body, `"error"`) {
			t.Fatalf("premise: the client was not told the stream was cut short (body: %q)", body)
		}

		if fails := breakerFails(table, table.Default.BaseURL); fails == 0 {
			t.Errorf("a stream that ended before the answer was complete was recorded as a healthy turn for %s — the client got an error event and the breaker got a success, so the leg's record never shows the failure the user saw on every turn", table.Default.BaseURL)
		}
	})
}

// The control: an ordinary complete stream still proves the leg healthy, so
// the tests above cannot be passed by failing every leg.
func TestACompletedStreamStillRecordsTheLegHealthy(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	table, proxy := translatedLegProxy(t, "sess-healthy-stream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hi\"}}]}\n\n")
		io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\n")
		io.WriteString(w, "data: [DONE]\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})
	// A failure already on the books, so the OK is observable as a reset.
	table.breakers.recordFail(table.Default.BaseURL)
	table.escalations.recordFail(table.SessionID, table.Default.BaseURL)

	code, _ := postStreamMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("premise: a complete stream answered %d", code)
	}
	if fails := breakerFails(table, table.Default.BaseURL); fails != 0 {
		t.Errorf("a complete turn left %d consecutive failures on the leg — the leg worked, and a fix that fails every 2xx would make every session escalate off a healthy primary", fails)
	}
	if esc := escalationFails(table, table.SessionID); esc != 0 {
		t.Errorf("a complete turn did not clear the session's failure streak (%d)", esc)
	}
}

// The non-streaming sibling of the same defect, at the same call site.
func TestANonStreamedErrorObjectOverTwoHundredIsNotHealthy(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	table, proxy := translatedLegProxy(t, "sess-http200-error-nonstream", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, `{"error":{"message":"CUDA out of memory"}}`)
	})

	code, _ := postStreamMessage(t, proxy, "glm-5.3", false)
	if code == http.StatusOK {
		t.Fatalf("premise: a 200 carrying an error object was relayed as %d", code)
	}
	if fails := breakerFails(table, table.Default.BaseURL); fails == 0 {
		t.Errorf("a non-streaming turn answered with an error object over HTTP 200 was recorded as a success for %s", table.Default.BaseURL)
	}
}

// truncatedUpstream answers every request with HTTP 200, a Content-Length it
// never satisfies, and a body that stops in the middle — a leg whose response
// dies after the headers. A raw listener, not an httptest handler: an HTTP
// server that finishes its handler produces a WELL-FORMED truncated response
// (the client gets a clean EOF), which is not the failure this is about.
func truncatedUpstream(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				br := bufio.NewReader(c)
				for {
					line, err := br.ReadString('\n')
					if err != nil || strings.TrimSpace(line) == "" {
						break
					}
				}
				io.WriteString(c, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nContent-Length: 4096\r\n\r\n")
				io.WriteString(c, "event: message_start\ndata: {\"type\":\"message_start\"}\n\n")
			}(c)
		}
	}()
	t.Cleanup(func() { ln.Close() })
	return "http://" + ln.Addr().String()
}

// The passthrough (Anthropic-wire) leg, at its own call site: it reports the
// upstream's status and nothing about whether the body arrived.
func TestAPassthroughLegThatNeverDeliversTheTurnIsNotHealthy(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai-coding-plan", BaseURL: truncatedUpstream(t), Token: "sk-remote", UpstreamModel: "glm-5.3", Wire: "anthropic",
	}})
	if !route.NativePassthrough {
		t.Fatalf("setup: an anthropic-wire remote is no longer NativePassthrough: %+v", route)
	}
	table := proxyRouteTable{
		Default: route, Policy: RouteAuto, SessionID: "sess-torn-passthrough",
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
	proxy := startProxyWithOversize(t, table)

	code, body := postStreamMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("premise: the truncated passthrough response was relayed as %d, so the client did not see the mid-stream failure this test is about", code)
	}
	if !strings.Contains(body, "message_start") {
		t.Fatalf("premise: nothing of the truncated body reached the client (body: %q)", body)
	}

	if fails := breakerFails(table, route.BaseURL); fails == 0 {
		t.Errorf("a passthrough leg whose response died in the middle was recorded as a healthy turn — the status byte was a 200 and the body never finished, which is the same dead turn the client saw")
	}
}

// The request-log row of a turn that failed after the headers must not read as
// a clean 200 either: `oaica usage` counts its ERR column off exactly this
// field, so a session in which every turn was cut short reported ERR 0.
func TestAFailedTurnIsLoggedAsAFailureEvenAfterTheHeaders(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	_, proxy := translatedLegProxy(t, "sess-log-torn", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		io.WriteString(w, "data: {\"id\":\"1\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"half\"}}]}\n\n")
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	})

	postStreamMessage(t, proxy, "glm-5.3", true)

	rows := readRequestLogRows(t)
	if len(rows) == 0 {
		t.Fatalf("no request-log row was written at all")
	}
	last := rows[len(rows)-1]
	if last.StatusCode < 400 {
		t.Errorf("a turn whose stream was cut short was logged with status_code %d — the client got an error event, and `oaica usage`'s ERR column counts rows below 400 as successes, so a session in which every turn was cut short reads clean", last.StatusCode)
	}
}
