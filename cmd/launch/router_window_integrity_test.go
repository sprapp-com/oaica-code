package launch

// router_window_integrity_test.go — the router's /v1/models advertises each
// model's windows ("context_length", "max_completion_tokens";
// tools/gateway/main.go emits both, and its own test pins the shape). This
// client's decoder dropped them, so the layer documented as
//
//	"A genuinely live number always wins over both layers here: launch.go's
//	 setDynamicCloudModelLimits sets per-launch limits straight from the OAICA
//	 router's own /v1/models response"
//
// (cloud_limits_catalog.go) was empty by construction: setDynamicCloudModel
// Limits received a map of zero-valued rows and cloudModelLimitsFrom
// Recommendations dropped every one of them, because it requires a stated
// window. Every router model therefore ran on the embedded catalog's number or
// on a 128k default the router contradicts (2026-09-26 audit, third round).

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

// The decoder keeps what the router states.
func TestRouterModelWindowsSurviveTheDecoder(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	router := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []map[string]any{
				{"id": "kat-awq", "description": "", "stars": 5, "status": "ready", "context_length": 262144, "max_completion_tokens": 32768},
				{"id": "no-window", "description": "", "stars": 0, "status": "ready"},
			},
		})
	}))
	defer router.Close()
	t.Setenv("OAICA_HOST", router.URL)

	// The decoder directly, not through the package var: setLaunchTestHome
	// installs a nil-returning stub there (the hermetic default), and this
	// assertion is about the parse, not about who supplies the bytes.
	entries, _, err := oaicaFetchCloudModelEntriesLiveUncached(oaicaLaunchHost(), "")
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("decoded %d entries, want 2: %+v", len(entries), entries)
	}
	got := map[string]oaicaModelEntry{}
	for _, e := range entries {
		got[e.ID] = e
	}
	if e := got["kat-awq"]; e.ContextLength != 262144 || e.MaxOutputTokens != 32768 {
		t.Errorf("kat-awq = context %d / output %d, want the router's 262144 / 32768 — the window the router states is what every session is sized against", e.ContextLength, e.MaxOutputTokens)
	}
	if e := got["no-window"]; e.ContextLength != 0 || e.MaxOutputTokens != 0 {
		t.Errorf("no-window = context %d / output %d, want 0 / 0 — an unstated window stays unknown", e.ContextLength, e.MaxOutputTokens)
	}
}

// And the live number reaches the limit layer, where the documented promise is
// that it outranks the catalogs.
func TestLiveRouterWindowOutranksTheCatalog(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	setDynamicCloudModelLimits(nil)
	t.Cleanup(func() { setDynamicCloudModelLimits(nil) })

	catalogWindow, ok := cloudLimitsFromCatalog()["kimi-k2.6"]
	if !ok || catalogWindow.Context <= 0 {
		t.Fatal("setup: the embedded catalog no longer lists kimi-k2.6")
	}
	live := cloudModelLimit{Context: catalogWindow.Context + 4096, Output: catalogWindow.Output + 1024}

	stubCloudFetch(t, []oaicaModelEntry{
		{ID: "kimi-k2.6:cloud", ContextLength: live.Context, MaxOutputTokens: live.Output},
	}, nil)

	c := &launcherClient{apiClient: deadClient(t)}
	recs := c.recommendations(context.Background())
	if len(recs) != 1 {
		t.Fatalf("recommendations = %d rows, want 1", len(recs))
	}
	if recs[0].Details.ContextLength != live.Context || recs[0].MaxOutputTokens != live.Output {
		t.Errorf("picker row carries context %d / output %d, want the live %d / %d",
			recs[0].Details.ContextLength, recs[0].MaxOutputTokens, live.Context, live.Output)
	}
	if got, ok := lookupCloudModelLimit("kimi-k2.6:cloud"); !ok || got != live {
		t.Errorf("lookupCloudModelLimit = %+v (ok=%v), want the live %+v — the catalog's %+v must not outrank the router's own number",
			got, ok, live, catalogWindow)
	}
}
