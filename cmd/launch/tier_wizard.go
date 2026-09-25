package launch

// tier_wizard.go — the interactive launch tier wizard (2026-08-31 design,
// option A; haiku tier added 2026-09-02). Step 1 is the model picker a
// plain `oaica launch claude` already runs; this file adds the remaining
// steps on the same list of picker models:
//
//	Step 0  Saved plans (2026-09-26) — only when one exists: reuse the plan
//	        launched last from this directory (Enter), pick another, or start
//	        from scratch. A reused plan supplies every tier, so steps 2-4 are
//	        skipped and the plan resolves exactly as a typed --plan.
//	Step 2  Sonnet/subagent tier (secondary) — same list, leading with
//	        "keep <model>" when ~/.oaica/config.json has a sonnet_model
//	        (Enter keeps it), then "auto"/"(same as primary)" and the rest.
//	Step 3  Haiku/background tier — same list, same "keep" row for
//	        haiku_model, then "(same as primary)" and the rest. No "auto":
//	        unlike Sonnet, there is no "best recommended model" concept for
//	        cheap/background requests.
//	Step 4  Compaction/oversize model — candidates filtered to models whose
//	        PROBED context window (remoteContextWindowFn, the same 2s /models
//	        probe the proxy uses) is at least the primary's (">=" is
//	        deliberate: an equal-window independent backend can still take
//	        over when the primary fails near the ceiling — only the size
//	        crossover itself needs strictly larger);
//	        "(none — fail honestly at the ceiling)" is the default. A small
//	        context is never silently oversized to a model that can't hold
//	        the request either.
//	Step 5  Route policy — five of the six --route-policy values, "auto"
//	        pre-selected (weighted is absent: the wizard has no step for
//	        per-leg weights).
//
// After step 5 a one-line preview prints (e.g.
// `fallback: a <-> b · oversize: c (256k) · policy: auto`) and the
// choice can be saved as a named plan (`oaica plan`, tier_plan_profiles.go).
//
// The wizard runs ONLY for interactive, picker-driven launches: a launch
// whose primary came from an explicit --model flag, a non-interactive
// session, or one that passed --plan/--sonnet-model/--haiku-model/
// --oversize/--route-policy never sees it (flag-only launches stay
// byte-identical).

