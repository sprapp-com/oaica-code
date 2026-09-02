package launch

import (
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestTierWizardEligible(t *testing.T) {
	origSession := isInteractiveSession
	t.Cleanup(func() { isInteractiveSession = origSession })
	isInteractiveSession = func() bool { return true }

	cases := []struct {
		name     string
		eligible bool
		args     []string
	}{
		{"plain interactive launch", true, nil},
		{"extra claude args", true, []string{"--", "--verbose"}},
		{"sonnet flag suppresses", false, []string{"--sonnet-model", "x"}},
		{"oversize flag suppresses", false, []string{"--oversize=x"}},
		{"policy flag suppresses", false, []string{"--route-policy", "auto"}},
		{"plan flag suppresses", false, []string{"--plan", "daily"}},
	}
	for _, c := range cases {
		tierWizardEligibleLaunch = c.eligible
		if got := tierWizardEligible(c.args); got != c.eligible {
			t.Errorf("%s: tierWizardEligible = %v, want %v", c.name, got, c.eligible)
		}
	}
	tierWizardEligibleLaunch = false

	// Non-interactive session: never eligible, even with everything else true.
	tierWizardEligibleLaunch = true
	isInteractiveSession = func() bool { return false }
	if tierWizardEligible(nil) {
		t.Error("wizard ran for a non-interactive session")
	}
}

func TestOversizeWindowCandidates(t *testing.T) {
	endpoints := map[string]launchEndpoint{
		"primary":  {RemoteEndpoint: RemoteEndpoint{BaseURL: "http://p/v1", UpstreamModel: "primary"}},
		"big":      {RemoteEndpoint: RemoteEndpoint{BaseURL: "http://b/v1", UpstreamModel: "big"}},
		"smaller":  {RemoteEndpoint: RemoteEndpoint{BaseURL: "http://s/v1", UpstreamModel: "smaller"}},
		"same leg": {RemoteEndpoint: RemoteEndpoint{BaseURL: "http://p/v1", UpstreamModel: "same leg"}},
	}
	resolve := func(m string) (launchEndpoint, error) {
		if ep, ok := endpoints[m]; ok {
			return ep, nil
		}
		return launchEndpoint{}, errors.New("not found")
	}
	windows := map[string]int{"primary": 262144, "big": 524288, "smaller": 262144, "same leg": 2000000}
	probe := func(r proxyRoute) int { return windows[r.UpstreamModel] }

	// "smaller" carries an EQUAL window on a different backend — still a
	// valid compaction leg (>= qualifier): the leg must also survive the
	// primary failing near the ceiling, which an equal-window leg from
	// another failure domain does.
	cands, pw := oversizeWindowCandidates([]string{"big", "smaller", "same leg", "missing"}, "primary", resolve, probe)
	if pw != 262144 {
		t.Fatalf("primary window = %d, want 262144", pw)
	}
	if len(cands) != 2 || cands[0] != "big" || cands[1] != "smaller" {
		t.Fatalf("candidates = %v, want [big smaller] (equal-window different-URL leg included)", cands)
	}

	// Unknown primary window: no honest oversize pick exists.
	windows["primary"] = 0
	if cands, _ := oversizeWindowCandidates([]string{"big"}, "primary", resolve, probe); len(cands) != 0 {
		t.Fatalf("candidates with unknown primary window = %v, want none", cands)
	}
}

func TestOversizeWindowCandidates_LiveProbeStub(t *testing.T) {
	// Same filter through the production defaults path: remoteContextWindowFn
	// is the swap point the proxy itself uses.
	orig := remoteContextWindowFn
	t.Cleanup(func() { remoteContextWindowFn = orig })
	remoteContextWindowFn = func(r proxyRoute) int {
		if r.UpstreamModel == "primary" {
			return 262144
		}
		return 524288
	}
	fakeResolve := func(m string) (launchEndpoint, error) {
		return launchEndpoint{RemoteEndpoint: RemoteEndpoint{
			BaseURL: "http://" + m + ".stub/v1", UpstreamModel: m,
		}}, nil
	}
	cands, _ := oversizeWindowCandidates([]string{"primary", "bigger"}, "primary", fakeResolve, nil)
	if len(cands) != 1 || cands[0] != "bigger" {
		t.Fatalf("candidates = %v, want [bigger]", cands)
	}
}

// withStubbedWizard replaces the interactive hooks with scripted answers and
// restores them on cleanup.
func withStubbedWizardUI(t *testing.T, selectLog *[]string, picks []string, saveName string) {
	origSelect, origRead := tierWizardSelect, tierWizardReadLine
	origResolve, origProbe := tierWizardResolveEndpoint, tierWizardProbeWindow
	t.Cleanup(func() {
		tierWizardSelect, tierWizardReadLine = origSelect, origRead
		tierWizardResolveEndpoint, tierWizardProbeWindow = origResolve, origProbe
	})
	i := 0
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		*selectLog = append(*selectLog, title)
		if i >= len(picks) {
			return items[0].Name, nil
		}
		p := picks[i]
		i++
		return p, nil
	}
	tierWizardReadLine = func(prompt string) (string, error) { return saveName, nil }
	tierWizardResolveEndpoint = func(m string) (launchEndpoint, error) {
		return launchEndpoint{RemoteEndpoint: RemoteEndpoint{
			BaseURL: "http://" + strings.ReplaceAll(m, "/", "-") + ".stub/v1", UpstreamModel: m,
		}}, nil
	}
	tierWizardProbeWindow = func(r proxyRoute) int {
		if r.UpstreamModel == "kat-awq" {
			return 262144
		}
		return 524288
	}
}

