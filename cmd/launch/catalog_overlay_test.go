package launch

// catalog_overlay_test.go — the overlay's two-copy, per-key merge.
//
// The property under test is not "the cache wins" but "the cache can only ever
// override what it NAMES". The layer this replaces (provider_catalog.go)
// overrode wholesale, so a field added to a newer binary was invisible on any
// host whose cache predated it — documented there as a 2026-09-25 silent
// failure. A per-key merge cannot do that, and these tests are what keep it
// that way as fields get added.

import (
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func writeOverlayCache(t *testing.T, body string) {
	t.Helper()
	home, _ := os.UserHomeDir()
	dir := filepath.Join(home, ".oaica", "cache", "providers")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "oaica.json"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

// The hazard this pins: provider_catalog.go's header records a 2026-09-25
// incident where a cache row synced before a new field existed made the field
// invisible, with no error. A per-key merge cannot do that.
func TestOverlay_MergeIsPerKeyNotWholesale(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	// The cache names one provider and corrects its base_url only. Every other
	// embedded provider must survive, and the corrected provider must keep the
	// embedded fields the cache did not mention.
	embedded := oaicaOverlay()
	var probe, other string
	for _, p := range embedded.Providers {
		if probe == "" {
			probe = p.Name
		} else if p.Name != probe {
			other = p.Name
		}
	}
	if other == "" {
		t.Skip("embedded overlay has fewer than two providers; nothing to pin yet")
	}
	writeOverlayCache(t, `{"version":1,"providers":[{"name":"`+probe+`","base_url":"https://corrected.example/v1"}]}`)

	got := oaicaOverlay()
	byName := map[string]providerCatalogEntry{}
	for _, p := range got.Providers {
		byName[p.Name] = p
	}
	if byName[probe].BaseURL != "https://corrected.example/v1" {
		t.Fatalf("%s base_url = %q, want the cache's correction", probe, byName[probe].BaseURL)
	}
	if _, ok := byName[other]; !ok {
		t.Fatalf("%s vanished: the cache replaced the overlay wholesale", other)
	}
}

func TestOverlay_CacheCannotEraseAnEmbeddedOnlyField(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	embedded := oaicaOverlay()
	if len(embedded.Providers) == 0 {
		t.Skip("overlay has no providers yet")
	}
	name := embedded.Providers[0].Name
	writeOverlayCache(t, `{"version":1,"providers":[{"name":"`+name+`","base_url":"https://corrected.example/v1"}]}`)
	for _, p := range oaicaOverlay().Providers {
		if p.Name == name && p.BaseURL == "https://corrected.example/v1" {
			return
		}
	}
	t.Fatalf("%s missing from the merged overlay", name)
}

// The property is not "something comes back" — it is "an unparseable cache
// contributes NOTHING", which is what makes a corrupt sync a no-op instead of a
// wipe. Asserting non-emptiness would pass vacuously while the embedded file is
// still being filled in, and would stop meaning anything the moment it wasn't.
func TestOverlay_UnparseableCacheFallsBackToEmbedded(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeOverlayCache(t, "{ this is not json")
	got := oaicaOverlay()
	want := parseOverlayBytes(oaicaOverlayEmbedded)
	if !reflect.DeepEqual(got.Providers, want.Providers) ||
		!reflect.DeepEqual(got.Models, want.Models) ||
		!reflect.DeepEqual(got.Limits, want.Limits) {
		t.Fatalf("an unparseable cache changed the overlay:\ngot  %+v\nwant the embedded copy %+v", got, want)
	}
	if got.Version != want.Version {
		t.Errorf("an unparseable cache changed version %d -> %d", want.Version, got.Version)
	}
}

func TestOverlay_LimitsMergeByModelID(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeOverlayCache(t, `{"version":1,"limits":{"glm-5":{"context":202752,"output":32768}}}`)
	got := overlayLimits()
	if got["glm-5"].Context != 202752 {
		t.Fatalf("glm-5 = %+v", got["glm-5"])
	}
}

// TestOverlay_MergeCoversEveryTextFieldOfAProviderEntry is the completeness pin
// the per-key merge needs and cannot get from an end-to-end test until the
// embedded overlay carries rows: every string field of providerCatalogEntry must
// be consulted by mergeProviderEntry. A field it forgets is a field a synced
// cache can never correct, and the failure is silent — the embedded value just
// keeps winning. Add the field to the merge in the same change that adds it to
// the struct, or this test says so by name.
func TestOverlay_MergeCoversEveryTextFieldOfAProviderEntry(t *testing.T) {
	typ := reflect.TypeOf(providerCatalogEntry{})
	override := providerCatalogEntry{}
	ov := reflect.ValueOf(&override).Elem()
	checked := 0
	for i := 0; i < typ.NumField(); i++ {
		if typ.Field(i).Type.Kind() != reflect.String {
			continue
		}
		ov.Field(i).SetString("set-by-the-cache")
		checked++
	}
	if checked < 5 {
		t.Fatalf("only %d string fields enumerated: reflection over providerCatalogEntry stopped working", checked)
	}

	got := reflect.ValueOf(mergeProviderEntry(providerCatalogEntry{}, override))
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if f.Type.Kind() != reflect.String {
			continue
		}
		// Name IS the merge key: mergeProviderEntry is only ever reached for an
		// entry whose name already matched, so both sides are equal by
		// construction and there is nothing for the override to correct.
		if f.Name == "Name" {
			continue
		}
		if got.Field(i).String() == "" {
			t.Errorf("providerCatalogEntry.%s is not merged by mergeProviderEntry: a synced cache that corrects it would be ignored with no error, because the embedded value keeps winning (2026-09-28, the per-key merge's whole point)", f.Name)
		}
	}
}

// TestOverlay_DeclaredModelsMergeByIdNotWholesale is the map-field half of the
// same property: a cache naming one declared model must not drop the others.
func TestOverlay_DeclaredModelsMergeByIdNotWholesale(t *testing.T) {
	base := providerCatalogEntry{Models: map[string]providerCatalogModelLimit{
		"glm-5":     {Context: 202752, Output: 32768},
		"glm-5-air": {Context: 131072, Output: 16384},
	}}
	got := mergeProviderEntry(base, providerCatalogEntry{
		Models: map[string]providerCatalogModelLimit{"glm-5": {Context: 999}},
	})
	if got.Models["glm-5"].Context != 999 {
		t.Errorf("the cache's correction to a declared model did not win: %+v", got.Models["glm-5"])
	}
	if got.Models["glm-5-air"].Context != 131072 {
		t.Errorf("a declared model the cache never named vanished: %+v", got.Models)
	}
	// And a cache that declares no models at all leaves the embedded ones alone.
	got = mergeProviderEntry(base, providerCatalogEntry{BaseURL: "https://x.example"})
	if len(got.Models) != 2 {
		t.Errorf("a cache that names no models dropped the embedded ones: %+v", got.Models)
	}
}

// TestOverlay_NamelessEntriesAreIgnored keeps the merge from being a way to
// inject an anonymous provider: an entry with no name has nothing to merge
// against, and appending it would put a row with no way to route to it in the
// catalog.
func TestOverlay_NamelessEntriesAreIgnored(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	before := len(oaicaOverlay().Providers)
	writeOverlayCache(t, `{"version":1,"providers":[{"base_url":"https://nameless.example"},{"name":"","base_url":"https://also-nameless.example"}]}`)
	after := oaicaOverlay().Providers
	if len(after) != before {
		t.Fatalf("nameless provider entries were appended: %d before, %d after (%+v)", before, len(after), after)
	}
}
