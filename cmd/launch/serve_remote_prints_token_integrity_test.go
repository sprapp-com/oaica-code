package launch

// serve_remote_prints_token_integrity_test.go — the hidden
// serve-anthropic-proxy harness printed its client token AFTER the blocking
// serve loop, so it never printed it at all (2026-09-26 audit).
//
// RunAnthropicOpenAIProxyRoutes blocks until its listener closes; that is the
// whole point of the subcommand. `fmt.Println(token)` sat after it, so the one
// thing a caller needs to use the proxy was written to stdout only during the
// process's shutdown path — after the process is killed, stdout is gone.
//
// The user-visible symptom was the documented smoke test itself:
//
//	oaica serve-anthropic-proxy --remote deepseek --model deepseek-v4-flash --port 8799
//	curl http://127.0.0.1:8799/v1/messages
//
// 401 "missing or invalid proxy token", with no way to learn the token: the
// proxy listens on loopback, which is shared with every other process on the
// box, so it deliberately accepts nothing else (2026-09-01 security audit H1).
// The token is not optional and not discoverable, so it has to be announced
// before the loop starts.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestServeAnthropicProxyForRemotePrintsAUsableToken runs the real entry point
// and then USES what it printed: the token must be the one the proxy accepts,
// because a token that is merely printed proves nothing about the 401.
func TestServeAnthropicProxyForRemotePrintsAUsableToken(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)

	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"c1","object":"chat.completion","model":"deepseek-v4-flash",` +
			`"choices":[{"index":0,"message":{"role":"assistant","content":"PONG"},"finish_reason":"stop"}],` +
			`"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer upstream.Close()
	writeRemotes(t, fmt.Sprintf(`{"remotes":[{"name":"deepseek","base_url":%q,"api_key":"sk-upstream-real"}]}`, upstream.URL))

	// Hold the listener: the serve loop only returns when it closes.
	origListen := listenProxyForRemote
	var ln net.Listener
	listenProxyForRemote = func(network, addr string) (net.Listener, error) {
		l, err := origListen(network, addr)
		if err == nil {
			ln = l
		}
		return l, err
	}
	defer func() { listenProxyForRemote = origListen }()

	out, err := os.Create(filepath.Join(dir, "stdout"))
	if err != nil {
		t.Fatal(err)
	}
	origStdout := os.Stdout
	os.Stdout = out
	defer func() { os.Stdout = origStdout }()

	go func() { _ = ServeAnthropicProxyForRemote("deepseek", "deepseek-v4-flash", 0) }()
	t.Cleanup(func() {
		if ln != nil {
			_ = ln.Close()
		}
		_ = out.Close()
	})

	port, token := waitForProxyAnnouncement(t, out)
	if token == "" {
		t.Fatalf("no client token on the second stdout line — the proxy rejects every request without it, so the documented curl cannot work")
	}
	proxyURL := "http://127.0.0.1:" + port

	// What it printed has to be what the proxy accepts.
	code, body := postAnthropicProxyMessage(t, proxyURL, token)
	if code != http.StatusOK {
		t.Fatalf("HTTP %d for the token this command printed — the smoke test it exists for is unusable\nbody: %s", code, body)
	}
	if !strings.Contains(body, "PONG") {
		t.Errorf("the translated answer did not come back through the announced port: %s", body)
	}

	// ...and nothing else is, which is why announcing it is load-bearing.
	code, _ = postAnthropicProxyMessage(t, proxyURL, "")
	if code != http.StatusUnauthorized {
		t.Errorf("HTTP %d with no credential, want 401 — an open loopback proxy lets any local process spend the remote's real key", code)
	}
}

// waitForProxyAnnouncement reads the two lines the command must print BEFORE it
// starts blocking, and fails with whatever it printed instead after 5s.
func waitForProxyAnnouncement(t *testing.T, f *os.File) (string, string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		b, _ := os.ReadFile(f.Name())
		lines := strings.Split(strings.TrimRight(string(b), "\n"), "\n")
		if len(lines) >= 2 {
			port := strings.TrimSpace(lines[0])
			if _, err := strconv.Atoi(port); err != nil {
				t.Fatalf("first stdout line is %q, want the chosen port — it is the documented hand-off", lines[0])
			}
			return port, strings.TrimSpace(strings.TrimPrefix(lines[1], "client token:"))
		}
		time.Sleep(10 * time.Millisecond)
	}
	b, _ := os.ReadFile(f.Name())
	t.Fatalf("serve-anthropic-proxy printed %q in 5s, want a port line and a client-token line — anything printed after the serve loop never reaches a caller, because the loop only returns when the process is killed", string(b))
	return "", ""
}

// postAnthropicProxyMessage sends the smallest well-formed /v1/messages request
// with token as the credential (empty = none) and returns status and body.
func postAnthropicProxyMessage(t *testing.T, proxyURL, token string) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": "deepseek-v4-flash", "max_tokens": 16, "stream": false,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("anthropic-version", "2023-06-01")
	if token != "" {
		req.Header.Set("x-api-key", token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request to the announced port failed: %v", err)
	}
	defer resp.Body.Close()
	var buf bytes.Buffer
	_, _ = buf.ReadFrom(resp.Body)
	return resp.StatusCode, buf.String()
}
