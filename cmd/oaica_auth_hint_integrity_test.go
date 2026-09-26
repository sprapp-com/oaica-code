package cmd

// oaica_auth_hint_integrity_test.go — the credential hint was keyed on the
// words "api key" appearing in the router's message (2026-09-26 audit).
//
// The hint ("Run `oaica signin`, or set OAICA_API_KEY") exists so a fresh user
// sees the fix next to the diagnosis. Deciding that by substring search gets it
// wrong in both directions, and both directions are user-visible:
//
//   - A 401 or 403 whose message does not spell out "api key" — "unauthorized",
//     "invalid bearer token", a proxy rewriting the body — is the exact failure
//     the hint is for, and the user gets a bare wall.
//   - A non-credential error that happens to mention a key — a rate limit
//     scoped to a key, a model that needs its own key on the backend — tells
//     the user to sign in, which is not a thing they can do about it.
//
// The response's status is the thing that actually says "this was an
// authentication failure"; the message is prose the router is free to change.

import (
	"strings"
	"testing"
)

func TestTheCredentialHintFollowsTheStatusNotTheWording(t *testing.T) {
	cases := []struct {
		name     string
		status   int
		msg      string
		wantHint bool
	}{
		{
			name:     "a 401 that names no key at all",
			status:   401,
			msg:      "unauthorized",
			wantHint: true,
		},
		{
			name:     "a 401 from a gateway that says bearer",
			status:   401,
			msg:      "invalid bearer token",
			wantHint: true,
		},
		{
			name:     "a 403 for a key without admin scope",
			status:   403,
			msg:      "forbidden: this key lacks admin scope",
			wantHint: true,
		},
		{
			name:     "the wording the gateway really sends",
			status:   401,
			msg:      "missing or invalid API key",
			wantHint: true,
		},
		{
			name:     "a 400 that merely mentions a key",
			status:   400,
			msg:      "model 'x' needs its own api key on the backend",
			wantHint: false,
		},
		{
			name:     "a rate limit scoped to a key",
			status:   429,
			msg:      "rate limit exceeded for this api key",
			wantHint: false,
		},
		{
			name:     "an ordinary bad request",
			status:   400,
			msg:      "context length exceeded",
			wantHint: false,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := oaicaWrapAuthError(tc.status, tc.msg).Error()
			hasHint := strings.Contains(got, oaicaAuthHint)
			if hasHint != tc.wantHint {
				t.Fatalf("HTTP %d %q → %q (hint present: %v), want hint present: %v",
					tc.status, tc.msg, got, hasHint, tc.wantHint)
			}
			if !hasHint && got != tc.msg {
				t.Errorf("the message was rewritten without a hint: %q", got)
			}
		})
	}
}
