package launch

// catalog_rows_test.go — a remote's picker rows when the sweep cannot answer.

import (
	"errors"
	"strings"
	"testing"
)

// The catalog is the list; the overlay's declared numbers correct it per field
// (that is what providers/oaica.json exists for — see provider_catalog_test.go).
func TestCatalogRowsFor_CarriesMarksAndOverlayWindows(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)

	byID := map[string]catalogModelRow{}
	for _, r := range catalogRowsFor("zai-coding-plan") {
		byID[r.ID] = r
	}

	dep, ok := byID["glm-4.6"]
	if !ok {
		t.Fatalf("the catalog's own row must appear, got %v", byID)
	}
	if !dep.Deprecated {
		t.Error("status deprecated must mark the row")
	}
	if dep.ToolCall {
		t.Error("an explicit tool_call:false must reach the row")
	}
	if !dep.HasContext || dep.Context != 200000 {
		t.Errorf("glm-4.6 window = %d (has=%v), want the catalog's 200000", dep.Context, dep.HasContext)
	}

	vis, ok := byID["glm-4.7"]
	if !ok {
		t.Fatal("glm-4.7 missing")
	}
	if !vis.Vision || !vis.ToolCall {
		t.Errorf("attachment + image input modality must mark vision, and an absent tool_call means yes: %+v", vis)
	}
	// The overlay states this window and the fixture's glm-4.7 states none, so
	// the overlay's number is the only one there is.
	if !vis.HasContext || vis.Context != 204800 {
		t.Errorf("glm-4.7 window = %d (has=%v), want the overlay's 204800", vis.Context, vis.HasContext)
	}

	// An overlay-only model (the subscription endpoints serve no /v1/models at
	// all) must still be listed: offline it is the only list there is.
	if only, ok := byID["glm-5.3"]; !ok || !only.HasContext || only.Context != 1000000 {
		t.Errorf("a model only the overlay declares must be listed with its window, got %+v", only)
	}
}

// Review focus 5: a model with no cost and no limit must render as free with
// an unknown window, never as a zero price and never panicking.
func TestCatalogRowDescription_MissingCostAndLimitDegrade(t *testing.T) {
	desc := catalogRowDescription(catalogModelRow{ID: "m", Name: "M"})
	if strings.Contains(desc, "$0.00") || strings.Contains(desc, "0/M") {
		t.Fatalf("absent cost must not print as a real price: %q", desc)
	}
	if !strings.Contains(desc, "ctx ?") {
		t.Fatalf("absent limit must read as unknown: %q", desc)
	}
	if !strings.Contains(desc, "free") {
		t.Fatalf("absent cost should read as free: %q", desc)
	}
}

func TestCatalogRowDescription_ShowsTierAndOver200kBands(t *testing.T) {
	desc := catalogRowDescription(catalogModelRow{
		ID: "glm-4.6", Name: "GLM-4.6", Context: 200000, HasContext: true,
		Cost: modelsDevCost{
			Input: 0.6, Output: 2.2,
			Tiers:           []modelsDevCostTier{{Tier: modelsDevTierBand{Type: "context", Size: 32000}, Input: 0.9, Output: 3}},
			ContextOver200k: &modelsDevCostTier{Input: 1.2, Output: 4.4},
		},
	})
	if !strings.Contains(desc, "0.90") || !strings.Contains(desc, "1.20") {
		t.Fatalf("banded pricing must be shown, not just the base rate: %q", desc)
	}
}

// A remote with no credential mechanism at all is genuinely unauthenticated:
// sweeping it burns a timeout for an answer that cannot be trusted anyway.
func TestRemoteIsSweepable(t *testing.T) {
	if remoteIsSweepable(userRemote{Name: "keyless", BaseURL: "http://x/v1"}) {
		t.Fatal("a remote with no key mechanism must not be swept")
	}
	if !remoteIsSweepable(userRemote{Name: "keyed", BaseURL: "http://x/v1", APIKeyEnv: "X_API_KEY"}) {
		t.Fatal("a remote with an env key mechanism must be swept")
	}
	if !remoteIsSweepable(userRemote{Name: "inline", BaseURL: "http://x/v1", APIKey: "k"}) {
		t.Fatal("a remote with an inline key must be swept")
	}
}