import (
	"bufio"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// tierWizardChoice is what the wizard collected.
type tierWizardChoice struct {
	SonnetModel   string // empty = same as primary
	HaikuModel    string // empty = same as primary
	OversizeModel string // empty = no oversize leg
	RoutePolicy   string // always a valid policy after the wizard
	PlanName      string // non-empty when the user saved the choice

	// The flags below say whether an EMPTY value above is an answer or
	// just an unanswered step. Both readings of "" are real: a step the user
	// never reached (or abandoned with esc) must leave the caller's own value
	// alone, while "(same as primary)" and "no oversize leg" are deliberate
	// clears that must not be refilled by ~/.oaica/config.json.
	SonnetAnswered   bool // the Sonnet step produced a choice (a model or "same as primary")
	HaikuAnswered    bool // likewise for the Haiku step
	SonnetCleared    bool // "(same as primary)" was answered
	HaikuCleared     bool // likewise
	OversizeAnswered bool // the oversize step produced a choice (a leg or "none")
	PolicyAnswered   bool // the policy step produced a choice
}

// tierWizardEligibleLaunch is set per launch by LaunchIntegration (launch.go):
// true for any interactive, non-restore launch (--model included — the
// wizard's defaults are Enter = keep it). Cleared/never set for flag-only
// and headless launches, which must stay untouched.
var tierWizardEligibleLaunch bool

// extractWizardFlag pulls a launcher-level "--wizard" (or "--wizard=true")
// flag that FORCES steps 2-4 even on a launch the eligibility gate would
// skip (a --model launch, mainly). Not forwarded to the child binary.
func extractWizardFlag(args []string) (bool, []string) {
	rest := args[:0:0]
	found := false
	for _, a := range args {
		switch a {
		case "--wizard":
			found = true
		case "--wizard=false":
		default:
			rest = append(rest, a)
		}
	}
	return found, rest
}

// tierWizardFlags are the launcher-level flags that suppress the wizard: a
// caller who passed any of them already made (part of) these decisions.
//
// --force-tools and --brief-mode are deliberately NOT in this list. They are
// not tier decisions (a tool-gate override and an output style), so passing
// them says nothing about which models to run: suppressing the wizard for them
// silently removed the tier steps from `oaica launch claude --brief-mode` with
// no message at all (2026-09-26 audit — the check runs on the raw argv, before
// the extractors strip them, so their membership only ever showed up after the
// gate moved earlier).
var tierWizardFlags = []string{"--sonnet-model", "--haiku-model", "--oversize", "--route-policy", "--plan", "--shard"}

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
		sel, err := DefaultSingleSelector(title, items, items[0].Name)
		// esc / arrow-left cancel the selector: translate to "go back a
		// step" for the wizard (runTierWizard's navigation), never abort
		// the launch underneath it.
		if errors.Is(err, ErrCancelled) {
			return tierWizardBack, nil
		}
		return sel, err
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
//
// The resolve default is MEMOIZED per model id for the process lifetime:
// resolveLaunchEndpoint does live network per call (router GET /v1/models
// with an 8s timeout, then a daemon POST /api/show with a 3s timeout), and
// the oversize step resolves EVERY candidate in the picker list — uncached
// that is minutes of dead latency after the secondary step. A launch
// process lives seconds-to-minutes; a stale resolution for one launch is
// not a real risk. Tests overriding the var are unaffected.
var (
	tierWizardResolveEndpoint = memoizedResolveLaunchEndpoint
	tierWizardProbeWindow     func(proxyRoute) int // nil = remoteContextWindowFn
)

var wizardResolveMemo sync.Map // model name -> wizardResolved (endpoint or error)

type wizardResolved struct {
	ep  launchEndpoint
	err error
}

func memoizedResolveLaunchEndpoint(model string) (launchEndpoint, error) {
	if v, ok := wizardResolveMemo.Load(model); ok {
		r := v.(wizardResolved)
		return r.ep, r.err
	}
	ep, err := resolveLaunchEndpoint(model)
	wizardResolveMemo.Store(model, wizardResolved{ep: ep, err: err})
	return ep, err
}

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
// probed context window is at least the primary's (">=" — see the ">=" note at
// the filter itself: only the size crossover needs strictly larger, while an
// equal-window independent backend is still usable as a failure fallback).
// resolve/probe are nil-able (defaults: resolveLaunchEndpoint /
// remoteContextWindowFn). The primary's window is returned too, for display; 0
// when unknown, in which case no candidate is offered (a "larger" pick would be
// guesswork).
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
	// Resolve and probe candidates concurrently. A full picker can contain
	// hundreds of entries, and a serial 2-second /models timeout made the
	// wizard appear hung after the tier choices. A small worker pool limits
	// the burst against providers while bounding menu latency to the slowest
	// few probes instead of every candidate.
	qualifies := make([]bool, len(models))
	jobs := make(chan int)
	var wg sync.WaitGroup
	workers := 12
	if len(models) < workers {
		workers = len(models)
	}
	for worker := 0; worker < workers; worker++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range jobs {
				m := models[i]
				if m == primary {
					continue
				}
				ep, err := resolve(m)
				if err != nil {
					continue
				}
				// Same base URL as the primary = same failure domain and the same
				// fit limit; oversizeSwap never crosses to it, so do not offer it.
				if ep.BaseURL == "" || ep.BaseURL == pr.BaseURL {
					continue
				}
				// >= is intentional: an equal-window independent backend can
				// still survive a primary failure near the ceiling.
				qualifies[i] = probe(routeFor(ep)) >= primaryWindow
			}
		}()
	}
	for i := range models {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	for i, m := range models {
		if qualifies[i] {
			cands = append(cands, m)
		}
	}
	return cands, primaryWindow
}

