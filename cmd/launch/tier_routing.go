package launch

// tier_routing.go — one Claude Code launch, any number of backends.
//
// Claude Code picks a model *tier* per request (Opus for plan mode under
// /model opusplan, Sonnet for execution and subagents, Haiku for quick
// background calls) and resolves each tier through the
// ANTHROPIC_DEFAULT_*_MODEL env vars; every request then carries that model
// id. Before this file, `oaica launch claude` could only split tiers across
// two models on the SAME user remote (one proxy = one base URL + key), and
// router/daemon models bypassed the translation proxy entirely and were
// pointed straight at a host that had to speak /v1/messages -- which the
// public gateway does not, so a fresh install's `launch claude --model
// kat-awq` died with "unrecognized_model" (2026-08-26).
//
// Now every picker model -- user remote, OAICA router, `oaica serve`
// (":local"), or the local Ollama daemon (pulled or ":cloud") -- resolves to
// an OpenAI-compatible endpoint, and ONE translation proxy carries a routing
// table keyed by the model id Claude Code sends. Primary and --sonnet-model
// may therefore live on different remotes, one local and one cloud, etc.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/ollama/ollama/envconfig"
)

type endpointSource string

const (
	sourceUserRemote endpointSource = "remote"
	sourceRouter     endpointSource = "router"
	sourceLocalServe endpointSource = "local-serve"
	sourceDaemon     endpointSource = "daemon"
)

// launchEndpoint is where one picker model actually lives.
type launchEndpoint struct {
	RemoteEndpoint
	Source endpointSource
}

// daemonHasModel is a package var so tests can stub the local daemon probe.
var daemonHasModel = daemonHasModelLive

// daemonProbeCache memoizes daemonHasModelLive per (host, model) for
// daemonProbeTTL. resolveLaunchEndpoint asks the daemon once per resolved
// model; the oversize step resolves EVERY picker candidate and each miss
// would otherwise POST /api/show (3s timeout) serially. Tests stub the
// daemonHasModel var, so they never touch this.
var daemonProbeCache struct {
	sync.Mutex
	m map[string]daemonProbeResult
}

type daemonProbeResult struct {
	found     bool
	reachable bool
	expiresAt time.Time
}

const daemonProbeTTL = 5 * time.Minute

// daemonHasModelLive asks the local Ollama daemon (OLLAMA_HOST) whether it
// knows model, via POST /api/show -- the same call upstream's launcher made
// (client.Show). /api/tags is not enough: a ":cloud" alias the daemon
// proxies to ollama.com answers /api/show without appearing in tags.
// Returns (found, reachable).
func daemonHasModelLive(model string) (bool, bool) {
	host := envconfig.Host().String()
	key := host + "|" + model
	daemonProbeCache.Lock()
	if daemonProbeCache.m == nil {
		daemonProbeCache.m = map[string]daemonProbeResult{}
	}
	if r, ok := daemonProbeCache.m[key]; ok && time.Now().Before(r.expiresAt) {
		daemonProbeCache.Unlock()
		return r.found, r.reachable
	}
	daemonProbeCache.Unlock()

	found, reachable := daemonHasModelLiveUncached(model)
	daemonProbeCache.Lock()
	daemonProbeCache.m[key] = daemonProbeResult{found: found, reachable: reachable, expiresAt: time.Now().Add(daemonProbeTTL)}
	daemonProbeCache.Unlock()
	return found, reachable
}

func daemonHasModelLiveUncached(model string) (bool, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	body, _ := json.Marshal(map[string]string{"model": model})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(envconfig.Host().String(), "/")+"/api/show", bytes.NewReader(body))
	if err != nil {
		return false, false
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, false
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode == http.StatusOK, true
}

// hasSourcePrefix reports whether model carries one of the explicit source
// prefixes resolveLaunchEndpoint understands ("router/", "oaica/",
// "ollama/", "daemon/"). Callers outside this file use it to skip the
// local-pull path for such names.
func hasSourcePrefix(model string) bool {
	for _, p := range []string{"router/", "oaica/", "ollama/", "daemon/"} {
		if strings.HasPrefix(model, p) {
			return true
		}
	}
	return false
}

