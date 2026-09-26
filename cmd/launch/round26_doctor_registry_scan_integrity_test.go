package launch

// round26_doctor_registry_scan_integrity_test.go — "unreadable" is not "empty"
// in the doctor report's leak scan (2026-09-27 audit, round 26).
//
// localServerKeys feeds reportSecrets, the value list the support bundle is
// scanned against. It answered nil for an absent ~/.oaica/local_servers.json
// and for one it could not parse alike, so a corrupt-but-present registry
// silently shrank the scan: the report printed the file as PRESENT and gave no
// hint that a credential recorded in it — `oaica serve --api-key K` writes K
// there — was not in the list it compared against. The corrupt case is the one
// that matters; the absent case is the one that is genuinely empty.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestAnUnreadableServeRegistryIsNotReportedAsEmpty(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	// Present, and holding an api_key — but truncated, exactly what a killed
	// write leaves behind, so it cannot be parsed.
	registryPath := filepath.Join(dir, "local_servers.json")
	const corrupt = `[{"model":"kat","origin":"http://127.0.0.1:30001","pid":4242,"api_key":"sk-serve-` + reportTestKey
	if err := os.WriteFile(registryPath, []byte(corrupt), 0o600); err != nil {
		t.Fatal(err)
	}

	// The reader keeps the two states apart: an absent file yields no keys and
	// no error, this one must not answer the same way.
	if keys, err := localServerKeys(home); err == nil {
		t.Fatalf("localServerKeys over a corrupt registry = %q, nil; want an error: a registry the scan cannot read is not a registry with no keys in it", keys)
	}

	// And the report says so, rather than describing the file as a healthy
	// present one.
	report, _ := buildDoctorReport()
	if !strings.Contains(report, "local_servers.json") {
		t.Fatalf("the report does not mention the registry at all:\n%s", report)
	}
	var warning string
	for _, line := range strings.Split(report, "\n") {
		if strings.Contains(line, "WARNING") && strings.Contains(line, "local_servers.json") {
			warning = line
			break
		}
	}
	if warning == "" {
		t.Errorf("the report lists local_servers.json as present with no warning that the leak scan could not read it, so a key recorded there would be unscanned and invisible:\n%s", report)
	}
}

// The control: an absent registry is empty, not broken, and draws no warning.
func TestAnAbsentServeRegistryIsQuietlyEmpty(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	keys, err := localServerKeys(home)
	if err != nil || len(keys) != 0 {
		t.Fatalf("localServerKeys with no registry = (%q, %v), want (none, nil): an absent file is nothing to scan, not a failure", keys, err)
	}
	if report, _ := buildDoctorReport(); strings.Contains(report, "WARNING") {
		t.Errorf("the report warns about a registry that does not exist:\n%s", report)
	}
}