const tierWizardNoOversize = "(none — fail honestly at the ceiling)"
const tierWizardScanOversize = "(find a compatible oversize model)"

// tierWizardBack is what tierWizardSelect returns when the user pressed
// esc/←: go back one step (off the first step = abandon the wizard and
// launch with the defaults).
const tierWizardBack = "\x00back"

// tierWizardTierItems builds the item list for one tier-selection step
// (Sonnet or Haiku) over the same picker model list: a "keep the saved tier"
// row leads when the standing config has one (keep != ""), then "auto" when
// withAuto (only the Sonnet step wants it), then "(same as primary)", then
// OAICA router recommendations marked "OAICA Models" (picker parity), then
// everything else in the alphabetized "Remote Models" tail. autoTarget is the
// first recommended non-primary model in its provider-prefixed form — the
// "auto" step's Sonnet answer, or "" when withAuto is false or there is no
// recommendation to resolve to.
//
// leadRow puts name at the front of items, described as keepDesc, adding it
// when the list does not offer it (a typed route policy is not restricted to
// the wizard's own menu). The selector's Enter key answers items[0], so a value
// already in effect has to lead its step for Enter to mean "keep it" — and it
// must be SELECTABLE (the row's name IS the value) rather than an implicit
// default, or the preview at the end shows a choice the launch will discard.
func leadRow(items []SelectionItem, name, keepDesc string) []SelectionItem {
	out := []SelectionItem{{Name: name, Description: keepDesc}}
	for _, it := range items {
		if it.Name != name {
			out = append(out, it)
		}
	}
	return out
}