// resolveLaunchEndpoint maps a picker model name to the endpoint that serves
// it. Order: user remote ("<remote>/<id>" or a bare id exactly one remote
// serves) -> "<model>:local" (a running `oaica serve`) -> OAICA router ->
// local Ollama daemon. The error names every place that was tried.
func resolveLaunchEndpoint(model string) (launchEndpoint, error) {
	model = strings.TrimSpace(model)
	if model == "" {
		return launchEndpoint{}, errors.New("no model given")
	}
	// User-defined alias (~/.oaica/aliases.json) wins over everything else:
	// a user who explicitly aliased a short name wants exactly that target,
	// not a different thing that happens to share the bare id. See
	// model_alias.go's doc for why this exists (manual override that never
	// waits on discovery/refresh).
	if target, ok := resolveModelAlias(model); ok {
		model = target
	}
	if ep, ok := resolveRemoteEndpoint(model); ok {
		return launchEndpoint{RemoteEndpoint: ep, Source: sourceUserRemote}, nil
	}

	base, wasLocal := oaicaStripLocalTag(model)
	if wasLocal {
		for _, e := range oaicaLocalServerEntries() {
			if e.Model == base {
				return launchEndpoint{Source: sourceLocalServe, RemoteEndpoint: RemoteEndpoint{
					Name: "local", BaseURL: strings.TrimRight(e.Origin, "/") + "/v1", Token: e.APIKey,
					UpstreamModel: base, Wire: "openai", ToolFormat: "tool_calls", ToolReliable: true,
				}}, nil
			}
		}
		return launchEndpoint{}, fmt.Errorf("%q: no running `oaica serve` for %q (start it, or drop the :local tag)", model, base)
	}

	// Explicit source prefixes, for when a bare id is ambiguous or the user
	// wants to pin a tier to the router / daemon regardless of what else
	// serves that id. A user remote literally named "router" or "ollama"
	// still wins above (resolveRemoteEndpoint ran first).
	wantRouter, wantDaemon := false, false
	switch {
	case strings.HasPrefix(base, "router/"), strings.HasPrefix(base, "oaica/"):
		wantRouter, base = true, base[strings.Index(base, "/")+1:]
	case strings.HasPrefix(base, "ollama/"), strings.HasPrefix(base, "daemon/"):
		wantDaemon, base = true, base[strings.Index(base, "/")+1:]
	}

	// A bare id served by SEVERAL remotes: say so instead of silently
	// falling through to the router with a different credential.
	if !wantRouter && !wantDaemon && !strings.Contains(base, "/") {
		if owners := bareRemoteModelIndex()[base]; len(owners) > 1 {
			return launchEndpoint{}, fmt.Errorf("model %q is served by several remotes (%s): pick one as <remote>/<id>", base, strings.Join(owners, ", "))
		}
	}

	var tried []string
	if !wantDaemon {
		// Router: the model list is the readiness check; OAICA_HOST override
		// is honoured by oaicaLaunchHost(). "<model>+<lora>" composites are
		// router syntax and resolve through the same list by base id (see
		// oaicaModelIsReady); the full composite goes upstream.
		routerModels, routerErr := oaicaFetchCloudModelEntries()
		if routerErr == nil {
			routerBase := base
			if i := strings.Index(base, "+"); i >= 0 {
				routerBase = base[:i]
			}
			for _, m := range routerModels {
				if m.ID == routerBase {
					return launchEndpoint{Source: sourceRouter, RemoteEndpoint: RemoteEndpoint{
						Name: "oaica", BaseURL: oaicaLaunchHost() + "/v1", Token: oaicaLaunchAPIKeyForEnv(),
						UpstreamModel: base, Wire: "openai", ToolFormat: "tool_calls", ToolReliable: true,
					}}, nil
				}
			}
			tried = append(tried, fmt.Sprintf("not on %s", oaicaLaunchHost()))
		} else if isOaicaRouterAuthErr(routerErr) {
			tried = append(tried, fmt.Sprintf("%s rejected the API key — set OAICA_API_KEY or run `oaica signin`", oaicaLaunchHost()))
		} else {
			tried = append(tried, fmt.Sprintf("%s unavailable (%v)", oaicaLaunchHost(), routerErr))
		}
		if wantRouter {
			return launchEndpoint{}, fmt.Errorf("model %q: %s", model, strings.Join(tried, "; "))
		}
	}

	found, reachable := daemonHasModel(base)
	if found {
		return launchEndpoint{Source: sourceDaemon, RemoteEndpoint: RemoteEndpoint{
			Name: "ollama", BaseURL: strings.TrimRight(envconfig.Host().String(), "/") + "/v1", Token: "ollama",
			UpstreamModel: base, Wire: "openai", ToolFormat: "tool_calls", ToolReliable: true,
		}}, nil
	}
	if reachable {
		tried = append(tried, fmt.Sprintf("not pulled on the local daemon at %s", envconfig.Host()))
	} else {
		tried = append(tried, fmt.Sprintf("no local daemon at %s", envconfig.Host()))
	}
	if wantDaemon {
		return launchEndpoint{}, fmt.Errorf("model %q: %s", model, strings.Join(tried, "; "))
	}
	return launchEndpoint{}, fmt.Errorf("model %q not found: not a user remote (~/.oaica/remotes.json); %s", model, strings.Join(tried, "; "))
}