// remoteNeedsSweep is the composition the picker actually asks: the credential
// rule above says a keyless endpoint is not worth sweeping, but that reasoning
// is about a VENDOR (which answers 401, or a list nothing can use). A user's
// own box needs no key — a llama.cpp on the LAN is legitimately open — and it
// has no catalog row to fall back on, so skipping it would empty its section.
func TestRemoteNeedsSweep(t *testing.T) {
	box := userRemote{Name: "box", BaseURL: "http://10.0.0.5:8080/v1"}
	if !remoteNeedsSweep(box) {
		t.Fatal("a keyless user box must still be swept: nothing else can list its models")
	}
	vendor := userRemote{Name: "zai-coding-plan", BaseURL: "https://api.z.ai/api/anthropic", CatalogOrigin: true}
	if remoteNeedsSweep(vendor) {
		t.Fatal("a keyless vendor row must not be swept: the catalog already lists it")
	}
	keyed := vendor
	keyed.APIKeyEnv = "Z_AI_API_KEY"
	if !remoteNeedsSweep(keyed) {
		t.Fatal("a keyed vendor row must be swept: /v1/models is the authority when it answers")
	}
}

// The inversion: an unreachable remote still lists the catalog's models, each
// marked unverified, instead of an empty section.
func TestRemoteLaunchModels_UnreachableListsTheCatalogUnverified(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)

	r := userRemote{Name: "groq", BaseURL: "http://127.0.0.1:1/v1", APIKey: "k", CatalogOrigin: true}
	rows, err := remoteLaunchModels(r, nil, nil) // the sweep answered nothing
	if err != nil {
		t.Fatalf("an unreachable remote with a catalog row must not be an error: %v", err)
	}
	if len(rows) == 0 {
		t.Fatal("an unreachable remote must still list the catalog's models")
	}
	for _, row := range rows {
		if !strings.HasPrefix(row.Name, "groq/") {
			t.Fatalf("row %q is not namespaced to its remote", row.Name)
		}
		if !row.Unverified {
			t.Fatalf("%s must be marked unverified: the remote never confirmed it", row.Name)
		}
		if row.ContextLength <= 0 {
			t.Errorf("%s lost the catalog's window", row.Name)
		}
	}
}

// The control: when the sweep answers, nothing listed is marked unverified —
// the endpoint confirmed it — and the swept ids keep the order the endpoint
// gave them. The declared extras a swept catalog row has always carried stay
// (that union is existing behaviour, and a vendor that hides models from its
// own list still serves them); what changes is only the mark.
func TestRemoteLaunchModels_ReachableMarksNothingUnverified(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)

	r := userRemote{Name: "zai-coding-plan", BaseURL: "https://api.z.ai/api/anthropic", APIKey: "k", CatalogOrigin: true}
	rows, err := remoteLaunchModels(r, []string{"a-model-the-catalog-never-heard-of", "glm-4.7"}, nil)
	if err != nil {
		t.Fatalf("remoteLaunchModels: %v", err)
	}
	seen := map[string]LaunchModel{}
	for _, row := range rows {
		seen[row.Name] = row
	}
	if rows[0].Name != "zai-coding-plan/a-model-the-catalog-never-heard-of" || rows[1].Name != "zai-coding-plan/glm-4.7" {
		t.Fatalf("swept ids lost their order: %v, %v", rows[0].Name, rows[1].Name)
	}
	for name, row := range seen {
		if row.Unverified {
			t.Errorf("%s was confirmed by the sweep and must not be marked unverified", name)
		}
	}
	// The sweep lists ids and no windows; the catalog supplies the window.
	if got := seen["zai-coding-plan/glm-4.7"].ContextLength; got != 204800 {
		t.Errorf("swept row window = %d, want the catalog's 204800", got)
	}
}

// The badge is how the mark reaches the user: an unverified row is badged in
// the picker rather than silently looking like a confirmed one.
func TestUnverifiedRowsAreBadgedInThePicker(t *testing.T) {
	item := modelItemFromInventory("groq/llama-x", LaunchModel{Name: "groq/llama-x", Remote: true, Unverified: true}, ModelItem{})
	if got := availabilityBadge(item, AccountState{}); got != "unverified" {
		t.Fatalf("badge = %q, want unverified", got)
	}
	confirmed := modelItemFromInventory("groq/llama-x", LaunchModel{Name: "groq/llama-x", Remote: true}, ModelItem{})
	if got := availabilityBadge(confirmed, AccountState{}); got != "" {
		t.Fatalf("a confirmed row must carry no badge, got %q", got)
	}
}

// A keyless row that is NOT a vendor — a user's own box — keeps today's
// behaviour: the sweep runs, and a failed one is still an error (the caller
// reports it), because nothing else can list that box's models.
func TestRemoteLaunchModels_KeylessUserBoxStillErrors(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)

	r := userRemote{Name: "groq", BaseURL: "http://10.0.0.5:8080/v1"} // same NAME, no CatalogOrigin
	if _, err := remoteLaunchModels(r, nil, errors.New("dial tcp: refused")); err == nil {
		t.Fatal("a user's own box that cannot be read is an error, not a catalog list")
	}
}
