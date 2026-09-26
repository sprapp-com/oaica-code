package launch

// path_credential_redaction_integrity_test.go — a credential carried in a base
// URL's PATH was printed verbatim under a footer claiming the report holds no
// credential values (2026-09-26 audit, tenth round).
//
// baseURLSecrets knew two places a base URL can carry a key — the userinfo and
// the query string — and redactBaseURL hid exactly those two. Some gateways put
// the key in the path instead (http://gw.example/sk-live-…/v1), which is a
// third place, and the report prints the base URL it failed on. The false
// footer is the real defect: the scan is what turns "this report contains no
// credential values" into a claim rather than a hope, and it can only check
// values it was told about.
//
// Only shapes that name themselves as credentials are matched — a key prefix
// (sk-, pk-, rk-, api_key, key, token, bearer, secret) followed by a token body.
// A rule that guessed at entropy would rewrite ordinary paths (/v1, /api,
// /models) on its way to a report a user has to read.

import (
	"strings"
	"testing"
)

func TestAPathSegmentCredentialIsRedactedAndKnownToTheScan(t *testing.T) {
	const secret = "sk-live-PATHSECRET-9876543210"
	baseURL := "http://gw.example/" + secret + "/v1"

	got := redactBaseURL(baseURL)
	if strings.Contains(got, secret) {
		t.Errorf("redactBaseURL printed the path credential: %q — doctor prints exactly this string, under a footer promising no credential values", got)
	}
	// The diagnosis survives: host and the rest of the path.
	if !strings.Contains(got, "gw.example") || !strings.Contains(got, "/v1") {
		t.Errorf("redaction removed the host or the path too: %q", got)
	}
	// And the scan knows the value, so a print site that adds it is caught.
	found := false
	for _, s := range baseURLSecrets(baseURL) {
		if s == secret {
			found = true
		}
	}
	if !found {
		t.Errorf("baseURLSecrets does not know the path credential in %q, so the leak scan cannot see it and the footer's promise is empty", baseURL)
	}
	// The general sanitizer, which every error path goes through, agrees.
	if strings.Contains(redactCredentials(baseURL), secret) {
		t.Errorf("redactCredentials left the path credential in place: %q", redactCredentials(baseURL))
	}
}

// The other two shapes are untouched: this may not become a rule that eats
// ordinary URLs.
func TestOrdinaryPathsAreNotRewritten(t *testing.T) {
	for _, baseURL := range []string{
		"https://api.example.com/v1",
		"https://api.example.com/v1/models",
		"https://openrouter.ai/api/v1",
		"https://generativelanguage.googleapis.com/v1beta",
		"http://127.0.0.1:11434/v1",
	} {
		if got := redactBaseURL(baseURL); got != baseURL {
			t.Errorf("an ordinary base URL was rewritten: %q -> %q", baseURL, got)
		}
		if len(baseURLSecrets(baseURL)) != 0 {
			t.Errorf("an ordinary base URL is treated as carrying credentials: %q -> %v", baseURL, baseURLSecrets(baseURL))
		}
	}
}

// The shapes that were already handled still are — userinfo and query.
func TestTheKnownShapesStillRedact(t *testing.T) {
	for _, baseURL := range []string{
		"https://sk-live-abcdef@api.example.com/v1",
		"https://api.example.com/v1?api_key=sk-live-abcdef",
	} {
		if got := redactBaseURL(baseURL); strings.Contains(got, "sk-live-abcdef") {
			t.Errorf("%q is printed as %q", baseURL, got)
		}
		if len(baseURLSecrets(baseURL)) == 0 {
			t.Errorf("baseURLSecrets lost the credential in %q", baseURL)
		}
	}
}
