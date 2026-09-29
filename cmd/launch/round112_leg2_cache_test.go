package launch

// round112_leg2_cache_test.go — leg 2, round 112 (2026-09-29 audit), F112-L2-1 and
// the healing half of F112-L2-2.

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// F112-L2-1: two disk caches trusted a future-dated saved_at. Age was
// time.Since(SavedAt), which is negative when the cache was written while the clock
// was ahead (a wrong RTC, a restored VM snapshot, a dual-boot box) and NTP has since
// corrected it, so `age < ttl` held until the clock caught up: a remembered failure
// was served for days with no upstream request at all, and a stale model list the
// same. model_inventory.go and license.go already treat a negative age as "not a
// cache"; these two siblings did not.
func TestMine112AFutureDatedRemoteModelsCacheIsNotACache(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var hits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"live-model"}]}`))
	}))
	t.Cleanup(up.Close)
	remote := userRemote{Name: "box", BaseURL: up.URL + "/v1"}
	path, perr := remoteModelsCachePath(remote.Name, remote.BaseURL)
	if perr != nil || path == "" {
		t.Skip("no cache path in this environment")
	}
	stale := remoteModelsCacheFile{SavedAt: time.Now().Add(10 * 24 * time.Hour), TTLSecond: remoteModelsErrorTTL.Seconds(), Error: "box: HTTP 502"}
	b, _ := json.Marshal(stale)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ids, err := fetchRemoteModelsCached(remote)
	if err != nil || len(ids) != 1 || ids[0] != "live-model" || hits == 0 {
		t.Errorf("ids=%v err=%v upstream hits=%d — a cache dated in the future is not a cache: the healthy upstream must be asked (2026-09-29 audit, round 112, F112-L2-1)", ids, err, hits)
	}
}

func TestMine112AFutureDatedOllamaCloudCacheIsNotACache(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	path, perr := ollamaCloudCachePath()
	if perr != nil || path == "" {
		t.Skip("no cache path in this environment")
	}
	b, _ := json.Marshal(ollamaCloudCache{SavedAt: time.Now().Add(100 * 24 * time.Hour), TTLSecond: ollamaCloudCacheTTL.Seconds(), IDs: []string{"stale-model"}})
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, b, 0o600); err != nil {
		t.Fatal(err)
	}
	ids, _ := ollamaCloudModelIDsErr()
	for _, id := range ids {
		if id == "stale-model" {
			t.Errorf("a cache dated 100 days ahead was served as fresh: %v (2026-09-29 audit, round 112, F112-L2-1)", ids)
		}
	}
}

// F112-L2-2 (healing): a catalog cache damaged by an interrupted write, with the old
// body's ETag still beside it, answered 304 for ever, and CatalogSync took its
// byte-identical early return before it validated anything: "unchanged, 0 providers",
// exit 0, until the upstream's ETag happened to change. A cache that will not parse
// is dropped along with its ETag and refetched.
func TestMine112ADamagedCatalogCacheHealsOnTheNextSync(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("If-None-Match") == `"v1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(modelsDevFixture))
	}))
	t.Cleanup(srv.Close)
	if _, err := CatalogSync(srv.URL); err != nil {
		t.Fatal(err)
	}
	path, _ := catalogCachePath()
	b, _ := os.ReadFile(path)
	// What a kill in the middle of a truncating write leaves: half the body, and the
	// ETag of the whole one still beside it.
	if err := os.WriteFile(path, b[:len(b)/2], 0o600); err != nil {
		t.Fatal(err)
	}
	rep, err := CatalogSync(srv.URL)
	if err != nil || rep.Providers == 0 {
		t.Fatalf("sync after damage: rep=%+v err=%v — a damaged cache must be refetched, not reported unchanged with no providers (2026-09-29 audit, round 112, F112-L2-2)", rep, err)
	}
	if _, ok := loadModelsDevCatalog(); !ok {
		t.Errorf("the catalog is still unreadable after the sync (2026-09-29 audit, round 112, F112-L2-2)")
	}
}
