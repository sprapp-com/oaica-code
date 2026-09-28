package launch

// catalog_offline_test.go — the last of the models.dev plan's tasks: what a
// launch looks like when the catalog has never been synced, and where a
// first-party model's endpoint comes from.

import (
	"strings"
	"testing"
)

// The plan's headline for this task: OAICA_GATEWAY_URL points oaica's own
// models at one gateway. It is checked without asking the router anything,
// which is the point — the gateway is what you point at when the router is not
// the thing you are running.
func TestResolveLaunchEndpoint_GatewayURLOverride(t *testing.T) {
	noRemotes(t)
	stubDaemon(t)
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	t.Setenv("OAICA_GATEWAY_URL", "http://gateway.local:8081/")

	ep, err := resolveLaunchEndpoint("oaica-default")
	if err != nil {
		t.Fatalf("the override must resolve without the router: %v", err)
	}
	if ep.BaseURL != "http://gateway.local:8081/v1" {
		t.Fatalf("base = %q, want the gateway's /v1", ep.BaseURL)
	}
	if ep.Source != sourceRouter || ep.UpstreamModel != "oaica-default" {
		t.Fatalf("endpoint = %+v, want the router source with the bare id upstream", ep)
	}
}

// The token is the gateway's own, and it is a real credential: it must be
// scrubbed from the child environment like the router's key.
func TestResolveLaunchEndpoint_GatewayTokenIsScrubbed(t *testing.T) {
	noRemotes(t)
	stubDaemon(t)
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	t.Setenv("OAICA_GATEWAY_URL", "http://gateway.local:8081")
	t.Setenv(oaicaGatewayTokenEnv, "gw-secret")

	ep, err := resolveLaunchEndpoint("router/oaica-default")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if ep.Token != "gw-secret" || ep.TokenEnv != oaicaGatewayTokenEnv {
		t.Fatalf("token = %q env = %q, want the gateway credential named for the scrubber", ep.Token, ep.TokenEnv)
	}
	if ep.UpstreamModel != "oaica-default" {
		t.Fatalf("upstream = %q, want the prefix stripped", ep.UpstreamModel)
	}
}

// The chain it must NOT disturb: an alias still beats everything, a user remote
// still beats it for that remote's ids, and a model that is not ours is not
// redirected at all.
func TestResolveLaunchEndpoint_OverrideDoesNotReorderTheChain(t *testing.T) {
	remoteBox(t, map[string][]string{"kat-awq": {"box/kat-awq"}})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t, "gemma4")
	t.Setenv("OAICA_GATEWAY_URL", "http://gateway.local:8081")

	if r, err := resolveLaunchEndpoint("box/kat-awq"); err != nil || r.BaseURL != "http://box:8080/v1" {
		t.Fatalf("a user remote must still win for its own models, got %+v (%v)", r, err)
	}
	if r, err := resolveLaunchEndpoint("gemma4"); err != nil || r.Source != sourceDaemon {
		t.Fatalf("a local model must still come from the daemon, got %+v (%v)", r, err)
	}
}

// Offline: no catalog cached, nothing errors, and the notice names the fix.
//
// The gateway override is set because that is what makes a first-party row
// launchable; see the companion below for the other half of the rule.
func TestModelList_NoCatalogStillListsOverlayAndLocal(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	t.Setenv("OAICA_GATEWAY_URL", "http://gateway.local:8081")

	// The local row is what the inventory holds (a pulled model), the
	// first-party rows are what the binary's own overlay holds: neither needs
	// the catalog, and neither needs the network.
	items, _, _, _ := buildModelListWithRecommendations([]modelInfo{{Name: "local-model"}}, nil, nil, "")
	var sawFirstParty, sawLocal bool
	for _, it := range items {
		if isFirstPartyOverlayModel(it.Name) {
			sawFirstParty = true
			if it.AvailabilityBadge != "unavailable" {
				t.Errorf("%s is listed with badge %q, want unavailable: nothing resolved it", it.Name, it.AvailabilityBadge)
			}
		}
		if it.Name == "local-model" {
			sawLocal = true
		}
	}
	if !sawFirstParty || !sawLocal {
		t.Fatalf("offline list must carry first-party and local rows: %+v", items)
	}
	if notice := catalogOfflineNotice(); notice == "" || !strings.Contains(notice, "oaica model catalog sync") {
		t.Fatalf("offline notice must name the sync command, got %q", notice)
	}
}

// The other half of the rule: with nothing pointed at oaica's own models, they
// are NOT padded into the picker. A row that no configured endpoint could serve
// is a row that cannot launch, and the fork already refuses to list that kind of
// row (upstream Ollama's built-in catalog). The router's own answer is the other
// way these models reach the picker — when it answered, a first-party id it did
// not carry is one it does not serve.
func TestModelList_FirstPartyRowsAreNotPaddedWithNoEndpointForThem(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	t.Setenv("OAICA_GATEWAY_URL", "")

	items, _, _, _ := buildModelListWithRecommendations([]modelInfo{{Name: "local-model"}}, nil, nil, "")
	for _, it := range items {
		if isFirstPartyOverlayModel(it.Name) {
			t.Fatalf("first-party row %q is listed with no endpoint to launch it against: %+v", it.Name, items)
		}
	}
	if len(items) != 1 || items[0].Name != "local-model" {
		t.Fatalf("the inventory is the whole list here, got %+v", items)
	}
}

// A first-party model the router DID list is available, and must not be
// duplicated or badged by the overlay's own row for it.
func TestModelList_FirstPartyRowFromTheRouterIsNotBadged(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	stubDaemon(t)

	items, _, _, _ := buildModelListWithRecommendations(nil, []ModelItem{
		{Name: "oaica-default", Recommended: true, Description: "from the router"},
	}, nil, "")
	count := 0
	for _, it := range items {
		if it.Name != "oaica-default" {
			continue
		}
		count++
		if it.AvailabilityBadge != "" {
			t.Errorf("a router-listed model must not be marked unavailable, got %q", it.AvailabilityBadge)
		}
	}
	if count != 1 {
		t.Fatalf("oaica-default appears %d times, want once", count)
	}
}

func TestCatalogAgeNotice_OnlyPastThirtyDays(t *testing.T) {
	if got := catalogAgeNotice(29); got != "" {
		t.Fatalf("29 days must stay quiet, got %q", got)
	}
	if got := catalogAgeNotice(31); !strings.Contains(got, "days old") {
		t.Fatalf("31 days must be visible, got %q", got)
	}
}

// With nothing cached the status command says so and names the fix, rather
// than printing an empty line.
func TestCatalogStatus_NoCacheNamesTheSyncCommand(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	_, line := CatalogStatus()
	if !strings.Contains(line, "oaica model catalog sync") {
		t.Fatalf("status with no cache = %q, want the sync command named", line)
	}
}

func TestCatalogStatus_ReportsWhatIsCached(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeModelsDevCache(t, modelsDevFixture)
	rep, line := CatalogStatus()
	if rep.Providers != 3 || rep.Models != 4 {
		t.Fatalf("status = %+v, want the fixture's 3 providers and 4 models", rep)
	}
	if !strings.Contains(line, "3 providers") || !strings.Contains(line, "sha256") {
		t.Fatalf("status line = %q, want counts and the hash", line)
	}
}
