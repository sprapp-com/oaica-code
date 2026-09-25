package launch

// catalog_sync_integrity_test.go — two ways a catalog sync could leave the
// machine worse than it found it (2026-09-26 audit, third round):
//
//   - S6: the response body was cached without ever being parsed. One bad
//     response (an HTML login page from a captive portal, a truncated
//     transfer) replaced the last good cache — and the synced cache WINS over
//     the embedded default, so every alias's limits silently collapsed to the
//     built-in `262144` → `1` until someone synced again;
//   - S5: the ETag lived in an unbound `…json.etag` file, so syncing from a
//     second URL sent the first URL's validator, and a reply of 304 then
//     served the OTHER catalog's cached body under the new URL's name.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// catalogServer serves body with whatever ETag the test asks for, and records
// the If-None-Match of every request it receives.
type catalogServer struct {
	*httptest.Server
	mu     sync.Mutex
	inm    []string
	etag   string
	body   string
	status int
}

func newCatalogServer(t *testing.T, body, etag string) *catalogServer {
	t.Helper()
	cs := &catalogServer{body: body, etag: etag}
	cs.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cs.mu.Lock()
		cs.inm = append(cs.inm, r.Header.Get("If-None-Match"))
		etag, body, status := cs.etag, cs.body, cs.status
		cs.mu.Unlock()
		if status != 0 {
			w.WriteHeader(status)
			return
		}
		if etag != "" {
			w.Header().Set("ETag", etag)
		}
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(cs.Close)
	return cs
}

func (cs *catalogServer) set(body, etag string) {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	cs.body, cs.etag = body, etag
}

func (cs *catalogServer) validators() []string {
	cs.mu.Lock()
	defer cs.mu.Unlock()
	return append([]string(nil), cs.inm...)
}

// providerCachePath is this HOME's synced provider catalog.
func providerCachePath(t *testing.T) string {
	t.Helper()
	p, err := providerCatalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func cloudLimitsCachePath(t *testing.T) string {
	t.Helper()
	p, err := cloudLimitsCatalogCachePath()
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// S6: a body that is not a catalog is never cached, and the last good copy
// survives. Asserted on what the NEXT reader sees, not just on the error.
func TestCatalogSyncRefusesToCacheAnUnparseableBody(t *testing.T) {
	const good = `{"version":1,"providers":[{"name":"good-provider","base_url":"https://good.example.com"}]}`

	for _, tc := range []struct {
		name  string
		bad   string
		sync  func(string) error
		path  func(*testing.T) string
		read  func(string) string
		count int
	}{
		{
			name: "provider catalog", bad: "<html>captive portal</html>",
			path:  providerCachePath,
			sync:  func(u string) error { _, err := ProviderSync(u); return err },
			count: 1,
		},
		{
			name: "cloud limits catalog",
			bad:  `{"version":1,"limits":[{"model":"x"}]}`, // array where a map belongs
			path: cloudLimitsCachePath,
			sync: func(u string) error { _, err := CloudLimitsSync(u); return err },
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			setLaunchTestHome(t, home)

			// Seed the last good copy, exactly as a previous successful sync
			// would have left it.
			cache := tc.path(t)
			if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
				t.Fatal(err)
			}
			seed := good
			if tc.count == 0 {
				seed = `{"version":1,"limits":{"glm-5":{"context":202752,"output":131072}}}`
			}
			if err := os.WriteFile(cache, []byte(seed), 0o600); err != nil {
				t.Fatal(err)
			}

			srv := newCatalogServer(t, tc.bad, "")
			if err := tc.sync(srv.URL + "/catalog.json"); err == nil {
				t.Error("sync returned nil for a body that is not a catalog — it cached the garbage and reported success")
			}

			after, err := os.ReadFile(cache)
			if err != nil {
				t.Fatalf("the cache was removed: %v", err)
			}
			if string(after) != seed {
				t.Errorf("the cache now holds %q, want the last good copy %q — an unreadable response must never replace it", after, seed)
			}
		})
	}
}

// The same defect seen through the consumer: a body cached by an earlier
// version of the code (or by hand) must not silently become the override that
// collapses every alias's limits.
func TestCloudLimitsFromCatalogFallsBackToEmbeddedDefaults(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	cache := cloudLimitsCachePath(t)
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte("<html>not json</html>"), 0o600); err != nil {
		t.Fatal(err)
	}
	got := cloudLimitsFromCatalog()
	if len(got) == 0 {
		t.Error("a corrupt cache emptied the whole limits catalogue — the embedded default is meant to be the floor")
	}
}