func TestRunTierWizard_SavesPlan(t *testing.T) {
	withTempOaicaHome(t)
	var selectLog []string
	withStubbedWizardUI(t, &selectLog, []string{"kat-awq-7b", "kat-awq-1.5b", "big-box/glm-9", "remote-first"}, "daily-driver")

	plan, err := runTierWizard(testLaunchModels("kat-awq", "kat-awq-7b", "kat-awq-1.5b", "big-box/glm-9"), "kat-awq")
	if err != nil {
		t.Fatalf("runTierWizard: %v", err)
	}
	if plan.SonnetModel != "kat-awq-7b" || plan.HaikuModel != "kat-awq-1.5b" || plan.OversizeModel != "big-box/glm-9" || plan.RoutePolicy != "remote-first" {
		t.Fatalf("unexpected choice: %+v", plan)
	}
	if plan.PlanName != "daily-driver" {
		t.Fatalf("plan name = %q", plan.PlanName)
	}
	if len(selectLog) != 4 {
		t.Fatalf("steps run = %d (%v), want 4 (sonnet, haiku, oversize, policy)", len(selectLog), selectLog)
	}
	prof, err := PlanGet("daily-driver")
	if err != nil {
		t.Fatal(err)
	}
	if prof.Model != "kat-awq" || prof.SonnetModel != "kat-awq-7b" || prof.HaikuModel != "kat-awq-1.5b" ||
		prof.OversizeModel != "big-box/glm-9" || prof.RoutePolicy != "remote-first" {
		t.Fatalf("stored plan: %+v", prof)
	}
}

func TestRunTierWizard_DefaultsLeaveEverythingUnset(t *testing.T) {
	withTempOaicaHome(t)
	origSelect, origRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = origSelect, origRead })
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) { return items[0].Name, nil }
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	c, err := runTierWizard(nil, "solo-model")
	if err != nil {
		t.Fatalf("runTierWizard: %v", err)
	}
	if c.SonnetModel != "" || c.OversizeModel != "" || c.RoutePolicy != string(RouteAuto) || c.PlanName != "" {
		t.Fatalf("unexpected choice: %+v", c)
	}
}

func TestTierWizardPreview(t *testing.T) {
	got := tierWizardPreview("a", tierWizardChoice{SonnetModel: "b", OversizeModel: "c", RoutePolicy: "remote-first"})
	if !strings.Contains(got, "fallback: a <-> b") || !strings.Contains(got, "oversize: c") || !strings.Contains(got, "policy: remote-first") {
		t.Fatalf("preview = %q", got)
	}
	// Window shown in k once probed; unprobed (0) omits the parenthetical.
	if strings.Contains(got, "(0k)") {
		t.Fatalf("preview advertises an unknown window: %q", got)
	}
	sameLeg := tierWizardPreview("a", tierWizardChoice{RoutePolicy: RouteLocalFirst.String()})
	if strings.Contains(sameLeg, "<->") || strings.Contains(sameLeg, "oversize") {
		t.Fatalf("single-leg preview should name only the primary: %q", sameLeg)
	}
}

