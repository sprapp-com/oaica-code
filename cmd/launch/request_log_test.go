package launch

// request_log_test.go — the logging proxy takes its target from
// oaicaResolveHostForModel, which returns OAICA_HOST verbatim. A host
// configured as https://key@api.oaica.com (the same userinfo shape
// remotes.json leaked) would otherwise reach two places: the Backend field
// written to ~/.oaica/requests.log, and the 502 body handed back to the
// caller.

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLocalLoggingProxy_RedactsCredentialInTarget(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var gotAuth string
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x"}`))
	}))
	defer up.Close()

	target := strings.Replace(up.URL, "http://", "http://"+redactKey+"@", 1) + "/v1"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunLocalLoggingProxy(ln, target) }()
	defer ln.Close()

	body := `{"model":"kat-awq","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("proxy status %d", resp.StatusCode)
	}
	// The key must still reach upstream as Basic auth — redaction is for
	// text we render, not for the request we send.
	if !strings.HasPrefix(gotAuth, "Basic ") {
		t.Fatalf("userinfo no longer became Basic auth upstream: %q", gotAuth)
	}

	path, err := RequestLogPath()
	if err != nil {
		t.Fatal(err)
	}
	// The row is appended after the relay finishes, and the relay now flushes
	// as it goes (proxy_stream_flush_integrity_test.go): the client can have
	// read its whole response while the handler is still on its way to the
	// log. Poll rather than assume the old buffering's ordering.
	var raw []byte
	deadline := time.Now().Add(3 * time.Second)
	for {
		raw, err = os.ReadFile(path)
		if err == nil && strings.Contains(string(raw), `"backend"`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no row appeared in %s: %v (%q)", path, err, raw)
		}
		time.Sleep(20 * time.Millisecond)
	}
	if strings.Contains(string(raw), redactKey) {
		t.Fatalf("requests.log leaked the key:\n%s", raw)
	}
	if !strings.Contains(string(raw), `"backend":"http://REDACTED@`) {
		t.Fatalf("backend should stay readable, redacted: %s", raw)
	}
}

func TestLocalLoggingProxy_UnreachableTargetErrorIsRedacted(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	// 127.0.0.1:1 refuses instantly; the transport error quotes the URL.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunLocalLoggingProxy(ln, "http://"+redactKey+"@127.0.0.1:1/v1") }()
	defer ln.Close()

	body := `{"model":"m","messages":[{"role":"user","content":"hi"}]}`
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("status %d, body %s", resp.StatusCode, out)
	}
	if strings.Contains(string(out), redactKey) {
		t.Fatalf("502 body leaked the key: %s", out)
	}
	if !strings.Contains(string(out), "REDACTED@127.0.0.1:1") {
		t.Fatalf("error should name the host with the credential redacted: %s", out)
	}
}