// tierPlan is everything a launch needs, computed without starting anything
// so it can be unit-tested: the proxy routing table, the model ids Claude
// Code will send per tier, and the env vars to set.
type tierPlan struct {
	PrimaryName   string // model id Claude Code sends for Opus/Haiku/--model
	SecondaryName string // model id for Sonnet/subagents (== PrimaryName without --sonnet-model)
	Primary       launchEndpoint
	Secondary     launchEndpoint
	Routes        proxyRouteTable
	// Real context windows probed from the upstreams' /models metadata
	// (context_window_remote.go); 0 = unknown, env stays unset.
	PrimaryContext   int
	SecondaryContext int
}

func routeFor(ep launchEndpoint) proxyRoute {
	return proxyRoute{BaseURL: ep.BaseURL, Key: ep.Token, KeyEnv: ep.TokenEnv, UpstreamModel: ep.UpstreamModel, Label: string(ep.Source) + ":" + ep.Name}
}

// resolveSecondaryEndpoint resolves --sonnet-model relative to the primary.
//
// When the primary is a user remote, an un-namespaced secondary means "on
// that same remote" -- the contract --sonnet-model always had ("muse-spark-1.2"
// on opencode-go, or an OpenRouter "vendor/id" the remote's /models did not
// enumerate). The 2026-08-26 review caught the first version of this file
// breaking that: an unlisted bare id failed with "not found", and an
// ambiguous one silently moved to the OAICA router with the OAICA key.
// Cross-source secondaries are still reachable, explicitly: "<remote>/<id>",
// "<model>:local", "router/<id>", "ollama/<id>".
//
// When the primary is not a user remote, the generic resolver applies.
func resolveSecondaryEndpoint(primary launchEndpoint, sonnetModel string) (launchEndpoint, error) {
	if primary.Source != sourceUserRemote {
		return resolveLaunchEndpoint(sonnetModel)
	}
	explicit := strings.HasSuffix(sonnetModel, oaicaLocalTagSuffix) ||
		strings.HasPrefix(sonnetModel, "router/") || strings.HasPrefix(sonnetModel, "oaica/") ||
		strings.HasPrefix(sonnetModel, "ollama/") || strings.HasPrefix(sonnetModel, "daemon/")
	if ep, ok := resolveRemoteEndpoint(sonnetModel); ok {
		// "<remote>/<id>" -- possibly a different remote; or a bare id that
		// exactly one remote serves, which must be the primary's remote to
		// count as "same remote" semantics... unless it IS the primary's.
		if !strings.Contains(sonnetModel, "/") && ep.Name != primary.Name {
			// bare id owned by another remote: ambiguous intent; keep the old
			// contract (primary's remote) unless the user namespaces it.
			return sameRemote(primary, sonnetModel), nil
		}
		return launchEndpoint{RemoteEndpoint: ep, Source: sourceUserRemote}, nil
	}
	if explicit {
		return resolveLaunchEndpoint(sonnetModel)
	}
	// Prefix with the primary's remote name in case the id is enumerated
	// there under the namespaced form; otherwise pass it through unchanged.
	if ep, ok := resolveRemoteEndpoint(primary.Name + "/" + sonnetModel); ok {
		return launchEndpoint{RemoteEndpoint: ep, Source: sourceUserRemote}, nil
	}
	return sameRemote(primary, sonnetModel), nil
}

// sameRemote is the primary's endpoint with a different upstream model id.
func sameRemote(primary launchEndpoint, upstreamModel string) launchEndpoint {
	ep := primary
	ep.UpstreamModel = upstreamModel
	return ep
}

