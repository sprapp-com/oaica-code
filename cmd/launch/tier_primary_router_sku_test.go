package launch

// tier_primary_router_sku_test.go — a bare OAICA router SKU routes to the
// ROUTER in every tier slot, including the primary (round-5 audit, 2026-09-26).
//
// opencode zen mirrors our SKUs in its own /models, so the bare-id
// single-owner match (resolveRemoteEndpoint → resolveBareRemoteModel) found
// exactly one remote advertising "oaica-35b-a3b-vision" and sent the request
// there with that remote's credential — 401 "Model ... is not supported" while
// the OAICA key sat unused (2026-09-01, 2026-09-02 fleet incidents).
// resolveSecondaryEndpoint and ResolveAgentModelWithOpts both guard it; the
// PRIMARY slot — the one leg every launch has — did not.

import "testing"

func TestBareRouterSKUResolvesToTheRouterInThePrimarySlot(t *testing.T) {
	const sku = "oaica-35b-a3b-vision"
	setLaunchTestHome(t, t.TempDir())
	// A user remote that MIRRORS the router SKU under its bare id.
	writeRemotes(t, `{"remotes":[{"name":"opencode-go","base_url":"https://zen.audit.invalid/v1","api_key":"zen-key","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{sku: {"opencode-go/" + sku}})
	stubCloudFetch(t, []oaicaModelEntry{{ID: sku}}, nil)
	t.Setenv("OAICA_HOST", "https://router.audit.invalid")
	t.Setenv("OAICA_API_KEY", "oaica-key")

	wantBase := oaicaLaunchHost() + "/v1"

	// The contract in the two slots that already had it.
	prefixed, err := resolveLaunchEndpoint("router/" + sku)
	if err != nil {
		t.Fatalf("resolveLaunchEndpoint(router/%s): %v", sku, err)
	}
	if prefixed.Source != sourceRouter || prefixed.BaseURL != wantBase || prefixed.Token != "oaica-key" {
		t.Fatalf("premise changed: the explicit router/ spelling = %s %q %q, want %s %q %q",
			prefixed.Source, prefixed.BaseURL, prefixed.Token, sourceRouter, wantBase, "oaica-key")
	}
	sec, err := resolveSecondaryEndpoint(launchEndpoint{Source: sourceDaemon}, sku)
	if err != nil {
		t.Fatalf("resolveSecondaryEndpoint(daemon, %s): %v", sku, err)
	}
	if sec.Source != sourceRouter {
		t.Fatalf("premise changed: the sonnet slot no longer guards the bare router SKU (got %s)", sec.Source)
	}

	// The primary slot, the way buildTierPlan and `oaica launch --model <bare
	// id>` resolve it.
	prod, err := resolveLaunchEndpoint(sku)
	if err != nil {
		t.Fatalf("resolveLaunchEndpoint(%s): %v", sku, err)
	}
	if prod.Source != sourceRouter || prod.BaseURL != wantBase {
		t.Errorf("the primary slot resolved the bare router SKU %q to %s %q, want the router (%s %q): a user remote "+
			"that merely mirrors our id in its /models must not win the bare-id match — the request goes to the "+
			"wrong backend and fails there (401 \"Model %s is not supported\")",
			sku, prod.Source, prod.BaseURL, sourceRouter, wantBase, sku)
	}
	if prod.Token != "oaica-key" {
		t.Errorf("the primary leg for %q carries token %q; the router leg requires the OAICA key \"oaica-key\" — the "+
			"mirroring remote's credential must not be presented to it", sku, prod.Token)
	}

	// The counterpart: a bare NON-router id still resolves to the remote that
	// serves it (the guard is the reserved prefix alone, not "any remote").
	if _, err := RemoteAdd(RemoteAddOptions{Name: "box", BaseURL: "https://box.audit.invalid/v1", APIKey: "box-key"}); err != nil {
		t.Fatal(err)
	}
	stubBareIndex(t, map[string][]string{"box-model": {"box/box-model"}})
	ep, err := resolveLaunchEndpoint("box-model")
	if err != nil {
		t.Fatalf("resolveLaunchEndpoint(box-model): %v", err)
	}
	if ep.Source != sourceUserRemote || ep.BaseURL != "https://box.audit.invalid/v1" || ep.Token != "box-key" {
		t.Errorf("resolveLaunchEndpoint(\"box-model\") = %s %q %q; a bare id one remote serves must still resolve "+
			"there — the router-SKU guard is the reserved prefix, not every bare id",
			ep.Source, ep.BaseURL, ep.Token)
	}
}
