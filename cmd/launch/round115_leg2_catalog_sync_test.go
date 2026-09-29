// round115_leg2_catalog_sync_test.go — F115-L2-2/3 (2026-09-29 audit, round 115): catalog sync binds its
// cache and ETag to the source URL and prints only the redacted form, as provider sync does.

package launch

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const probeOverlayBody = `{"providers":[]}`

// Same shape on both sync doors: sync from source A (online), then sync from a
// DIFFERENT source B that is unreachable.
func TestRound115CatalogSyncRefusesAForeignCacheForAnUnreachableSource(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"a1"`)
		if strings.Contains(r.URL.Path, "overlay") {
			w.Write([]byte(probeOverlayBody))
			return
		}
		w.Write([]byte(modelsDevFixture))
	}))
	defer a.Close()
	b := httptest.NewServer(http.NotFoundHandler())
	bURL := b.URL
	b.Close() // unreachable

	if _, err := ProviderSync(a.URL + "/overlay.json"); err != nil {
		t.Fatalf("provider sync A: %v", err)
	}
	_, perr := ProviderSync(bURL + "/overlay.json")
	t.Logf("ProviderSync(B unreachable) err = %v", perr)

	if _, err := CatalogSync(a.URL + "/api.json"); err != nil {
		t.Fatalf("catalog sync A: %v", err)
	}
	rep, cerr := CatalogSync(bURL + "/api.json")
	t.Logf("CatalogSync(B unreachable) rep = %+v err = %v", rep, cerr)
	if perr != nil && cerr == nil {
		t.Fatalf("doors diverge: provider sync refuses a foreign cache for an unreachable source, catalog sync reports A's catalog as %s", rep.URL)
	}
}

func TestRound115CatalogSyncReportNeverCarriesTheCredential(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.Contains(r.URL.Path, "overlay") {
			w.Write([]byte(probeOverlayBody))
			return
		}
		w.Write([]byte(modelsDevFixture))
	}))
	defer a.Close()
	cred := strings.Replace(a.URL, "http://", "http://bob:s3cr3t-token@", 1)
	prep, perr := ProviderSync(cred + "/overlay.json?key=QSECRET")
	crep, cerr := CatalogSync(cred + "/api.json?key=QSECRET")
	t.Logf("ProviderSync URL = %q err=%v", prep.URL, perr)
	t.Logf("CatalogSync  URL = %q err=%v", crep.URL, cerr)
	if strings.Contains(crep.URL, "s3cr3t") || strings.Contains(crep.URL, "QSECRET") {
		t.Fatalf("CatalogSync report (printed by `oaica model catalog sync`) carries the credential: %s", crep.URL)
	}
}

// The control: the SAME source, unreachable, still serves its own cache. The refusal
// is for a different source, not for being offline.
func TestRound115CatalogSyncOfflineSameSourceStillServesItsCache(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte(modelsDevFixture))
	}))
	aURL := a.URL + "/api.json"
	if _, err := CatalogSync(aURL); err != nil {
		t.Fatalf("catalog sync A: %v", err)
	}
	a.Close()
	rep, err := CatalogSync(aURL)
	if err != nil {
		t.Fatalf("offline sync of the source the cache came from was refused: %v", err)
	}
	if rep.Providers == 0 {
		t.Fatalf("offline sync reported an empty catalog: %+v", rep)
	}
}

// A validator issued by source A is never sent to source B.
func TestRound115CatalogSyncNeverSendsOneSourcesETagToAnother(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"from-a"`)
		w.Write([]byte(modelsDevFixture))
	}))
	defer a.Close()
	sent := make(chan string, 4)
	b := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent <- r.Header.Get("If-None-Match")
		w.Write([]byte(modelsDevFixture))
	}))
	defer b.Close()
	if _, err := CatalogSync(a.URL + "/api.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := CatalogSync(b.URL + "/api.json"); err != nil {
		t.Fatal(err)
	}
	if got := <-sent; got != "" {
		t.Fatalf("source B was sent source A's validator %q", got)
	}
}

// The control: the same source is sent its own validator back, so the binding costs a
// mirror user no revalidation.
func TestRound115CatalogSyncSendsASourceItsOwnETag(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	sent := make(chan string, 4)
	a := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		sent <- r.Header.Get("If-None-Match")
		w.Header().Set("ETag", `"from-a"`)
		w.Write([]byte(modelsDevFixture))
	}))
	defer a.Close()
	for i := 0; i < 2; i++ {
		if _, err := CatalogSync(a.URL + "/api.json"); err != nil {
			t.Fatal(err)
		}
	}
	<-sent
	if got := <-sent; got != `"from-a"` {
		t.Fatalf("second sync of the same source sent If-None-Match %q, want its own validator", got)
	}
}
