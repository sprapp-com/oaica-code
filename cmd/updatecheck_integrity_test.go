package cmd

// updatecheck_integrity_test.go — the update notice is the only channel that
// tells an already-installed client a fix exists, so two ways it could go
// quiet are worth pinning (2026-09-26 audit, third round):
//
//   - fetchLatestVersion read the body with ONE 256-byte Read. A single Read
//     may return fewer bytes than the file holds, so a version stamp could
//     arrive as "vers" and be treated as "no answer";
//   - checkForUpdate stamped LastChecked even when the fetch FAILED, so a
//     timeout, a 404 or a captive portal silenced the notice for the full
//     20-hour interval.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/ollama/ollama/version"
)

// setUpdateCheckVersion makes this a RELEASE build as far as the check is
// concerned: checkForUpdate returns immediately for "0.0.0" (a dev build is
// never nagged about being outdated), so a test that leaves it alone exercises
// nothing at all.
func setUpdateCheckVersion(t *testing.T, v string) {
	t.Helper()
	old := version.Version
	version.Version = v
	t.Cleanup(func() { version.Version = old })
}

// A body that arrives in dribs and drabs still parses: the read must not rely
// on one Read returning the whole file.
func TestFetchLatestVersionReadsTheWholeBody(t *testing.T) {
	const body = "version=0.5.47\ncommit=deadbeef\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Flush the first two bytes, then the rest — the shape a single
		// Read can legitimately return, and the shape the old code lost.
		w.(http.Flusher).Flush()
		_, _ = w.Write([]byte(body[:2]))
		w.(http.Flusher).Flush()
		time.Sleep(10 * time.Millisecond)
		_, _ = w.Write([]byte(body[2:]))
	}))
	defer srv.Close()

	old := updateCheckURLForTest(srv.URL)
	defer old()

	if got := fetchLatestVersion(context.Background()); got != "0.5.47" {
		t.Errorf("fetchLatestVersion() = %q, want 0.5.47 — a body delivered in more than one Read must still be read whole", got)
	}
}

// A large body (a captive portal's HTML, a proxy's error page) is bounded and
// does not parse into a bogus version.
func TestFetchLatestVersionIgnoresAnUnrelatedBody(t *testing.T) {
	page := make([]byte, 200_000)
	for i := range page {
		page[i] = 'x'
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write(page)
	}))
	defer srv.Close()

	old := updateCheckURLForTest(srv.URL)
	defer old()

	if got := fetchLatestVersion(context.Background()); got != "" {
		t.Errorf("fetchLatestVersion() = %q for a body with no version stamp, want empty", got)
	}
}

// S10: a failed check does not stamp LastChecked.
func TestFailedUpdateCheckDoesNotStampLastChecked(t *testing.T) {
	setUpdateCheckHome(t)
	setUpdateCheckVersion(t, "0.5.0")

	dead := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	url := dead.URL
	dead.Close() // guaranteed connection refused

	restore := updateCheckURLForTest(url)
	defer restore()
	t.Setenv("OAICA_NO_UPDATE_CHECK", "")

	checkForUpdate()

	cache := loadUpdateCheckCache()
	if !cache.LastChecked.IsZero() {
		t.Errorf("LastChecked = %v after a FAILED check — the next invocation sees a fresh-looking cache and stays quiet for %v instead of trying again (an offline machine, a 404, a captive portal)",
			cache.LastChecked, updateCheckInterval)
	}
	if cache.LastAttempt.IsZero() {
		t.Error("LastAttempt was not recorded after a failed check, so nothing throttles the retry — every command would pay the network timeout while offline")
	}
}

// A successful check does stamp it, and survives a round-trip through the file.
func TestSuccessfulUpdateCheckStampsLastChecked(t *testing.T) {
	setUpdateCheckHome(t)
	setUpdateCheckVersion(t, "0.5.0")

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("version=0.5.47\ncommit=deadbeef\n"))
	}))
	defer srv.Close()

	restore := updateCheckURLForTest(srv.URL)
	defer restore()
	t.Setenv("OAICA_NO_UPDATE_CHECK", "")

	checkForUpdate()

	cache := loadUpdateCheckCache()
	if cache.LastChecked.IsZero() {
		t.Error("a successful check did not stamp LastChecked — the interval would never throttle anything")
	}
	if cache.LatestVersion != "0.5.47" {
		t.Errorf("LatestVersion = %q, want 0.5.47", cache.LatestVersion)
	}
	b, err := os.ReadFile(mustUpdateCheckCachePath(t))
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(b, &raw); err != nil {
		t.Fatalf("the cache file is not JSON: %v", err)
	}
}

// And the retry backoff: a second invocation right after a failure does not
// hit the network again.
func TestFailedUpdateCheckBacksOffRetries(t *testing.T) {
	setUpdateCheckHome(t)
	setUpdateCheckVersion(t, "0.5.0")

	hits := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	restore := updateCheckURLForTest(srv.URL)
	defer restore()
	t.Setenv("OAICA_NO_UPDATE_CHECK", "")

	checkForUpdate()
	first := hits
	checkForUpdate()
	if hits != first {
		t.Errorf("the second invocation issued another request (%d -> %d) — a machine that is offline pays %v on every single command", first, hits, updateCheckTimeout)
	}
	if first == 0 {
		t.Fatal("the first invocation never reached the server, so this test proves nothing")
	}
}

func mustUpdateCheckCachePath(t *testing.T) string {
	t.Helper()
	p, err := updateCheckCachePath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}
