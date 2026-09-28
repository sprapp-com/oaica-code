package launch

// catalog_offline_source_binding_integrity_test.go — the offline fallback
// served a cache that belonged to a different catalogue URL (2026-09-26 audit,
// tenth round, auditor B).
//
// Sync once from a private mirror, then run `oaica remote sync` offline with
// the default URL: the transport-error branch read the one fixed cache path
// regardless of the URL asked for, and the report echoed the URL that was ASKED
// FOR with FromCache set. The user concludes the default catalogue was verified
// from cache when a private mirror's contents are what is on disk — the
// transport-error sibling of the third-round 304 fix, which bound the validator
// to its URL but left the cached BODY claimable by anyone.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// deadCatalogURL is a URL nothing answers.
func deadCatalogURL(t *testing.T) string {
	t.Helper()
	return "http://" + deadUpstreamAddress(t) + "/catalog.json"
}

// The catalogue caches, each with its own sync entry point. One entry now: the
// overlay carries the provider rows, the first-party models and the cloud-alias
// limits, so `oaica remote sync` is the only sync left (CloudLimitsSync and its
// cache were retired with the file they synced).
var catalogSyncCases = []struct {
	name   string
	body   string
	marker string
	path   func(*testing.T) string
	sync   func(string) (int, bool, error)
}{
	{
		name:   "oaica overlay",
		body:   `{"version":1,"providers":[{"name":"from-mirror","base_url":"https://mirror.example.com"}]}`,
		marker: "from-mirror",
		path:   providerCachePath,
		sync: func(u string) (int, bool, error) {
			r, err := ProviderSync(u)
			return r.Count, r.FromCache, err
		},
	},
}

// A cache written from one URL must not answer a request for another.
func TestAnOfflineSyncRefusesAnotherCatalogsCache(t *testing.T) {
	for _, tc := range catalogSyncCases {
		t.Run(tc.name, func(t *testing.T) {
			setLaunchTestHome(t, t.TempDir())

			mirror := newCatalogServer(t, tc.body, "etag-mirror")
			if _, fromCache, err := tc.sync(mirror.URL + "/catalog.json"); err != nil {
				t.Fatalf("first sync from the mirror: %v", err)
			} else if fromCache {
				t.Fatalf("the first sync was answered from cache")
			}
			cache := tc.path(t)
			if b, err := os.ReadFile(cache); err != nil || !strings.Contains(string(b), tc.marker) {
				t.Fatalf("premise: the mirror's body is not in the cache (%v)", err)
			}
			mirror.Close() // the mirror is gone; only its cache remains

			// Now the DEFAULT catalogue, offline.
			dead := deadCatalogURL(t)
			count, fromCache, err := tc.sync(dead)
			if fromCache {
				t.Errorf("an offline sync of %s reported FromCache with count %d — the cache on disk came from a DIFFERENT url, so the report names one catalogue and hands back another's contents", dead, count)
			}
			if err == nil {
				t.Errorf("an offline sync of %s returned no error after serving another catalogue's cache", dead)
			}
			if count != 0 {
				t.Errorf("the report carries count %d from the other catalogue", count)
			}
		})
	}
}

// Control: the offline fallback still works for the URL the cache really came
// from — the fix must not disable offline sync altogether.
func TestAnOfflineSyncStillServesItsOwnCatalogsCache(t *testing.T) {
	for _, tc := range catalogSyncCases {
		t.Run(tc.name, func(t *testing.T) {
			setLaunchTestHome(t, t.TempDir())

			mirror := newCatalogServer(t, tc.body, "etag-mirror")
			if _, _, err := tc.sync(mirror.URL + "/catalog.json"); err != nil {
				t.Fatalf("first sync: %v", err)
			}
			mirror.Close()

			count, fromCache, err := tc.sync(mirror.URL + "/catalog.json")
			if err != nil {
				t.Fatalf("offline sync of the URL the cache came from failed: %v", err)
			}
			if !fromCache || count == 0 {
				t.Errorf("the offline fallback answered fromCache=%t count=%d for the cache's own URL, want the cached catalogue", fromCache, count)
			}
		})
	}
}

// A cache written before the source was recorded has no record to check, so
// only the package's own default catalogue may claim it — and it must still be
// claimable, or every user upgrading would lose offline sync.
func TestALegacyCacheIsClaimableOnlyByTheDefaultCatalog(t *testing.T) {
	home := t.TempDir()
	setLaunchTestHome(t, home)

	cache := providerCachePath(t)
	if err := os.MkdirAll(filepath.Dir(cache), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cache, []byte(`{"version":1,"providers":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	// No .url sibling: the shape an older version left behind.

	if !catalogCacheSourceMatches(cache, redactBaseURL(defaultProviderSyncURL), defaultProviderSyncURL) {
		t.Errorf("a legacy cache is not claimable by the default catalogue — every existing user would lose offline sync after upgrading")
	}
	if catalogCacheSourceMatches(cache, "https://mirror.example.com/catalog.json", defaultProviderSyncURL) {
		t.Errorf("a legacy cache is claimable by an arbitrary URL — that is the cross-serving hole itself")
	}
}
