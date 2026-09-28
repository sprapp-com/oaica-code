package main

// round89_dead_upstream_determinism_test.go — leg 3 (2026-09-29 audit, round
// 89). Found by the flake hunt for the one unnamed FAIL in the round-89 suite.
//
// Two tests in this package want an upstream that FAILS TO CONNECT, and both
// built it the obvious way: stand up an httptest server, read its URL, close
// it, and comment that the port is "guaranteed connection refused". The port is
// an ephemeral one, and the kernel hands ephemeral ports out in order — so the
// next listener this package creates in the same process is frequently given
// the port that just came free, and the request that was supposed to hit
// nothing is answered by a sibling test's fake upstream.
//
// Measured on six back-to-back runs of this suite at HEAD `ffc099225`, two of
// them failed, on two different tests, in exactly that way:
//
//	run4: TestUpstreamErrorLog_CapturesConnectionLevelFailure — "expected 502
//	      from the dead upstream, got 200"
//	run2: TestHealth_DownWhenUpstreamDead — "health with dead upstream: got
//	      200 want 503"
//
// A 200 from an address nothing is listening on is not a possible reading of a
// closed port; it is another test's handler. Both tests are load-sensitive for
// that reason, which is what makes a green suite a coin flip under parallel
// load.
//
// The fix is to stop asking the kernel for a port and then giving it back:
// deadUpstreamPort is below both the ephemeral range and 1024, so no listener
// this suite creates can be given it, and a non-root process cannot bind it at
// all. The pin below proves both halves of that property rather than trusting
// the comment.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	neturl "net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// deadUpstreamPort is a port nothing listens on and nothing in this suite can
// be handed: below the ephemeral range (so no sibling :0 bind can get it) and
// below 1024 (so an unprivileged test cannot bind it by asking).
const deadUpstreamPort = 1

// deadUpstreamURL is the address of an upstream that cannot answer: it is
// refused for the whole test, whatever else this package binds meanwhile.
// Prefer it to standing up a listener and closing it — see the file note.
func deadUpstreamURL(t *testing.T) string {
	t.Helper()
	url := fmt.Sprintf("http://127.0.0.1:%d", deadUpstreamPort)
	if err := deadUpstreamProblem(url); err != "" {
		t.Fatalf("the dead upstream this test needs is not dead: %s", err)
	}
	return url
}

// deadUpstreamProblem reports why `url` might not stay dead, or "" when it is
// the address this file promises: refused, unwritable by this process, and
// outside the range the kernel hands out. Every property is asked of the port
// the URL actually names — a check written against the constant instead passed
// the closed httptest listener this file exists to replace, which is a pin that
// cannot fail (2026-09-29 audit, round 89).
func deadUpstreamProblem(rawURL string) string {
	u, err := neturl.Parse(rawURL)
	if err != nil {
		return fmt.Sprintf("%q is not a URL: %v", rawURL, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return fmt.Sprintf("%q names no port (%v)", rawURL, err)
	}
	if low := ephemeralLow(); port >= low {
		return fmt.Sprintf("port %d is inside the ephemeral range (>= %d), so a listener in this process can be given it once it comes free", port, low)
	}
	if port >= 1024 {
		return fmt.Sprintf("port %d is unprivileged, so a test could bind it", port)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 2*time.Second)
	if err == nil {
		conn.Close()
		return fmt.Sprintf("something is already listening on %s", u.Host)
	}
	return ""
}

// ephemeralLow is the bottom of the range the kernel allocates ephemeral ports
// from. A port below it cannot be handed to a `:0` bind. Falls back to 1024 —
// the privileged boundary, which is a stricter bound than any ephemeral range —
// when the range cannot be read.
func ephemeralLow() int {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 1024
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 1024
	}
	low, err := strconv.Atoi(f[0])
	if err != nil || low < 1024 {
		return 1024
	}
	return low
}

// TestTheDeadUpstreamsOfThisSuiteStayDead is the pin. It holds the two
// properties the two fixed tests depend on, and it holds the mechanism: a
// sibling `:0` listener's port really does come from the range deadUpstreamPort
// is below, which is the reason the old construction could be answered by it.
func TestTheDeadUpstreamsOfThisSuiteStayDead(t *testing.T) {
	// Asked through the helper the two fixed tests call, so that the helper's
	// own construction is what this pin holds to the property — not a constant
	// restated beside it.
	url := deadUpstreamURL(t)
	if p := deadUpstreamProblem(url); p != "" {
		t.Errorf("the address these tests call a dead upstream is not reliably dead: %s (2026-09-29 audit, round 89)", p)
	}

	// The mechanism, measured rather than assumed: a listener this package
	// creates with :0 is given a port from the range the dead one is below.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if port < ephemeralLow() {
		t.Errorf("a sibling listener got port %d, below the ephemeral range %d — the range this pin reasons about is not the one this kernel allocates from", port, ephemeralLow())
	}

	// And end to end: the gateway refuses a turn against it, which is what the
	// two tests that use it assert.
	g, _ := newTestGateway(t, url)
	srv := httptest.NewServer(mux(g))
	defer srv.Close()
	body, _ := json.Marshal(map[string]any{
		"model":      "kat-awq",
		"messages":   []map[string]any{{"role": "user", "content": "hello"}},
		"max_tokens": 10,
	})
	req, err := http.NewRequest("POST", srv.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer sk-new")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Errorf("a turn against an upstream that cannot be dialled answered %d, want 502 — a port nothing listens on cannot answer 200, and a 200 here means something else was given it (2026-09-29 audit, round 89)", resp.StatusCode)
	}
}
