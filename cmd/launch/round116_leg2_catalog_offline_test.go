// round116_leg2_catalog_offline_test.go — F116-L2-1 (2026-09-29 audit, round 116): a sync that could not
// reach its source says so, and a torn cache is refused, as provider sync does.

package launch

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// Offline + damaged catalog cache: catalog sync vs provider sync.
func TestRound116DamagedCatalogCacheOffline(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	dead := srv.URL + "/api.json"
	srv.Close()

	cp, _ := catalogCachePath()
	_ = os.MkdirAll(filepath.Dir(cp), 0o700)
	_ = os.WriteFile(cp, []byte(`{"openai":{"id":"openai","models":{"gp`), 0o600) // torn write
	saveCatalogCacheSource(cp, redactBaseURL(dead))
	rep, err := CatalogSync(dead)
	t.Logf("catalog sync (no etag):  rep=%+v err=%v", rep, err)
	saveCatalogETag(cp+".etag", redactBaseURL(dead), `"v1"`)
	rep, err = CatalogSync(dead)
	t.Logf("catalog sync (etag):     rep=%+v err=%v", rep, err)
	if err == nil {
		t.Errorf("RED: offline catalog sync over a torn cache reported success: %+v", rep)
	}

	pp, _ := oaicaOverlayCachePath()
	_ = os.MkdirAll(filepath.Dir(pp), 0o700)
	_ = os.WriteFile(pp, []byte(`{"providers":[{"id":"x"`), 0o600)
	saveCatalogCacheSource(pp, redactBaseURL(dead))
	prep, perr := ProviderSync(dead)
	t.Logf("provider sync:           rep=%+v err=%v", prep, perr)
}
func TestRound116OfflineValidCatalogLabel(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"v1"`)
		_, _ = w.Write([]byte(modelsDevFixture))
	}))
	u := srv.URL + "/api.json"
	rep, err := CatalogSync(u)
	t.Logf("online first:  rep=%+v err=%v", rep, err)
	srv.Close()
	rep, err = CatalogSync(u)
	t.Logf("offline:       rep=%+v err=%v", rep, err)
	if rep.Unchanged {
		t.Errorf("RED: an unreachable source reported as 'unchanged' (checked) rather than cached")
	}
}
