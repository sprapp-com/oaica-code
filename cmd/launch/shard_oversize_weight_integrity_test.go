package launch

// shard_oversize_weight_integrity_test.go — `--shard X:7 --oversize X` dropped
// the weight (2026-09-26 audit). Both flags name a leg by model, and a user who
// names the SAME model in both is asking for that leg to carry hash weight AND
// to be the overflow crossover. The --shard loop ran before the oversize block,
// so the leg --oversize installs was not among the routes it stamped: it was
// appended to the fallbacks a few lines later, at whatever weight remotes.json
// gave it (0 — excluded from the ring — unless the file set one), while
// docs/CLAUDE_TIERS.md says --shard "overrides weight for a single launch
// without editing the file".
//
// applyRouteOverrides is the extracted pair, oversize first; this test drives
// it exactly as the launcher does.

import (
	"testing"
)

func fallbackOn(routes proxyRouteTable, baseURL string) (proxyRoute, bool) {
	for _, f := range routes.Fallbacks {
		if f.BaseURL == baseURL {
			return f, true
		}
	}
	return proxyRoute{}, false
}

func TestAShardWeightReachesTheOversizeLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"big","base_url":"http://big:8080/v1","api_key":"k2","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	plan, err := buildTierPlan("box/kat-awq", "", "big/kat-awq", false)
	if err != nil {
		t.Fatal(err)
	}

	// The user names the oversize model in --shard too: the leg --oversize
	// installs is a leg of this launch.
	if err := applyRouteOverrides(&plan, "big/kat-awq", map[string]int{"big/kat-awq": 7}, false); err != nil {
		t.Fatal(err)
	}

	const big = "http://big:8080/v1"
	if plan.Routes.Oversize.BaseURL != big {
		t.Fatalf("setup: --oversize resolved to %q, want %q", plan.Routes.Oversize.BaseURL, big)
	}
	if plan.Routes.Oversize.Weight != 7 {
		t.Errorf("--shard big/kat-awq:7 left the oversize leg at weight %d — the leg the user weighted is the one --oversize installed, and a weight of 0 keeps it out of the weighted ring entirely",
			plan.Routes.Oversize.Weight)
	}
	// And its copy in the fallback list — the entry the ring is actually built
	// from — carries the same weight.
	fb, ok := fallbackOn(plan.Routes, big)
	if !ok {
		t.Fatalf("the oversize leg was not appended to the fallbacks, so there is nothing for the ring to weight")
	}
	if fb.Weight != 7 {
		t.Errorf("the oversize leg's fallback entry has weight %d, want 7", fb.Weight)
	}
}

// The control: the ordinary legs --shard has always stamped still are, so this
// cannot be passed by a --shard loop that stamps nothing.
func TestAShardWeightStillReachesTheOrdinaryLegs(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"big","base_url":"http://big:8080/v1","api_key":"k2","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	plan, err := buildTierPlan("box/kat-awq", "", "big/kat-awq", false)
	if err != nil {
		t.Fatal(err)
	}
	if err := applyRouteOverrides(&plan, "", map[string]int{"big/kat-awq": 5, "box/kat-awq": 3}, false); err != nil {
		t.Fatal(err)
	}

	if plan.Routes.Default.Weight != 3 {
		t.Errorf("the base route on the sharded URL has weight %d, want 3", plan.Routes.Default.Weight)
	}
	fb, ok := fallbackOn(plan.Routes, "http://big:8080/v1")
	if !ok {
		t.Fatalf("the secondary leg is not among the fallbacks")
	}
	if fb.Weight != 5 {
		t.Errorf("the fallback on the sharded URL has weight %d, want 5", fb.Weight)
	}
}
