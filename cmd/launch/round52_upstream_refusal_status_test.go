package launch

// round52_upstream_refusal_status_test.go — round 52's finding on the client
// leg's error path.
//
// Every non-429/503/529 upstream status was collapsed to a 502, which every
// SDK reads as an outage and retries: a 400 naming a field the client got wrong
// was re-sent with the whole prompt each time and the client could not tell the
// cause. The upstream is oaica's own door and its own 4xx verdict is the class
// every SDK treats as terminal, so the 4xx status passes through — except 401
// and 403, which Claude Code would read as its own authentication failure and
// answer with its login flow (2026-09-27 audit, round 52).

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestUpstreamRefusalKeepsItsOwnStatus(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	for _, tc := range []struct {
		upstream int
		want     int
	}{
		{http.StatusBadRequest, http.StatusBadRequest},
		{http.StatusUnprocessableEntity, http.StatusUnprocessableEntity},
		{http.StatusTooManyRequests, http.StatusTooManyRequests},
		{http.StatusUnauthorized, http.StatusBadGateway},
		{http.StatusForbidden, http.StatusBadGateway},
		{http.StatusInternalServerError, http.StatusBadGateway},
	} {
		// A 1s hint for the statuses the proxy backs off on: the forwarded
		// value is what this checks, not the duration.
		upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Retry-After", "1")
			w.WriteHeader(tc.upstream)
			w.Write([]byte(`{"error":{"message":"max_tokens must be a positive integer"}}`))
		}))
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: proxyRoute{
			BaseURL: upstream.URL + "/v1", UpstreamModel: "kat-awq", Label: "box/kat-awq",
		}})
		body := `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		raw, _ := io.ReadAll(resp.Body)
		resp.Body.Close()
		ln.Close()
		upstream.Close()

		if resp.StatusCode != tc.want {
			t.Errorf("upstream %d -> %d, want %d (body %s)\na 4xx the client cannot fix by retrying must not be re-sent with the whole prompt, and a 401/403 must not look like the client's own credentials\n",
				tc.upstream, resp.StatusCode, tc.want, raw)
		}
		if !strings.Contains(string(raw), "upstream HTTP") {
			t.Errorf("upstream %d: the refusal does not name the upstream status: %s", tc.upstream, raw)
		}
	}
}

// TestUpstreamClientErrorIsNotRetried is the same finding from the other side:
// a 400 that can never succeed must not be re-sent. The counter is the point —
// a retried refusal re-billed the whole prompt.
func TestUpstreamClientErrorIsNotRetried(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var hits int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":{"message":"tools[0].input_schema is not an object"}}`))
	}))
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go RunAnthropicOpenAIProxyRoutes(ln, proxyRouteTable{Default: proxyRoute{
		BaseURL: upstream.URL + "/v1", UpstreamModel: "kat-awq", Label: "box/kat-awq",
	}})
	body := `{"model":"kat-awq","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()

	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status %d, want 400", resp.StatusCode)
	}
	if hits != 1 {
		t.Errorf("the upstream was asked %d times for a body it refuses; a refusal that cannot succeed was re-sent with the whole prompt", hits)
	}
}
