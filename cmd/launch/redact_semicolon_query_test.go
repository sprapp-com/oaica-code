package launch

// redact_semicolon_query_test.go — RFC 3986 lets ";" separate query
// parameters, and this repository's own redaction pipeline treated it as an
// ordinary value character: with the "[?&]" prefix, the scanner matched the
// FIRST parameter and consumed everything after it as that parameter's value,
// so a credential riding in a ";"-separated query was neither hidden by the
// redactor nor known to the leak scan. doctor --report then printed it while
// its own scan reported no credentials (2026-09-26 audit).

import (
	"strings"
	"testing"
)

func TestSemicolonSeparatedQueryCredentialIsRedactedAndScanned(t *testing.T) {
	const key = "sk-live-semicolon-0123456789"
	u := "https://host.example.test/v1?seed=1;api_key=" + key

	// Both shapes the key reaches a user through: a transport error echoing
	// the request URL, and a remote's stored base_url.
	shapes := map[string]string{
		"redactCredentials on a transport error": redactCredentials(`Get "` + u + `": dial tcp 10.0.0.1:443: connect: connection refused`),
		"redactBaseURL on a stored base_url":     redactBaseURL(u),
	}
	for name, got := range shapes {
		if strings.Contains(got, key) {
			t.Errorf("%s printed the key in the clear:\n  %s", name, got)
		}
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("%s did not redact the credential at all:\n  %s", name, got)
		}
	}

	// The leak scan is only as good as its value list: a credential the scan
	// does not know is one the report can print while claiming it holds none.
	var scanned []string
	for _, s := range baseURLSecrets(u) {
		scanned = append(scanned, s)
		if s == key {
			return
		}
	}
	t.Errorf("baseURLSecrets does not know the key a \";\"-separated query carries (got %v) — doctor --report would print it and then report no credentials", scanned)
}
