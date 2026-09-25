package launch

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
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

// A leg named for ANOTHER family may only claim it on the SONNET slot — the one
// slot whose env value the launcher sets (opusplan resolves its opus and haiku
// slots from Claude Code's own catalog), so "claude/opus" there is a statement
// about the sonnet tier. On any other slot the same name moves traffic the user
// never aimed at that leg: a claude/opus HAIKU leg would take the opus family
// (under opusplan, the main plan-mode conversation) off the configured primary
// and onto the Anthropic login.
func TestTierFamilyRoutes_OnlyTheSonnetSlotClaimsAForeignFamily(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	stubNativeModelCatalog(t, map[string]string{"opus": "claude-opus-4-5-20251101"})

	plan, err := buildTierPlan("box/kat-awq", "", "claude/opus", false)
	if err != nil {
		t.Fatal(err)
	}
	route, model := plan.Routes.resolve("claude-opus-4-5-20251101")
	if route.BaseURL != "http://box:8080/v1" || model != "kat-awq" {
		t.Fatalf("opus id resolved to %+v/%q, want the primary remote (a haiku leg's name must not claim the opus family)", route, model)
	}
	// The haiku tier the user named still serves the haiku family, via the
	// positional pass — only the FOREIGN claim is restricted.
	route, _ = plan.Routes.resolve("claude-haiku-4-5-20251001")
	if !route.NativePassthrough {
		t.Fatalf("haiku id resolved to %+v, want the native haiku leg the plan configured", route)
	}

	// And the documented case is untouched: a SECONDARY named for another
	// family claims it, which is the only way the sonnet tier can be pointed
	// at a family other than its own slot's.
	plan, err = buildTierPlan("box/kat-awq", "claude/opus", "", false)
	if err != nil {
		t.Fatal(err)
	}
	route, _ = plan.Routes.resolve("claude-opus-4-5-20251101")
	if !route.NativePassthrough {
		t.Fatalf("opus id resolved to %+v, want the native secondary leg (--sonnet-model claude/opus)", route)
	}
}

// Two shapes the slot rule must not break (round-4 audit):
//
//   - a NATIVE primary whose tier name is the slot the haiku leg belongs to
//     ("--model claude/haiku --haiku-model <remote>"): without a distinct
//     sonnet leg the secondary is a COPY of the primary, so a slot-1 exemption
//     honoured the primary's haiku claim through that copy and the configured
//     haiku leg served nothing;
//   - a leg named for a family no slot owns ("--haiku-model claude/fable"):
//     pass 2 can never place "fable", so claiming it steals nothing — refusing
//     the claim sent the fable id to the primary, which cannot serve it.
func TestTierFamilyRoutes_SlotRuleKeepsTheConfiguredLegReachable(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"zai","base_url":"http://zai:8080/v1","api_key":"k2","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	stubNativeModelCatalog(t, map[string]string{"haiku": "claude-haiku-4-5-20251001", "fable": "claude-fable-5-1"})

	plan, err := buildTierPlan("claude/haiku", "", "zai/glm-4.5-air", false)
	if err != nil {
		t.Fatal(err)
	}
	route, model := plan.Routes.resolve("claude-haiku-4-5-20251001")
	if route.BaseURL != "http://zai:8080/v1" || model != "glm-4.5-air" {
		t.Fatalf("haiku id resolved to %+v/%q, want the configured zai haiku leg (a native primary's own tier name must not claim the family through its sonnet-slot copy)", route, model)
	}

	plan, err = buildTierPlan("box/kat-awq", "", "claude/fable", false)
	if err != nil {
		t.Fatal(err)
	}
	route, _ = plan.Routes.resolve("claude-fable-5-1")
	if !route.NativePassthrough {
		t.Fatalf("fable id resolved to %+v, want the native fable leg that was named for it (no slot owns the fable family, so nothing is stolen)", route)
	}
}

