package launch

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// claudeModelFamily decides which plan leg a Claude Code request lands on when
// the client sends a family id of its own (opusplan resolves its opus/haiku
// slots internally, so it never uses our ANTHROPIC_DEFAULT_*_MODEL values).
// Getting a tier wrong here bills the wrong leg silently — hence a table.
func TestClaudeModelFamily(t *testing.T) {
	cases := []struct {
		model string
		want  string
		ok    bool
	}{
		// Bare tiers: the aliases Claude Code resolves in its own catalog.
		{"opus", "opus", true},
		{"sonnet", "sonnet", true},
		{"haiku", "haiku", true},
		{"fable", "fable", true},
		// Un-tiered Claude names: the default leg owns them.
		{"claude", "claude", true},
		{"anthropic", "claude", true},
		{"anthropic.claude", "claude", true},
		// Current catalog ids: tier immediately after "claude-".
		{"claude-haiku-4-5-20251001", "haiku", true},
		{"claude-sonnet-4-5-20250929", "sonnet", true},
		{"claude-opus-4-1-20250805", "opus", true},
		{"claude-fable-5-1", "fable", true},
		// Legacy versioned ids: generation and date come FIRST, so a prefix
		// match misses and the tier is only findable as a segment.
		{"claude-3-5-sonnet-20241022", "sonnet", true},
		{"claude-3-5-haiku-20241022", "haiku", true},
		{"claude-3-opus-20240229", "opus", true},
		{"claude-3-sonnet", "sonnet", true},
		// Claude-shaped but tierless: default leg, never a raw forward.
		{"claude-2.1", "claude", true},
		{"claude-instant-1.2", "claude", true},
		{"claude-3-5", "claude", true},
		// Foreign ids must never be claimed: they belong to their upstream.
		{"glm-5.3", "", false},
		{"box/kat-awq", "", false},
		{"oaica-35b-a3b-vision", "", false},
		{"zai-coding-plan/glm-4.5-air", "", false},
		{"", "", false},
	}
	for _, c := range cases {
		got, ok := claudeModelFamily(c.model)
		if ok != c.ok || got != c.want {
			t.Errorf("claudeModelFamily(%q) = %q, %v; want %q, %v", c.model, got, ok, c.want, c.ok)
		}
	}
}

// The consequence of the family: with a native leg registered for that family,
// the legacy id must reach it, not the primary's model.
func TestProxyRouteTable_ResolveLegacyClaudeIDToItsFamilyLeg(t *testing.T) {
	primary := proxyRoute{Label: "primary", BaseURL: "http://primary", UpstreamModel: "glm-5.3"}
	sonnetLeg := proxyRoute{Label: "sonnet-leg", BaseURL: "http://sonnet", UpstreamModel: "claude/sonnet"}
	table := proxyRouteTable{
		Default:    primary,
		ByModel:    map[string]proxyRoute{},
		FamilyLegs: map[string]proxyRoute{"sonnet": sonnetLeg},
	}
	route, upstream := table.resolve("claude-3-5-sonnet-20241022")
	if route.Label != sonnetLeg.Label || upstream != sonnetLeg.UpstreamModel {
		t.Fatalf("legacy sonnet id resolved to %q/%q, want the sonnet leg %q", route.Label, upstream, sonnetLeg.Label)
	}
	// And with no native leg for the family, it still lands on the default leg
	// with the default's upstream model (never the raw id).
	bare := proxyRouteTable{Default: primary, ByModel: map[string]proxyRoute{}}
	route, upstream = bare.resolve("claude-3-5-sonnet-20241022")
	if route.Label != primary.Label || upstream != primary.UpstreamModel {
		t.Fatalf("unconfigured family resolved to %q/%q, want the primary %q/%q", route.Label, upstream, primary.Label, primary.UpstreamModel)
	}
	// A foreign id is passed through untouched — it must reach its upstream.
	route, upstream = table.resolve("glm-5.3")
	if route.Label != primary.Label || upstream != "glm-5.3" {
		t.Fatalf("foreign id resolved to %q/%q, want the default leg with the id unchanged", route.Label, upstream)
	}
}

