package launch

// redact_query_param_integrity_test.go — a query-string credential was only
// hidden when its parameter name was one the allowlist happened to name
// (2026-09-26 audit, fourth round).
//
// The list was anchored and exact: api_key, key, token, access_token,
// auth_token, apikey, secret, password, auth. Every other spelling was invisible
// BOTH to the print-site redactor and to the pre-print leak scan, which share
// the predicate — so `oaica doctor --report` printed a base_url carrying
// `?client_secret=sk-…` in the clear and then printed its own footer promising
// the report holds no credential values. Same for `?api_token=`, `?X-Api-Key=`
// and `?sig=`, on every path that renders a remote's base URL: `oaica remote
// list/show/add`, `oaica usage` (and --json), requests.log, and doctor.
//
// The names below are the ones a real gateway uses. A name list can never catch
// the spelling nobody thought of, which is why the fix is a word test.

import (
	"strings"
	"testing"
)

// offAllowlistSecrets is <param name>|<secret value> for names the old
// allowlist did not cover.
var offAllowlistSecrets = []struct{ name, value string }{
	{"client_secret", "sk-CLIENTSECRET-77889900"},
	{"api_token", "sk-APITOKEN-22334455"},
	{"X-Api-Key", "sk-XAPIKEY-55667788"},
	{"sig", "sk-SIG-99112233"},
	{"signature", "sk-SIGNATURE-11223344"},
	{"bearer", "sk-BEARER-00998877"},
	{"clientSecret", "sk-CAMEL-66554433"},
	{"access-token", "sk-DASHED-44332211"},
	{"apikey", "sk-NOSEP-12312312"},
	{"authtoken", "sk-AUTHTOKEN-90909090"},
	{"refresh_token", "sk-REFRESH-10101010"},
	{"subscription-key", "sk-SUBKEY-20202020"},
	{"X-Auth-Token", "sk-XAUTH-30303030"},
	{"sas", "sk-SAS-40404040"},
	{"jwt", "sk-JWT-50505050"},
	{"client_password", "sk-PW-60606060"},
}

func TestEveryCredentialQuerySpellingIsRedacted(t *testing.T) {
	for _, tc := range offAllowlistSecrets {
		u := "https://api.example.com/v1?" + tc.name + "=" + tc.value

		got := redactBaseURL(u)
		if strings.Contains(got, tc.value) {
			t.Errorf("redactBaseURL(%q) = %q — the value is printed verbatim by `oaica remote list/show/add`, `oaica usage` and `oaica doctor --report`, and doctor's footer goes on to promise the report holds no credential values", u, got)
		}
		if !strings.Contains(got, "REDACTED") {
			t.Errorf("redactBaseURL(%q) = %q, want the value replaced with REDACTED (the parameter name is the diagnostic)", u, got)
		}

		// The parameter name stays: a support report must still say which shape
		// failed.
		if !strings.Contains(got, tc.name) {
			t.Errorf("redactBaseURL(%q) = %q, want the parameter name kept", u, got)
		}

		// The leak scan is only as good as this list, so it must know the value
		// too — otherwise doctor prints the report.
		secrets := baseURLSecrets(u)
		if len(secrets) == 0 {
			t.Errorf("baseURLSecrets(%q) found nothing — the pre-print leak scan cannot refuse a value it was never told about", u)
		}
		found := false
		for _, s := range secrets {
			if s == tc.value {
				found = true
			}
		}
		if !found {
			t.Errorf("baseURLSecrets(%q) = %v, want it to include the query value", u, secrets)
		}
	}
}

// The word test must not swallow plain diagnostic parameters — a URL is printed
// for the shape it shows, so redacting "?stream=true" would hide the one thing
// the line exists to say.
func TestOrdinaryQueryParametersAreStillPrinted(t *testing.T) {
	for _, u := range []string{
		"https://api.example.com/v1?stream=true",
		"https://api.example.com/v1?timeout=30&retries=2",
		"https://api.example.com/v1?seed=1",
		"https://api.example.com/v1/models",
	} {
		if got := redactBaseURL(u); got != u {
			t.Errorf("redactBaseURL(%q) = %q, want it unchanged — over-redacting an ordinary parameter hides the diagnostic the line exists for", u, got)
		}
	}
}

// A userinfo credential is the other spelling a base URL carries; the word test
// must not have displaced it.
func TestUserinfoCredentialsAreStillRedacted(t *testing.T) {
	u := "https://sk-LIVE-1234567890@api.example.com/v1"
	if got := redactBaseURL(u); strings.Contains(got, "sk-LIVE-1234567890") {
		t.Errorf("redactBaseURL(%q) = %q, want the userinfo credential hidden", u, got)
	}
}