// keep must lead, because the selector's default is items[0]: Enter has to
// mean "keep my saved tier" for a standing config to be worth anything, and it
// must be SELECTABLE (the row's name IS the model) rather than an implicit
// default, or the preview at the end could show a tier the launch discards.
func tierWizardTierItems(models []LaunchModel, names []string, primary string, withAuto bool, keep string) (items []SelectionItem, autoTarget string) {
	if len(names) == 0 && keep == "" {
		return nil, ""
	}
	recommended := map[string]bool{}
	routerRec := map[string]bool{}
	for _, m := range models {
		if !m.Recommended {
			continue
		}
		recommended[m.Name] = true
		// The picker's "Recommended" set is a curated mix (local
		// gemma4/qwen3.5, cloud glm/deepseek, router SKUs); the wizard
		// labels its pinned section "OAICA Models", so only actual
		// router SKUs may be pinned there — anything else would read as
		// served by the router when it isn't (2026-09-02 .46: deepseek/
		// glm/gemma rows under "OAICA Models"). Other recommendations
		// stay in the general Remote section and still feed "auto".
		if strings.HasPrefix(m.Name, "oaica-") {
			routerRec[m.Name] = true
		}
	}
	var lead []SelectionItem
	if keep != "" {
		// The caller passes the value in effect, which is the standing config
		// tier OR a --sonnet-model/--haiku-model typed on this command line
		// (with --wizard, the only way a typed tier reaches this step), so the
		// label names both sources rather than guessing wrong.
		lead = append(lead, SelectionItem{Name: keep, Description: "your tier for this launch (--sonnet-model/--haiku-model or ~/.oaica/config.json) — enter keeps it", Recommended: true})
	}
	autoIdx := -1
	if withAuto {
		autoIdx = len(lead)
		lead = append(lead, SelectionItem{Name: "auto", Description: "let OAICA pick this tier (best recommended model) — recommended", Recommended: true})
	}
	lead = append(lead, SelectionItem{Name: "(same as primary)", Description: "route this tier to " + primary, Recommended: true})
	items = lead
	// tierItemName namespaces every row by its provider so the stored plan
	// is unambiguous at launch (resolveSecondaryEndpoint's explicit forms):
	// "oaica-*" stays bare (the router's own id), anything already carrying
	// "<owner>/" stays as-is (a user remote), everything else is
	// "ollama/<id>" — the local Ollama daemon, including its ":cloud"
	// catalog models. Bare ambiguous ids are NOT offered: we don't serve
	// them as OAICA models (2026-09-02).
	tierItemName := func(n string) string {
		switch {
		case strings.HasPrefix(n, "oaica-"), strings.Contains(n, "/"):
			return n
		default:
			return "ollama/" + n
		}
	}
	for _, n := range names {
		if n == primary {
			continue
		}
		if recommended[n] {
			if withAuto && autoTarget == "" {
				autoTarget = tierItemName(n)
			}
			// Recommended+Remote flags pin the row into the "OAICA
			// Models" section at the top — router SKUs only; other
			// recommended rows fall through to the Remote section below
			// but still count for "auto".
			items = append(items, SelectionItem{Name: tierItemName(n), Description: "(Recommended)", Recommended: routerRec[n], Remote: true})
		}
	}
	for _, n := range names {
		if n != primary && !recommended[n] {
			// Remote flag: "Remote Models" section (alphabetical),
			// NOT the scrollable "More" bucket.
			items = append(items, SelectionItem{Name: tierItemName(n), Remote: true})
		}
	}
	// Alphabetize the non-recommended tail (recommended rows already lead);
	// keep the leading synthetic rows untouched.
	sort.SliceStable(items[len(lead):], func(a, b int) bool {
		return items[len(lead)+a].Name < items[len(lead)+b].Name
	})
	// No recommendation available: "auto" would be a promise it can't keep —
	// drop it (by index, since "keep" may sit ahead of it).
	if withAuto && autoTarget == "" && autoIdx >= 0 {
		items = append(append([]SelectionItem{}, items[:autoIdx]...), items[autoIdx+1:]...)
	}
	// Nothing but "(same as primary)": a single-model inventory with no saved
	// tier has nothing to choose. A saved tier IS a choice, so keep the step.
	if len(items) <= 1 && keep == "" {
		return nil, autoTarget
	}
	return items, autoTarget
}

// tierWizardOtherPlan / tierWizardScratch are the two non-plan rows of the
// wizard's first step (see tierWizardReusedPlan).
const (
	tierWizardOtherPlan = "(another saved plan…)"
	tierWizardScratch   = "(start from scratch — walk the tiers)"
)

// tierWizardReusedPlan runs the wizard's first step: start from a saved plan,
// or walk the tiers. Returns (name, true) when the user chose a plan — the
// caller then resolves it exactly as a typed --plan does, so every tier comes
// from the plan and the remaining steps are skipped — and ("", false) for
// "start from scratch".
//
// Only offered when at least one plan exists: with none, the step would be a
// menu of one real option. An unreadable plans.json degrades to "no plans"
// rather than failing the launch — the same forgiveness PlanLastUsed applies,
// and the alternative is that a corrupt preference file blocks every launch.
func tierWizardReusedPlan() (string, bool, error) {
	names, err := PlanSortedNames()
	if err != nil || len(names) == 0 {
		return "", false, nil
	}
	last, err := PlanLastUsed()
	if err != nil {
		last = ""
	}
	items := []SelectionItem{}
	if last != "" {
		// "its tiers", not "it": the primary is the one just picked above, and
		// a plan's stored Model is not forced on it (the same rule as
		// --model with --plan).
		items = append(items, SelectionItem{Name: last, Description: "the last plan launched from this directory — enter reuses its tiers for the model you picked"})
	}
	items = append(items, SelectionItem{Name: tierWizardOtherPlan, Description: "pick from your saved plans"})
	items = append(items, SelectionItem{Name: tierWizardScratch, Description: "choose the tiers for this launch (and optionally save them as a new plan)"})
	if last == "" {
		// Nothing to reuse as the default, so do not make the picker's
		// Enter-key default a second menu.
		items[0], items[1] = items[1], items[0]
	}
	sel, err := tierWizardSelect("Start from a saved plan?", items)
	if err != nil {
		return "", false, err
	}
	switch sel {
	case "":
		// No selection at all — cmd/tui's selector returns "" with a nil error
		// when Enter is pressed on a filter that matches nothing. That is not
		// an answer, and it must not silently become "reuse the last plan".
		return "", false, nil
	case last:
		return last, true, nil
	case tierWizardOtherPlan:
		picked, err := tierWizardSelect("Saved plans", planNameItems(names))
		if err != nil {
			return "", false, err
		}
		if picked == "" || picked == tierWizardBack || picked == tierWizardScratch {
			return "", false, nil
		}
		return picked, true, nil
	case tierWizardScratch, tierWizardBack:
		return "", false, nil
	}
	// A selector that answered with a plan name directly (production pickers
	// return items[0].Name on empty input, and tests may stub a name).
	for _, n := range names {
		if n == sel {
			return n, true, nil
		}
	}
	return "", false, nil
}

