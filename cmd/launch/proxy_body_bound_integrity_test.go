package launch

// proxy_body_bound_integrity_test.go — the proxy buffered whole bodies whose
// size the other end chooses (2026-09-26 audit).
//
// io.ReadAll on an upstream response, or on the client's request, allocates
// whatever the sender sends. An upstream that answers a large request with a
// wrong Content-Length, or a client that posts an enormous body, turned a
// routine turn into this process's peak memory — and a proxy that dies is the
// launched session dying. Both reads now go through httpbody.ReadCapped; this
// is the end-to-end evidence that the bound is applied on the real path, with
// the error the user sees.

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ollama/ollama/cmd/internal/httpbody"
)

// endless is a reader that never ends, so the test does not have to hold the
// oversized body in memory to prove the bound.
type endless struct{}

func (endless) Read(p []byte) (int, error) {
	for i := range p {
		p[i] = 'x'
	}
	return len(p), nil
}

// An upstream answer larger than the cap is refused with an error, not
// buffered.
func TestAnOversizedUpstreamBodyIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("writes a few hundred MiB over loopback")
	}
	setLaunchTestHome(t, t.TempDir())

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = io.CopyN(w, endless{}, httpbody.DefaultMax+1)
	}))
	t.Cleanup(up.Close)

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai", BaseURL: up.URL + "/v1", Token: "sk", UpstreamModel: "glm-5.3", Wire: "openai",
	}})
	table := proxyRouteTable{
		Default: route, Policy: RouteAuto, SessionID: "sess-oversize-body",
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
	proxy := startProxyWithOversize(t, table)

	code, body := postStreamMessage(t, proxy, "glm-5.3", false)
	if code == http.StatusOK {
		t.Fatalf("an upstream body past the %d-byte cap was relayed as a success (%d bytes of it)", httpbody.DefaultMax, len(body))
	}
	if !strings.Contains(body, "larger than") {
		t.Errorf("the client was not told the body exceeded the bound (status %d, body %q)", code, truncateForLog(body, 200))
	}
}

// And a request body larger than the cap is refused at the door.
func TestAnOversizedRequestBodyIsRefused(t *testing.T) {
	if testing.Short() {
		t.Skip("posts a few hundred MiB over loopback")
	}
	setLaunchTestHome(t, t.TempDir())

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an over-limit request was forwarded upstream instead of being refused")
	}))
	t.Cleanup(up.Close)

	route := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "zai", BaseURL: up.URL + "/v1", Token: "sk", UpstreamModel: "glm-5.3", Wire: "openai",
	}})
	table := proxyRouteTable{
		Default: route, Policy: RouteAuto, SessionID: "sess-oversize-request",
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
	proxy := startProxyWithOversize(t, table)

	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", io.LimitReader(endless{}, httpbody.DefaultMax+1))
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
	if resp.StatusCode == http.StatusOK {
		t.Errorf("a request body past the cap was accepted and forwarded")
	}
	if !strings.Contains(string(raw), "larger than") {
		t.Errorf("the refusal does not name the bound (status %d, body %q)", resp.StatusCode, truncateForLog(string(raw), 200))
	}
}

func truncateForLog(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}
