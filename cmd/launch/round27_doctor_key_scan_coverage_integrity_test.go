package launch

// round27_doctor_key_scan_coverage_integrity_test.go — the leak scan must not
// be smaller than the file it scans (2026-09-27 audit, round 27, B-D).
//
// The scan collected api_key values by decoding each row into a struct, and a
// struct decode keeps ONE value per field: a row carrying "api_key" twice
// answered with the second alone, so the first — just as printable as the
// second — was invisible to the check whose whole purpose is to know what must
// not be printed. A credential field this scan does not model is the same
// problem in another shape, and the report now says so rather than presenting a
// clean scan it did not earn.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLocalServersForTest(t *testing.T, home, body string) {
	t.Helper()
	dir := filepath.Join(home, ".oaica")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "local_servers.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// TestEveryAPIKeyInARowIsScanned: the duplicate key in one row is two values,
// and both are credentials.
func TestEveryAPIKeyInARowIsScanned(t *testing.T) {
	home := t.TempDir()
	writeLocalServersForTest(t, home, `[{"model":"m","origin":"http://127.0.0.1:1","pid":1,"started_at":"2026-09-27T00:00:00Z","api_key":"sk-first-key","api_key":"sk-second-key"}]`)

	keys, unclassified, err := localServerKeys(home)
	if err != nil {
		t.Fatalf("localServerKeys: %v", err)
	}
	if len(unclassified) != 0 {
		t.Errorf("unclassified = %v, want none for a document that only uses api_key", unclassified)
	}
	found := map[string]bool{}
	for _, k := range keys {
		found[k] = true
	}
	for _, want := range []string{"sk-first-key", "sk-second-key"} {
		if !found[want] {
			t.Errorf("localServerKeys = %v, want it to hold %q: a second key under the same field name is just as printable as the first, and a scan that misses it reports a leak-free bundle it never checked", keys, want)
		}
	}
}

// TestANestedAPIKeyIsScanned: a value does not stop being a credential by being
// nested inside the row.
func TestANestedAPIKeyIsScanned(t *testing.T) {
	home := t.TempDir()
	writeLocalServersForTest(t, home, `[{"model":"m","proxy":{"api_key":"sk-nested-key"}}]`)

	keys, _, err := localServerKeys(home)
	if err != nil {
		t.Fatalf("localServerKeys: %v", err)
	}
	if len(keys) != 1 || keys[0] != "sk-nested-key" {
		t.Errorf("localServerKeys = %v, want [sk-nested-key]", keys)
	}
}

// TestTheReportOwnsUpToCredentialFieldsItDoesNotModel: a document holding a
// credential under another name is a document this scan cannot cover, and the
// report says which field that is.
func TestTheReportOwnsUpToCredentialFieldsItDoesNotModel(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)
	writeLocalServersForTest(t, home, `[{"model":"m","origin":"http://127.0.0.1:1","pid":1,"api_key":"sk-scanned-key","fallback_token":"tok-not-scanned-value"}]`)

	_, unclassified, err := localServerKeys(home)
	if err != nil {
		t.Fatalf("localServerKeys: %v", err)
	}
	if len(unclassified) != 1 || unclassified[0] != "fallback_token" {
		t.Fatalf("unclassified = %v, want [fallback_token]", unclassified)
	}

	report, _ := buildDoctorReport()
	if !strings.Contains(report, "fallback_token") || !strings.Contains(report, "does not classify") {
		t.Error("the report does not name the credential-shaped field it cannot classify: a scan with a known edge must say where the edge is, or the reader takes it for complete")
	}
	if strings.Contains(report, "sk-scanned-key") || strings.Contains(report, "tok-not-scanned-value") {
		t.Error("the report printed a credential value")
	}
}