// The HIGH defect of the round-1 audit: family routing restricted to NATIVE
// legs left the ordinary case broken. A haiku tier pointing at a normal
// remote (the natural cheap-model choice) registered no family, so Claude
// Code's own claude-haiku-4-5-* ids still fell to Default — the primary's
// model at the primary's price, which is the exact cost `haiku_model` exists
// to remove. The second pass of tierFamilyRoutes (positional: opus slot =
// primary, sonnet = secondary, haiku = haiku leg) is what fixes it.
func TestBuildTierPlan_NonNativeHaikuLegGetsTheHaikuFamily(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"zai","base_url":"http://zai:8080/v1","api_key":"k2","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	plan, err := buildTierPlan("box/kat-awq", "", "zai/glm-4.5-air", false)
	if err != nil {
		t.Fatal(err)
	}
	haikuLeg := routeFor(plan.Haiku)
	if haikuLeg.BaseURL != "http://zai:8080/v1" {
		t.Fatalf("plan.Haiku = %+v, want the zai leg", haikuLeg)
	}
	// Every shape Claude Code sends for background work must land there.
	for _, id := range []string{"claude-haiku-4-5-20251001", "claude-3-5-haiku-20241022", "haiku"} {
		route, model := plan.Routes.resolve(id)
		if route.Label != haikuLeg.Label || model != "glm-4.5-air" {
			t.Errorf("%q resolved to %q/%q, want the haiku leg %q/glm-4.5-air", id, route.Label, model, haikuLeg.Label)
		}
	}
	// The opus slot (the plan tier) still belongs to the primary, and its own
	// id must keep reaching the box upstream unchanged.
	route, model := plan.Routes.resolve("claude-opus-4-5")
	if route.BaseURL != "http://box:8080/v1" || model != "kat-awq" {
		t.Fatalf("opus id resolved to %+v/%q, want the primary leg", route, model)
	}
	route, model = plan.Routes.resolve("kat-awq")
	if route.BaseURL != "http://box:8080/v1" || model != "kat-awq" {
		t.Fatalf("kat-awq resolved to %+v/%q, want the box remote unchanged", route, model)
	}
}

// A native PRIMARY with a non-native haiku leg: the family map must send the
// haiku slot to the configured leg rather than to api.anthropic.com (which is
// what an empty family entry fell back to, since Default is the native
// passthrough). The native family that no leg claims (opus here) stays native.
func TestBuildTierPlan_NativePrimaryWithRemoteHaikuLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"zai","base_url":"http://zai:8080/v1","api_key":"k2","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	plan, err := buildTierPlan("claude/opus", "", "zai/glm-4.5-air", false)
	if err != nil {
		t.Fatal(err)
	}
	route, model := plan.Routes.resolve("claude-haiku-4-5-20251001")
	if route.BaseURL != "http://zai:8080/v1" || model != "glm-4.5-air" {
		t.Fatalf("haiku id resolved to %+v/%q, want the zai haiku leg", route, model)
	}
	if route.NativePassthrough {
		t.Error("a remote leg must not be a native passthrough")
	}
	// opus is the primary's own family: still the native passthrough.
	route, _ = plan.Routes.resolve("claude-opus-4-5")
	if !route.NativePassthrough {
		t.Fatalf("opus id resolved to %+v, want the native primary leg", route)
	}
}