// planNameItems is one SelectionItem per plan name, for the "another saved
// plan" sub-step.
func planNameItems(names []string) []SelectionItem {
	items := make([]SelectionItem, 0, len(names))
	for _, n := range names {
		items = append(items, SelectionItem{Name: n, Description: "saved plan"})
	}
	return items
}

// runTierWizard runs the wizard's steps on the picker model list. models is
// the same inventory the picker showed; primary is the already-picked model;
// keepSonnet/keepHaiku are the standing config tiers (~/.oaica/config.json) and
// keepOversize/keepPolicy the --oversize/--route-policy values typed on this
// command line — each leads its step as a selectable "keep" row so Enter means
// what it looks like it means, and each is "" when nothing is set. Navigation:
// enter advances, esc/arrow-left steps back (re-asking the previous prompt);
// esc on the very first step abandons the wizard and the launch continues with
// the defaults.
//
// A first step is offered when at least one plan exists: reuse the plan used
// last from this directory (Enter), pick another saved plan, or start from
// scratch and walk the tiers. A reused plan supplies the tiers, so the tier
// steps are skipped and the choice comes back in PlanName — the caller
// resolves it exactly like a typed --plan. Every other field of the returned
// choice stays UNANSWERED on that path, including RoutePolicy: the plan's own
// route_policy belongs to resolvePlanTier, and pre-setting "auto" here used to
// outrank it (the plan's local-only silently became auto; 2026-09-26 audit).
func runTierWizard(models []LaunchModel, primary, keepSonnet, keepHaiku, keepOversize, keepPolicy string) (tierWizardChoice, error) {
	c := tierWizardChoice{RoutePolicy: string(RouteAuto)}
	names := launchModelNames(models)

	if plan, reused, err := tierWizardReusedPlan(); err != nil {
		return c, err
	} else if reused {
		// Clear the pre-set default: nothing else was answered, and the caller
		// must treat the policy as unset so the plan's own route_policy wins.
		c.PlanName, c.RoutePolicy = plan, ""
		return c, nil
	}

	// Step 2 — Sonnet/subagent tier, and step 3 — Haiku/background tier.
	// Same picker vocabulary for both; a saved tier leads as "keep", else
	// "(same as primary)" leads so Enter keeps the single-model launch; OAICA
	// router recommendations lead the rest and are marked (the picker's "OAICA
	// Models" section order, carried over). tierWizardTierItems builds one
	// step's item list; only the Sonnet step offers "auto" (a background/haiku
	// tier has no "best recommended model" concept the product otherwise uses).
	sonnetItems, autoSecondary := tierWizardTierItems(models, names, primary, true, keepSonnet)
	haikuItems, _ := tierWizardTierItems(models, names, primary, false, keepHaiku)

	// Step 4 — route policy. `auto` first and pre-selected: it is the wizard's
	// default (a plain launch has no explicit policy to honor), and today it
	// starts from local-first. `weighted` is deliberately absent — the wizard
	// has no step for per-leg weights, so picking it here could only ever
	// behave as plain failover; use --shard / remotes.json "weight".
	policyItems := []SelectionItem{
		{Name: string(RouteAuto), Description: "let OAICA route on failure — recommended (today: local-first, prefer a local backend, else any healthy alternate)"},
		{Name: string(RouteLocalFirst), Description: "on failure prefer a local backend, else any healthy alternate"},
		{Name: string(RouteRemoteFirst), Description: "on failure prefer a remote backend, else any healthy alternate"},
		{Name: string(RouteLocalOnly), Description: "never leave local legs — fail visibly rather than cross over"},
		{Name: string(RouteRemoteOnly), Description: "never leave remote legs — same"},
	}
	// A policy or oversize leg typed on the command line leads its step, the
	// same way a saved config tier leads its own: the flag wins regardless
	// (see the caller), so a step whose Enter key landed anywhere else would
	// print a preview line naming a leg the launch is not going to use.
	if keepPolicy != "" {
		policyItems = leadRow(policyItems, keepPolicy, "typed on the command line — enter keeps it")
	}

	type wizardStep struct {
		title    string
		items    []SelectionItem
		optional bool // nil items = skipped entirely, but still a back-stop
	}
	// oversizeItems is the oversize step's menu. A leg typed with --oversize
	// leads it for the same reason a typed policy leads its own step: the flag
	// is what the launch will use, and Enter must not make the preview claim
	// otherwise.
	oversizeItems := func() []SelectionItem {
		items := []SelectionItem{
			{Name: tierWizardNoOversize, Description: "requests that cannot fit fail visibly (today's behavior)"},
			{Name: tierWizardScanOversize, Description: "probe the catalog for a larger-context fallback (may take a while)"},
		}
		if keepOversize != "" {
			items = leadRow(items, keepOversize, "typed on the command line — enter keeps it")
		}
		return items
	}
	steps := []wizardStep{
		{title: "Sonnet/subagent tier (secondary model)", items: sonnetItems, optional: true},
		{title: "Haiku/background tier", items: haikuItems, optional: true},
		// Do not probe the entire catalog on the interactive launch path. A
		// catalog can have hundreds of remote models, and a context probe can
		// take seconds each. The default stays the existing honest failure
		// behavior; discovery is an explicit, opt-in action below.
		{title: "Compaction/oversize model", items: oversizeItems(), optional: true},
		{title: "Route policy (what the launch proxy does when a backend fails)", items: policyItems},
	}
	// clearStep resets the field the step at index i writes, on stepping
	// back off of it — every step keeps "empty = keep it" as its default.
	clearStep := func(i int) {
		switch i {
		case 0:
			// Sonnet: the step is being re-asked, so back to "unset", which
			// means KEEP the caller's own tier — not the deliberate clear a
			// "(same as primary)" answer records.
			c.SonnetModel, c.SonnetCleared, c.SonnetAnswered = "", false, false
		case 1:
			c.HaikuModel, c.HaikuCleared, c.HaikuAnswered = "", false, false
		case 2:
			c.OversizeModel, c.OversizeAnswered = "", false
		case 3:
			c.RoutePolicy, c.PolicyAnswered = string(RouteAuto), false
		}
	}
	for i := 0; i < len(steps); {
		s := steps[i]
		if s.items == nil {
			i++
			continue
		}
		sel, err := tierWizardSelect(s.title, s.items)
		if err != nil {
			return c, err
		}
		if sel == tierWizardBack {
			if i == 0 {
				// Backing off the SONNET step goes back to the plan step — it
				// IS the previous prompt whenever one was offered, and the doc
				// above promises esc re-asks it. With no plan step to return
				// to (no plans saved), the wizard ends as before.
				if planNames, _ := PlanSortedNames(); len(planNames) > 0 {
					clearStep(0)
					if plan, reused, err := tierWizardReusedPlan(); err != nil {
						return c, err
					} else if reused {
						c.PlanName, c.RoutePolicy = plan, ""
						return c, nil
					}
					continue // chose "start from scratch" again: re-ask this step
				}
				return c, nil
			}
			clearStep(i)
			i--
			// Walk back past any skipped (nil-items) steps too — a plain
			// i-- landed on a skipped step forward-skips it right back to
			// where we started, making Esc/Left look like it does nothing
			// (2026-09-03, reported: oversize step is nil whenever no
			// candidate qualifies, which is common, so backing off Route
			// policy silently no-oped almost every time).
			for i > 0 && steps[i].items == nil {
				i--
			}
			continue
		}
		switch i {
		case 0:
			// An ANSWER, like the oversize/policy steps below: esc or an
			// empty picker result must leave a typed --sonnet-model alone.
			// It used to be applied unconditionally, so backing out of the
			// wizard wiped the flag it never asked about (2026-09-26).
			if sel == "" {
				break
			}
			c.SonnetAnswered = true
			if sel == "auto" {
				// Resolved here, not downstream: plans store a concrete
				// model name, and "auto" has no meaning after launch.
				sel = autoSecondary
			}
			// "(same as primary)" is an explicit CLEAR, not "leave what is
			// there": the step may lead with a saved tier, and without this
			// there was no row that could take a split back off. It is
			// recorded as a clear because "" alone is indistinguishable from
			// an unanswered step, which the caller refills from
			// ~/.oaica/config.json.
			if sel == "(same as primary)" {
				c.SonnetModel, c.SonnetCleared = "", true
			} else if sel != "" {
				c.SonnetModel, c.SonnetCleared = sel, false
			}
		case 1:
			if sel == "" {
				break
			}
			c.HaikuAnswered = true
			if sel == "(same as primary)" {
				c.HaikuModel, c.HaikuCleared = "", true
			} else if sel != "" {
				c.HaikuModel, c.HaikuCleared = sel, false
			}
		case 2:
			if sel == tierWizardScanOversize {
				// Scan only after the user explicitly asks for it. Keep this
				// step selected afterwards so they can choose a discovered
				// model, or the first "none" row to abandon the fallback.
				cands, primaryWindow := oversizeWindowCandidates(names, primary, tierWizardResolveEndpoint, tierWizardProbeWindow)
				if len(cands) > 0 {
					items := []SelectionItem{{Name: tierWizardNoOversize, Description: "requests that cannot fit fail visibly (today's behavior)"}}
					for _, n := range cands {
						w := probedModelWindow(n)
						desc := ""
						if w > 0 {
							desc = fmt.Sprintf("probed window %dk", w/1024)
						}
						items = append(items, SelectionItem{Name: n, Description: desc, Remote: true})
					}
					if keepOversize != "" {
						items = leadRow(items, keepOversize, "typed on the command line — enter keeps it")
					}
					steps[i].items = items
					steps[i].title = fmt.Sprintf("Compaction/oversize model (at least %s's probed %dk window)", primary, primaryWindow/1024)
				} else {
					steps[i].items = []SelectionItem{{Name: tierWizardNoOversize, Description: "no compatible larger-context fallback answered"}}
				}
				continue
			}
			// Either row is an ANSWER, including "none": with a --oversize leg
			// on the command line, picking "none" is how the user declines it,
			// and an unanswered step (esc, or a picker that returned nothing)
			// must leave that flag alone.
			if sel == "" {
				break
			}
			c.OversizeAnswered = true
			if sel == tierWizardNoOversize {
				c.OversizeModel = ""
			} else {
				c.OversizeModel = sel
			}
		case 3:
			if sel == "" {
				break
			}
			c.RoutePolicy, c.PolicyAnswered = sel, true
		}
		i++
	}

	fmt.Fprintf(os.Stderr, "%s\n", tierWizardPreview(primary, c))

	// Plan save: Enter reuses the last saved plan name when there is one
	// (plans.json last_used), blank only when none exists. Reaching this
	// prompt means the user walked the tiers — i.e. they did NOT take the
	// reuse step at the top — so the default is only ever accepted by someone
	// who read it, and the wording says plainly that it REPLACES that plan's
	// tiers rather than adding one ("enter = dev" read like a save-as).
	last, _ := PlanLastUsed()
	prompt := "Save as plan (name, blank = skip)"
	defaultName := ""
	if last != "" {
		defaultName = last
		prompt += ", enter = overwrite " + last
	}
	name, err := tierWizardReadLine(prompt + ": ")
	if err != nil && name == "" {
		return c, err
	}
	if name == "" {
		name = defaultName
	}
	if name != "" {
		desc := "interactive launch wizard"
		if err := PlanSet(name, TierPlanProfile{
			Model:         primary,
			SonnetModel:   c.SonnetModel,
			HaikuModel:    c.HaikuModel,
			OversizeModel: c.OversizeModel,
			RoutePolicy:   c.RoutePolicy,
			Description:   desc,
		}); err != nil {
			// A warning, not a failure: every tier question has been answered
			// by now, and a plans.json we cannot write (corrupt, or read-only
			// home) must not throw the launch away — the same forgiveness the
			// plan-reuse step promises for the read side.
			fmt.Fprintf(os.Stderr, "warning: could not save plan %q (%v) — continuing with the answers above\n", name, err)
			return c, nil
		}
		c.PlanName = name
		fmt.Fprintf(os.Stderr, "saved plan %q — reuse with `oaica launch claude --plan %s`\n", name, name)
	}
	return c, nil
}

