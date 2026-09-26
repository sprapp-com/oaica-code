package launch

// doctor_row_forgery_integrity_test.go — `oaica doctor` printed a remote's name
// and route_policy raw, so a hand-edited remotes.json forged rows in the
// section a user reads to find out what is configured (2026-09-26 audit, tenth
// round).
//
// `remote list` and `remote show` quote a name carrying control characters
// (printableName, ninth round) — doctor, the command whose whole job is
// inventory, did not. A name in the file that reached it before add-time
// validation existed (or that was edited in by hand — the reason printableName
// exists at all) rendered as extra rows in doctor's table, and a route_policy
// value rendered as a second field on the line, either of which a reader takes
// as real configuration. remotes.json is explicitly hand-editable, so no
// single print site can assume the file was validated.

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDoctorDoesNotPrintAnUnquotedRemoteName(t *testing.T) {
	path := withTempRemotesFile(t)

	name := "mine" + string(rune('\n')) + "zai-coding-plan  ********1234  ready  sk-l"
	policy := "local-only" + string(rune('\n')) + "  base_url: https://evil.example/v1"
	// The store is JSON, so the newlines are escaped in the FILE and real once
	// parsed — which is exactly how a hand-edited file looks.
	body := `{"remotes":[{"name":"` + asJSON(name) + `","base_url":"https://api.example.com","route_policy":"` + asJSON(policy) + `"}]}`
	// The store is written by hand, as the doc says it may be.
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	report, _ := buildDoctorReport()
	if strings.Contains(report, "mine\nzai-coding-plan") {
		t.Errorf("doctor printed a remote name raw, so the row is split and the rest of the name reads as another provider's line:\n%s", report)
	}
	if strings.Contains(report, policy) {
		t.Errorf("doctor printed route_policy raw, so the injected text reads as a field of its own:\n%s", report)
	}
	// The real content still has to be there — quoting, not dropping.
	if !strings.Contains(report, "zai-coding-plan") || !strings.Contains(report, "local-only") {
		t.Errorf("the remote vanished from the report instead of being quoted:\n%s", report)
	}
	// And the report may still claim what it claims (see the footer): the
	// injected text carries no credential, so this is about structure only.
	if leaked := scanReportForSecrets(report, reportSecrets()); len(leaked) > 0 {
		t.Errorf("report leaks %v", leaked)
	}
}

// asJSON escapes a string for embedding in a JSON literal.
func asJSON(s string) string {
	b, _ := json.Marshal(s)
	return strings.Trim(string(b), `"`)
}

// Control: an ordinary remote still prints as a plain row, so this cannot be
// passed by quoting everything.
func TestDoctorStillPrintsAnOrdinaryRemotePlainly(t *testing.T) {
	withTempRemotesFile(t)
	if _, err := RemoteAdd(RemoteAddOptions{Name: "acme", BaseURL: "https://api.example.com"}); err != nil {
		t.Fatal(err)
	}

	report, _ := buildDoctorReport()
	if !strings.Contains(report, "acme") {
		t.Fatalf("the remote is missing from the report:\n%s", report)
	}
	if strings.Contains(report, `"acme"`) {
		t.Errorf("an ordinary name was quoted in the report:\n%s", report)
	}
}
