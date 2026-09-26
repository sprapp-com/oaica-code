package launch

// license_future_validation_integrity_test.go — a hand-written `validated_at`
// in the FUTURE was trusted as "freshly validated", which made the licence file
// (and the OAICA_LICENSE_KEY anchor) a permanent offline bypass (2026-09-26
// audit, seventh round).
//
// Both freshness tests are `time.Since(validatedAt) < window`. `time.Since` of
// a future stamp is NEGATIVE, and a negative duration is less than any positive
// window — so
//
//	printf '{"key":"NOT-A-REAL-KEY","validated_at":"2126-01-01T00:00:00Z"}' \
//	  > ~/.oaica/license.json
//
// passed the launch gate with no purchase, no activation and no network call,
// forever. The same stamp in ~/.oaica/license_env.json did the same for
// OAICA_LICENSE_KEY. A record cannot vouch for its own future: a timestamp the
// machine has not reached yet is not evidence of anything, and the file is one
// the user (or a backup/clock-skewed host) can write.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// futureValidatedAt is far enough out that no plausible clock skew reaches it.
var futureValidatedAt = time.Date(2126, 1, 1, 0, 0, 0, 0, time.UTC)

// writeLicenseFile lays down the stored activation the gate reads.
func writeLicenseFile(t *testing.T, f licenseFile) {
	t.Helper()
	path, err := licenseFilePath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// A stored licence whose validated_at is in the future must still be checked
// against the licence server — and when the server says the key is not valid,
// the gate must refuse. Before the fix this returned nil: negative age beat
// licenseRevalidateTTL and licenseOfflineGrace alike.
func TestAFutureValidatedAtDoesNotVouchForAStoredLicense(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":false}`))
	}))
	t.Cleanup(srv.Close)
	prev := lemonSqueezyLicenseAPI
	lemonSqueezyLicenseAPI = srv.URL
	t.Cleanup(func() { lemonSqueezyLicenseAPI = prev })

	writeLicenseFile(t, licenseFile{
		Key:         "NOT-A-REAL-PURCHASED-KEY",
		InstanceID:  "x",
		ValidatedAt: futureValidatedAt,
	})

	err := requireLicenseLive(nil, nil)
	if err == nil {
		t.Errorf("a ~/.oaica/license.json with a hand-written validated_at of %s launched with no purchase and no network call — a future timestamp makes time.Since negative, so it satisfies `age < licenseRevalidateTTL` AND `age < licenseOfflineGrace` forever. A record cannot certify its own future (server calls: %d)",
			futureValidatedAt.Format(time.RFC3339), calls)
	}
	if calls != 1 {
		t.Errorf("the licence server was consulted %d times, want exactly 1 — a future validated_at must be treated as stale, so the key is re-checked rather than trusted", calls)
	}
}

// Same stamp, offline: the grace window must not be opened by it either. A
// never-actually-validated key that has never once reached the server has no
// grace to spend.
func TestAFutureValidatedAtDoesNotOpenTheOfflineGrace(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	licenseUnreachableServer(t)

	writeLicenseFile(t, licenseFile{
		Key:         "NOT-A-REAL-PURCHASED-KEY",
		ValidatedAt: futureValidatedAt,
	})

	if err := requireLicenseLive(nil, nil); err == nil {
		t.Error("a stored licence with a future validated_at launched against an unreachable licence server — the offline grace is anchored to a validation that never happened, so the file is a permanent offline bypass")
	}
}

// The env anchor is the same record shape with the same comparison, so it needs
// the same treatment: a future stamp there must not skip the network.
func TestAFutureEnvAnchorDoesNotVouchForTheInjectedKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":false}`))
	}))
	t.Cleanup(srv.Close)
	prev := lemonSqueezyLicenseAPI
	lemonSqueezyLicenseAPI = srv.URL
	t.Cleanup(func() { lemonSqueezyLicenseAPI = prev })

	key := "NOT-A-REAL-ENV-KEY"
	writeEnvAnchor(t, key, futureValidatedAt)
	t.Setenv("OAICA_LICENSE_KEY", key)

	if err := requireLicenseLive(nil, nil); err == nil {
		t.Errorf("an OAICA_LICENSE_KEY whose anchor claims a validation on %s launched with no purchase and no network call (server calls: %d)", futureValidatedAt.Format(time.RFC3339), calls)
	}
	if calls != 1 {
		t.Errorf("the licence server was consulted %d times, want exactly 1 — the anchor's future timestamp must not count as a fresh validation", calls)
	}
}

// And offline, for the same reason as the stored path.
func TestAFutureEnvAnchorDoesNotOpenTheOfflineGrace(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	licenseUnreachableServer(t)

	key := "NOT-A-REAL-ENV-KEY"
	writeEnvAnchor(t, key, futureValidatedAt)
	t.Setenv("OAICA_LICENSE_KEY", key)

	if err := requireLicenseLive(nil, nil); err == nil {
		t.Error("an OAICA_LICENSE_KEY with a future-stamped anchor launched against an unreachable licence server — the offline grace must be anchored to a validation the server actually performed")
	}
}

// The controls: a genuinely recent stamp still skips the network on both paths.
// The fix must reject the future, not the cache.
func TestAPastValidatedAtStillSkipsTheNetwork(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true}`))
	}))
	t.Cleanup(srv.Close)
	prev := lemonSqueezyLicenseAPI
	lemonSqueezyLicenseAPI = srv.URL
	t.Cleanup(func() { lemonSqueezyLicenseAPI = prev })

	writeLicenseFile(t, licenseFile{
		Key:         "OAICA-REAL-RECENTLY-VALIDATED",
		ValidatedAt: time.Now().Add(-time.Hour),
	})

	if err := requireLicenseLive(nil, nil); err != nil {
		t.Fatalf("a licence validated an hour ago was refused: %v", err)
	}
	if calls != 0 {
		t.Errorf("the licence server was called %d times for a licence validated an hour ago — inside the %s TTL the cached validation must carry the launch", calls, licenseRevalidateTTL)
	}

	key := "OAICA-REAL-ENV-RECENTLY-VALIDATED"
	writeEnvAnchor(t, key, time.Now().Add(-time.Hour))
	t.Setenv("OAICA_LICENSE_KEY", key)
	if err := requireLicenseLive(nil, nil); err != nil {
		t.Fatalf("the env path with a recent anchor was refused: %v", err)
	}
	if calls != 0 {
		t.Errorf("the licence server was called %d times for an env anchor an hour old", calls)
	}
}