// tierWizardPreview is the one-line summary printed before the save prompt,
// e.g. `fallback: a <-> b · oversize: c (256k) · policy: auto`.
func tierWizardPreview(primary string, c tierWizardChoice) string {
	line := "fallback: " + primary
	if c.SonnetModel != "" {
		line += " <-> " + c.SonnetModel
	}
	if c.HaikuModel != "" {
		line += " · haiku: " + c.HaikuModel
	}
	if c.OversizeModel != "" {
		line += " · oversize: " + c.OversizeModel
		if w := probedModelWindow(c.OversizeModel); w > 0 {
			line += fmt.Sprintf(" (%dk)", w/1024)
		}
	}
	policy := c.RoutePolicy
	if policy == "" {
		policy = string(RouteAuto)
	}
	return line + " · policy: " + policy
}

// claudeResumeHint turns Claude Code's bare "No conversation found with
// session ID" exit into an actionable one-liner: point at the directory the
// session transcript actually lives in (Claude sessions are per-cwd), or
// suggest --continue when the id is unknown anywhere on this machine.
// Best-effort; never masks the child's own error output.
func claudeResumeHint(err error, args []string) {
	if err == nil {
		return
	}
	if ee, ok := err.(*exec.ExitError); !ok || ee.ExitCode() != 1 {
		return
	}
	var resumeID string
	for i, a := range args {
		if a == "--resume" && i+1 < len(args) {
			resumeID = args[i+1]
		}
	}
	if resumeID == "" {
		return
	}
	if home, e := os.UserHomeDir(); e == nil {
		matches, _ := filepath.Glob(filepath.Join(home, ".claude", "projects", "*", resumeID+".jsonl"))
		if len(matches) > 0 {
			dir := filepath.Dir(matches[0])
			fmt.Fprintf(os.Stderr, "\nresume: session %s lives in %s — launch claude from that directory (or use --continue there)\n", resumeID, dir)
			return
		}
	}
	fmt.Fprintf(os.Stderr, "\nresume: no session %s exists for this directory — use --continue to resume the latest one here\n", resumeID)
}
