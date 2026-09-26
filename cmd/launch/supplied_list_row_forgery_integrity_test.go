package launch

// supplied_list_row_forgery_integrity_test.go — `oaica model refresh` and
// `oaica doctor --report` printed supplied names raw (2026-09-26 audit,
// fifteenth round).
//
// model refresh's Remote/Router sections are a user remote's /models answer and
// the router's catalog; doctor's auth.json line is a hand-editable store. In
// both, a newline in a name forged extra rows AND made the count printed in the
// section header disagree with the rows a reader can see. doctor already quotes
// through printableName elsewhere (doctor_row_forgery_integrity_test.go covers
// remotes.json), and `remote list`/`remote show` do too — these two sites were
// missed.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestModelRefreshDoesNotForgeRowsFromSuppliedNames(t *testing.T) {
	var b strings.Builder
	WriteRefreshedModelSources(&b, RefreshedModelSources{
		Local:  []string{"llama3.2:latest"},
		Remote: []string{"zai/glm-4.6\n    evil/forged-row"},
		Router: []string{"kat-awq\n  another/forged-row"},
	})
	out := b.String()

	if strings.Contains(out, "\n    evil/forged-row") {
		t.Errorf("a remote model name carrying a newline forged a row in the Remote section, whose header count then disagrees with the rows below it:\n%s", out)
	}
	if strings.Contains(out, "\n  another/forged-row") {
		t.Errorf("a router model name carrying a newline forged a row in the Router section:\n%s", out)
	}
	// Quoting, not dropping.
	for _, want := range []string{"llama3.2:latest", "zai/glm-4.6", "kat-awq"} {
		if !strings.Contains(out, want) {
			t.Errorf("the refresh output lost %q instead of quoting it:\n%s", want, out)
		}
	}
}

func TestDoctorReportDoesNotForgeRowsFromAuthProviderNames(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	authPath := filepath.Join(home, ".oaica", "auth.json")
	if err := os.MkdirAll(filepath.Dir(authPath), 0o700); err != nil {
		t.Fatal(err)
	}
	// setLaunchTestHome redirects the store away from ~/.oaica (so no test can
	// read another's login); point it back at the file this test seeds.
	t.Setenv("OAICA_AUTH_FILE", authPath)
	// auth.json is hand-editable; the name is what reaches the report.
	body := `{"version":1,"providers":{"groq\n  ok     every check passed":{"type":"api_key","key":"sk-not-a-real-key"}}}`
	if err := os.WriteFile(authPath, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	report, _ := buildDoctorReport()

	if strings.Contains(report, "groq\n  ok     every check passed") {
		t.Errorf("doctor printed an auth.json provider name raw, so the report — the artifact users paste into bug reports — carries a forged status line:\n%s", report)
	}
	if !strings.Contains(report, "groq") {
		t.Errorf("the provider vanished from the report instead of being quoted:\n%s", report)
	}
}