// buildTierPlan resolves primary and optional secondary models and gates
// both for Anthropic-wire tool calling.
func buildTierPlan(model, sonnetModel string, forceTools bool) (tierPlan, error) {
	primary, err := resolveLaunchEndpoint(model)
	if err != nil {
		return tierPlan{}, err
	}
	if err := gateRemoteToolsEndpoint(primary.RemoteEndpoint, toolWireAnthropic, forceTools); err != nil {
		return tierPlan{}, err
	}
	plan := tierPlan{PrimaryName: model, SecondaryName: model, Primary: primary, Secondary: primary}
	plan.Routes = proxyRouteTable{
		Default: routeFor(primary),
		ByModel: map[string]proxyRoute{model: routeFor(primary)},
	}
	// The bare upstream id also routes to the primary, so a user who types
	// the id Claude Code shows (or a subagent config that does) still lands
	// on the right backend.
	if primary.UpstreamModel != model {
		plan.Routes.ByModel[primary.UpstreamModel] = routeFor(primary)
	}
	if sonnetModel != "" && sonnetModel != model {
		secondary, err := resolveSecondaryEndpoint(primary, sonnetModel)
		if err != nil {
			return tierPlan{}, fmt.Errorf("--sonnet-model: %w", err)
		}
		if err := gateRemoteToolsEndpoint(secondary.RemoteEndpoint, toolWireAnthropic, forceTools); err != nil {
			return tierPlan{}, fmt.Errorf("--sonnet-model: %w", err)
		}
		plan.SecondaryName = sonnetModel
		plan.Secondary = secondary
		plan.Routes.ByModel[sonnetModel] = routeFor(secondary)
		if _, taken := plan.Routes.ByModel[secondary.UpstreamModel]; !taken {
			plan.Routes.ByModel[secondary.UpstreamModel] = routeFor(secondary)
		}
	}
	// Route-policy fallback legs (route_policy.go): the OTHER legs of the
	// plan, deduped by base URL. A plan with both legs on one remote has
	// nothing to fall back onto — the URL is the failure domain — so a
	// plain `--sonnet-model muse-x` (same remote) builds no fallbacks and
	// behaves byte-identically to before.
	fallbacks := []proxyRoute{plan.Routes.Default}
	if plan.Secondary.Source != plan.Primary.Source || plan.Secondary.BaseURL != plan.Primary.BaseURL {
		fallbacks = append(fallbacks, routeFor(plan.Secondary))
	}
	seenURL := map[string]bool{}
	plan.Routes.Fallbacks = plan.Routes.Fallbacks[:0]
	for _, f := range fallbacks {
		if f.BaseURL != "" && !seenURL[f.BaseURL] {
			seenURL[f.BaseURL] = true
			plan.Routes.Fallbacks = append(plan.Routes.Fallbacks, f)
		}
	}
	return plan, nil
}

// envVars is the Claude Code environment for a plan. ANTHROPIC_AUTH_TOKEN is
// the per-launch proxy token (see proxyRouteTable.ClientToken): the proxy
// attaches each route's real key upstream, so no real key enters the child
// environment (where Claude Code's Bash tool could print it).
func (p tierPlan) envVars(anthropicBaseURL, clientToken string) []string {
	token := clientToken
	if token == "" {
		token = "oaica-local"
	}
	env := []string{
		"ANTHROPIC_BASE_URL=" + anthropicBaseURL,
		"ANTHROPIC_API_KEY=",
		"ANTHROPIC_AUTH_TOKEN=" + token,
		"CLAUDE_CODE_ATTRIBUTION_HEADER=0",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_FEEDBACK_COMMAND=1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=" + p.PrimaryName,
		"ANTHROPIC_DEFAULT_SONNET_MODEL=" + p.SecondaryName,
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + p.PrimaryName,
		"CLAUDE_CODE_SUBAGENT_MODEL=" + p.SecondaryName,
		// See modelEnvVars: Auto mode would address model ids no backend of
		// ours has.
		"CLAUDE_CODE_ENABLE_AUTO_MODE=0",
		"CLAUDE_CODE_AUTO_MODE_MODEL=" + p.PrimaryName,
		// Long-prefill backends (262k ctx on vLLM) can take minutes to first
		// token on big system prompts. Claude Code's default request timeout
		// aborts and retries, showing "Waiting for API response · will retry
		// in Nm · check your network" churn. Give it room instead.
		"API_TIMEOUT_MS=600000",
	}
	if isCloudModelName(p.PrimaryName) {
		if l, ok := lookupCloudModelLimit(p.PrimaryName); ok {
			// Full window (no reserved-output subtraction): the cloud
			// alias path always used the raw number and the probed path
			// below applies its own reserve. MAX_CONTEXT_TOKENS added too
			// — it also silences Claude Code's "unknown model, assuming
			// 200k" warning on unmatched ids (e.g. "glm-5.3-flash:cloud"),
			// which AUTO_COMPACT_WINDOW alone never did.
			v := strconv.Itoa(l.Context)
			env = append(env,
				"CLAUDE_CODE_MAX_CONTEXT_TOKENS="+v,
				"CLAUDE_CODE_AUTO_COMPACT_WINDOW="+v,
			)
		}
	}
	// Probed upstream windows (kat-awq on vLLM: 262144) — this must not
	// run for the cloud path twice.
	if p.PrimaryContext > 0 && !isCloudModelName(p.PrimaryName) {
		env = append(env, p.contextEnvVars()...)
	}
	return env
}

