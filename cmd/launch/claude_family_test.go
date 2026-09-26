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
	resetNativeModelCatalog()
	if got := resolveNativeModelAlias("sonnet"); got != "claude-sonnet-5" {
		t.Fatalf("resolveNativeModelAlias(sonnet) = %q, want the catalog id", got)
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
	resetNativeModelCatalog()
	if got := resolveNativeModelAlias("sonnet"); got != "claude-sonnet-5" {
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

// A saved config tier (oaica config set sonnet-model X) must LEAD its wizard
// step, so that Enter — the way a user keeps a standing preference — keeps X.
// It did not: the step led with "(same as primary)", Enter therefore meant "no
// split", and answering the step discarded the saved model.
func TestRun_WizardStepLeadsWithTheSavedConfigTier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	home, envLog := setupWizardConfigTierRun(t, `{"sonnet_model":"zai/glm-4.5-air"}`)

	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	var sonnetLead string
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if strings.Contains(title, "Sonnet") {
			sonnetLead = items[0].Name
			return items[0].Name, nil // the Enter key
		}
		return "", nil
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })

	if err := (&Claude{}).Run("box/kat-awq", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	if sonnetLead != "zai/glm-4.5-air" {
		t.Errorf("the saved sonnet tier does not lead its step (got %q) — Enter would drop the standing split", sonnetLead)
	}
	data, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ANTHROPIC_DEFAULT_SONNET_MODEL=zai/glm-4.5-air\n") {
		t.Error("Enter on the saved tier's own row did not keep it")
	}
	_ = home
}

// ...and an explicit pick is a choice made on THIS command line, exactly like
// a flag, so it beats the standing config for that launch. The saved value is
// the step's default, not a ceiling on its answers.
func TestRun_WizardPickBeatsTheSavedConfigTier(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	_, envLog := setupWizardConfigTierRun(t, `{"sonnet_model":"zai/glm-4.5-air"}`)

	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if strings.Contains(title, "Sonnet") {
			return "box/glm-4.6", nil
		}
		return "", nil
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })

	if err := (&Claude{}).Run("box/kat-awq", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "ANTHROPIC_DEFAULT_SONNET_MODEL=box/glm-4.6\n") {
		t.Error("the wizard's explicit pick did not beat the standing config tier")
	}
}

// "(same as primary)" is the ONLY way to take a standing config split back off,
// and it has to survive the plan-save prompt at the end of the wizard. It did
// not: the caller keyed "the wizard handed back a plan name" on PlanName != "",
// which is true for a plan the user just SAVED as well as one reused — so a
// save took the reuse branch, skipped the clear, and ~/.oaica/config.json's
// sonnet_model came straight back into the child's environment, right after the
// wizard's own preview said the split was gone (2026-09-26 audit).
func TestRun_WizardClearSurvivesThePlanSave(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	_, envLog := setupWizardConfigTierRun(t, `{"sonnet_model":"zai/glm-4.5-air"}`)

	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if strings.Contains(title, "Sonnet") {
			return "(same as primary)", nil // the explicit clear
		}
		return "", nil
	}
	// A name at the save prompt is what made the bug reachable: it puts the
	// wizard on the save path with a non-empty PlanName.
	tierWizardReadLine = func(prompt string) (string, error) { return "clearing-split", nil }
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })

	if err := (&Claude{}).Run("box/kat-awq", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ANTHROPIC_DEFAULT_SONNET_MODEL=zai/glm-4.5-air") {
		t.Errorf("saving the plan resurrected the standing sonnet tier the wizard had cleared:\n%s", data)
	}
}

