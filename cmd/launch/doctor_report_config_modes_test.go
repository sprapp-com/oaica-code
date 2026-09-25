package launch

// doctor_report_config_modes_test.go — the --report flag help promises
// "config paths and their permissions", and docs/ENTERPRISE.md's checklist
// item 3 sends a reviewer to the bundle for exactly that ("config paths and
// their permissions"; item 4 is `ls -la ~/.oaica/` for the 0700/0600 layout).
//
// Two paths could not answer the question: ~/.oaica/config.json and the cache
// dir were passed sensitive=false, so describeFile printed no mode bits at
// all — and the directory branch returned before the note, so no directory
// ever showed its bits (2026-09-26 audit). Both matter: config.json is where
// OAICA_HOST lives and that value can carry a key, and the cache directory's
// mode is what decides whether anything inside it is reachable.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctorReportPrintsConfigPathPermissions(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	// The cache dir exists (a report on an ABSENT path has nothing to say
	// about its mode), and it is 0755 on purpose: the 0700/0600 layout
	// question is mostly about this directory, since it is what decides
	// whether anything inside it is reachable.
	if err := os.MkdirAll(filepath.Join(dir, "cache"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Group/world-readable on purpose: this is the layout the checklist asks a
	// reviewer to notice, and a report silent about it cannot answer the
	// question it says it answers.
	if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(`{"integrations":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}

	report, _ := buildDoctorReport()

	configLine := reportLineContaining(report, filepath.Join(".oaica", "config.json"))
	if configLine == "" {
		t.Fatalf("no config.json line in the report at all:\n%s", report)
	}
	if !strings.Contains(configLine, "mode 0644") {
		t.Errorf("config.json is mode 0644 on disk and the report prints it as:\n  %s\n— the mode belongs on this line: config.json is where OAICA_HOST lives, and that value can carry a key", configLine)
	}
	if !strings.Contains(configLine, "readable by other users") {
		t.Errorf("a 0644 config.json should be flagged as group/world-readable; got:\n  %s", configLine)
	}

	// A directory's own bits: whether anything inside it is reachable at all.
	dirLine := reportLineContaining(report, filepath.Join(".oaica", "cache"))
	if dirLine == "" {
		t.Fatalf("no cache line in the report at all:\n%s", report)
	}
	if !strings.Contains(dirLine, "mode") {
		t.Errorf("the cache directory's mode is not printed, so the report cannot answer the 0700/0600 layout question:\n  %s", dirLine)
	}
}

// TestDoctorReportDescribesTheOaicaDirectoryItself is the other half of the
// same question, missed by the round-5 fix: the list described eight paths
// UNDER ~/.oaica and never ~/.oaica itself — the one directory whose own mode
// decides whether a 0600 credential file inside it is reachable at all, which
// is the fact the 0700/0600 checklist item sends a reviewer to this bundle to
// establish (2026-09-26 audit).
func TestDoctorReportDescribesTheOaicaDirectoryItself(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	dir := filepath.Join(home, ".oaica")
	// 0755 on purpose: the layout the report has to make visible.
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	report, _ := buildDoctorReport()

	var dirLine string
	for _, l := range strings.Split(report, "\n") {
		if strings.HasSuffix(strings.TrimSpace(l), dir) {
			dirLine = l
			break
		}
	}
	if dirLine == "" {
		t.Fatalf("the report has no line for %s itself, so the 0700/0600 layout question cannot be answered for the directory that decides it:\n%s", dir, report)
	}
	if !strings.Contains(dirLine, "directory") {
		t.Errorf("the ~/.oaica line does not present it as a directory:\n  %s", dirLine)
	}
	if !strings.Contains(dirLine, "mode 0755") {
		t.Errorf("~/.oaica is mode 0755 on disk and the report prints it as:\n  %s\n— a 0755 ~/.oaica makes every credential file inside it reachable whatever that file's own mode says", dirLine)
	}
	if !strings.Contains(dirLine, "readable by other users") {
		t.Errorf("a 0755 ~/.oaica should be flagged as group/world-readable; got:\n  %s", dirLine)
	}
}

// The OAICA_LICENSE_KEY anchor is listed too: it decides whether an
// env-supplied licence keeps working offline, and a deployment review that
// cannot see it cannot answer that.
func TestDoctorReportListsTheEnvLicenseAnchor(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	anchor := filepath.Join(home, ".oaica", "license_env.json")
	if line := reportLineContaining(buildDoctorReportString(t), anchor); line == "" {
		t.Errorf("the report has no line for %s, so an operator cannot see whether an OAICA_LICENSE_KEY deployment has a validation on record:\n%s", anchor, buildDoctorReportString(t))
	}
}

func buildDoctorReportString(t *testing.T) string {
	t.Helper()
	report, _ := buildDoctorReport()
	return report
}

// reportLineContaining returns the first report line naming path, or "".
func reportLineContaining(report, path string) string {
	for _, l := range strings.Split(report, "\n") {
		if strings.Contains(l, path) {
			return l
		}
	}
	return ""
}
