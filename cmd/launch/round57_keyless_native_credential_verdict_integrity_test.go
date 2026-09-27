package launch

// round57_keyless_native_credential_verdict_integrity_test.go — round 57's
// finding on whose credential a passthrough 401 belongs to (F57-L2-1).
//
// The `anthropic` catalog row is api.anthropic.com itself and carries no key of
// oaica's, so for a user who signed in with `claude /login` (or exported
// ANTHROPIC_API_KEY) anthropicPassthroughTarget falls back to the user's OWN
// credential rather than refusing — the request is byte-identical to the one
// the native claude/* leg sends. Both /v1/messages callers still decided whose
// credential it was with `BaseURL != ""`, and that row HAS a BaseURL, so
// Anthropic's 401 under the user's own key was rewritten as if oaica's key had
// been refused: a 502 api_error, which reads as a broken proxy and sends Claude
// Code into retrying the whole prompt, while the native leg under the same
// credential relays the 401 and its `claude /login` instruction untouched
// (round 55's contract). The target reports whose credential it resolved now,
// and the callers use that.
//
// The plan row's own rule is pinned beside it, so a later round cannot "fix"
// this by handing every row the native verdict: on a row whose key oaica
// resolved, a 401 IS our key being wrong, and the 502 that keeps Claude Code
// out of a login flow over a credential it never had still applies.

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

const r57AnthropicAuthError = `{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`

func TestAKeylessAnthropicRowRelaysTheRefusalItsOwnCredentialEarns(t *testing.T) {
	for _, tc := range []struct {
		name     string
		env      string
		oauth    string
		wantAuth string
	}{
		{"env API key", "sk-ant-user-key", "", "sk-ant-user-key"},
		{"claude /login OAuth session", "", "sk-ant-oat-user-token", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			setLaunchTestHome(t, t.TempDir())
			t.Setenv("ANTHROPIC_API_KEY", tc.env)
			if tc.oauth != "" {
				old := readClaudeOAuthAccessTokenFn
				readClaudeOAuthAccessTokenFn = func() (string, error) { return tc.oauth, nil }
				t.Cleanup(func() { readClaudeOAuthAccessTokenFn = old })
			}

			// The row's BaseURL must stay api.anthropic.com — that host is what
			// lets the keyless fallback resolve at all — so the fake stands in
			// for the wire itself.
			var seen http.Header
			oldClient := proxyUpstreamClient
			proxyUpstreamClient = &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
				seen = req.Header.Clone()
				return &http.Response{
					StatusCode: http.StatusUnauthorized,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(bytes.NewReader([]byte(r57AnthropicAuthError))),
					Request:    req,
				}, nil
			})}
			t.Cleanup(func() { proxyUpstreamClient = oldClient })

			keyless := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
				Name: "anthropic", BaseURL: "https://api.anthropic.com/v1", UpstreamModel: "claude-opus-5", Wire: "anthropic",
			}})
			proxy := startProxyWithOversize(t, proxyRouteTable{Default: keyless})
			code, body, _ := postEntitlementTestMessage(t, proxy, keyless.UpstreamModel)

			// The premise: this row goes out under the USER's credential, which
			// is exactly why its refusal is the user's to see.
			gotCredential := seen.Get("x-api-key")
			if gotCredential == "" {
				gotCredential = seen.Get("authorization")
			}
			if tc.env != "" && gotCredential != tc.wantAuth {
				t.Fatalf("the request carried %q, want the user's key %q — this test cannot judge the verdict without the premise", gotCredential, tc.wantAuth)
			}
			if tc.oauth != "" && !strings.Contains(gotCredential, tc.oauth) {
				t.Fatalf("the request carried %q, want the user's OAuth session %q", gotCredential, tc.oauth)
			}

			if code != http.StatusUnauthorized {
				t.Errorf("the keyless api.anthropic.com row answered %d, want the upstream's own 401\nbody: %s\nthe credential is the user's own, so its refusal means the user re-runs `claude /login` — a 502 reads as a broken proxy and makes the client retry the whole prompt, while the byte-identical native claude/* leg relays this same 401 (2026-09-28 audit, round 57, F57-L2-1; round 55)", code, body)
			}
			if !strings.Contains(body, "authentication_error") {
				t.Errorf("the client was not handed Anthropic's own refusal verbatim:\n%s", body)
			}
			if strings.Contains(body, "api_error") {
				t.Errorf("the user's own refused credential was rewritten as an oaica-side fault:\n%s", body)
			}
		})
	}
}

// The other half: on a plan row the key IS oaica's, and the 502 stays.
func TestAPlanRowStillAnswersItsOwnRefusedKeyWithA502(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OAICA_R57_PLAN_KEY", "sk-plan-key")

	plan := proxyRoute{BaseURL: "https://plan.example/v1", UpstreamModel: "glm-5.3", Wire: "anthropic", KeyEnv: "OAICA_R57_PLAN_KEY"}
	upstream, headerName, headerValue, credentialIsOurs, ok := plan.anthropicPassthroughTarget()
	if !ok {
		t.Fatal("the plan row resolved no target with its key set")
	}
	if !credentialIsOurs {
		t.Errorf("a row carrying oaica's own key (%s=%q) reported the credential as the user's: the 401 under it would reach Claude Code as an authentication_error and send it into a login flow over a credential it never had", headerName, headerValue)
	}
	if upstream != "https://plan.example/v1/messages" {
		t.Errorf("upstream = %q", upstream)
	}

	// And the same discriminator on the two shapes that are NOT oaica's key.
	// Both resolve the user's credential, so both must report it as theirs.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-user-key")
	native := nativeAnthropicTestRoute()
	if _, _, _, ours, ok := native.anthropicPassthroughTarget(); !ok || ours {
		t.Errorf("the native claude/* leg reported credentialIsOurs=%v (ok=%v), want the user's own credential", ours, ok)
	}
	keyless := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "anthropic", BaseURL: "https://api.anthropic.com/v1", UpstreamModel: "claude-opus-5", Wire: "anthropic",
	}})
	if _, _, _, ours, ok := keyless.anthropicPassthroughTarget(); !ok || ours {
		t.Errorf("the keyless api.anthropic.com row reported credentialIsOurs=%v (ok=%v), want the user's own credential — this is the row whose BaseURL made the callers guess wrong (2026-09-28 audit, round 57, F57-L2-1)", ours, ok)
	}
}
