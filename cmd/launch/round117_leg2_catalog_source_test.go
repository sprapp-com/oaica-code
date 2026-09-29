package launch

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// Mirror M then default-like source D serving the SAME bytes; then D offline.
func TestRound117IdenticalBytesKeepForeignSource(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var dSawINM []string
	m := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"m1"`)
		w.Write([]byte(modelsDevFixture))
	}))
	defer m.Close()
	d := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		dSawINM = append(dSawINM, r.Header.Get("If-None-Match"))
		if r.Header.Get("If-None-Match") == `"d1"` {
			w.WriteHeader(304)
			return
		}
		w.Header().Set("ETag", `"d1"`)
		w.Write([]byte(modelsDevFixture))
	}))
	dURL := d.URL + "/api.json"
	if _, err := CatalogSync(m.URL + "/api.json"); err != nil {
		t.Fatal(err)
	}
	rep, err := CatalogSync(dURL)
	t.Logf("catalog D online #1: %+v err=%v", rep, err)
	rep, err = CatalogSync(dURL)
	t.Logf("catalog D online #2: %+v err=%v  If-None-Match seen by D: %q", rep, err, dSawINM)
	d.Close()
	rep, cerr := CatalogSync(dURL)
	t.Logf("catalog D offline:   %+v err=%v", rep, cerr)

	// Provider door, same sequence.
	pm := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"m1"`)
		w.Write([]byte(probeOverlayBody))
	}))
	defer pm.Close()
	pd := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("ETag", `"d1"`)
		w.Write([]byte(probeOverlayBody))
	}))
	pdURL := pd.URL + "/overlay.json"
	if _, err := ProviderSync(pm.URL + "/overlay.json"); err != nil {
		t.Fatal(err)
	}
	if _, err := ProviderSync(pdURL); err != nil {
		t.Fatal(err)
	}
	pd.Close()
	prep, perr := ProviderSync(pdURL)
	t.Logf("provider D offline:  %+v err=%v", prep, perr)
	if perr == nil && cerr != nil {
		t.Errorf("RED: doors diverge — provider sync serves D's cache offline, catalog sync refuses the cache it just confirmed byte-identical to D")
	}
}
