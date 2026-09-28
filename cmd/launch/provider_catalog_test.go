package launch

// provider_catalog_test.go — providerCatalog's two layers: the ported
// models.dev catalog as the base, corrected per field by the oaica overlay,
// with the overlay's own rows appended.
//
// These are the tests the port exists for. Before it, a provider models.dev
// knows and providers.json did not was simply not offered, and the rows that
// were offered could be wrong in ways nothing caught: opencode-go's row was
// missing its version path (every request 404s, the picker cheerfully lists the
// models), and zai-coding-plan reused upstream's env name instead of ours.

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

// writeModelsDevCache plants a synced catalog exactly where `oaica model
// catalog sync` leaves one.
func writeModelsDevCache(t *testing.T, body string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".oaica", "cache", "catalog")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "modelsdev.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func catalogEntryByName(t *testing.T, name string) (providerCatalogEntry, bool) {
	t.Helper()
	for _, e := range providerCatalog() {
		if e.Name == name {
			return e, true
		}
	}
	return providerCatalogEntry{}, false
}

// The port's headline: a provider models.dev knows and our old file did not
// must now be offered, with upstream's endpoint and env array.
func TestProviderCatalog_UpstreamProvidersAppear(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	e, ok := catalogEntryByName(t, "groq")
	if !ok {
		t.Fatal("groq must appear from the catalog")
	}
	if e.BaseURL != "https://api.groq.com/openai/v1" {
		t.Fatalf("groq base_url = %q", e.BaseURL)
	}
	if len(e.Env) != 1 || e.Env[0] != "GROQ_API_KEY" {
		t.Fatalf("groq env = %v", e.Env)
	}
}

// Two live bugs the port fixes, asserted directly because they are the reason
// the port exists: opencode-go's row is missing its version path and is
// unselectable, and zai-coding-plan reuses upstream's env name instead of ours.
func TestProviderCatalog_OverlayCorrectsUpstream(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	overlay := map[string]providerCatalogEntry{}
	for _, p := range oaicaOverlay().Providers {
		overlay[p.Name] = p
	}
	for _, name := range []string{"opencode-go", "zai-coding-plan"} {
		corr, ok := overlay[name]
		if !ok {
			t.Fatalf("%s must have an overlay correction", name)
		}
		got, ok := catalogEntryByName(t, name)
		if !ok {
			t.Fatalf("%s missing from the merged catalog", name)
		}
		if got.BaseURL != corr.BaseURL {
			t.Fatalf("%s base_url = %q, want the overlay's %q", name, got.BaseURL, corr.BaseURL)
		}
		if got.APIKeyEnv != corr.APIKeyEnv {
			t.Fatalf("%s api_key_env = %q, want %q", name, got.APIKeyEnv, corr.APIKeyEnv)
		}
	}
}

// An overlay row for a provider models.dev does not carry must still be
// offered — that is what keeps our own endpoints working.
func TestProviderCatalog_OverlayOnlyProviderSurvives(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	for _, p := range oaicaOverlay().Providers {
		if _, ok := providerCatalogFromCatalogOnly()[p.Name]; ok {
			continue
		}
		if _, ok := catalogEntryByName(t, p.Name); !ok {
			t.Fatalf("overlay-only provider %s vanished", p.Name)
		}
		return
	}
	t.Skip("every overlay row is also in the fixture catalog")
}

func TestProviderCatalog_NoCatalogFallsBackToOverlayOnly(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	if got := providerCatalog(); len(got) == 0 {
		t.Fatal("with no catalog cached the overlay alone must still produce rows")
	}
}

func TestCloudLimits_FromOverlayNotRetiredFile(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	limits := cloudLimitsFromCatalog()
	if len(limits) == 0 {
		t.Fatal("overlay limits must reach cloudLimitsFromCatalog")
	}
	// glm-5's overlay value is the one that must win over any upstream figure.
	if l, ok := limits["glm-5"]; !ok || l.Context != 202752 {
		t.Fatalf("glm-5 = %+v (want the overlay's 202752, not upstream's 1000000)", l)
	}
}

// An overlay cache correcting one window field must not blank the other: a zero
// states nothing (the rule mergeDeclaredModelLimits pins, and the reason a
// per-id assignment was not good enough).
func TestProviderCatalog_OverlayWindowCorrectionKeepsTheUnstatedField(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	shipped, ok := catalogEntryByName(t, "zai")
	if !ok {
		t.Fatal("setup: the overlay no longer carries a zai row")
	}
	old, ok := shipped.Models["glm-5.3"]
	if !ok || old.Context == 0 || old.Output == 0 {
		t.Fatalf("setup: the overlay's zai row no longer declares glm-5.3 with both windows (%+v)", shipped.Models)
	}
	corrected := old.Context + 12345

	// The cache corrects the context and says nothing about the output.
	writeOverlayCache(t, fmt.Sprintf(
		`{"version":1,"providers":[{"name":"zai","models":{"glm-5.3":{"context":%d}}}]}`, corrected))

	got, _ := catalogEntryByName(t, "zai")
	if got.Models["glm-5.3"].Context != corrected {
		t.Errorf("glm-5.3 context = %d, want the cache's %d", got.Models["glm-5.3"].Context, corrected)
	}
	if got.Models["glm-5.3"].Output != old.Output {
		t.Errorf("glm-5.3 output = %d, want %d: a field the cache does not state keeps the value the row already carried, it is not blanked",
			got.Models["glm-5.3"].Output, old.Output)
	}
}
