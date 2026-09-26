package launch

// native_no_credential_refusal_row_integrity_test.go — the native claude/* leg
// refused with no credential and wrote no usage row (2026-09-26 audit, tenth
// round, auditor B).
//
// A native launch with neither ANTHROPIC_API_KEY nor a `claude /login` session
// answers every turn with "401 no Anthropic credential found" — and
// ~/.oaica/requests.log stayed empty, so `oaica usage` showed a session with
// zero traffic while the user was looking at an error on every turn. The
// sibling anthropic-wire remote branch has always refused through refuse(),
// which writes the row; the native leg was the only refusal path that did not,
// which is the exact state the refusal-row machinery exists to prevent.
//
// The missing credential is also the one error a user must act on, so its
// absence from the report is the worst row to lose.

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestANativeLegRefusalIsStillAReportedTurn(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "")
	stubNativeModelCatalog(t, map[string]string{"fable": "claude-fable-5-1"})
	restore := readClaudeOAuthAccessTokenFn
	readClaudeOAuthAccessTokenFn = func() (string, error) { return "", nil }
	defer func() { readClaudeOAuthAccessTokenFn = restore }()

	ep, err := resolveLaunchEndpoint("claude/fable")
	if err != nil {
		t.Fatalf("resolveLaunchEndpoint(claude/fable) = %v, want the native tier", err)
	}
	route := routeFor(ep)
	if !route.NativePassthrough || route.BaseURL != "" {
		t.Fatalf("premise changed: the native leg is BaseURL=%q NativePassthrough=%t", route.BaseURL, route.NativePassthrough)
	}
	if _, ok := resolveNativeAnthropicAuth(); ok {
		t.Fatalf("premise: a credential was found, so this test cannot cover the no-credential refusal")
	}

	proxy := startProxyWithOversize(t, proxyRouteTable{Default: route, SessionID: "sess-no-credential"})

	body := `{"model":"claude-fable-5-1","max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`
	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("premise: the native leg answered %d (%.200s), want 401", resp.StatusCode, raw)
	}

	rows := waitForRequestLogRows(t, 1)
	if len(rows) != 1 {
		t.Fatalf("got %d row(s) for a turn the proxy refused, want 1 — a launch with no Anthropic credential reports a session with no traffic while the user sees an error on every turn", len(rows))
	}
	if rows[0].StatusCode != http.StatusUnauthorized {
		t.Errorf("the row records status %d, want 401", rows[0].StatusCode)
	}
	requests, errors := reportedTurn(t)
	if requests != 1 || errors != 1 {
		t.Errorf("`oaica usage` reports %d requests / %d errors for one refused turn, want 1 / 1", requests, errors)
	}
}

// Control: with a credential present the same leg is served and wrote a row
// before this change too — the fix must not turn a served turn into a refusal.
func TestTheNativeLegWithACredentialIsUnaffected(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-native-row")
	stubNativeModelCatalog(t, map[string]string{"fable": "claude-fable-5-1"})

	nativeHits := 0
	upstream := anthropicTestUpstream(&nativeHits)
	defer upstream.Close()
	orig := nativeAnthropicUpstream
	nativeAnthropicUpstream = upstream.URL
	defer func() { nativeAnthropicUpstream = orig }()

	ep, err := resolveLaunchEndpoint("claude/fable")
	if err != nil {
		t.Fatal(err)
	}
	route := routeFor(ep)
	proxy := startProxyWithOversize(t, proxyRouteTable{Default: route, SessionID: "sess-with-credential"})

	body := `{"model":"claude-fable-5-1","max_tokens":8,"messages":[{"role":"user","content":"ping"}]}`
	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("a native leg WITH a credential answered %d, want 200", resp.StatusCode)
	}
	if nativeHits == 0 {
		t.Errorf("the native upstream was never reached")
	}
}