func TestTierPlanProfile_OldJSONRoundTrip(t *testing.T) {
	withTempOaicaHome(t)
	// A plans.json written before oversize_model/route_policy existed: must
	// load unchanged (missing = empty = today's defaults), and re-save must
	// not corrupt it.
	old := `{"version":1,"profiles":{"legacy":{"model":"kat-awq","sonnet_model":"kat-awq-7b"}}}`
	path, err := tierPlanProfilesPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(old), 0o600); err != nil {
		t.Fatal(err)
	}
	prof, err := PlanGet("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if prof.Model != "kat-awq" || prof.SonnetModel != "kat-awq-7b" {
		t.Fatalf("unexpected profile: %+v", prof)
	}
	if prof.OversizeModel != "" || prof.RoutePolicy != "" {
		t.Fatalf("new fields should default to empty, got %+v", prof)
	}
	if err := PlanSet("legacy", prof); err != nil {
		t.Fatal(err)
	}
	reloaded, err := PlanGet("legacy")
	if err != nil {
		t.Fatal(err)
	}
	if reloaded != prof {
		t.Fatalf("round-trip changed the profile: %+v vs %+v", reloaded, prof)
	}
}

func TestTierPlanProfile_RoundTripWithNewFields(t *testing.T) {
	withTempOaicaHome(t)
	in := TierPlanProfile{
		Model: "kat-awq", SonnetModel: "kat-awq-7b", OversizeModel: "big-box/glm-9",
		RoutePolicy: "remote-first", Description: "d",
	}
	if err := PlanSet("full", in); err != nil {
		t.Fatal(err)
	}
	out, err := PlanGet("full")
	if err != nil {
		t.Fatal(err)
	}
	if out != in {
		t.Fatalf("round-trip: %+v vs %+v", out, in)
	}
	data, err := os.ReadFile(mustPlanPath(t))
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"oversize_model", "route_policy"} {
		if !strings.Contains(string(data), field) {
			t.Fatalf("plans.json missing %s: %s", field, data)
		}
	}
	var decoded TierPlanProfile
	var wrapper struct {
		Profiles map[string]TierPlanProfile `json:"profiles"`
	}
	if err := json.Unmarshal(data, &wrapper); err != nil {
		t.Fatal(err)
	}
	decoded = wrapper.Profiles["full"]
	if decoded.OversizeModel != "big-box/glm-9" || decoded.RoutePolicy != "remote-first" {
		t.Fatalf("decoded: %+v", decoded)
	}
}

func TestPlanSet_RejectsBadRoutePolicy(t *testing.T) {
	withTempOaicaHome(t)
	if err := PlanSet("x", TierPlanProfile{Model: "m", RoutePolicy: "sometimes"}); err == nil {
		t.Fatal("expected error for invalid route_policy")
	}
}

