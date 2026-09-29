package launch

// Round 121 leg 2 (2026-09-29 audit): OAICA_ADMIN_KEY and OAICA_AGENT_HOST do not reach a child
// (F121-L2-1/2); the network-facing serve proxy drops a half-sent request (F121-L2-3); a catalog
// row whose base URL is a template is expanded from non-secret names or not offered (F121-L2-4).

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestRound121AdminKeyAndAgentHostDoNotReachAChild(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OAICA_ADMIN_KEY", "sk-admin-secret")
	t.Setenv("OAICA_AGENT_HOST", "https://sk-agent-secret@side.example")
	plan := tierPlan{Primary: launchEndpoint{Source: sourceRouter, RemoteEndpoint: RemoteEndpoint{Name: "oaica", TokenEnv: "OAICA_API_KEY"}}}
	for name, env := range map[string][]string{"directLaunchEnv": directLaunchEnv(), "claude childEnv": plan.childEnv("http://127.0.0.1:1", "tok"), "openclawEnv": openclawEnv()} {
		joined := strings.Join(env, "\n")
		if strings.Contains(joined, "sk-admin-secret") || strings.Contains(joined, "sk-agent-secret") {
			t.Errorf("%s: a credential reached the child", name)
		}
	}
}

func TestRound121ServeProxyDropsAHalfSentRequest(t *testing.T) {
	old := normalizingProxyHeaderTimeout
	normalizingProxyHeaderTimeout = 300 * time.Millisecond
	t.Cleanup(func() { normalizingProxyHeaderTimeout = old })
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close()
	go func() { _ = RunNormalizingProxyOn("127.0.0.1", port, 1, "") }()
	waitForListener(t, "127.0.0.1", port)
	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	c.Write([]byte("POST /v1/chat/completions HTTP/1.1\r\nHost: x\r\nX-Partial: "))
	c.SetReadDeadline(time.Now().Add(3 * time.Second))
	buf := make([]byte, 512)
	if _, err := c.Read(buf); err == nil || strings.Contains(err.Error(), "timeout") {
		t.Errorf("the proxy still holds a half-sent request after its header timeout (read err %v)", err)
	}
}

func TestRound121TemplatedCatalogBaseURL(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	dir := filepath.Join(home, ".oaica", "cache", "catalog")
	os.MkdirAll(dir, 0o700)
	os.WriteFile(filepath.Join(dir, "modelsdev.json"), []byte(`{
 "tpl":{"id":"tpl","name":"Tpl","api":"https://api.tpl.example/acct/${TPL_ACCOUNT_ID}/v1","npm":"@ai-sdk/openai-compatible","env":["TPL_ACCOUNT_ID","TPL_API_KEY"],"models":{}},
 "sec":{"id":"sec","name":"Sec","api":"https://api.sec.example/${SEC_API_KEY}/v1","npm":"@ai-sdk/openai-compatible","env":["SEC_API_KEY"],"models":{}}
}`), 0o600)
	t.Setenv("TPL_API_KEY", "k")
	t.Setenv("SEC_API_KEY", "k2")
	find := func(name string) *userRemote {
		for _, r := range builtinRemotes() {
			if r.Name == name {
				r := r
				return &r
			}
		}
		return nil
	}
	if find("tpl") != nil {
		t.Errorf("a row whose base URL still holds ${TPL_ACCOUNT_ID} was offered")
	}
	t.Setenv("TPL_ACCOUNT_ID", "acct-1234")
	if r := find("tpl"); r == nil || r.BaseURL != "https://api.tpl.example/acct/acct-1234/v1" {
		t.Errorf("the templated row was not expanded from its non-secret variable: %+v", r)
	}
	if r := find("sec"); r != nil {
		t.Errorf("a secret-shaped name was substituted into a URL: %q", r.BaseURL)
	}
}
