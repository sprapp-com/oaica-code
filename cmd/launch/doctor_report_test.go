package launch

// doctor_report_test.go — `oaica doctor --report` is a support bundle users
// paste into tickets, so the contract worth pinning is negative: no
// credential value may appear in it, and if one would, nothing is printed at
// all. A test that only checked "the report looks right" would not catch the
// leak.

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const reportTestKey = "sk-live-do-not-print-me-0123456789"

// TestDoctorReport_WithholdsEveryCredential is the main guarantee: with the
// same secret present in all three places a client can hold one, none of them
// reaches the report.
func TestDoctorReport_WithholdsEveryCredential(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	t.Setenv("OAICA_API_KEY", reportTestKey)
	t.Setenv("OPENAI_API_KEY", "sk-openai-"+reportTestKey)

	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://`+reportTestKey+`@example.test/v1","api_key":"`+reportTestKey+`","tool_format":"tool_calls"}]}`)

	authPath := filepath.Join(home, ".oaica", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, []byte(`{"version":1,"providers":{"groq":{"type":"api_key","key":"`+reportTestKey+`"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	report, _ := buildDoctorReport()

	if strings.Contains(report, reportTestKey) {
		line := ""
		for _, l := range strings.Split(report, "\n") {
			if strings.Contains(l, reportTestKey) {
				line = l
				break
			}
		}
		t.Fatalf("report leaked a credential value; offending line: %q", line)
	}
	// The redaction must not have gone so far as to drop the useful parts:
	// this report is only worth printing if it tells support something.
	for _, want := range []string{"version:", "platform:", "remotes.json", "auth.json", "checks:"} {
		if !strings.Contains(report, want) {
			t.Errorf("report is missing %q — it would not be usable for support", want)
		}
	}
	// The credential-embedded remote URL must still be listed: support needs to
	// know which host was configured. The userinfo is gone rather than shown as
	// "REDACTED@" — the value the client prints is the resolved endpoint
	// (openAIBase), and that strips a userinfo credential outright
	// (splitRemoteUserinfo), so no placeholder is left to print.
	if !strings.Contains(report, "example.test") {
		t.Errorf("report should list the remote's endpoint; got no example.test line")
	}
	if strings.Contains(report, "@example.test") {
		t.Errorf("report printed a userinfo-carrying URL — the credential's position is showing")
	}
}

// TestDoctorReport_RefusesToPrintOnLeak covers the enforcement path: when a
// secret somehow renders into the text, the command fails and prints nothing,
// rather than trusting the redaction to have worked.
func TestDoctorReport_RefusesToPrintOnLeak(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OAICA_API_KEY", reportTestKey)

	secrets := reportSecrets()
	if len(secrets) == 0 {
		t.Fatal("test setup: no secrets collected")
	}
	// A report that (incorrectly) contains the value must be caught, and the
	// error must name the source without repeating the value.
	leaked := scanReportForSecrets("version: 0.5.46\nkey: "+reportTestKey+"\n", secrets)
	if len(leaked) != 1 || leaked[0] != "OAICA_API_KEY" {
		t.Fatalf("scanReportForSecrets = %v, want exactly [OAICA_API_KEY]", leaked)
	}
	if strings.Contains(strings.Join(leaked, " "), reportTestKey) {
		t.Fatal("the leak report itself contains the value")
	}

	// And the same scanner must not fire on text that merely resembles a key.
	if found := scanReportForSecrets("version: 0.5.46\nplatform: linux/amd64\n", secrets); len(found) != 0 {
		t.Fatalf("clean report flagged as leaking: %v", found)
	}
}

// TestDoctorReport_ShortValuesAreNotTreatedAsSecrets: test keys and dev
// configs contain short strings, and matching those against the whole report
// would make every report "leak" and become unprintable.
func TestDoctorReport_ShortValuesAreNotTreatedAsSecrets(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("OAICA_API_KEY", "k")
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://127.0.0.1:1/v1","api_key":"k","tool_format":"tool_calls"}]}`)

	if got := reportSecrets(); len(got) != 0 {
		t.Fatalf("reportSecrets collected %d values from 1-character keys, want 0", len(got))
	}

	var buf bytes.Buffer
	// The remote is unreachable (port 1), so this exits non-zero — what
	// matters here is that it printed a report rather than refusing.
	if err := runDoctorReport(&buf); err == nil {
		t.Fatal("expected a non-nil error for the unreachable probe")
	}
	if !strings.Contains(buf.String(), "oaica doctor --report") {
		t.Fatalf("no report printed; got %q", buf.String())
	}
}

// TestDoctorReport_FlagsWorldReadableCredentials: a credential file readable
// by other users is the one thing in this report a user can act on, so the
// mode has to be visible.
func TestDoctorReport_FlagsWorldReadableCredentials(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	authPath := filepath.Join(home, ".oaica", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(authPath, []byte(`{"version":1,"providers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	report, _ := buildDoctorReport()
	if !strings.Contains(report, "readable by other users") {
		t.Fatalf("a 0644 auth.json should be flagged; report:\n%s", report)
	}
}

// TestDoctorReport_ScansEveryKeyFile: the scan is only as good as its value
// list, and three key files were missing from it — ~/.oaica/api_key (written
// by `oaica signin`), ~/.oaica/license_key and ~/.oaica/license.json. A key
// absent from the list is a key the report could start printing without
// anything noticing, so the list is pinned, not the current rendering.
func TestDoctorReport_ScansEveryKeyFile(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	const (
		signinKey  = "sk-signin-file-0123456789"
		licenseKey = "lic-file-key-0123456789"
		jsonKey    = "lic-json-key-0123456789"
		serveKey   = "sk-serve-apikey-0123456789"
	)
	files := map[string]string{
		"api_key":      signinKey + "\n",
		"license_key":  licenseKey + "\n",
		"license.json": `{"key":"` + jsonKey + `","instance_id":"i","instance_name":"n"}`,
		// `oaica serve --api-key K` records K here, so it is a credential
		// file like the other three (2026-09-26 audit).
		"local_servers.json": `[{"model":"kat","origin":"http://127.0.0.1:9/v1","pid":1,"started_at":"t","api_key":"` + serveKey + `"}]`,
	}
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	values := map[string]bool{}
	for _, s := range reportSecrets() {
		values[s.value] = true
	}
	for _, want := range []string{signinKey, licenseKey, jsonKey, serveKey} {
		if !values[want] {
			t.Errorf("the scan does not know about %q — a report containing it would print it", want)
		}
	}

	// And the report must show all four files, each mode-annotated as
	// sensitive, so the user can see where their keys live.
	report, _ := buildDoctorReport()
	for _, name := range []string{"api_key", "license_key", "license.json", "local_servers.json"} {
		line := ""
		for _, l := range strings.Split(report, "\n") {
			if strings.HasSuffix(strings.TrimSpace(l), name) {
				line = l
				break
			}
		}
		if line == "" {
			t.Errorf("the report does not list %s:\n%s", name, report)
			continue
		}
		if !strings.Contains(line, "present  mode 0600") {
			t.Errorf("%s should be listed as a present 0600 credential file, got %q", name, line)
		}
	}
}

// TestDoctorCmd_ReportFlagWired keeps the flag honest: `--report` must change
// what doctor prints, and the default path must not.
func TestDoctorCmd_ReportFlagWired(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	cmd := DoctorCmd()
	if err := cmd.Flags().Set("report", "true"); err != nil {
		t.Fatal(err)
	}
	out := captureDoctorStdout(t, func() { _ = cmd.RunE(cmd, nil) })
	if !strings.Contains(out, "oaica doctor --report") {
		t.Fatalf("--report did not print the report:\n%s", out)
	}
	if !strings.Contains(out, "configuration files (contents are never included):") {
		t.Fatalf("--report is missing the environment section:\n%s", out)
	}

	plain := captureDoctorStdout(t, func() { _ = DoctorCmd().RunE(DoctorCmd(), nil) })
	if strings.Contains(plain, "configuration files") {
		t.Fatalf("the plain doctor run should not print the report section:\n%s", plain)
	}
}

// TestDoctorReport_QueryCredentialInBaseURLIsRedactedAndScanned: a key can
// ride in a base URL's query string ("?api_key=…"), a shape some gateways use
// instead of a header, and the transport echoes the request URL in its errors.
// Redacting userinfo alone left this value in cleartext while the report still
// claimed to hold no credential values — and the scan could not have caught it
// either, because reportSecrets only knew the userinfo form (2026-09-26 audit).
func TestDoctorReport_QueryCredentialInBaseURLIsRedactedAndScanned(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	const token = "sk-live-query-credential-0123456789"
	writeRemotes(t, `{"remotes":[{"name":"qbox","base_url":"http://127.0.0.1:1/v1?api_key=`+token+`","tool_format":"tool_calls"}]}`)

	report, _ := buildDoctorReport()
	if strings.Contains(report, token) {
		t.Errorf("the report prints a credential carried in base_url's query string:\n%s", report)
	}
	// The parameter NAME stays: "/v1?api_key=REDACTED" is what tells a support
	// reader which shape failed, which is the line's whole diagnostic value.
	if !strings.Contains(report, "api_key=REDACTED") {
		t.Errorf("the query parameter name should survive redaction:\n%s", report)
	}

	// The scan has to know the value, or a future print site could add it
	// without anything refusing.
	values := map[string]bool{}
	for _, s := range reportSecrets() {
		values[s.value] = true
	}
	if !values[token] {
		t.Errorf("reportSecrets does not know about the query credential %q", token)
	}

	// End to end: the printed bundle must not carry it.
	var out bytes.Buffer
	_ = runDoctorReport(&out)
	if strings.Contains(out.String(), token) {
		t.Errorf("`oaica doctor --report` printed the query credential:\n%s", out.String())
	}
}

// TestDoctorReport_UnparseableBasicPasswordIsRedactedAndScanned: a Basic
// password containing "/" makes url.Parse reject the whole URL (the authority
// ends at the first slash), so the parseable path cannot tell where the
// userinfo ends and the report printed the password in the clear — while the
// leak scan had never been told the value, so nothing refused (2026-09-26
// audit). Both halves are pinned here: the redaction and the secret list.
func TestDoctorReport_UnparseableBasicPasswordIsRedactedAndScanned(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	const secret = "aBc9/xY7z-do-not-print-me"
	writeRemotes(t, `{"remotes":[{"name":"basicbox","base_url":"https://user:`+secret+`@127.0.0.1:1/v1"}]}`)

	raw := "https://user:" + secret + "@127.0.0.1:1/v1"
	if got := redactBaseURL(raw); strings.Contains(got, secret) {
		t.Errorf("redactBaseURL(%q) = %q — an unparseable Basic password must still be redacted", raw, got)
	}
	if got := userinfoSecret(raw); got == "" {
		t.Errorf("userinfoSecret(%q) = \"\" — the scan must know a password url.Parse cannot find", raw)
	}

	values := map[string]bool{}
	for _, s := range reportSecrets() {
		values[s.value] = true
	}
	if !values[secret] {
		t.Errorf("reportSecrets does not know about the Basic password %q", secret)
	}

	report, _ := buildDoctorReport()
	if strings.Contains(report, secret) {
		t.Errorf("the report prints an unparseable Basic password in cleartext:\n%s", report)
	}
	// The parse error ALSO re-states the fragment Go mistook for a port
	// ("invalid port \":aBc9\" after host") outside the URL's quotes, so the
	// password's head leaks even when the quoted URL is redacted. Both halves
	// of the value must be gone from the printed bundle.
	if head, _, _ := strings.Cut(secret, "/"); strings.Contains(report, head) {
		t.Errorf("the report prints %q, the password's head as Go's invalid-port fragment:\n%s", head, report)
	}
	if strings.Contains(report, "aBc9") {
		t.Errorf("the report prints part of the Basic password:\n%s", report)
	}

	var out bytes.Buffer
	if err := runDoctorReport(&out); err != nil && strings.Contains(err.Error(), secret) {
		t.Errorf("the refusal/pass-through error text repeats the credential: %v", err)
	}
	if strings.Contains(out.String(), secret) {
		t.Errorf("`oaica doctor --report` printed a report containing the Basic password:\n%s", out.String())
	}
}
