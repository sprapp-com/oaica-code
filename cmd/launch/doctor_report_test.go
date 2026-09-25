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
	// The credential-embedded remote URL must still be listed, redacted.
	if !strings.Contains(report, "REDACTED@example.test") {
		t.Errorf("report should list the remote's base URL with the userinfo redacted")
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
