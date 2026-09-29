package launch

// round110_leg2_rebinding_test.go — leg 2, round 110 (2026-09-29 audit), F110-L2-1.
//
// The local server refuses a request whose Host header is a foreign domain
// (allowedHostsMiddleware), and the two loopback proxies in front of the same
// model — the `oaica serve` normalizing proxy, which runs with no API key by
// default, and the per-launch logging proxy — answered any Host at all. A web page
// the operator visits can rebind its own domain to 127.0.0.1 and then read
// completions from the local model and spend its compute, with no credential, on a
// port that is well known. On a loopback bind a request whose Host names a foreign
// DOMAIN is now refused; an IP literal (a rebinding page cannot make a browser send
// one), localhost and the local TLDs the server also accepts are not.

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

func r110GetWithHost(t *testing.T, base, host string) int {
	t.Helper()
	req, _ := http.NewRequest("GET", base+"/v1/models", nil)
	req.Host = host
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.StatusCode
}

func TestMine110TheLoopbackProxiesRefuseAForeignDomainHost(t *testing.T) {
	backend := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[]}`)
	}))
	t.Cleanup(backend.Close)
	bport := backend.Listener.Addr().(*net.TCPAddr).Port

	// The logging proxy, on a loopback listener.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = RunLocalLoggingProxy(ln, backend.URL) }()
	// The normalizing proxy, on a port picked here.
	pl, _ := net.Listen("tcp", "127.0.0.1:0")
	pport := pl.Addr().(*net.TCPAddr).Port
	pl.Close()
	go func() { _ = RunNormalizingProxyOn("127.0.0.1", pport, bport, "") }()

	for name, base := range map[string]string{
		"logging proxy":     "http://" + ln.Addr().String(),
		"normalizing proxy": "http://127.0.0.1:" + strconv.Itoa(pport),
	} {
		r110WaitForPort(t, strings.TrimPrefix(base, "http://"))
		for _, host := range []string{"evil.example.com", "evil.example.com:8080", "rebind.attacker.io"} {
			if code := r110GetWithHost(t, base, host); code != http.StatusForbidden {
				t.Errorf("%s: Host %q answered %d, want 403 — a foreign domain on a loopback bind is how DNS rebinding reaches the local model (2026-09-29 audit, round 110, F110-L2-1)", name, host, code)
			}
		}
		for _, host := range append([]string{"", "127.0.0.1:1234", "localhost:1234", "[::1]:1234", "192.168.0.7", "box.local", "svc.internal", "app.localhost"}, machineName()...) {
			if code := r110GetWithHost(t, base, host); code != http.StatusOK {
				t.Errorf("%s: Host %q answered %d, want 200 — a local Host must keep working (2026-09-29 audit, round 110, F110-L2-1)", name, host, code)
			}
		}
	}
}

func r110WaitForPort(t *testing.T, addr string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if c, err := net.Dial("tcp", addr); err == nil {
			c.Close()
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("nothing listened on %s", addr)
}

func machineName() []string {
	if n, err := os.Hostname(); err == nil && n != "" {
		return []string{n, strings.ToUpper(n) + ":1234"}
	}
	return nil
}

// TestMine110ANetworkFacingBindIsNotGuardedByHost is the control: a listener that
// is not on loopback is network-facing on purpose and is behind its bearer check.
func TestMine110ANetworkFacingBindIsNotGuardedByHost(t *testing.T) {
	h := rebindingGuard(false, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(204) }))
	req := httptest.NewRequest("GET", "/x", nil)
	req.Host = "evil.example.com"
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	if rec.Code != 204 {
		t.Errorf("a non-loopback bind answered %d for a foreign Host, want the handler's own 204 (2026-09-29 audit, round 110, F110-L2-1)", rec.Code)
	}
}