// setupWizardConfigTierRun arms an interactive, wizard-eligible launch whose
// only tier is a standing config sonnet_model, with a fake `claude` that dumps
// its environment. Returns the temp home and the env dump's path.
func setupWizardConfigTierRun(t *testing.T, configJSON string) (home, envLog string) {
	t.Helper()
	home = t.TempDir()
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
	if err := os.WriteFile(filepath.Join(home, ".oaica", "config.json"), []byte(configJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := t.TempDir()
	envLog = filepath.Join(t.TempDir(), "env.txt")
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nenv > "+envLog+"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return home, envLog
}

// A policy or oversize leg TYPED on the command line must survive `--wizard`,
// exactly like every other flag: the wizard fills what the caller left empty.
// It overwrote both unconditionally until 2026-09-26, so `--wizard
// --route-policy remote-first` silently ran the wizard's `auto` (and `--shard`,
// which only means anything under `weighted`, went inert with it).
func TestRun_TypedPolicyAndOversizeSurviveTheWizard(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	_, _ = setupWizardConfigTierRun(t, `{}`)

	origProbe := remoteContextWindowFn
	t.Cleanup(func() { remoteContextWindowFn = origProbe })
	remoteContextWindowFn = func(r proxyRoute) int {
		switch strings.TrimRight(r.BaseURL, "/") {
		case "http://box:8080/v1":
			return 32768
		case "http://zai:8080/v1":
			return 262144
		}
		return 0
	}

	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })
	// Only the Enter key in this test: the typed policy and oversize leg must
	// LEAD their steps (else the wizard's closing preview names a leg the
	// launch will not use) and Enter must therefore return them back.
	var oversizeLead, policyLead string
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if strings.Contains(title, "oversize") || strings.Contains(title, "Compaction") {
			oversizeLead = items[0].Name
		}
		if strings.Contains(title, "policy") {
			policyLead = items[0].Name
		}
		if len(items) > 0 {
			return items[0].Name, nil // the Enter key
		}
		return "", nil
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	models := []LaunchModel{
		{Name: "box/kat-awq", Remote: true},
		{Name: "zai/glm-4.6", Remote: true},
		{Name: "box/glm-4.6", Remote: true},
	}
	stderr := captureStderr(t, func() {
		if err := (&Claude{}).Run("box/kat-awq", models, []string{"--wizard", "--route-policy", "remote-first", "--oversize", "zai/glm-4.6"}); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if !strings.Contains(stderr, "route policy: remote-first") {
		t.Errorf("the typed --route-policy did not survive the wizard (stderr: %s)", stderr)
	}
	if policyLead != "remote-first" {
		t.Errorf("the typed policy does not lead its step (got %q) — Enter would leave the preview claiming another", policyLead)
	}
	if oversizeLead != "zai/glm-4.6" {
		t.Errorf("the typed oversize leg does not lead its step (got %q)", oversizeLead)
	}
	if !strings.Contains(stderr, "oversize: requests past the serving leg's window -> remote:zai (glm-4.6, 256k window)") {
		t.Errorf("the typed --oversize leg is not the one in effect (stderr: %s)", stderr)
	}
	if strings.Contains(stderr, "box/glm-4.6") {
		t.Errorf("the wizard's own oversize answer replaced the typed --oversize leg (stderr: %s)", stderr)
	}
}

// `--wizard --plan x` ran nothing at all: the gate skips the wizard whenever a
// plan is present, so the flag was silently dropped, while
// docs/CLAUDE_TIERS.md promised --wizard forced the steps past that gate.
// Running the wizard is not the fix: its first step offers the last-used plan
// (and Enter there would REPLACE the typed one), and its tier steps lead with
// the standing ~/.oaica/config.json tiers rather than the plan's, so its
// Enter-key defaults would drop the plan's tiers. The combination is refused by
// name instead, and the doc says so (2026-09-26 audit).
func TestRun_WizardWithPlanIsRefusedNotIgnored(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	setLaunchTestHome(t, t.TempDir())
	withInteractiveSession(t, true)
	withWizardEligibleLaunch(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	if err := PlanSet("p", TierPlanProfile{Model: "box/kat-awq"}); err != nil {
		t.Fatal(err)
	}

	old := tierWizardSelect
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		t.Fatalf("the wizard ran for a launch that also named --plan (%s)", title)
		return "", nil
	}
	t.Cleanup(func() { tierWizardSelect = old })

	err := (&Claude{}).Run("box/kat-awq", nil, []string{"--wizard", "--plan", "p"})
	if err == nil || !strings.Contains(err.Error(), "--wizard") || !strings.Contains(err.Error(), "plan") {
		t.Fatalf("err = %v, want a refusal naming the conflict — a flag that silently does nothing is what the audit found", err)
	}
}

// A remote's own remotes.json route_policy governs the launch, and the wizard
// must not silently outrank it. The policy step pre-selects "auto" and any
// Enter answers it — and the wizard's answer beats everything (the caller
// assigns policyArg from it), so a primary on a remote declaring local-only
// ran `auto` instead, which escalates to remote legs on accumulated failures
// (route_policy.go's autoEscalateAfterFails) — exactly what local-only
// forbids. The step now leads with the policy actually in effect for this
// launch (the typed flag, else the primary's own remotes.json value), so
// Enter keeps that rather than answering the wizard's default over it
// (2026-09-26 audit).
func TestRun_WizardPolicyStepKeepsTheRemotesRoutePolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	setLaunchTestHome(t, t.TempDir())
	withInteractiveSession(t, true)
	withWizardEligibleLaunch(t)
	writeRemotes(t, `{"remotes":[
		{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls","route_policy":"local-only"},
		{"name":"zai","base_url":"http://zai:8080/v1","api_key":"k2","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })
	var policyLead string
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		switch {
		case strings.Contains(title, "policy"):
			if len(items) > 0 {
				policyLead = items[0].Name
				return items[0].Name, nil // the Enter key
			}
			return "", nil
		case strings.Contains(title, "Sonnet"):
			// A cross-source secondary, so the launch has fallback legs and
			// prints the policy it is actually using.
			return "zai/glm-4.6", nil
		}
		if len(items) > 0 {
			return items[0].Name, nil
		}
		return "", nil
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	models := []LaunchModel{{Name: "box/kat-awq", Remote: true}, {Name: "zai/glm-4.6", Remote: true}}
	stderr := captureStderr(t, func() {
		if err := (&Claude{}).Run("box/kat-awq", models, []string{"--wizard"}); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if policyLead != "local-only" {
		t.Errorf("the primary remote's route_policy does not lead the policy step (got %q) — Enter answers the wizard's auto over it", policyLead)
	}
	if !strings.Contains(stderr, "route policy: local-only") {
		t.Errorf("the launch did not run the remote's route_policy (stderr: %s)", stderr)
	}
}

// A native primary with a native --oversize leg is the documented setup (swap
// to your own Anthropic login's real window when the primary's overflow needs
// it). Its banner must say that, and must NOT talk about a size comparison it
// never made: the primary's window is never probed (native bypasses probing),
// so the crossover cannot be described in the same terms as a remote leg's.
func TestRun_NativeOversizeBannerSaysNoComparisonWasMade(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	home := t.TempDir()
	setLaunchTestHome(t, home)
	withInteractiveSession(t, false)
	writeRemotes(t, `{"remotes":[]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	stubNativeModelCatalog(t, map[string]string{"fable": "claude-fable-5-1", "opus": "claude-opus-4-5-20251101"})
	binDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(binDir, "claude"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

	stderr := captureStderr(t, func() {
		if err := (&Claude{}).Run("claude/fable", nil, []string{"--oversize", "claude/opus"}); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if !strings.Contains(stderr, "oversize: requests past the serving leg's window -> claude/") {
		t.Errorf("no native-oversize banner naming the leg:\n%s", stderr)
	}
	if !strings.Contains(stderr, "your own Anthropic login") {
		t.Errorf("the banner does not say whose credential serves it:\n%s", stderr)
	}
	if strings.Contains(stderr, "not larger than the primary's") {
		t.Errorf("the banner claims a size comparison against a window native never probes:\n%s", stderr)
	}
}

// Reusing a plan from the wizard's first step must resolve it exactly like a
// typed --plan — including its route_policy. The wizard used to pre-set its own
// "auto" on the reuse path, and because "auto" is not empty it outranked both
// the plan's stored policy and the primary remote's route_policy: a plan
// deliberately set to local-only (never leave local legs) silently became
// auto, i.e. allowed crossover to a remote on failure.
func TestRun_WizardPlanReuseKeepsThePlansRoutePolicy(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	home, _ := setupWizardConfigTierRun(t, `{}`)
	if err := PlanSet("dev", TierPlanProfile{
		Model: "box/kat-awq", SonnetModel: "zai/glm-4.6", RoutePolicy: string(RouteLocalOnly),
	}); err != nil {
		t.Fatal(err)
	}
	// PlanSet records the repo it was saved from; launch from the same
	// directory so "the last plan used here" is the one on offer.
	if wd, err := os.Getwd(); err == nil {
		_ = wd
	}
	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		return items[0].Name, nil // the Enter key everywhere
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	var runErr error
	stderr := captureStderr(t, func() {
		runErr = (&Claude{}).Run("box/kat-awq", nil, nil)
	})
	if runErr != nil {
		t.Fatalf("Run: %v", runErr)
	}
	if !strings.Contains(stderr, "route policy: local-only") {
		t.Errorf("the reused plan's route_policy did not survive the wizard (want local-only):\n%s", stderr)
	}
	if strings.Contains(stderr, "route policy: auto") {
		t.Errorf("the wizard's own default outranked the plan:\n%s", stderr)
	}
	_ = home
}

// "(same as primary)" is the wizard's only way to take a standing split back
// off, and it has to outrank ~/.oaica/config.json — otherwise the config rung
// refilled the tier the user just removed, after the wizard's own preview had
// said it was gone.
func TestRun_WizardSameAsPrimaryBeatsTheStandingConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	_, envLog := setupWizardConfigTierRun(t, `{"sonnet_model":"zai/glm-4.5-air","haiku_model":"zai/glm-4.5-air"}`)

	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if strings.Contains(title, "Sonnet") || strings.Contains(title, "Haiku") {
			return "(same as primary)", nil
		}
		if len(items) > 0 {
			return items[0].Name, nil
		}
		return "", nil
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	if err := (&Claude{}).Run("box/kat-awq", nil, nil); err != nil {
		t.Fatalf("Run: %v", err)
	}
	data, err := os.ReadFile(envLog)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "ANTHROPIC_DEFAULT_SONNET_MODEL=zai/glm-4.5-air") {
		t.Error("the standing sonnet tier came back after the user cleared it (the config rung refills an empty value)")
	}
	if strings.Contains(string(data), "ANTHROPIC_DEFAULT_HAIKU_MODEL=zai/glm-4.5-air") {
		t.Error("the standing haiku tier came back after the user cleared it")
	}
	if !strings.Contains(string(data), "ANTHROPIC_DEFAULT_SONNET_MODEL=box/kat-awq") {
		t.Error("the cleared sonnet tier should fall back to the primary")
	}
}

// An ANSWERED wizard step is a choice made in the wizard the user explicitly
// asked for (--wizard), so it applies even over a typed --oversize /
// --route-policy — which supplies that step's default row instead. Applying
// only the answered steps is also what makes the closing preview truthful: the
// launch now runs what the preview showed, either way.
func TestRun_WizardAnsweredStepBeatsTheTypedFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	setupWizardConfigTierRun(t, `{}`)
	origProbe := remoteContextWindowFn
	t.Cleanup(func() { remoteContextWindowFn = origProbe })
	remoteContextWindowFn = func(r proxyRoute) int {
		switch strings.TrimRight(r.BaseURL, "/") {
		case "http://box:8080/v1":
			return 32768
		case "http://zai:8080/v1":
			return 262144
		}
		return 0
	}
	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if strings.Contains(title, "oversize") || strings.Contains(title, "Compaction") {
			return tierWizardNoOversize, nil // decline the leg this launch
		}
		if strings.Contains(title, "policy") {
			return string(RouteLocalOnly), nil // ...and the typed policy too
		}
		if len(items) > 0 {
			return items[0].Name, nil
		}
		return "", nil
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	models := []LaunchModel{{Name: "box/kat-awq", Remote: true}, {Name: "zai/glm-4.6", Remote: true}}
	stderr := captureStderr(t, func() {
		if err := (&Claude{}).Run("box/kat-awq", models, []string{"--wizard", "--route-policy", "remote-first", "--oversize", "zai/glm-4.6"}); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if !strings.Contains(stderr, "policy: local-only") {
		t.Errorf("the wizard's answered policy was discarded in favour of the flag:\n%s", stderr)
	}
	if strings.Contains(stderr, "oversize: requests past") {
		t.Errorf("the declined oversize leg is still in effect:\n%s", stderr)
	}
}

// An ABANDONED wizard (esc on the first tier step) must not wipe the typed
// flags it never asked about.
func TestRun_AbandonedWizardLeavesTypedFlagsAlone(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	setupWizardConfigTierRun(t, `{}`)
	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		return tierWizardBack, nil // esc, immediately
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	// A second leg on another remote is what makes the effective policy
	// observable (the banner prints only when there is somewhere to fall).
	var runErr error
	stderr := captureStderr(t, func() {
		runErr = (&Claude{}).Run("box/kat-awq", nil, []string{"--wizard", "--route-policy", "remote-first", "--sonnet-model", "zai/glm-4.6"})
	})
	t.Logf("runErr=%v stderr=%q", runErr, stderr)
	if !strings.Contains(stderr, "route policy: remote-first") {
		t.Errorf("an abandoned wizard wiped the typed --route-policy:\n%s", stderr)
	}
}

// The other half of the answered/abandoned distinction: when the user DOES
// answer the Sonnet step with a model, that choice is this launch's tier even
// though a --sonnet-model supplied the step's default row. The flag is the
// default, not a ceiling -- otherwise the wizard would be a dialog that cannot
// change the thing it asks about.
func TestRun_WizardAnsweredSonnetBeatsTheTypedFlag(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	_, envLog := setupWizardConfigTierRun(t, `{}`)

	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if strings.Contains(title, "Sonnet") {
			// Take a row that is NOT the typed default.
			for _, it := range items {
				if it.Name == "zai/glm-4.6" {
					return it.Name, nil
				}
			}
			t.Errorf("the wizard's Sonnet step must offer zai/glm-4.6: %+v", items)
			return "", nil
		}
		if len(items) > 0 {
			return items[0].Name, nil
		}
		return "", nil
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	models := []LaunchModel{{Name: "box/kat-awq", Remote: true}, {Name: "zai/glm-4.6", Remote: true}}
	stderr := captureStderr(t, func() {
		if err := (&Claude{}).Run("box/kat-awq", models, []string{"--wizard", "--sonnet-model", "zai/glm-4.5-air"}); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	data, _ := os.ReadFile(envLog)
	if !strings.Contains(string(data), "ANTHROPIC_DEFAULT_SONNET_MODEL=zai/glm-4.6") {
		t.Errorf("the wizard's answer did not become the tier (env):\n%s", data)
	}
	if !strings.Contains(stderr, "zai/glm-4.6") {
		t.Errorf("want the wizard summary to name the picked tier:\n%s", stderr)
	}
}

// Esc at the first (Sonnet) step must re-ask the plan step when plans exist,
// not end the wizard with the tiers silently discarded. The step order makes
// this easy to get wrong: the plan prompt is offered BEFORE the tier steps, so
// "back" from the first tier step has somewhere to go.
func TestRun_WizardEscAtSonnetReasksThePlanStep(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("fake claude binary is a /bin/sh script")
	}
	setupWizardConfigTierRun(t, `{}`)
	if err := PlanSet("saved-here", TierPlanProfile{Model: "box/kat-awq", SonnetModel: "box/kat-awq", RoutePolicy: string(RouteLocalOnly)}); err != nil {
		t.Fatal(err)
	}

	var planStepAsks int
	oldSelect, oldRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = oldSelect, oldRead })
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if strings.Contains(title, "plan") {
			planStepAsks++
			return tierWizardScratch, nil // scratch: walk the tiers
		}
		if planStepAsks == 0 {
			// The very first prompt was not the plan step: the wizard then has
			// no step to back into, which is what this pins.
			t.Errorf("first prompt was %q, want the saved-plan step", title)
			return tierWizardBack, nil
		}
		if planStepAsks == 1 {
			return tierWizardBack, nil // esc on Sonnet -> back to the plan step
		}
		if len(items) > 0 {
			return items[0].Name, nil
		}
		return "", nil
	}
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	stderr := captureStderr(t, func() {
		if err := (&Claude{}).Run("box/kat-awq", nil, nil); err != nil {
			t.Errorf("Run: %v", err)
		}
	})
	if planStepAsks < 2 {
		t.Errorf("esc at the Sonnet step did not re-ask the plan step (asked %d times):\n%s", planStepAsks, stderr)
	}
}
