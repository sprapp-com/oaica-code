package launch

// tier_wizard.go — the interactive launch tier wizard (2026-08-31 design,
// option A). Step 1 is the model picker a plain `oaica launch claude`
// already runs; this file adds the remaining three steps on the same list
// of picker models:
//
//	Step 2  Sonnet/subagent tier (secondary) — same list, "(same as
//	        primary)" first and the default.
//	Step 3  Compaction/oversize model — candidates filtered to models whose
//	        PROBED context window (remoteContextWindowFn, the same 2s /models
//	        probe the proxy uses) is strictly larger than the primary's;
//	        "(none — fail honestly at the ceiling)" is the default. A small
//	        context is never silently oversized to a model that can't hold
//	        the request either.
//	Step 4  Route policy — the same five values as --route-policy,
//	        local-first the default.
//
// After step 4 a one-line preview prints (e.g.
// `fallback: a <-> b · oversize: c (256k) · policy: local-first`) and the
// choice can be saved as a named plan (`oaica plan`, tier_plan_profiles.go).
//
// The wizard runs ONLY for interactive, picker-driven launches: a launch
// whose primary came from an explicit --model flag, a non-interactive
// session, or one that passed --plan/--sonnet-model/--oversize/
// --route-policy never sees it (flag-only launches stay byte-identical).

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"strings"
)

// tierWizardChoice is what the wizard collected.
type tierWizardChoice struct {
	SonnetModel   string // empty = same as primary
	OversizeModel string // empty = no oversize leg
	RoutePolicy   string // always a valid policy after the wizard
	PlanName      string // non-empty when the user saved the choice
}

// tierWizardEligibleLaunch is set per launch by LaunchIntegration (launch.go):
// true when the primary model came from the interactive picker (no
// --model/--plan override) and the session is interactive. Cleared/never set
// for flag-only launches, which must stay untouched.
var tierWizardEligibleLaunch bool

// tierWizardFlags are the launcher-level flags that suppress the wizard: a
// caller who passed any of them already made (part of) these decisions.
var tierWizardFlags = []string{"--sonnet-model", "--oversize", "--route-policy", "--plan", "--force-tools", "--brief-mode"}

// tierWizardEligible reports whether this launch should run steps 2-4.
func tierWizardEligible(args []string) bool {
	if !tierWizardEligibleLaunch || !isInteractiveSession() {
		return false
	}
	for _, a := range args {
		for _, f := range tierWizardFlags {
			if a == f || strings.HasPrefix(a, f+"=") {
				return false
			}
		}
	}
	return true
}

// tierWizardSelect picks one option from a list. Production uses the same
// Bubbletea single selector the rest of the launcher uses; the fallback (raw
// terminal, tests) is a numbered prompt. Returning items[0].Name ("leave
// unset") on empty input makes the first item the default, which every step
// relies on.
var tierWizardSelect = func(title string, items []SelectionItem) (string, error) {
	if l := len(items); l == 0 {
		return "", errors.New("no choices offered")
	}
	if DefaultSingleSelector != nil {
		return DefaultSingleSelector(title, items, items[0].Name)
	}
	fmt.Fprintf(os.Stderr, "%s\n", title)
	for i, it := range items {
		fmt.Fprintf(os.Stderr, "  %d) %s  %s\n", i+1, it.Name, it.Description)
	}
	fmt.Fprintf(os.Stderr, "choose 1-%d [enter = %q]: ", len(items), items[0].Name)
	line, err := tierWizardReadLine("")
	if err != nil && line == "" {
		return "", err
	}
	idx := 1
	if line != "" {
		if _, err := fmt.Sscanf(line, "%d", &idx); err != nil || idx < 1 || idx > len(items) {
			return "", fmt.Errorf("invalid choice %q", line)
		}
	}
	return items[idx-1].Name, nil
}

// tierWizardReadLine reads one line of plain (cooked) terminal input. A hook
// var so tests can script the answers.
var tierWizardReadLine = func(prompt string) (string, error) {
	fmt.Fprint(os.Stderr, prompt)
	r := bufio.NewReader(os.Stdin)
	line, err := r.ReadString('\n')
	return strings.TrimSpace(line), err
}

// tierWizardResolveEndpoint / tierWizardProbeWindow are the wizard's only
// doorways into endpoint resolution and the /models context probe — both
// swappable for tests (the probe default is remoteContextWindowFn itself,
// the same 2s swap point the proxy uses).
var (
	tierWizardResolveEndpoint = resolveLaunchEndpoint
	tierWizardProbeWindow     func(proxyRoute) int // nil = remoteContextWindowFn
)

// probedModelWindow is a model's live context window (0 = unknown).
func probedModelWindow(model string) int {
	ep, err := tierWizardResolveEndpoint(model)
	if err != nil {
		return 0
	}
	if tierWizardProbeWindow == nil {
		return remoteContextWindowFn(routeFor(ep))
	}
	return tierWizardProbeWindow(routeFor(ep))
}

