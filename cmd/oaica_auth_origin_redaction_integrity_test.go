package cmd

// oaica_auth_origin_redaction_integrity_test.go — `oaica auth login` and
// `oaica auth list` printed the provider origin verbatim (2026-09-26 audit).
//
// A provider origin is a URL this project's own convention allows to carry a
// credential in its userinfo — the same shape a remote's base_url may have, and
// the same one `launch.redactBaseURL` exists to hide, because that text lands
// in terminals, shell history, CI logs and pasted support reports. `auth list`
// printed it unredacted in its ORIGIN column and `auth login` echoed it in its
// confirmation line, so the two commands whose whole job is to say which
// provider you are talking to were also the two that could print the secret.

import (
	"strings"
	"testing"
)

// The redacting helper is the single place these commands render an origin.
func TestAProviderOriginIsPrintedRedacted(t *testing.T) {
	for _, tc := range []struct {
		origin      string
		mustNotHave string
	}{
		{"https://sk-live-abcdef@api.example.com/v1", "sk-live-abcdef"},
		{"https://user:pass@api.example.com", "pass"},
		{"https://api.example.com/v1?api_key=sk-live-abcdef", "sk-live-abcdef"},
	} {
		got := oaicaPrintedOrigin(tc.origin)
		if strings.Contains(got, tc.mustNotHave) {
			t.Errorf("origin %q is printed as %q, which still contains the credential %q — this line goes to terminals, CI logs and support reports",
				tc.origin, got, tc.mustNotHave)
		}
	}
	// The host has to survive: a support report needs to know which provider.
	if got := oaicaPrintedOrigin("https://sk-live-abcdef@api.example.com/v1"); !strings.Contains(got, "api.example.com") {
		t.Errorf("redaction removed the host as well: %q — the point is to hide the credential, not the provider", got)
	}
	// A plain origin is untouched.
	if got := oaicaPrintedOrigin("https://api.example.com/v1"); got != "https://api.example.com/v1" {
		t.Errorf("a credential-free origin was rewritten to %q", got)
	}
}

// And nothing else in the command file prints an origin directly. Pinned at the
// source because both sites are `Printf`s inside RunE closures that need the
// router to run.
func TestNoAuthVerbPrintsAnUnredactedOrigin(t *testing.T) {
	src := cmdGoSource(t)
	for _, bad := range []string{
		`fmt.Printf("Registered '%s' -> %s\n", name, origin)`,
		`line := fmt.Sprintf("  %-28s %-45s %s", e.Name, e.Origin, authState)`,
	} {
		if strings.Contains(src, bad) {
			t.Errorf("cmd/cmd.go prints a provider origin unredacted again:\n  %s", bad)
		}
	}
}