func TestResolvePlanTier(t *testing.T) {
	withTempOaicaHome(t)
	if err := PlanSet("oaica-full", TierPlanProfile{
		Model: "kat-awq", SonnetModel: "kat-awq-7b",
		OversizeModel: "big-box/glm-9", RoutePolicy: "remote-first",
	}); err != nil {
		t.Fatal(err)
	}

	// Plan fills what the flags left empty.
	p, o, err := resolvePlanTier("oaica-full", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if p != "remote-first" || o != "big-box/glm-9" {
		t.Fatalf("plan tier resolution = (%q, %q)", p, o)
	}

	// Flags win over the stored plan.
	p, o, err = resolvePlanTier("oaica-full", "local-only", "explicit-oversize")
	if err != nil {
		t.Fatal(err)
	}
	if p != "local-only" || o != "explicit-oversize" {
		t.Fatalf("flag > plan precedence broke: (%q, %q)", p, o)
	}

	// No --plan: passthrough unchanged.
	p, o, err = resolvePlanTier("", "auto", "")
	if err != nil {
		t.Fatal(err)
	}
	if p != "auto" || o != "" {
		t.Fatalf("no-plan passthrough = (%q, %q)", p, o)
	}

	// Unknown plan: error.
	// Unknown plan: error.
	if _, _, err := resolvePlanTier("does-not-exist", "", ""); err == nil {
		t.Fatal("expected error for unknown plan")
	}
}

func TestTierWizardEligibleCleanup(t *testing.T) {
	t.Cleanup(func() { tierWizardEligibleLaunch = false })
}

func mustPlanPath(t *testing.T) string {
	t.Helper()
	path, err := tierPlanProfilesPath()
	if err != nil {
		t.Fatal(err)
	}
	return path
}

func TestExtractWizardFlag(t *testing.T) {
	forced, rest := extractWizardFlag([]string{"--wizard", "--model", "m"})
	if !forced {
		t.Fatal("expected --wizard to force")
	}
	for _, a := range rest {
		if a == "--wizard" {
			t.Fatalf("--wizard not stripped: %v", rest)
		}
	}
	forced, rest = extractWizardFlag([]string{"--wizard=false", "x"})
	if forced {
		t.Fatal("--wizard=false must not force")
	}
	if len(rest) != 1 || rest[0] != "x" {
		t.Fatalf("unexpected rest %v", rest)
	}
	forced, _ = extractWizardFlag([]string{"--model", "m"})
	if forced {
		t.Fatal("no --wizard given")
	}
}

func TestRunTierWizard_BackNavigationAndLastPlan(t *testing.T) {
	withTempOaicaHome(t)
	origSelect, origRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = origSelect, origRead })

	// 1. Back on the very first step abandons the wizard with defaults.
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		return tierWizardBack, nil
	}
	c, err := runTierWizard(testLaunchModels("kat-awq", "kat-awq-7b"), "kat-awq")
	if err != nil {
		t.Fatalf("runTierWizard: %v", err)
	}
	if c.SonnetModel != "" || c.OversizeModel != "" || c.RoutePolicy != string(RouteAuto) {
		t.Fatalf("back on first step should leave defaults: %+v", c)
	}

	// 2. Back mid-flow: enter step2, esc on step3 -> step2 re-asked, then
	// finish through the policy step.
	origResolve, origProbe := tierWizardResolveEndpoint, tierWizardProbeWindow
	t.Cleanup(func() { tierWizardResolveEndpoint, tierWizardProbeWindow = origResolve, origProbe })
	tierWizardResolveEndpoint = func(m string) (launchEndpoint, error) {
		return launchEndpoint{RemoteEndpoint: RemoteEndpoint{
			BaseURL: "http://" + strings.ReplaceAll(m, "/", "-") + ".stub/v1", UpstreamModel: m,
		}}, nil
	}
	tierWizardProbeWindow = func(r proxyRoute) int { return 262144 }
	var logged []string
	answers := []string{"kat-awq-7b", tierWizardBack, "kat-awq-7b", "kat-awq-1.5b", "big-box/glm-9", "remote-first"}
	calls := 0
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		logged = append(logged, title)
		ans := answers[min(calls, len(answers)-1)]
		calls++
		return ans, nil
	}
	c, err = runTierWizard(testLaunchModels("kat-awq", "kat-awq-7b", "kat-awq-1.5b", "big-box/glm-9"), "kat-awq")
	if err != nil {
		t.Fatalf("runTierWizard: %v", err)
	}
	if c.SonnetModel != "kat-awq-7b" || c.HaikuModel != "kat-awq-1.5b" || c.OversizeModel != "big-box/glm-9" || c.RoutePolicy != "remote-first" {
		t.Fatalf("unexpected choice: %+v", c)
	}
	if len(logged) != 6 {
		t.Fatalf("step sequence = %d prompts (%v), want 6 (step2, step3, re-ask step2, step3, step4, policy)", len(logged), logged)
	}

	// 3. Plan save: Enter reuses the last saved plan name.
	PlanSet("myplan", TierPlanProfile{Model: "kat-awq", RoutePolicy: "auto"})
	names, _ := PlanSortedNames()
	if len(names) == 0 || names[0] != "myplan" {
		t.Fatalf("plans = %v", names)
	}
	if last, _ := PlanLastUsed(); last != "myplan" {
		t.Fatalf("PlanLastUsed = %q, want myplan", last)
	}
	PlanSet("other", TierPlanProfile{Model: "kat-awq"})
	if last, _ := PlanLastUsed(); last != "other" {
		t.Fatalf("PlanLastUsed after second save = %q, want other", last)
	}
	PlanRemove("other")
	if last, _ := PlanLastUsed(); last != "" {
		t.Fatalf("PlanLastUsed after removal = %q, want empty", last)
	}
}