// oversizeWindowCandidates filters the picker model list down to models whose
// probed context window is STRICTLY larger than the primary's — the honest
// definition of an oversize leg for the primary. resolve/probe are nil-able
// (defaults: resolveLaunchEndpoint / remoteContextWindowFn). The primary's
// window is returned too, for display; 0 when unknown, in which case no
// candidate is offered (a "larger" pick would be guesswork).
func oversizeWindowCandidates(models []string, primary string, resolve func(string) (launchEndpoint, error), probe func(proxyRoute) int) ([]string, int) {
	if resolve == nil {
		resolve = resolveLaunchEndpoint
	}
	if probe == nil {
		probe = remoteContextWindowFn
	}
	var cands []string
	pr, err := resolve(primary)
	if err != nil {
		return nil, 0
	}
	primaryWindow := probe(routeFor(pr))
	if primaryWindow <= 0 {
		return nil, 0
	}
	for _, m := range models {
		if m == primary {
			continue
		}
		ep, err := resolve(m)
		if err != nil {
			continue
		}
		// Same base URL as the primary = same failure domain and the same
		// fit limit; oversizeSwap (route_policy.go) never crosses to it, so
		// do not offer it.
		if ep.BaseURL == "" || ep.BaseURL == pr.BaseURL {
			continue
		}
		if w := probe(routeFor(ep)); w > primaryWindow {
			cands = append(cands, m)
		}
	}
	return cands, primaryWindow
}

const tierWizardNoOversize = "(none — fail honestly at the ceiling)"

// runTierWizard runs steps 2-4 on the picker model list. models is the same
// inventory the picker showed; primary is the already-picked model.
func runTierWizard(models []LaunchModel, primary string) (tierWizardChoice, error) {
	c := tierWizardChoice{RoutePolicy: string(RouteLocalFirst)}
	names := launchModelNames(models)

	if len(names) > 0 {
		// Step 2 — Sonnet/subagent tier. Same picker vocabulary; "(same as
		// primary)" first so Enter keeps the single-model launch.
		items := []SelectionItem{{Name: "(same as primary)", Description: "route all tiers to " + primary}}
		for _, n := range names {
			if n != primary {
				items = append(items, SelectionItem{Name: n})
			}
		}
		if len(items) > 1 {
			sel, err := tierWizardSelect("Sonnet/subagent tier (secondary model)", items)
			if err != nil {
				return c, err
			}
			if sel != items[0].Name {
				c.SonnetModel = sel
			}
		}

		// Step 3 — compaction/oversize model. Only models whose PROBED window
		// is strictly larger than the primary's probed window qualify; with
		// no such model (or no answered probe) the step offers nothing and
		// the ceiling fails honestly, as it does today.
		cands, primaryWindow := oversizeWindowCandidates(names, primary, tierWizardResolveEndpoint, tierWizardProbeWindow)
		if len(cands) > 0 {
			items := []SelectionItem{{Name: tierWizardNoOversize, Description: "requests that cannot fit fail visibly (today's behavior)"}}
			for _, n := range cands {
				w := probedModelWindow(n)
				desc := ""
				if w > 0 {
					desc = fmt.Sprintf("probed window %dk", w/1024)
				}
				items = append(items, SelectionItem{Name: n, Description: desc})
			}
			sel, err := tierWizardSelect(fmt.Sprintf("Compaction/oversize model (strictly larger than %s's probed %dk window)", primary, primaryWindow/1024), items)
			if err != nil {
				return c, err
			}
			if sel != items[0].Name {
				c.OversizeModel = sel
			}
		}
	}

	// Step 4 — route policy. Every value --route-policy accepts; local-first
	// first (the default), same ordering as `oaica doctor`'s legend.
	policyItems := []SelectionItem{
		{Name: string(RouteLocalFirst), Description: "on failure prefer a local backend, else any healthy alternate (default)"},
		{Name: string(RouteRemoteFirst), Description: "on failure prefer a remote backend, else any healthy alternate"},
		{Name: string(RouteAuto), Description: "alias of local-first for now (per-request escalation ships later)"},
		{Name: string(RouteLocalOnly), Description: "never leave local legs — fail visibly rather than cross over"},
		{Name: string(RouteRemoteOnly), Description: "never leave remote legs — same"},
	}
	sel, err := tierWizardSelect("Route policy (what the launch proxy does when a backend fails)", policyItems)
	if err != nil {
		return c, err
	}
	c.RoutePolicy = sel

	fmt.Fprintf(os.Stderr, "%s\n", tierWizardPreview(primary, c))

	name, err := tierWizardReadLine("Save as plan (name, blank = skip): ")
	if err != nil && name == "" {
		return c, err
	}
	if name != "" {
		desc := "interactive launch wizard"
		if err := PlanSet(name, TierPlanProfile{
			Model:         primary,
			SonnetModel:   c.SonnetModel,
			OversizeModel: c.OversizeModel,
			RoutePolicy:   c.RoutePolicy,
			Description:   desc,
		}); err != nil {
			return c, fmt.Errorf("save plan %q: %w", name, err)
		}
		c.PlanName = name
		fmt.Fprintf(os.Stderr, "saved plan %q — reuse with `oaica launch claude --plan %s`\n", name, name)
	}
	return c, nil
}

// tierWizardPreview is the one-line summary printed before the save prompt,
// e.g. `fallback: a <-> b · oversize: c (256k) · policy: local-first`.
func tierWizardPreview(primary string, c tierWizardChoice) string {
	line := "fallback: " + primary
	if c.SonnetModel != "" {
		line += " <-> " + c.SonnetModel
	}
	if c.OversizeModel != "" {
		line += " · oversize: " + c.OversizeModel
		if w := probedModelWindow(c.OversizeModel); w > 0 {
			line += fmt.Sprintf(" (%dk)", w/1024)
		}
	}
	policy := c.RoutePolicy
	if policy == "" {
		policy = string(RouteLocalFirst)
	}
	return line + " · policy: " + policy
}