// S5a: the validator of one catalog is never sent to another host.
func TestCatalogSyncDoesNotSendAForeignETag(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	mirror := newCatalogServer(t, `{"version":1,"providers":[{"name":"from-mirror","base_url":"https://mirror.example.com"}]}`, "etag-from-mirror")
	other := newCatalogServer(t, `{"version":1,"providers":[{"name":"from-other","base_url":"https://other.example.com"}]}`, "etag-from-other")

	if _, err := ProviderSync(mirror.URL + "/providers.json"); err != nil {
		t.Fatalf("first sync: %v", err)
	}
	if _, err := ProviderSync(other.URL + "/providers.json"); err != nil {
		t.Fatalf("second sync: %v", err)
	}

	for i, v := range other.validators() {
		if strings.Contains(v, "etag-from-mirror") {
			t.Errorf("request %d to the second host carried the first host's validator %q — a 304 in reply would serve the other catalog's cached body while the report names this URL", i, v)
		}
	}
	b, err := os.ReadFile(providerCachePath(t))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "from-other") {
		t.Errorf("the cache holds %q, want the second host's catalog", b)
	}
}

// S5b: a response with no ETag clears the previous one.
func TestCatalogSyncClearsAStaleETag(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	srv := newCatalogServer(t, `{"version":1,"providers":[{"name":"p1"}]}`, "etag-1")
	if _, err := ProviderSync(srv.URL + "/providers.json"); err != nil {
		t.Fatal(err)
	}
	etagPath := providerCachePath(t) + ".etag"
	if _, err := os.Stat(etagPath); err != nil {
		t.Fatalf("premise: the first sync recorded no validator: %v", err)
	}

	srv.set(`{"version":1,"providers":[{"name":"p2"}]}`, "") // same URL, no validator this time
	if _, err := ProviderSync(srv.URL + "/providers.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(etagPath); err == nil {
		t.Error("the stale validator is still on disk after a response that carried none — the next sync would send a validator for a body that has been replaced")
	}
}

// The binding itself, at the unit level: a legacy bare-word etag belongs only
// to the default catalog.
func TestCatalogETagBinding(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "catalog.json.etag")

	saveCatalogETag(path, "https://a.example/catalog.json", "etag-a")
	if got := loadCatalogETag(path, "https://a.example/catalog.json", defaultProviderSyncURL); got != "etag-a" {
		t.Errorf("loadCatalogETag for the URL it was saved under = %q, want etag-a", got)
	}
	if got := loadCatalogETag(path, "https://b.example/catalog.json", defaultProviderSyncURL); got != "" {
		t.Errorf("loadCatalogETag for a DIFFERENT url = %q, want empty — this is the cross-host validator leak", got)
	}

	// A file written before the URL was recorded: usable only for the default.
	if err := os.WriteFile(path, []byte("bare-etag\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := loadCatalogETag(path, "https://mirror.example/catalog.json", defaultProviderSyncURL); got != "" {
		t.Errorf("a legacy unbound etag was handed to a non-default URL (%q) — it cannot be known to belong there", got)
	}
	if got := loadCatalogETag(path, redactBaseURL(defaultProviderSyncURL), defaultProviderSyncURL); got != "bare-etag" {
		t.Errorf("a legacy unbound etag for the default catalog = %q, want bare-etag", got)
	}

	// No validator in the response clears the file.
	saveCatalogETag(path, "https://a.example/catalog.json", "")
	if _, err := os.Stat(path); err == nil {
		t.Error("saveCatalogETag with an empty validator left the file in place")
	}

	// And the stored file never carries a credential.
	saveCatalogETag(path, redactBaseURL("https://sk-live-SECRET@mirror.example/catalog.json"), "e")
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(b), "sk-live-SECRET") {
		t.Errorf("the etag binding file holds the mirror credential:\n%s", b)
	}
	var check map[string]any
	if err := json.Unmarshal(b, &check); err != nil {
		t.Fatalf("the etag file is not JSON (%v): %s", err, b)
	}
}
