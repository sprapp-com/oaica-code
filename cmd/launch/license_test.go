package launch

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// stubLemonSqueezy starts an httptest server implementing just enough of
// the /activate and /validate endpoints for these tests, and points
// lemonSqueezyLicenseAPI at it for the duration of the test.
func stubLemonSqueezy(t *testing.T, handler http.HandlerFunc) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	prev := lemonSqueezyLicenseAPI
	lemonSqueezyLicenseAPI = srv.URL
	t.Cleanup(func() { lemonSqueezyLicenseAPI = prev })
}

func TestActivateLicenseLive_Success(t *testing.T) {
	stubLemonSqueezy(t, func(w http.ResponseWriter, r *http.Request) {
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
	stubLemonSqueezy(t, func(w http.ResponseWriter, r *http.Request) {
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
	stubLemonSqueezy(t, func(w http.ResponseWriter, r *http.Request) {
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
	stubLemonSqueezy(t, func(w http.ResponseWriter, r *http.Request) {
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
	stubLemonSqueezy(t, func(w http.ResponseWriter, r *http.Request) {
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
	prev := lemonSqueezyLicenseAPI
	lemonSqueezyLicenseAPI = "http://127.0.0.1:1"
	t.Cleanup(func() { lemonSqueezyLicenseAPI = prev })

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
	prev := lemonSqueezyLicenseAPI
	lemonSqueezyLicenseAPI = "http://127.0.0.1:1"
	t.Cleanup(func() { lemonSqueezyLicenseAPI = prev })

	pastGrace := time.Now().Add(-licenseOfflineGrace - time.Hour)
	if err := saveLicenseFile(licenseFile{Key: "K", InstanceID: "I", ValidatedAt: pastGrace}); err != nil {
		t.Fatalf("saveLicenseFile: %v", err)
	}

	if err := requireLicenseLive(nil, nil); err == nil {
		t.Error("an unreachable server past the offline grace window should block")
	}
}

func TestRedactLicenseKey(t *testing.T) {
	cases := map[string]string{
		"":                     "****",
		"short":                "****",
		"OAICA-ABCD-EFGH-1234": "OAIC…1234",
	}
	for in, want := range cases {
		if got := redactLicenseKey(in); got != want {
			t.Errorf("redactLicenseKey(%q) = %q, want %q", in, got, want)
		}
	}
}
