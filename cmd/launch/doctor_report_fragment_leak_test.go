package launch

// doctor_report_fragment_leak_test.go — `oaica doctor --report` prints the
// remotes it probed and the error each returned, under a footer promising the
// report "contains no credential values; that is enforced by scanning this text
// against every key before printing it". That promise is only as wide as the
// list of values the scan was handed (redact.go's baseURLSecrets), and two
// shapes a key can ride in were missing from it:
//
//   - a fragment: `…#api_key=sk-…` — the separator class held "?&;" and not
//     "#", so neither the redactor nor the scan saw the value;
//   - a parameter whose name is not on the credential list: `?auth=sk-…` — the
//     report printed it verbatim because "auth" was not an alternative in
//     credentialQueryParam.
//
// Both reached a report designed to be pasted into a support ticket
// (2026-09-26 audit, third round).

import (
	"strings"
	"testing"
)

func TestDoctorReportRedactsCredentialShapesItPrintsURLsWith(t *testing.T) {
	for _, tc := range []struct {
		name   string
		base   string
		secret string
	}{
		{name: "fragment", base: "https://api.example.com/v1#api_key=sk-live-FRAGMENT", secret: "sk-live-FRAGMENT"},
		{name: "unlisted parameter name", base: "https://api.example.com/v1?auth=sk-live-PARAMNAME", secret: "sk-live-PARAMNAME"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setLaunchTestHome(t, home)
			writeRemotes(t, `{"remotes":[{"name":"leaky","base_url":"`+tc.base+`","api_key":"sk-remote"}]}`)

			// The scan is what makes the footer a claim rather than a hope:
			// baseURLSecrets has to know the value before the report can refuse
			// to print it.
			secrets := baseURLSecrets(tc.base)
			if !containsString(secrets, tc.secret) {
				t.Errorf("baseURLSecrets(%q) = %v, which does not include %q — the leak scan cannot catch a value it was never told about",
					tc.base, secrets, tc.secret)
			}

			report, _ := buildDoctorReport()
			if strings.Contains(report, tc.secret) {
				t.Errorf("the report prints %q in the clear while promising it contains no credential values:\n%s", tc.secret, report)
			}
		})
	}
}

func containsString(hay []string, needle string) bool {
	for _, s := range hay {
		if s == needle {
			return true
		}
	}
	return false
}
