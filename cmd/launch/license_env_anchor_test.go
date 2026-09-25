package launch

// license_env_anchor_test.go — OAICA_LICENSE_KEY is a licence MODE, and it has
// to be the same mode a stored activation is, or it is a bypass:
//
//   - it must WIN over a stored ~/.oaica/license.json (before 2026-09-26 the
//     file was read first, so a stale activation on the same machine shadowed
//     the injected key and the same variable meant two different things
//     depending on which command ran);
//   - an unreachable licence server must be bounded by the same offline grace
//     the stored path has. The env path originally ran no network check at all
//     and treated every error as a warning, so a key that had never been
//     validated — or had been refunded — worked forever offline;
//   - and the key itself must never be written down. The anchor records a
//     SHA-256 and a timestamp, nothing else.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// licenseStubServer answers the Lemon Squeezy /validate call with valid.
func licenseStubServer(t *testing.T, valid bool) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":` + map[bool]string{true: "true", false: "false"}[valid] + `}`))
	}))
	t.Cleanup(srv.Close)
	prev := lemonSqueezyLicenseAPI
	lemonSqueezyLicenseAPI = srv.URL
	t.Cleanup(func() { lemonSqueezyLicenseAPI = prev })
}

// licenseUnreachableServer points the licence API at a closed port.
func licenseUnreachableServer(t *testing.T) {
	t.Helper()
	prev := lemonSqueezyLicenseAPI
	lemonSqueezyLicenseAPI = "http://127.0.0.1:1"
	t.Cleanup(func() { lemonSqueezyLicenseAPI = prev })
}

func writeEnvAnchor(t *testing.T, key string, at time.Time) {
	t.Helper()
	path, err := envLicenseAnchorPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(envLicenseAnchor{KeySHA256: sha256Hex(key), ValidatedAt: at})
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
}

// The env key wins over a stored activation that would itself be refused.
func TestEnvLicenseKeyWinsOverAStoredActivation(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)
	licenseStubServer(t, false) // the stored key would be rejected live

	licensePath := filepath.Join(dir, ".oaica", "license.json")
	if err := os.MkdirAll(filepath.Dir(licensePath), 0o700); err != nil {
		t.Fatal(err)
	}
	stale, _ := json.Marshal(licenseFile{
		Key: "OAICA-STORED-STALE", ActivatedAt: time.Now().Add(-400 * 24 * time.Hour),
		ValidatedAt: time.Now().Add(-400 * 24 * time.Hour),
	})
	if err := os.WriteFile(licensePath, stale, 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("OAICA_LICENSE_KEY", testLicenseKey)
	if err := requireLicenseLive(nil, nil); err != nil {
		t.Errorf("with OAICA_LICENSE_KEY set and a stale ~/.oaica/license.json on disk, the launch gate refused: %v\n— the file was read first and shadowed the injected key", err)
	}
}

// No anchor, unreachable server: the env key cannot vouch for itself.
func TestUnvalidatedEnvLicenseKeyDoesNotSurviveAnOfflineServer(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	licenseUnreachableServer(t)

	t.Setenv("OAICA_LICENSE_KEY", "OAICA-REAL-NEVER-VALIDATED")
	if err := requireLicenseLive(nil, nil); err == nil {
		t.Error("a never-validated OAICA_LICENSE_KEY launched against an unreachable licence server — the env path has no offline bound, so any string in that variable is a permanent free pass")
	}
}

// A fresh anchor: offline, no network call needed.
func TestFreshEnvAnchorRidesOutAnOfflineServer(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)
	licenseUnreachableServer(t)

	key := "OAICA-REAL-ANCHORED"
	writeEnvAnchor(t, key, time.Now().Add(-time.Hour))

	t.Setenv("OAICA_LICENSE_KEY", key)
	if err := requireLicenseLive(nil, nil); err != nil {
		t.Errorf("an env key validated an hour ago was refused with the server unreachable: %v\n— it is inside both the revalidate TTL and the offline grace", err)
	}
}

// An anchor past the offline grace: hard block, same as the stored path.
func TestStaleEnvAnchorIsBlocked(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)
	licenseUnreachableServer(t)

	key := "OAICA-REAL-EXPIRED"
	writeEnvAnchor(t, key, time.Now().Add(-(licenseOfflineGrace + 48*time.Hour)))

	t.Setenv("OAICA_LICENSE_KEY", key)
	if err := requireLicenseLive(nil, nil); err == nil {
		t.Errorf("an env key last validated %s ago still launched offline — past the %s grace the gate must block, or the env path is a bypass rather than a licence mode", licenseOfflineGrace+48*time.Hour, licenseOfflineGrace)
	}
}

// A fresh anchor costs no network call: the stored path skips the request
// inside the TTL and the env path must too, or every launch waits on it.
func TestFreshEnvAnchorSkipsTheNetwork(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)

	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"valid":true}`))
	}))
	defer srv.Close()
	prev := lemonSqueezyLicenseAPI
	lemonSqueezyLicenseAPI = srv.URL
	t.Cleanup(func() { lemonSqueezyLicenseAPI = prev })

	key := "OAICA-REAL-CACHED"
	writeEnvAnchor(t, key, time.Now().Add(-time.Hour))
	t.Setenv("OAICA_LICENSE_KEY", key)

	if err := requireLicenseLive(nil, nil); err != nil {
		t.Fatalf("fresh anchor: %v", err)
	}
	if calls != 0 {
		t.Errorf("a key validated an hour ago made %d network call(s) — the stored path skips the call inside the TTL and the env path must match", calls)
	}
}

// The anchor holds a digest, never the key.
func TestEnvAnchorNeverStoresTheKey(t *testing.T) {
	dir := t.TempDir()
	setLaunchTestHome(t, dir)
	licenseStubServer(t, true)

	key := "OAICA-REAL-SECRET-VALUE"
	t.Setenv("OAICA_LICENSE_KEY", key)
	if err := requireLicenseLive(nil, nil); err != nil {
		t.Fatalf("validated key refused: %v", err)
	}

	anchorPath, err := envLicenseAnchorPath()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(anchorPath)
	if err != nil {
		t.Fatalf("no anchor was written, so offline validation has nothing to vouch for: %v", err)
	}
	if strings.Contains(string(b), key) {
		t.Errorf("the anchor at %s contains the licence key verbatim — an injected secret must stay in the secret manager:\n%s", anchorPath, b)
	}

	if _, err := os.Stat(filepath.Join(dir, ".oaica", "license.json")); err == nil {
		t.Error("the env path wrote ~/.oaica/license.json — it would then shadow the env key it came from")
	}
}
