package launch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// devTestKey is the string the `devtest` build tag registers (see
// license_devkey_dev.go). A _test file may name it: test files are never
// compiled into the shipped binary, which is exactly where the key must not be.
const devTestKey = "OAICA-TEST-DEV-FREE"

// withDevTestKey registers that key as locally-honoured for the duration of the
// test and returns it.
//
// A test cannot inherit it from the build any more: the default (release-shaped)
// build has an empty list, which is the property
// TestReleaseBuildDoesNotHonourADevTestKey pins. A test that wants the dev-key
// path opts in, so the behaviour stays covered in the build `go test` actually
// runs.
func withDevTestKey(t *testing.T) string {
	t.Helper()
	prev := devTestLicenseKeys
	devTestLicenseKeys = []string{devTestKey}
	t.Cleanup(func() { devTestLicenseKeys = prev })
	return devTestKey
}

// stubLicenseServer starts an httptest server implementing just enough of
// the /activate and /validate endpoints for these tests, and points
// licenseServerAPI at it for the duration of the test.
func stubLicenseServer(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	prev := licenseServerAPI
	licenseServerAPI = srv.URL
	t.Cleanup(func() { licenseServerAPI = prev })
}

func TestActivateLicenseLive_Success(t *testing.T) {
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/activate" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		_ = r.ParseForm()
		if r.FormValue("license_key") != "OAICA-TEST-KEY" {
			t.Errorf("license_key = %q", r.FormValue("license_key"))
		}
		if r.FormValue("instance_name") == "" {
			t.Error("instance_name should default to something non-empty")
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"activated": true,
			"instance":  map[string]any{"id": "inst-123", "name": r.FormValue("instance_name")},
		})
	})

	f, err := activateLicenseLive("OAICA-TEST-KEY", "")
	if err != nil {
		t.Fatalf("activateLicenseLive: %v", err)
	}
	if f.Key != "OAICA-TEST-KEY" || f.InstanceID != "inst-123" {
		t.Errorf("got %+v", f)
	}
	if f.ValidatedAt.IsZero() || f.ActivatedAt.IsZero() {
		t.Error("ActivatedAt/ValidatedAt should be set on activation")
	}
}

func TestActivateLicenseLive_Rejected(t *testing.T) {
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"activated": false,
			"error":     "license key not found",
		})
	})

	_, err := activateLicenseLive("BAD-KEY", "")
	if err == nil {
		t.Fatal("expected an error for a rejected activation")
	}
	if !strings.Contains(err.Error(), "license key not found") {
		t.Errorf("error = %v, want it to surface the server's message", err)
	}
}

func TestActivateLicenseLive_EmptyKey(t *testing.T) {
	_, err := activateLicenseLive("   ", "")
	if err == nil {
		t.Fatal("expected an error for an empty/whitespace key")
	}
}

func TestActivateLicenseLive_TestKeyNeverHitsNetwork(t *testing.T) {
	key := withDevTestKey(t)
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a dev/test key must not call the license server")
	})

	f, err := activateLicenseLive(key, "")
	if err != nil {
		t.Fatalf("activateLicenseLive(dev test key): %v", err)
	}
	if f.Key != key || f.ValidatedAt.IsZero() || f.ActivatedAt.IsZero() {
		t.Errorf("got %+v", f)
	}
}

func TestRequireLicenseLive_TestKeyNeverRevalidates(t *testing.T) {
	key := withDevTestKey(t)
	setLaunchTestHome(t, t.TempDir())
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		t.Fatal("a dev/test key must not call the license server, even on a stale cache")
	})

	stale := time.Now().Add(-licenseRevalidateTTL - time.Hour)
	if err := saveLicenseFile(licenseFile{Key: key, InstanceID: "test", ValidatedAt: stale}); err != nil {
		t.Fatalf("saveLicenseFile: %v", err)
	}

	if err := requireLicenseLive(nil, nil); err != nil {
		t.Errorf("a dev/test key should always pass requireLicenseLive: %v", err)
	}
}

func TestRequireLicenseLive_NoLicenseFile(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	err := requireLicenseLive(nil, nil)
	if err == nil {
		t.Fatal("expected an error when no license.json exists")
	}
	if !strings.Contains(err.Error(), oaicaPurchaseURL) {
		t.Errorf("error should point at the purchase URL, got: %v", err)
	}
}