// TestRunTierWizard_BackSkipsOverNilStep verifies the fix for a real
// reported bug: with no oversize candidates (a common case — oversizeItems
// is nil whenever nothing probes larger than the primary), backing off the
// Route policy step must land on Haiku, not bounce right back to Route
// policy. Before the fix, i-=2 followed by the loop's forward-skip-if-nil
// check landed back on the same nil step and re-advanced past it to the
// exact step the user just backed off of — Esc/Left looked like it did
// nothing (2026-09-03).
func TestRunTierWizard_BackSkipsOverNilStep(t *testing.T) {
	withTempOaicaHome(t)
	origSelect, origRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = origSelect, origRead })
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }

	// No tierWizardResolveEndpoint/ProbeWindow stub set up: probedModelWindow
	// fails closed (real network lookup in a test sandbox), so
	// oversizeWindowCandidates returns nil and the oversize step is skipped
	// entirely — exactly the common real-world case this bug hit.
	var logged []string
	answers := []string{"kat-awq-7b", "kat-awq-1.5b", tierWizardBack, "kat-awq-1.5b", "remote-first"}
	calls := 0
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		logged = append(logged, title)
		ans := answers[min(calls, len(answers)-1)]
		calls++
		return ans, nil
	}
	c, err := runTierWizard(testLaunchModels("kat-awq", "kat-awq-7b", "kat-awq-1.5b"), "kat-awq")
	if err != nil {
		t.Fatalf("runTierWizard: %v", err)
	}
	if c.SonnetModel != "kat-awq-7b" || c.HaikuModel != "kat-awq-1.5b" || c.RoutePolicy != "remote-first" {
		t.Fatalf("unexpected choice: %+v", c)
	}
	want := []string{
		"Sonnet/subagent tier (secondary model)",
		"Haiku/background tier",
		"Route policy (what the launch proxy does when a backend fails)",
		"Haiku/background tier", // back from policy must re-ask Haiku, skipping the nil oversize step
		"Route policy (what the launch proxy does when a backend fails)",
	}
	if len(logged) != len(want) {
		t.Fatalf("step sequence = %v, want %v", logged, want)
	}
	for i := range want {
		if logged[i] != want[i] {
			t.Fatalf("step %d = %q, want %q (full sequence: %v)", i, logged[i], want[i], logged)
		}
	}
}

func TestRunTierWizard_AutoResolvesToRecommended(t *testing.T) {
	withTempOaicaHome(t)
	origSelect, origRead := tierWizardSelect, tierWizardReadLine
	t.Cleanup(func() { tierWizardSelect, tierWizardReadLine = origSelect, origRead })
	tierWizardReadLine = func(prompt string) (string, error) { return "", nil }
	models := testLaunchModels("kat-awq", "kat-awq-7b", "big-box/glm-9")
	models[2].Recommended = true // big-box/glm-9
	// First selection = items[0] = "auto" (auto leads the step).
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if title == "Sonnet/subagent tier (secondary model)" {
			if items[0].Name != "auto" {
				t.Fatalf("auto must lead the secondary step, got %q", items[0].Name)
			}
			return "auto", nil
		}
		return items[0].Name, nil // "(same as primary)" / local-first
	}
	c, err := runTierWizard(models, "kat-awq")
	if err != nil {
		t.Fatalf("runTierWizard: %v", err)
	}
	if c.SonnetModel != "big-box/glm-9" {
		t.Fatalf("auto resolved to %q, want big-box/glm-9", c.SonnetModel)
	}
	// No recommended non-primary model: "auto" is dropped, "(same as
	// primary)" leads, and the default keeps the single-model launch.
	models2 := testLaunchModels("kat-awq", "kat-awq-7b")
	tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
		if title == "Sonnet/subagent tier (secondary model)" && items[0].Name != "(same as primary)" {
			t.Fatalf("without a recommendation auto must be dropped, got %q", items[0].Name)
		}
		return items[0].Name, nil
	}
	c, err = runTierWizard(models2, "kat-awq")
	if err != nil {
		t.Fatalf("runTierWizard: %v", err)
	}
	if c.SonnetModel != "" {
		t.Fatalf("default with no recommendation = %q, want empty", c.SonnetModel)
	}
}
