package launch

// Round 126 leg 1 (2026-09-29 audit): the `oaica serve` proxy on a loopback bind refuses web pages from
// other origins (F126-L1-1) and presents its own key to the backend (F126-L1-2).

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
)

func round126Proxy(t *testing.T, apiKey, backendKey string) (base string, hits *atomic.Int32, lastAuth *atomic.Value) {
	hits, lastAuth = &atomic.Int32{}, &atomic.Value{}
	be := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.Copy(io.Discard, r.Body)
		hits.Add(1)
		lastAuth.Store(r.Header.Get("Authorization"))
		w.Header().Set("Access-Control-Allow-Origin", r.Header.Get("Origin"))
		w.Write([]byte(`{"choices":[{"message":{"content":"secret completion"}}]}`))
	}))
	t.Cleanup(be.Close)
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	go func() {
		_ = RunNormalizingProxyOnKeyed("127.0.0.1", port, portOfURL(t, be.URL), apiKey, backendKey)
	}()
	waitForListener(t, "127.0.0.1", port)
	return "http://127.0.0.1:" + strconv.Itoa(port), hits, lastAuth
}

func TestRound126ServeProxyRefusesForeignWebPages(t *testing.T) {
	base, hits, _ := round126Proxy(t, "", "")
	do := func(origin, site string) int {
		req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`))
		req.Header.Set("Content-Type", "text/plain")
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		if site != "" {
			req.Header.Set("Sec-Fetch-Site", site)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, c := range []struct {
		origin, site string
		want         int
	}{
		{"https://evil.example", "cross-site", 403},
		{"https://evil.example", "", 403},
		{"null", "", 403},
		{"", "cross-site", 403},
		{"http://localhost:3000", "same-site", 200},
		{"http://127.0.0.1:8080", "", 200},
		{"", "", 200}, // curl, an SDK: no Origin at all
	} {
		before := hits.Load()
		if got := do(c.origin, c.site); got != c.want {
			t.Errorf("Origin %q Sec-Fetch-Site %q -> %d, want %d", c.origin, c.site, got, c.want)
		}
		if c.want == 403 && hits.Load() != before {
			t.Errorf("Origin %q reached the backend", c.origin)
		}
	}
}

func TestRound126ServeProxyPresentsItsOwnKeyToTheBackend(t *testing.T) {
	base, _, lastAuth := round126Proxy(t, "outer-key", "inner-backend-key")
	req, _ := http.NewRequest("POST", base+"/v1/chat/completions", strings.NewReader(`{"model":"m","messages":[]}`))
	req.Header.Set("Authorization", "Bearer outer-key")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if got, _ := lastAuth.Load().(string); got != "Bearer inner-backend-key" {
		t.Errorf("the backend saw Authorization %q, want the proxy's own backend key", got)
	}
}

// ServeHandler must start llama-server with that key in its environment (never in argv) and hand it to the proxy.
func TestRound126ServeHandlerWiresTheBackendKey(t *testing.T) {
	b, err := os.ReadFile("../oaica_pull_serve.go")
	if err != nil {
		t.Fatal(err)
	}
	src := string(b)
	for _, want := range []string{`"LLAMA_API_KEY="+backendKey`, "RunNormalizingProxyOnKeyed(bindHost, port, internalPort, apiKey, backendKey)"} {
		if !strings.Contains(src, want) {
			t.Errorf("oaica_pull_serve.go does not contain %s", want)
		}
	}
	if strings.Contains(src, `"--api-key", backendKey`) {
		t.Errorf("the backend key is on the command line")
	}
}