func TestRequireLicenseLive_FreshCacheSkipsNetwork(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	networkHit := false
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		networkHit = true
		w.WriteHeader(http.StatusInternalServerError)
	})

	if err := saveLicenseFile(licenseFile{
		Key: "K", InstanceID: "I", ActivatedAt: time.Now(), ValidatedAt: time.Now(),
	}); err != nil {
		t.Fatalf("saveLicenseFile: %v", err)
	}

	if err := requireLicenseLive(nil, nil); err != nil {
		t.Errorf("fresh cached license should pass without touching the network: %v", err)
	}
	if networkHit {
		t.Error("requireLicenseLive hit the network despite a fresh cached ValidatedAt")
	}
}

func TestRequireLicenseLive_StaleCacheRevalidatesAndPersists(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/validate" {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"valid": true})
	})

	stale := time.Now().Add(-licenseRevalidateTTL - time.Hour)
	if err := saveLicenseFile(licenseFile{Key: "K", InstanceID: "I", ValidatedAt: stale}); err != nil {
		t.Fatalf("saveLicenseFile: %v", err)
	}

	if err := requireLicenseLive(nil, nil); err != nil {
		t.Fatalf("requireLicenseLive: %v", err)
	}

	after, err := loadLicenseFile()
	if err != nil {
		t.Fatalf("loadLicenseFile: %v", err)
	}
	if !after.ValidatedAt.After(stale) {
		t.Error("ValidatedAt should have been refreshed after a successful revalidate")
	}
}

func TestRequireLicenseLive_RevokedBlocks(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	stubLicenseServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"valid": false})
	})

	stale := time.Now().Add(-licenseRevalidateTTL - time.Hour)
	if err := saveLicenseFile(licenseFile{Key: "K", InstanceID: "I", ValidatedAt: stale}); err != nil {
		t.Fatalf("saveLicenseFile: %v", err)
	}

	err := requireLicenseLive(nil, nil)
	if err == nil {
		t.Fatal("expected an error when the server reports the license invalid")
	}
	if !strings.Contains(err.Error(), oaicaPurchaseURL) {
		t.Errorf("error should point at the purchase URL, got: %v", err)
	}
}

func TestRequireLicenseLive_OfflineWithinGraceStillPasses(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	// Point at a server that refuses connections outright (nothing listening).
	prev := licenseServerAPI
	licenseServerAPI = "http://127.0.0.1:1"
	t.Cleanup(func() { licenseServerAPI = prev })

	staleButInGrace := time.Now().Add(-licenseRevalidateTTL - time.Hour)
	if err := saveLicenseFile(licenseFile{Key: "K", InstanceID: "I", ValidatedAt: staleButInGrace}); err != nil {
		t.Fatalf("saveLicenseFile: %v", err)
	}

	if err := requireLicenseLive(nil, nil); err != nil {
		t.Errorf("an unreachable server within the offline grace window should not block: %v", err)
	}
}

func TestRequireLicenseLive_OfflinePastGraceBlocks(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	prev := licenseServerAPI
	licenseServerAPI = "http://127.0.0.1:1"
	t.Cleanup(func() { licenseServerAPI = prev })

	pastGrace := time.Now().Add(-licenseOfflineGrace - time.Hour)
	if err := saveLicenseFile(licenseFile{Key: "K", InstanceID: "I", ValidatedAt: pastGrace}); err != nil {
		t.Fatalf("saveLicenseFile: %v", err)
	}

	if err := requireLicenseLive(nil, nil); err == nil {
		t.Error("an unreachable server past the offline grace window should block")
	}
}

// A key below redactMinLen is reported by its LENGTH, not by its characters:
// eight of a nine-character key is the key. See
// license_redact_integrity_test.go for why the old flat "first four and last
// four" rule was a disclosure and not a redaction.
func TestRedactLicenseKey(t *testing.T) {
	cases := map[string]string{
		"":                     "****",
		"short":                "****(5 chars)",
		"OAICA-ABCD-EFGH-123":  "****(19 chars)",
		"OAICA-ABCD-EFGH-1234": "OAIC…1234",
	}
	for in, want := range cases {
		if got := redactLicenseKey(in); got != want {
			t.Errorf("redactLicenseKey(%q) = %q, want %q", in, got, want)
		}
	}
}