// The untouched native path is for launches with nothing for the proxy to do.
// A --oversize/--route-policy/--shard given on the command line is consumed by
// the launcher and re-applied on the PLAN path, so runNative (execs Claude Code
// with the passthrough args only) would drop it silently — a flag that cannot
// work must not evaporate (round-4 audit).
func TestNativeTierOnly_ExplicitFlagsKeepThePlanPath(t *testing.T) {
	// --route-policy and --shard are deliberately NOT inputs: with no second
	// leg they have nothing to act on (the policy only governs failover
	// between legs, and a --shard id that matches no existing leg is already
	// a silent no-op), so they must not block the native fast path. They are
	// validated before this check instead — a typo'd policy is reported on
	// every launch, native or not.
	cases := []struct {
		name                           string
		model, sonnet, haiku, oversize string
		wantTier                       string
		wantOK                         bool
	}{
		{"plain native launch", "claude/opus", "", "", "", "opus", true},
		{"native with a sonnet split", "claude/opus", "zai/glm-4.5-air", "", "", "", false},
		{"native with a haiku split", "claude/opus", "", "zai/glm-4.5-air", "", "", false},
		{"native with --oversize", "claude/opus", "", "", "zai/glm-4.6", "", false},
		{"empty tier is not native", "claude/", "", "", "", "", false},
		{"non-native primary", "box/kat-awq", "", "", "", "", false},
	}
	for _, c := range cases {
		tier, ok := nativeTierOnly(c.model, c.sonnet, c.haiku, c.oversize)
		if ok != c.wantOK || tier != c.wantTier {
			t.Errorf("%s: nativeTierOnly = %q/%v, want %q/%v", c.name, tier, ok, c.wantTier, c.wantOK)
		}
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

// withWizardEligibleLaunch arms the gate Claude.Run consults. Production sets
// it in LaunchIntegration (launch.go), so a test that calls Run directly must
// arm it itself — otherwise the wizard is skipped for a reason unrelated to
// what is under test.
func withWizardEligibleLaunch(t *testing.T) {
	t.Helper()
	old := tierWizardEligibleLaunch
	tierWizardEligibleLaunch = true
	t.Cleanup(func() { tierWizardEligibleLaunch = old })
}

// The wizard must never run for a launch that already names its tiers on the
// command line: eligibility has to be decided BEFORE the extractors strip
// --sonnet-model & co from args, because a bare `oaica launch claude
// --sonnet-model x` has no --model flag and is interactive, so asking
// afterwards always answered "eligible" — and the wizard's Enter-to-keep-it
// answers then replaced the typed tier with "unset", running the launch
// single-model at the primary's price with the flag silently gone.
func TestRun_TypedTierFlagSuppressesTheWizard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	setLaunchTestHome(t, t.TempDir())
	withInteractiveSession(t, true)
	withWizardEligibleLaunch(t)
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"zai","base_url":"http://zai:8080/v1","api_key":"k2","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	binDir := t.TempDir()
	envLog := filepath.Join(t.TempDir(), "env.txt")
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nenv > "+envLog+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	// Keep the real PATH behind the fake bin dir: the stub script shells
	// out to `env`.
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	old := tierWizardSelect
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		t.Fatalf("wizard ran for a launch that already named its tier (%s)", title)
		return "", nil
	}
	t.Cleanup(func() { tierWizardSelect = old })

	if err := (&Claude{}).Run("box/kat-awq", nil, []string{"--sonnet-model", "zai/glm-4.5-air"}); err != nil {
		t.Fatalf("Run: %v", err)
	}

	data, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	env := string(data)
	if !strings.Contains(env, "ANTHROPIC_DEFAULT_SONNET_MODEL=zai/glm-4.5-air\n") {
		t.Errorf("the typed sonnet tier did not reach the child: no matching ANTHROPIC_DEFAULT_SONNET_MODEL in its environment")
	}
	// The flag must also not have been forwarded to Claude Code verbatim —
	// the launcher consumed it.
	if strings.Contains(env, "--sonnet-model") {
		t.Errorf("--sonnet-model leaked into the child environment")
	}
}

// The saved config sits ABOVE the wizard in the documented ladder, so a wizard
// answer must not replace a tier `oaica config set` already supplies. It used
// to: the wizard assigned first and the config only filled what was still
// empty, so answering the sonnet step silently discarded the saved model.
func TestRun_SavedConfigTierBeatsTheWizard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	home := t.TempDir()
	setLaunchTestHome(t, home)
	withInteractiveSession(t, true)
	withWizardEligibleLaunch(t)
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"},
		{"name":"zai","base_url":"http://zai:8080/v1","api_key":"k2","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	if err := os.MkdirAll(filepath.Join(home, ".oaica"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".oaica", "config.json"),
		[]byte(`{"sonnet_model":"zai/glm-4.5-air"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	binDir := t.TempDir()
	envLog := filepath.Join(t.TempDir(), "env.txt")
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nenv > "+envLog+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		// The wizard still runs (it also covers oversize and the policy), but
		// its answer must not be what the launch ends up using. A REAL pick,
		// not the empty "same as primary": the empty answer would let the
		// config rung fill the tier afterwards and the test would pass even
		// with the wizard winning the rung.
		switch {
		case strings.Contains(title, "Sonnet"):
			return "box/glm-4.6", nil
		case strings.Contains(title, "Route policy"):
			return "auto", nil
		}
		return "", nil // keep the step's default (haiku, oversize)
	}
	// Blank at the "Save as plan" prompt: don't write a plan, and don't block
	// on stdin (the test's EOF was reaching it as an error).
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })

	if err := (&Claude{}).Run("box/kat-awq", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ANTHROPIC_DEFAULT_SONNET_MODEL=zai/glm-4.5-air\n") {
		t.Error("the saved sonnet tier was replaced by the wizard's answer")
	}
}
