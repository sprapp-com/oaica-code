package launch

// tier_model_padding_integrity_test.go — a padded tier model silently rerouted
// the tier (2026-09-26 audit, tenth round).
//
// resolveLaunchEndpoint trims the model it is given, and so does
// agent_routing.go's flag extraction — but resolveSecondaryEndpoint, the
// tier slot's own resolver, did not. A padded value
// (`--sonnet-model " other/big "`, or a plan saved with one) therefore matched
// no remote namespace, no alias, no ":local" suffix and no native tier, fell
// through to the "on the primary's remote" contract, and ran the sonnet tier on
// the PRIMARY's host with the PRIMARY's credential, under a model id with
// spaces in it that no backend serves. The user asked for another remote's
// model and got their primary's backend — silently, with no error naming the
// padding.
//
// PlanSet only checked that Model was non-empty after trimming, and stored
// SonnetModel/HaikuModel verbatim, so `oaica plan set` was a second way in.

import (
	"strings"
	"testing"
)

func TestAPaddedTierModelIsTrimmedNotRerouted(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"https://box.example/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"big","base_url":"https://big.example/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	primary, err := resolveLaunchEndpoint("box/primary-model")
	if err != nil {
		t.Fatal(err)
	}

	want, err := resolveSecondaryEndpoint(primary, "big/small-model")
	if err != nil {
		t.Fatal(err)
	}
	for _, padded := range []string{" big/small-model ", "big/small-model ", " big/small-model"} {
		got, err := resolveSecondaryEndpoint(primary, padded)
		if err != nil {
			t.Errorf("resolveSecondaryEndpoint(%q) = %v, want the same endpoint as %q", padded, err, "big/small-model")
			continue
		}
		if got.RemoteEndpoint != want.RemoteEndpoint || got.Source != want.Source {
			t.Errorf("%q resolved to %+v, want %+v — a padded model id matches nothing by name, so it fell through to the primary's-remote contract and ran the tier on the PRIMARY's backend with a model id no server accepts",
				padded, got, want)
		}
		if got.Name == primary.Name {
			t.Errorf("%q ran on the primary's remote %q instead of the remote the user named", padded, primary.Name)
		}
		if strings.TrimSpace(got.UpstreamModel) != got.UpstreamModel {
			t.Errorf("%q reached the upstream as %q — the padding is still on the wire", padded, got.UpstreamModel)
		}
	}
}

// A plan is the other way a padded value arrives, and it is stored on disk.
func TestAPlanStoresTierModelsTrimmed(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	if err := PlanSet("padded", TierPlanProfile{
		Model:       " box/big ",
		SonnetModel: " box/mid ",
		HaikuModel:  " box/small ",
	}); err != nil {
		t.Fatalf("plan set: %v", err)
	}
	prof, err := PlanGet("padded")
	if err != nil {
		t.Fatal(err)
	}
	if prof.Model != "box/big" || prof.SonnetModel != "box/mid" || prof.HaikuModel != "box/small" {
		t.Errorf("the plan stored padding: model=%q sonnet=%q haiku=%q — the wizard writes these straight into launch args and tier resolution",
			prof.Model, prof.SonnetModel, prof.HaikuModel)
	}

	// And a plan written by hand (or by an older version) is normalised on the
	// way out, not just on the way in.
	_, sonnet, haiku, err := resolvePlanModels("padded", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if sonnet != "box/mid" || haiku != "box/small" {
		t.Errorf("resolvePlanModels handed back sonnet=%q haiku=%q", sonnet, haiku)
	}
}

// Control: a model id that legitimately carries no namespace still lands on the
// primary's remote with its id intact — the contract is not changed by
// trimming.
func TestAnUnNamespacedTierModelStillLandsOnThePrimary(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"https://box.example/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	primary, err := resolveLaunchEndpoint("box/primary-model")
	if err != nil {
		t.Fatal(err)
	}
	got, err := resolveSecondaryEndpoint(primary, "unlisted-id")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != primary.Name || got.UpstreamModel != "unlisted-id" {
		t.Errorf("an un-namespaced secondary resolved to %+v, want %s/unlisted-id", got, primary.Name)
	}
}