// Run launches Claude Code against the plan: one local translation proxy,
// routing per request model id.
func (c *Claude) Run(model string, models []LaunchModel, args []string) error {
	// "claude/<tier>" picker entries take the native path: the REAL Claude
	// Code binary, clean env, Anthropic's own auth — no OAICA proxy.
	if tier, ok := nativeClaudeModelTier(model); ok {
		return c.runNative(tier, args)
	}
	forceTools, args := extractForceTools(args)
	sonnetModel, args := extractSonnetModel(args)
	planName, args := extractPlanFlag(args)
	briefMode, args := extractBriefMode(args)
	policyArg, args := extractRoutePolicy(args)
	oversizeModel, args := extractOversizeModel(args)
	// --wizard forces steps 2-4 even when the eligibility gate would skip
	// them (a --model launch, mainly); it cannot rescue a non-interactive
	// session, where the selectors have nowhere to draw.
	wizardForced, args := extractWizardFlag(args)
	if wizardForced && !isInteractiveSession() {
		return fmt.Errorf("launch wizard: --wizard requires an interactive session")
	}

	// "plan/<name>" picker entries enter here as the "model": unwrap to the
	// plan name and let the plan supply the primary model.
	if picked, ok := strings.CutPrefix(model, planPickerPrefix); ok && planName == "" {
		planName, model = picked, ""
	}
	if planName != "" {
		resolvedModel, resolvedSonnet, err := resolvePlanModels(planName, model, sonnetModel)
		if err != nil {
			return fmt.Errorf("--plan: %w", err)
		}
		model, sonnetModel = resolvedModel, resolvedSonnet
		// Flags > plan > remotes.json route_policy: the plan only fills what
		// the flags left empty (tier_plan_profiles.go).
		policyArg, oversizeModel, err = resolvePlanTier(planName, policyArg, oversizeModel)
		if err != nil {
			return fmt.Errorf("--plan: %w", err)
		}
	} else if wizardForced || tierWizardEligible(args) {
		w, err := runTierWizard(models, model)
		if err != nil {
			return fmt.Errorf("launch wizard: %w", err)
		}
		sonnetModel = w.SonnetModel
		oversizeModel = w.OversizeModel
		policyArg = w.RoutePolicy
	}
	if briefMode {
		// Claude Code's own flag, not a bespoke mechanism — see
		// briefModeSystemPrompt's doc for why this exact wording and why
		// not "--compact".
		args = append(args, "--append-system-prompt", briefModeSystemPrompt)
	}

	claudePath, err := ensureClaudeInstalled()
	if err != nil {
		return err
	}

	plan, err := buildTierPlan(model, sonnetModel, forceTools)
	if err != nil {
		return err
	}
	// Policy precedence: --route-policy flag > primary remote's
	// route_policy (remotes.json) > local-first (parseRoutePolicy default).
	if policyArg == "" {
		policyArg = plan.Primary.RoutePolicy
	}
	policy, err := parseRoutePolicy(policyArg)
	if err != nil {
		return fmt.Errorf("route_policy %q (remotes.json or --route-policy) is not one of local-first, remote-first, auto, local-only, remote-only", policyArg)
	}
	plan.Routes.Policy = policy
	if oversizeModel != "" {
		over, err := resolveLaunchEndpoint(oversizeModel)
		if err != nil {
			return fmt.Errorf("--oversize: %w", err)
		}
		if over.BaseURL == plan.Primary.BaseURL {
			return fmt.Errorf("--oversize %q resolves to the same backend as the primary (%s): the oversize leg exists to serve requests the primary cannot hold, so it must be a different base URL (a larger-context remote)", oversizeModel, over.BaseURL)
		}
		if err := gateRemoteToolsEndpoint(over.RemoteEndpoint, toolWireAnthropic, forceTools); err != nil {
			return fmt.Errorf("--oversize: %w", err)
		}
		plan.Routes.Oversize = routeFor(over)
		// The oversize leg is a full route leg: it can also serve as the
		// breaker fallback for the other legs (and it gets health-probed).
		urlSeen := map[string]bool{}
		for _, f := range plan.Routes.Fallbacks {
			urlSeen[f.BaseURL] = true
		}
		if !urlSeen[plan.Routes.Oversize.BaseURL] {
			plan.Routes.Fallbacks = append(plan.Routes.Fallbacks, plan.Routes.Oversize)
		}
	}
	// Real context window from the upstreams' /models metadata (claude.go's
	// cloud-alias map covers :cloud; this covers user remotes and the
	// router). Probed here, not in buildTierPlan: unit tests build plans
	// against fake/unroutable URLs and must not wait on network.
	if s := plan.Primary.Source; s == sourceUserRemote || s == sourceRouter {
		plan.withContextWindows().applyContextWindowsToRoutes()
	}
	clientToken, err := newProxyClientToken()
	if err != nil {
		return fmt.Errorf("proxy token: %w", err)
	}
	plan.Routes.ClientToken = clientToken

	// One session ID per launch (not per request — see SessionID's doc):
	// lets a session-hash-aware LB in front of the backend (e.g. oaicalb's
	// :8091) pin this whole conversation to one replica, so its own prefix
	// cache actually gets reused turn-to-turn instead of scattering across
	// replicas under plain leastconn. A backend with no such LB just sees
	// (and ignores) an extra header.
	sessionID, err := newProxySessionID()
	if err != nil {
		return fmt.Errorf("proxy session id: %w", err)
	}
	plan.Routes.SessionID = sessionID

	ln, port, err := ListenAnthropicOpenAIProxy(userRemote{}, "")
	if err != nil {
		// Never fall back to pointing Claude Code at a backend directly: none
		// of them speak /v1/messages, the failure would be a confusing 404.
		return fmt.Errorf("failed to start translation proxy: %w", err)
	}
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, plan.Routes) }()
	anthropicBaseURL := fmt.Sprintf("http://127.0.0.1:%d", port)

	if plan.SecondaryName != plan.PrimaryName {
		fmt.Fprintf(os.Stderr, "tiers: opus/haiku -> %s (%s)  sonnet/subagents -> %s (%s)\n",
			plan.PrimaryName, plan.Primary.Source, plan.SecondaryName, plan.Secondary.Source)
	}
	if len(plan.Routes.Fallbacks) > 1 {
		fmt.Fprintf(os.Stderr, "route policy: %s (fallback legs: %d)\n", policy, len(plan.Routes.Fallbacks)-1)
	}
	if plan.Routes.Oversize.BaseURL != "" {
		fmt.Fprintf(os.Stderr, "oversize: >%dk-token requests -> %s (%s)\n",
			plan.PrimaryContext/1024, plan.Routes.Oversize.Label, plan.Routes.Oversize.UpstreamModel)
	}

	// Default model mode: with a tier split configured, launch straight
	// into opusplan — Claude Code plans on the opus tier (the primary) and
	// executes on the sonnet tier (the secondary), no /model opusplan
	// needed after every relaunch. An explicit --model in the user's args
	// wins (c.args skips ours), and a single-model launch (no secondary)
	// keeps pinning the primary as before.
	claudeModel := plan.PrimaryName
	if plan.SecondaryName != plan.PrimaryName && !hasClaudeModelFlag(args) {
		claudeModel = "opusplan"
		fmt.Fprintf(os.Stderr, "model mode: opusplan (plan with %s, execute with %s)\n",
			plan.PrimaryName, plan.SecondaryName)
	}

	cmd := exec.Command(claudePath, c.args(claudeModel, args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = append(os.Environ(), plan.envVars(anthropicBaseURL, clientToken)...)
	if err := cmd.Run(); err != nil {
		claudeResumeHint(err, args)
		return err
	}
	return nil
}