// An empty tier is a truncated picker string, not a family: it must not claim
// the "" key, and it must not resolve to the first Anthropic catalog entry.
func TestTierFamilyRoutes_EmptyTierClaimsNothing(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	stubNativeModelCatalog(t, map[string]string{"": "claude-opus-5-20260101"})

	plan, err := buildTierPlan("box/kat-awq", "claude/", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, claimed := plan.Routes.FamilyLegs[""]; claimed {
		t.Error(`FamilyLegs has a "" entry; an empty tier must claim no family`)
	}
	if got := claudeCodeModelAlias("claude/"); got != "claude/" {
		t.Fatalf("claudeCodeModelAlias(%q) = %q, want it unchanged (resolving an empty tier matches the whole catalog)", "claude/", got)
	}
}

// A remote that OWNS an "anthropic/<slug>" id keeps it: that spelling is also
// how an aggregator's own Claude slug looks, and un-namespaced it means "on
// the primary's remote". Only an id no remote owns reaches the native claim —
// the alternative there was handing the remote the literal string.
func TestResolveSecondaryEndpoint_RemoteOwnedSlugBeatsNativeClaim(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"openrouter","base_url":"https://openrouter.ai/api/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	primary, err := resolveLaunchEndpoint("openrouter/anthropic/claude-opus-4.5")
	if err != nil {
		t.Fatal(err)
	}
	ep, err := resolveSecondaryEndpoint(primary, "anthropic/claude-sonnet-4.5")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Source != sourceUserRemote || ep.Name != "openrouter" || ep.UpstreamModel != "anthropic/claude-sonnet-4.5" {
		t.Fatalf("anthropic/<slug> resolved to %+v, want it left on the openrouter remote", ep)
	}
	// And a native tier nothing owns still goes native.
	ep, err = resolveSecondaryEndpoint(primary, "claude/sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Source != sourceNativeAnthropic {
		t.Fatalf("claude/sonnet resolved to %+v, want the native source", ep)
	}
}

// The native-alias lookup builds its own /v1/models GET, so it has no client
// request to inherit headers from — and an OAuth bearer from `claude /login`
// is rejected without the anthropic-beta header Claude Code itself sends. The
// failure is silent (resolution falls back to the bare tier, which Claude Code
// then rejects at startup), so pin the header here.
func TestResolveNativeModelAlias_SendsOAuthBetaHeader(t *testing.T) {
	var gotBeta, gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotBeta, gotAuth = r.Header.Get("anthropic-beta"), r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"claude-sonnet-5","display_name":"Claude Sonnet 5"}]}`))
	}))
	defer srv.Close()

	oldUpstream := nativeAnthropicModelsUpstream
	nativeAnthropicModelsUpstream = srv.URL
	defer func() { nativeAnthropicModelsUpstream = oldUpstream }()
	oldToken := readClaudeOAuthAccessTokenFn
	readClaudeOAuthAccessTokenFn = func() (string, error) { return "oauth-token-abc", nil }
	defer func() { readClaudeOAuthAccessTokenFn = oldToken }()

	// OAuth-only box: no ANTHROPIC_API_KEY, a token in the credentials file.
	t.Setenv("ANTHROPIC_API_KEY", "")
	if got := resolveNativeModelAliasUncached("sonnet"); got != "claude-sonnet-5" {
		t.Fatalf("resolveNativeModelAliasUncached(sonnet) = %q, want the catalog id", got)
	}
	if gotAuth != "Bearer oauth-token-abc" {
		t.Fatalf("Authorization = %q, want the OAuth bearer", gotAuth)
	}
	if gotBeta != oauthBetaHeaderValue {
		t.Fatalf("anthropic-beta = %q, want %q (an OAuth bearer needs it; without it the lookup 401s and the alias silently stays a bare tier)", gotBeta, oauthBetaHeaderValue)
	}

	// A plain API key is not an OAuth bearer: the beta header must not be sent
	// on its behalf.
	gotBeta = ""
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-test")
	if got := resolveNativeModelAliasUncached("sonnet"); got != "claude-sonnet-5" {
		t.Fatalf("API-key lookup = %q, want the catalog id", got)
	}
	if gotBeta != "" {
		t.Fatalf("anthropic-beta = %q on an x-api-key request, want it unset", gotBeta)
	}
}
