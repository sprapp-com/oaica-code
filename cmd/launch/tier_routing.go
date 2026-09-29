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
	sourceUserRemote      endpointSource = "remote"
	sourceRouter          endpointSource = "router"
	sourceLocalServe      endpointSource = "local-serve"
	sourceDaemon          endpointSource = "daemon"
	sourceNativeAnthropic endpointSource = "native-anthropic"
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
	host := envconfig.ConnectableHost().String()
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
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(envconfig.ConnectableHost().String(), "/")+"/api/show", bytes.NewReader(body))
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

// flagPassed reports whether args carries name as a flag, in either spelling
// ("--x", "--x=…"), including one with an empty value. Run's extractors strip
// a flag's value and cannot say afterwards whether they ever saw it, which is
// all the difference between "the caller asked for nothing" and "the caller
// said nothing" (2026-09-26 audit).
func flagPassed(args []string, name string) bool {
	for _, a := range args {
		if a == name || strings.HasPrefix(a, name+"=") {
			return true
		}
	}
	return false
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
// oaicaGatewayTokenEnv names the environment variable the gateway credential
// comes from, for the child-environment scrubber (see credentialEnvNames). The
// variable is never read anywhere else.
const oaicaGatewayTokenEnv = "OAICA_GATEWAY_TOKEN"

// oaicaGatewayURLOverride is OAICA_GATEWAY_URL, without its trailing slash: the
// base every first-party model resolves to when it is set.
func oaicaGatewayURLOverride() string {
	return strings.TrimRight(strings.TrimSpace(os.Getenv("OAICA_GATEWAY_URL")), "/")
}

// oaicaGatewayTokenOverride is the gateway credential, if the host configured
// one. Empty is the common case: a gateway on the same box usually runs with an
// empty api_keys list, which is open by design.
func oaicaGatewayTokenOverride() string {
	return strings.TrimSpace(os.Getenv(oaicaGatewayTokenEnv))
}

// isFirstPartyGatewayModel reports whether a model id is one of oaica's own
// SKUs — a bare "oaica-*" id, or the same id written with the explicit
// "router/"/"oaica/" source prefix. Everything else keeps resolving through the
// ordinary chain, so the override can never hijack somebody else's model.
func isFirstPartyGatewayModel(model string) bool {
	if isBareRouterSKU(model) {
		return true
	}
	return strings.HasPrefix(model, "router/") || strings.HasPrefix(model, "oaica/")
}

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
	// claude/*, anthropic/* — native Anthropic, forwarded raw to
	// api.anthropic.com by nativeAnthropicPassthrough
	// (anthropic_openai_proxy.go). Checked before the user-remote lookup so
	// a remote literally named "claude" or "anthropic" can never shadow
	// this reserved prefix. UpstreamModel carries the Claude Code --model
	// alias (opus/sonnet/fable/...); BaseURL/Token are unused for this
	// source (see proxyRoute.NativePassthrough).
	if tier, ok := nativeClaudeModelTier(model); ok {
		return launchEndpoint{Source: sourceNativeAnthropic, RemoteEndpoint: RemoteEndpoint{
			Name: "native-anthropic", UpstreamModel: tier, Wire: "anthropic", ToolFormat: "tool_calls", ToolReliable: true,
		}}, nil
	}
	// A bare "oaica-*" id is the router's own SKU: opencode zen mirrors our
	// SKUs in its /models, so the single-owner bare-id match inside
	// resolveRemoteEndpoint hijacked the primary leg onto zen with zen's key
	// — 401 "Model ... is not supported" while the OAICA key sat unused.
	// resolveSecondaryEndpoint and ResolveAgentModelWithOpts already carry
	// this guard (see isBareRouterSKU's doc); the primary slot, the one leg
	// every launch resolves, did not. PREFIX-only and bare-only: an explicit
	// "<remote>/<id>" or "router/<id>" is untouched, so a remote can still
	// name any id it likes except the reserved router prefix.
	if !isBareRouterSKU(model) {
		if ep, ok := resolveRemoteEndpoint(model); ok {
			return launchEndpoint{RemoteEndpoint: ep, Source: sourceUserRemote}, nil
		}
	}

	// OAICA_GATEWAY_URL points oaica's own models at one gateway. The gateway's
	// port differs per machine, so a first-party SKU cannot carry a fixed
	// endpoint; this is how a host says where its gateway is. It is checked
	// here — after the alias, the native Claude prefix, the user-remote lookup
	// and the ":local" servers — so it redirects our own models without
	// reordering anything else: a user's remote still beats it for that
	// remote's ids, an alias still beats everything, and a `<model>:local`
	// entry still means the box that is serving it.
	//
	// OAICA_GATEWAY_TOKEN carries the credential when the gateway requires one
	// (its config's api_keys); TokenEnv is set from it so the scrubber keeps it
	// out of the agent's environment, the same rule the router leg follows.
	if gw := oaicaGatewayURLOverride(); gw != "" && isFirstPartyGatewayModel(model) {
		upstream := strings.TrimPrefix(strings.TrimPrefix(model, "router/"), "oaica/")
		ep := RemoteEndpoint{
			Name: "oaica-gateway", BaseURL: gw + "/v1", Token: oaicaGatewayTokenOverride(),
			UpstreamModel: upstream, Wire: "openai", ToolFormat: "tool_calls", ToolReliable: true,
		}
		if ep.Token != "" {
			ep.TokenEnv = oaicaGatewayTokenEnv
		}
		return launchEndpoint{Source: sourceRouter, RemoteEndpoint: ep}, nil
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
						// TokenEnv, so the scrubber knows this leg's credential is a REAL one
						// (childEnv removes it from the agent's environment — see
						// credentialEnvNames). Without it OAICA_API_KEY was the single
						// credential a router launch spends that still reached the child, where
						// the agent's Bash tool reads it and spends the user's account
						// (2026-09-26 audit). oaicaLaunchAPIKeyForEnv also falls back to
						// ~/.oaica/api_key and to the OAICA_HOST userinfo, neither of which is an
						// environment variable — those never enter a child's environment to
						// begin with, so the env name is the whole set.
						TokenEnv:      "OAICA_API_KEY",
						UpstreamModel: base, Wire: "openai", ToolFormat: "tool_calls", ToolReliable: true,
					}}, nil
				}
			}
			tried = append(tried, fmt.Sprintf("not on %s", redactBaseURL(oaicaLaunchHost())))
		} else if isOaicaRouterAuthErr(routerErr) {
			tried = append(tried, fmt.Sprintf("%s rejected the API key — set OAICA_API_KEY or run `oaica signin`", redactBaseURL(oaicaLaunchHost())))
		} else {
			tried = append(tried, fmt.Sprintf("%s unavailable (%v)", redactBaseURL(oaicaLaunchHost()), redactErr(routerErr)))
		}
		if wantRouter {
			return launchEndpoint{}, fmt.Errorf("model %q: %s", model, strings.Join(tried, "; "))
		}
	}

	found, reachable := daemonHasModel(base)
	if found {
		return daemonEndpoint(base), nil
	}
	if reachable {
		tried = append(tried, fmt.Sprintf("not pulled on the local daemon at %s", redactBaseURL(envconfig.Host().String())))
	} else {
		tried = append(tried, fmt.Sprintf("no local daemon at %s", redactBaseURL(envconfig.Host().String())))
	}
	if wantDaemon {
		return launchEndpoint{}, fmt.Errorf("model %q: %s", model, strings.Join(tried, "; "))
	}
	return launchEndpoint{}, fmt.Errorf("model %q not found: not a user remote (~/.oaica/remotes.json); %s", model, strings.Join(tried, "; "))
}

// tierValueFlags are the launcher-level flags whose value slot holds a value,
// never another flag. A flag in that slot is a missing value: the extractors
// take whatever token follows, so `--sonnet-model --wizard` pinned the sonnet
// tier to the literal string "--wizard" AND consumed the wizard flag — the
// wizard never ran, nothing warned, and the bogus id was exported to the
// child, where the first subagent request failed against a model of that name
// (2026-09-26 audit). No model id, plan name, policy or shard spec begins with
// "--", so this is always the caller's mistake, refused by name like the empty
// values below.
var tierValueFlags = []string{
	"--sonnet-model", "--haiku-model", "--oversize", "--plan", "--route-policy", "--shard",
}

// flagSwallowedValue reports the token consumed as name's value when that
// token is itself a flag, for the space spelling ("--name --other"). The
// "--name=--other" spelling is the caller typing that string as the value
// deliberately, and is left to the flag's own validation.
func flagSwallowedValue(args []string, name string) string {
	for i, a := range args {
		if a == name && i+1 < len(args) && strings.HasPrefix(args[i+1], "--") {
			return args[i+1]
		}
	}
	return ""
}

// oversizeNoneValue is the --oversize value that means "no oversize leg",
// which is how a plan's stored leg is dropped for one launch. It is spelled
// because no model id can be: an empty value is what an unset shell variable
// produces, and reading THAT as a deliberate drop would silently remove a leg
// a user meant to keep.
const oversizeNoneValue = "none"

// daemonEndpoint is the local Ollama daemon's OpenAI-compatible endpoint for
// one of its own model ids. One definition, so the primary slot and a tier
// slot cannot disagree about which host or which credential the daemon means.
func daemonEndpoint(model string) launchEndpoint {
	return launchEndpoint{Source: sourceDaemon, RemoteEndpoint: RemoteEndpoint{
		Name: "ollama", BaseURL: strings.TrimRight(envconfig.ConnectableHost().String(), "/") + "/v1", Token: "ollama",
		UpstreamModel: model, Wire: "openai", ToolFormat: "tool_calls", ToolReliable: true,
	}}
}

// tierPlan is everything a launch needs, computed without starting anything
// so it can be unit-tested: the proxy routing table, the model ids Claude
// Code will send per tier, and the env vars to set.
type tierPlan struct {
	PrimaryName   string // model id Claude Code sends for Opus/--model (and Haiku, without --haiku-model)
	SecondaryName string // model id for Sonnet/subagents (== PrimaryName without --sonnet-model)
	HaikuName     string // model id for Haiku-tier requests (== PrimaryName without --haiku-model)
	Primary       launchEndpoint
	Secondary     launchEndpoint
	Haiku         launchEndpoint
	Routes        proxyRouteTable
	// Real context windows probed from the upstreams' /models metadata
	// (context_window_remote.go); 0 = unknown, env stays unset.
	PrimaryContext   int
	SecondaryContext int
	HaikuContext     int
}

func routeFor(ep launchEndpoint) proxyRoute {
	// Wire decides the passthrough: a user remote declaring wire "anthropic"
	// (z.ai's coding plan, MiniMax's /anthropic, ...) speaks the same wire the
	// proxy receives, so it is forwarded untranslated to its OWN /v1/messages
	// from BaseURL with its own key — the same path a native claude/* tier
	// takes to api.anthropic.com, minus the native credential
	// (anthropicPassthroughTarget picks the upstream). Without this, the
	// translation path POSTs <base>/chat/completions at an Anthropic-shaped
	// endpoint and the vendor answers 404 ("502 upstream HTTP 404" in Claude
	// Code) — the failure that shipped as zai-coding-plan, 2026-09-25.
	return proxyRoute{
		BaseURL: ep.BaseURL, Key: ep.Token, KeyEnv: ep.TokenEnv, APIKeyEnv: ep.APIKeyEnv, ModelsURL: ep.ModelsURL, UpstreamModel: ep.UpstreamModel,
		Label:             string(ep.Source) + ":" + ep.Name,
		Wire:              ep.Wire,
		NativePassthrough: ep.Source == sourceNativeAnthropic || ep.Wire == "anthropic",
		Weight:            ep.Weight,
	}
}

// nativeSonnetDisplayModel is the id Claude Code's own restore validation
// currently accepts for the sonnet slot (observed directly: "Session model
// oaica-35b-a3b-vision could not be restored ... using claude-sonnet-5
// instead", 2026-09-02) with oaicaDisplayModelSuffix appended so the label
// stays distinguishable from Anthropic's real Sonnet in a transcript a
// human reads later — see proxyRoute.DisplayModel's doc for why this
// exists at all and why the distinction matters despite nothing being
// presented to Anthropic itself.
const nativeSonnetDisplayModel = "claude-sonnet-5" + oaicaDisplayModelSuffix

// routeForDisguised is routeFor, but sets DisplayModel when primary is
// native Anthropic and leg is not (see DisplayModel's doc: only THIS
// combination hits Claude Code's real session-restore validation, because
// only a native primary owns real Claude Code session persistence — every
// other primary re-injects env vars fresh each launch and Claude Code's own
// stale restored id never matters).
func routeForDisguised(primary, leg launchEndpoint) proxyRoute {
	r := routeFor(leg)
	if primary.Source == sourceNativeAnthropic && leg.Source != sourceNativeAnthropic {
		r.DisplayModel = nativeSonnetDisplayModel
	}
	return r
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
	// Trimmed here, like resolveLaunchEndpoint trims the primary and
	// agent_routing.go's flag extraction trims the tier flags. A padded value
	// matched no remote namespace, no alias, no ":local" suffix and no native
	// tier, so it fell through to the "on the primary's remote" contract and
	// ran the tier on the PRIMARY's host with the PRIMARY's credential, under a
	// model id no backend serves — the user asked for another remote's model
	// and silently got their primary's backend (2026-09-26 audit, tenth round).
	sonnetModel = strings.TrimSpace(sonnetModel)
	// A user alias wins in a tier slot exactly as it does in the primary slot
	// (resolveLaunchEndpoint resolves it first, before every other source).
	// Without this the alias NAME was treated as a literal upstream id and, at
	// the bottom of this function, prefixed with the primary's remote — so
	// `--sonnet-model fast` where fast aliases "zai/glm-air" ran the sonnet
	// tier on the PRIMARY's host with the PRIMARY's credential and never
	// reached the model the user aliased, while the same name in the primary
	// slot went to zai (2026-09-26 audit).
	if target, ok := resolveModelAlias(sonnetModel); ok {
		sonnetModel = target
	}
	if primary.Source != sourceUserRemote {
		// OAICA router SKUs resolve to the router even when a user remote
		// mirrors the bare id in its own /models (opencode zen proxies our
		// SKUs): resolveLaunchEndpoint's bare-name fallback found exactly one
		// remote advertising "oaica-35b-a3b-vision" and sent the sonnet tier
		// there, which 401'd "Model ... is not supported" (2026-09-01 fleet).
		// The router is the authority on its own ids — a bare id on the
		// router catalog always routes there; a bare NON-router id keeps the
		// generic path (daemon / :local / single-owner remote).
		if oaicaRouterSKU(sonnetModel) {
			return resolveLaunchEndpoint("router/" + sonnetModel)
		}
		return resolveLaunchEndpoint(sonnetModel)
	}
	// A bare "oaica-*" id is the router's own SKU even when the primary is
	// a user remote: opencode zen mirrors our SKUs in its /models, so the
	// "un-namespaced secondary = on the primary's remote" contract sent the
	// sonnet tier to zen with zen's key → 401 "Model oaica-35b-a3b-vision is
	// not supported" (2026-09-02 .46 fleet, plan myplan: primary
	// glm-5.3-flash on zen). PREFIX-only: a catalog match must not hijack a
	// bare non-prefixed id away from the primary's remote (the contract
	// below still governs those). Explicit cross-provider forms
	// ("<remote>/<id>", "router/<id>", ...) still win further down.
	if isBareRouterSKU(sonnetModel) {
		return resolveLaunchEndpoint("router/" + sonnetModel)
	}
	// A bare Claude tier name ("claude/sonnet", "anthropic/opus") is a native
	// tier, never a model id any remote serves: Claude Code's own tier words
	// are reserved here, so nothing is taken from a remote. Checked BEFORE the
	// remote lookups because the primary's-remote fallback below accepts ANY
	// "<primary>/<id>" — the literal string "claude/sonnet" included, which is
	// not a model name at all. Without this the id was handed to the remote as
	// an upstream model and the tier the user asked for silently became a bad
	// model name (found 2026-09-25 while fixing the bare-alias env bug; a
	// native PRIMARY escaped it because its legs never reach this branch).
	if tier, ok := nativeClaudeModelTier(sonnetModel); ok && isBareClaudeTier(tier) {
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
	// A local model id can contain "/" and never carries the "daemon/" prefix:
	// `oaica pull` records the publisher's id verbatim ("hf.co/Qwen/Qwen3-8B"),
	// and the registry serves "library/…". The primary resolves such a string
	// through resolveLaunchEndpoint's daemon fallback, so a tier slot must
	// resolve it the same way — otherwise one spelling names a local model in
	// the primary slot and an id on the primary's remote in the
	// --sonnet-model slot, with that remote's credential, and subagent traffic
	// leaves the machine the weights were pulled onto (2026-09-26 audit).
	//
	// An exact local match beats the guess below, which forwards the id to a
	// remote that does not enumerate it. The remote reading is not lost: it
	// stays reachable by naming it, "<primary>/<id>". Bare ids are not probed
	// here — the un-namespaced contract above is deliberate and a bare id is
	// never a daemon-only spelling (TestSecondary_BareIDNeverSilentlyLeaves
	// PrimaryRemote pins that).
	if strings.Contains(sonnetModel, "/") {
		if found, _ := daemonHasModel(sonnetModel); found {
			return daemonEndpoint(sonnetModel), nil
		}
	}
	// Prefix with the primary's remote name in case the id is enumerated
	// there under the namespaced form; otherwise pass it through unchanged.
	// This is the un-namespaced contract, and for a user-remote primary it
	// always matches (findUserRemoteForModel splits at the first "/" and finds
	// the primary by name), which is why no native-tier fallback follows it:
	// a primary that is NOT a user remote already returned the native endpoint
	// at the top of this function, and every other spelling was handled above.
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

// buildTierPlan resolves primary and optional secondary/haiku models and
// gates all three for Anthropic-wire tool calling.
func buildTierPlan(model, sonnetModel, haikuModel string, forceTools bool) (tierPlan, error) {
	primary, err := resolveLaunchEndpoint(model)
	if err != nil {
		return tierPlan{}, err
	}
	if err := gateRemoteToolsEndpoint(primary.RemoteEndpoint, toolWireAnthropic, forceTools); err != nil {
		return tierPlan{}, err
	}
	plan := tierPlan{PrimaryName: model, SecondaryName: model, HaikuName: model, Primary: primary, Secondary: primary, Haiku: primary}
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
		plan.Routes.ByModel[sonnetModel] = routeForDisguised(primary, secondary)
		if _, taken := plan.Routes.ByModel[secondary.UpstreamModel]; !taken {
			plan.Routes.ByModel[secondary.UpstreamModel] = routeForDisguised(primary, secondary)
		}
	}
	if haikuModel != "" && haikuModel != model {
		// Same "un-namespaced = on the primary's remote, unless it's a bare
		// router SKU" contract as --sonnet-model: resolveSecondaryEndpoint's
		// logic doesn't depend on the tier name, only on primary + the
		// requested id.
		haiku, err := resolveSecondaryEndpoint(primary, haikuModel)
		if err != nil {
			return tierPlan{}, fmt.Errorf("--haiku-model: %w", err)
		}
		if err := gateRemoteToolsEndpoint(haiku.RemoteEndpoint, toolWireAnthropic, forceTools); err != nil {
			return tierPlan{}, fmt.Errorf("--haiku-model: %w", err)
		}
		plan.HaikuName = haikuModel
		plan.Haiku = haiku
		if _, taken := plan.Routes.ByModel[haikuModel]; !taken {
			plan.Routes.ByModel[haikuModel] = routeForDisguised(primary, haiku)
		}
		if _, taken := plan.Routes.ByModel[haiku.UpstreamModel]; !taken {
			plan.Routes.ByModel[haiku.UpstreamModel] = routeForDisguised(primary, haiku)
		}
	}
	// Tier fidelity: Claude Code's opusplan mode resolves its opus and haiku
	// slots from its OWN built-in catalog, ignoring ANTHROPIC_DEFAULT_OPUS_MODEL
	// and ANTHROPIC_DEFAULT_HAIKU_MODEL entirely (probed 2026-09-25: both were
	// set to sentinel ids and only the SONNET slot's value ever reached the
	// wire). The request then arrives carrying a real Anthropic id
	// ("claude-haiku-4-5-20251001") that means nothing to a local/remote leg.
	// Registering the plan's legs by FAMILY is what keeps a tier split honest
	// in that mode: the haiku slot reaches the haiku leg the user configured
	// instead of silently falling to the primary's model — on a claude/opus
	// primary that was every background call (title generation, topic
	// detection) billed at Opus rates.
	plan.Routes.FamilyLegs = tierFamilyRoutes(plan)

	// Route-policy fallback legs (route_policy.go): the OTHER legs of the
	// plan, deduped by base URL. A plan with both legs on one remote has
	// nothing to fall back onto — the URL is the failure domain — so a
	// plain `--sonnet-model muse-x` (same remote) builds no fallbacks and
	// behaves byte-identically to before.
	fallbacks := []proxyRoute{plan.Routes.Default}
	if plan.Secondary.Source != plan.Primary.Source || plan.Secondary.BaseURL != plan.Primary.BaseURL {
		fallbacks = append(fallbacks, routeFor(plan.Secondary))
	}
	if plan.Haiku.Source != plan.Primary.Source || plan.Haiku.BaseURL != plan.Primary.BaseURL {
		fallbacks = append(fallbacks, routeFor(plan.Haiku))
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

// savedTiersToDrop decides which ~/.oaica/config.json tier(s) a failed
// buildTierPlan justifies dropping for one launch. buildTierPlan prefixes each
// leg's failure with the flag name for that leg ("--sonnet-model: ..."), which
// is how the failing tier is identified here; the error text is our own, so the
// prefixes only change if buildTierPlan's wrapping changes. The prefix — not a
// substring anywhere in the message — is what identifies the leg: the leg's own
// error interpolates the model name it could not resolve, and a value that
// merely CONTAINS flag-like text ("ghost--sonnet-model:x", a botched config
// value) would otherwise make a haiku failure look like it named both tiers and
// cost the healthy sonnet tier for that launch.
//
// Dropping only the failing tier is the whole point: both keys are set across
// the fleet, and a stale sonnet_model must not cost the (healthy) haiku tier —
// losing it silently re-bills Claude Code's background work at the primary's
// price, the exact cost `haiku_model` exists to remove.
//
// An error with no leg prefix is the primary's (buildTierPlan wraps only the
// two secondary legs), which no saved tier can influence — so dropping them
// would cost both tiers for nothing, and (observed 2026-09-26) a primary that
// blipped once and then answered turned into a silent single-model launch at
// the primary's price, the exact harm the tier split exists to prevent. Such
// an error is retried UNCHANGED (the resolution reaches the network, so a
// transient failure is possible); if the retry fails too, the error stands and
// the launch fails, as documented. A value the user typed on this command line
// is never forgiven: savedSonnet/savedHaiku say which tiers came from the file.
func savedTiersToDrop(err error, savedSonnet, savedHaiku bool) (dropSonnet, dropHaiku, attributed bool) {
	if err == nil || (!savedSonnet && !savedHaiku) {
		return false, false, false
	}
	msg := err.Error()
	switch {
	case strings.HasPrefix(msg, "--sonnet-model:"):
		return savedSonnet, false, true
	case strings.HasPrefix(msg, "--haiku-model:"):
		return false, savedHaiku, true
	}
	// Unattributed: no leg named, so nothing justifies dropping a saved key.
	// (This used to report both keys as droppable, which meant a mistyped
	// PRIMARY — an input no `oaica config set` can invalidate — stripped the
	// user's standing sonnet and haiku tiers and told them those were to
	// blame; 2026-09-26 audit.)
	return false, false, false
}

// resolveTierPlan builds the launch's tier plan, forgiving a stale saved tier.
//
// A preference from ~/.oaica/config.json can rot long after the user set it —
// the model gets decommissioned, its remote is removed — and the error
// buildTierPlan returns names a FLAG ("--haiku-model: ...") the user never
// typed on THIS command line. Dropping both saved values at the first failure
// would be the blunt fix and costs the healthy key (see savedTiersToDrop), so
// this retries: each attempt drops only the tier the error names, and a second
// attempt exists because one error can only name one leg — with both keys
// stale, the first retry fails on the other one. A flag or plan value that
// fails is returned as-is, as before.
//
// The returned tiers are the ones the returned plan was built with.
func resolveTierPlan(model, sonnetModel, haikuModel string, forceTools, savedSonnet, savedHaiku bool) (tierPlan, string, string, error) {
	// buildTierPlan prints notices as it resolves (price banner, tool-wire
	// warnings) and an attempt may be discarded below, so buffer them: only
	// the plan the launch keeps gets to print.
	var buf strings.Builder
	plan, err := buildTierPlanBuffered(model, sonnetModel, haikuModel, forceTools, &buf)
	if err == nil {
		_, _ = io.WriteString(os.Stderr, buf.String())
		return plan, sonnetModel, haikuModel, nil
	}
	for attempt := 0; attempt < 2; attempt++ {
		dropSonnet, dropHaiku, _ := savedTiersToDrop(err, savedSonnet, savedHaiku)
		if !dropSonnet && !dropHaiku {
			if attempt == 0 && (savedSonnet || savedHaiku) {
				// Unattributed and there ARE saved tiers: the error cannot be
				// theirs, so rebuild the identical plan once instead of
				// stripping the split for an error it did not cause. If the
				// retry fails the same way, that is the answer — a second
				// unattributed failure must NOT fall through to the drop
				// branch below, which is what blamed the saved tiers for a
				// mistyped primary (2026-09-26 audit).
				var retryBuf strings.Builder
				plan, err = buildTierPlanBuffered(model, sonnetModel, haikuModel, forceTools, &retryBuf)
				if err == nil {
					_, _ = io.WriteString(os.Stderr, retryBuf.String())
					return plan, sonnetModel, haikuModel, nil
				}
				return tierPlan{}, "", "", err
			}
			return tierPlan{}, "", "", err
		}
		var dropped []string
		if dropSonnet {
			sonnetModel, savedSonnet, dropped = "", false, append(dropped, "sonnet_model")
		}
		if dropHaiku {
			haikuModel, savedHaiku, dropped = "", false, append(dropped, "haiku_model")
		}
		// Only an ATTRIBUTED drop can reach here: a drop is reported only when
		// the error names its leg, and naming it is what sets attributed. The
		// unattributed case returns above (one identical retry, then fail), so
		// there is no "we are guessing it was yours" message any more.
		fmt.Fprintf(os.Stderr, "config: %v — that value came from ~/.oaica/config.json, not this command line; ignoring %s for this launch. Repair it with `oaica config set sonnet-model|haiku-model <model>`, or clear the key with `-`\n", err, strings.Join(dropped, " + "))
		var retryBuf strings.Builder
		plan, err = buildTierPlanBuffered(model, sonnetModel, haikuModel, forceTools, &retryBuf)
		if err == nil {
			_, _ = io.WriteString(os.Stderr, retryBuf.String())
			return plan, sonnetModel, haikuModel, nil
		}
	}
	return tierPlan{}, "", "", err
}

// buildTierPlanBuffered runs buildTierPlan with its user-facing notices
// (price banner, tool-wire warning — planNotices) collected into buf instead of
// printed, so a caller that may discard the attempt (resolveTierPlan) does not
// print the same banner twice. The writer is restored on every path, panic
// included.
func buildTierPlanBuffered(model, sonnetModel, haikuModel string, forceTools bool, buf *strings.Builder) (tierPlan, error) {
	prev := planNotices
	planNotices = buf
	defer func() { planNotices = prev }()
	return buildTierPlan(model, sonnetModel, haikuModel, forceTools)
}

// standingTierModels applies the user's saved preferences
// (~/.oaica/config.json, user_config.go) to tiers neither a flag nor a plan
// filled. Flag > plan > config > wizard/default — this is the config step of
// that ladder, kept as its own function so the precedence is testable without
// running a launch. The two bools report which tiers actually came FROM the
// file, so a later failure can tell a saved value worth forgiving from a flag
// the user typed (see the retry in Run).
func standingTierModels(sonnetModel, haikuModel string) (string, string, bool, bool) {
	sonnetSaved, haikuSaved := false, false
	if sonnetModel == "" {
		if v := UserConfigSonnetModel(); v != "" {
			sonnetModel, sonnetSaved = v, true
		}
	}
	if haikuModel == "" {
		if v := UserConfigHaikuModel(); v != "" {
			haikuModel, haikuSaved = v, true
		}
	}
	return sonnetModel, haikuModel, sonnetSaved, haikuSaved
}

// nativeTierOnly reports whether this launch has nothing for the local proxy
// to do: a native primary, no tier split, and no --oversize. --oversize is
// consumed by the launcher (extractOversizeModel) and re-applied on the PLAN
// path, and it adds a real leg (the larger-window crossover), so runNative —
// which execs Claude Code with the passthrough args only — would drop it
// silently. --route-policy and --shard are deliberately NOT part of this test:
// with no second leg there is nothing to fall back to or to weight, so both are
// inert (docs/CLAUDE_TIERS.md), and keeping the untouched path is worth more
// than routing an inert flag through a proxy. A malformed --route-policy is
// still reported, because Run validates it before this check.
func nativeTierOnly(model, sonnetModel, haikuModel, oversizeModel string) (string, bool) {
	tier, ok := nativeClaudeModelTier(model)
	if !ok || tier == "" || sonnetModel != "" || haikuModel != "" || oversizeModel != "" {
		return "", false
	}
	return tier, true
}

// isSlotFamily reports whether a family is one the positional pass can place:
// Claude Code's own tier slots (opus, sonnet, haiku). No slot owns any other
// family ("fable", "claude"), so nothing competes for one — a leg named for it
// claims it from wherever it sits, which is the only way that family id can
// reach the leg instead of Default (the primary's model, which cannot serve
// an api.anthropic.com id at all).
func isSlotFamily(family string) bool {
	switch family {
	case "opus", "sonnet", "haiku":
		return true
	}
	return false
}

// tierFamilyRoutes maps Claude model families ("opus", "sonnet", "haiku",
// "fable") to the plan leg that owns that TIER (see proxyRouteTable.FamilyLegs
// for the whole story: opusplan resolves its opus/haiku slots internally, so
// the client sends real family ids we would otherwise have to send to Default
// — i.e. to the PRIMARY's model, whatever the user asked the tier for).
//
// Two passes, most explicit signal first:
//
//  1. A leg whose NAME is a native tier claims that family. "--sonnet-model
//     claude/opus" is the user naming a family for a tier, so it beats the
//     positional default: the sonnet tier IS Claude's opus. Two limits on that
//     claim, both in the loop below: it is confined to a genuinely distinct
//     SONNET leg (the one slot whose env value the launcher controls, so the
//     only place a foreign family can arrive FROM a leg name), and it never
//     applies to a family no slot owns ("fable", "claude") because nothing
//     competes for those.
//  2. Positional defaults fill the families nothing claimed: the opus slot is
//     the plan's primary, sonnet the secondary, haiku the haiku leg.
//
// Pass 2 is what makes a NON-native haiku leg work at all. Restricting this
// map to native legs (the first cut of this feature) left the ordinary case
// broken — a `haiku_model` of "zai-coding-plan/glm-4.5-air" registered no
// family, so Claude Code's own claude-haiku-4-5-* ids still fell to Default
// and were billed at the primary's price, which is the exact cost the setting
// promises to remove. Pass 2 for opus/sonnet is a no-op in that same case
// (Default is the primary, and the secondary's own id — not a family id — is
// what the client was told to send), so the blast radius stays the haiku tier.
//
// A leg's own ByModel route is reused whenever one exists, so a family route
// and the picker string can never disagree about the leg they name.
func tierFamilyRoutes(plan tierPlan) map[string]proxyRoute {
	legs := []struct {
		name string
		ep   launchEndpoint
	}{{plan.PrimaryName, plan.Primary}, {plan.SecondaryName, plan.Secondary}, {plan.HaikuName, plan.Haiku}}
	routeOf := func(leg struct {
		name string
		ep   launchEndpoint
	}) (proxyRoute, bool) {
		if leg.name == "" {
			return proxyRoute{}, false
		}
		if existing, ok := plan.Routes.ByModel[leg.name]; ok {
			return existing, true
		}
		return routeFor(leg.ep), true
	}
	var routes map[string]proxyRoute
	claim := func(family string, leg struct {
		name string
		ep   launchEndpoint
	}) {
		r, ok := routeOf(leg)
		if !ok {
			return
		}
		if routes == nil {
			routes = map[string]proxyRoute{}
		}
		routes[family] = r
	}
	slotFamilies := []string{"opus", "sonnet", "haiku"}
	// A slot-1 foreign claim only means anything when slot 1 is a leg of its
	// own: without --sonnet-model the secondary is a COPY of the primary
	// (buildTierPlan sets SecondaryName = model), so the exemption would let a
	// native primary's tier name claim a family through its own copy —
	// `--model claude/haiku --haiku-model zai/glm-4.5-air` claimed the haiku
	// family for the primary at the sonnet slot and the configured remote leg
	// was left serving nothing.
	secondaryOwnLeg := plan.SecondaryName != "" && plan.SecondaryName != plan.PrimaryName
	for i, leg := range legs {
		// An empty tier ("claude/") is a picker typo, not a family: it would
		// otherwise claim the "" key, which no client id ever produces.
		tier, ok := nativeClaudeModelTier(leg.name)
		if !ok || tier == "" {
			continue
		}
		// A family no slot can place is nobody's to lose (see isSlotFamily).
		if !isSlotFamily(tier) {
			claim(tier, leg)
			continue
		}
		// For a SLOT family, a leg naming another family may only do so on a
		// genuinely distinct sonnet leg. That slot is the one Claude Code lets
		// us set (opusplan resolves its opus and haiku slots from its own
		// catalog), so "claude/opus" there is a deliberate statement about the
		// sonnet tier. Elsewhere the same name moves traffic the user never
		// aimed at that leg: "--haiku-model claude/opus" would take the OPUS
		// family — the main plan-mode conversation — off the configured primary
		// and onto the Anthropic login, and the haiku leg the plan configured
		// would serve nothing. Those legs keep their own slot's family and let
		// pass 2 place the rest.
		if i == 1 && secondaryOwnLeg {
			claim(tier, leg)
			continue
		}
		if tier == slotFamilies[i] {
			claim(tier, leg)
		}
	}
	for i, family := range []string{"opus", "sonnet", "haiku"} {
		if _, claimed := routes[family]; claimed {
			continue
		}
		claim(family, legs[i])
	}
	return routes
}

// claudeCodeModelAlias turns a plan leg name into the value for an
// ANTHROPIC_DEFAULT_*_MODEL / CLAUDE_CODE_* env var (or the --model flag),
// which the REAL Claude Code binary reads. Every non-native model id (the
// normal OAICA/remote/daemon case) passes through completely unchanged --
// this only ever touches the claude/anthropic prefix.
//
// A native tier resolves to the real catalog id (resolveNativeModelAlias),
// not the bare tier name. Two independent reasons, both verified live
// 2026-09-25 against Claude Code 2.1.282:
//
//   - The bare name is rejected outright. `claude` with nothing but
//     ANTHROPIC_DEFAULT_SONNET_MODEL=sonnet set (no oaica in the picture)
//     exits with `[claude-code:unrecognized_model] {"model":"sonnet"}` /
//     "There's an issue with the selected model (sonnet)". That is exactly
//     what `oaica launch claude --model claude/opus --sonnet-model
//     claude/sonnet` did: the SONNET slot is the one opusplan mode actually
//     reads (its opus/haiku slots come from its own built-in catalog), and
//     the launch died before the first request. A real id
//     (claude-sonnet-4-5-20250929, claude-opus-4-5) is accepted; a
//     made-up one (claude-bogus-9-9, oaica/native-sonnet) is rejected, so
//     this must be a real resolution, not a rename.
//   - The bare name means nothing on the wire. A native leg's request is
//     forwarded to api.anthropic.com as-is (nativeAnthropicPassthrough), and
//     Anthropic answers "model: fable not found" for the alias (2026-09-02).
//
// Resolution failing (no Anthropic credential, offline) falls back to the
// bare tier: the same visible failure as before rather than a silent launch
// on some other model.
//
// An EMPTY tier ("claude/", a truncated picker string) is not a tier at all:
// resolving it would match the first Anthropic catalog entry, silently
// pinning the slot to the flagship model. Left alone, so the caller's own
// value is used and the mistake stays visible.
// The resolution is a network call (the catalog GET), so it is bounded by its
// own timeout. Launch setup has no request whose cancellation could cut it
// short — the child process has not started yet — so it passes a context that
// only the timeout can end; callers reached from a live request pass that
// request's context, so a client that goes away stops the wait.
func claudeCodeModelAlias(model string) string {
	if tier, ok := nativeClaudeModelTier(model); ok && tier != "" {
		return resolveNativeModelAlias(context.Background(), tier)
	}
	return model
}

// childEnv is the environment the launched agent runs in: the launcher's own
// environment, minus every variable this launch reads an upstream credential
// from, plus the plan's Claude Code variables.
func (p tierPlan) childEnv(anthropicBaseURL, clientToken string) []string {
	return append(scrubCredentialEnv(os.Environ(), p.credentialEnvNames()), p.envVars(anthropicBaseURL, clientToken)...)
}

// credentialEnvNames lists every environment variable this plan reads a REAL
// upstream credential out of, which is the set that must not reach the child.
//
// The proxy attaches each route's real key upstream (see envVars), so the
// child never needs any of them — and the key a leg is configured with is, by
// design, the variable the user exported (api_key_env), which os.Environ()
// therefore carried into the child for the taking: `env` in a Bash tool call,
// an install hook, or a prompt-injected command spends the user's account.
// Both names of a comma-joined api_key_env are included, because keyEnvName
// picks whichever one is set and this cannot know which without reading them.
func (p tierPlan) credentialEnvNames() []string {
	seen := map[string]bool{}
	var out []string
	add := func(spec string) {
		for _, name := range strings.Split(spec, ",") {
			name = strings.TrimSpace(name)
			if name == "" || seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, name)
		}
	}
	add(p.Primary.TokenEnv)
	add(p.Secondary.TokenEnv)
	add(p.Haiku.TokenEnv)
	// ...and every OTHER name those rows accept: TokenEnv holds only the one
	// name that happened to be set (keyEnvName), so a row listing
	// "BOX_KEY_A,BOX_KEY_B" otherwise left its sibling in the child's
	// environment, still holding a real key for the same account. add()
	// splits the comma-joined spec itself.
	add(p.Primary.APIKeyEnv)
	add(p.Secondary.APIKeyEnv)
	add(p.Haiku.APIKeyEnv)
	addRoute := func(r proxyRoute) {
		add(r.KeyEnv)
		add(r.APIKeyEnv)
	}
	addRoute(p.Routes.Default)
	addRoute(p.Routes.Oversize)
	for _, r := range p.Routes.ByModel {
		addRoute(r)
	}
	for _, r := range p.Routes.Fallbacks {
		addRoute(r)
	}
	return out
}

// scrubCredentialEnv drops NAME=... entries for the named variables. Removal,
// not blanking: a blanked name still tells the child the variable is there.
// Everything else — PATH, the user's own tooling, Claude Code's config — is
// passed through untouched; this is a credential rule, not a clean slate.
func scrubCredentialEnv(env []string, names []string) []string {
	if len(names) == 0 {
		return env
	}
	drop := make(map[string]bool, len(names))
	for _, n := range names {
		drop[n] = true
	}
	out := make([]string, 0, len(env))
	for _, kv := range env {
		name := kv
		if i := strings.IndexByte(kv, '='); i >= 0 {
			name = kv[:i]
		}
		if drop[name] {
			continue
		}
		out = append(out, kv)
	}
	return out
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
		// Claude Code appends a "tokens left" system message after every tool
		// result. Our translators hoist system messages to the front of the
		// prompt, so that message lands ahead of everything cached and the
		// prefix cache misses on every single request. Off keeps the prefix
		// stable across a turn (upstream add1f92bd, #17918).
		"CLAUDE_CODE_TOTAL_TOKENS_REMINDER=off",
		"DISABLE_ERROR_REPORTING=1",
		"DISABLE_FEEDBACK_COMMAND=1",
		"CLAUDE_CODE_DISABLE_FEEDBACK_SURVEY=1",
		"ANTHROPIC_DEFAULT_OPUS_MODEL=" + claudeCodeModelAlias(p.PrimaryName),
		"ANTHROPIC_DEFAULT_SONNET_MODEL=" + claudeCodeModelAlias(p.SecondaryName),
		"ANTHROPIC_DEFAULT_HAIKU_MODEL=" + claudeCodeModelAlias(p.HaikuName),
		"CLAUDE_CODE_SUBAGENT_MODEL=" + claudeCodeModelAlias(p.SecondaryName),
		// See modelEnvVars: Auto mode would address model ids no backend of
		// ours has.
		"CLAUDE_CODE_ENABLE_AUTO_MODE=0",
		"CLAUDE_CODE_AUTO_MODE_MODEL=" + claudeCodeModelAlias(p.PrimaryName),
		// Long-prefill backends (262k ctx on vLLM) can take minutes to first
		// token on big system prompts. Claude Code's default request timeout
		// aborts and retries, showing "Waiting for API response · will retry
		// in Nm · check your network" churn. Give it room instead.
		"API_TIMEOUT_MS=600000",
	}
	// No backend of ours answers Anthropic's server-side auto-mode checks, so
	// Claude Code's own client-side classifier is the default — and an explicit
	// setting of the user's is preserved rather than overwritten (upstream
	// 01c0fbfd3, #18596). A guarded append rather than a literal entry: the
	// child environment is the user's own (scrubbed) plus this list, so an
	// unconditional entry would shadow what they exported.
	if _, ok := os.LookupEnv("CLAUDE_CODE_AUTO_MODE_SERVER"); !ok {
		env = append(env, "CLAUDE_CODE_AUTO_MODE_SERVER=0")
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
	// A native primary has no probed PrimaryContext (native bypasses our
	// context-window probing entirely — see context_window_remote.go's
	// doc) and isn't a cloud model name either, so neither branch above
	// ever fires for it: Claude Code prints "isn't described by this
	// version's model catalog ... auto-compact keeps this session within
	// 200k tokens" for the SONNET/HAIKU tier's real (usually larger, e.g.
	// 262144) window (2026-09-02 — benign, same class of warning
	// documented for glm-5.3-flash:cloud/deepseek-v4-flash elsewhere, but
	// worth silencing when we do know the real number). Fall back to
	// whichever OAICA leg's probed window is known, since that's the
	// backend actually generating tokens under auto-compact's window
	// guess.
	if isNativeClaudeModel(p.PrimaryName) && p.PrimaryContext == 0 {
		// Net of the output budget, exactly as contextEnvVars and the merge
		// loop below do: the loop only RAISES an existing pair, so a raw
		// window written here is never corrected downwards, and auto-compact
		// then fires 32k too late — into the upstream's "prompt is too long"
		// 400 the reserve exists to prevent (2026-09-26 audit).
		if v := p.SecondaryContext; v > 0 {
			v = usableContextWindow(p.SecondaryName, v)
			env = append(env, "CLAUDE_CODE_MAX_CONTEXT_TOKENS="+strconv.Itoa(v), "CLAUDE_CODE_AUTO_COMPACT_WINDOW="+strconv.Itoa(v))
		} else if v := p.HaikuContext; v > 0 {
			v = usableContextWindow(p.HaikuName, v)
			env = append(env, "CLAUDE_CODE_MAX_CONTEXT_TOKENS="+strconv.Itoa(v), "CLAUDE_CODE_AUTO_COMPACT_WINDOW="+strconv.Itoa(v))
		}
	}
	// Cloud-primary + non-native secondary/haiku: the branch above only
	// knows the cloud primary's limit (e.g. glm-5.2's 202752) and never
	// checks the router/user-remote legs' probed windows. That window is
	// what the SONNET/HAIKU subagent sessions actually run against, so
	// Claude Code still warns about an "unknown model" and clamps to 200k
	// even though the router (oaica-35b-a3b-vision) exposes 262144.
	// Take the max of the cloud-primary limit and any probed non-native
	// leg windows so every session gets the correct ceiling. This is the only
	// place the LEG windows are folded in, so no case may skip it: an earlier
	// `continue` here claimed the native-primary fallback above had already
	// set the pair, but that fallback requires PrimaryContext == 0 while this
	// loop runs on PrimaryContext > 0 — the pair in hand therefore holds the
	// PRIMARY's window only, and a larger probed sonnet leg (65536 primary vs
	// 262144 sonnet) was silently dropped, leaving Claude Code to auto-compact
	// a subagent session at a quarter of the leg's real window (2026-09-26).
	for _, leg := range []struct {
		name string
		raw  int
	}{{p.SecondaryName, p.SecondaryContext}, {p.HaikuName, p.HaikuContext}} {
		v := leg.raw
		if v > 0 {
			// Compare USABLE windows, not raw ones: the pair already in env
			// holds PrimaryContext minus the output reserve (contextEnvVars),
			// so raising it to a RAW leg window would quietly hand back the
			// 32k that reserve exists to hold — and auto-compact would fire
			// 32k tokens later, straight into the upstream's "prompt is too
			// long" 400 that reserving the output budget prevents
			// (2026-09-26 audit; the removed `continue` had hidden it).
			v = max(usableContextWindow(leg.name, v), usableContextWindow(p.PrimaryName, p.PrimaryContext))
			if l, ok := lookupCloudModelLimit(p.PrimaryName); ok && v < l.Context {
				v = l.Context
			}
			// Raise the existing pair if our v is larger, and append one only
			// if no pair exists yet. NEVER append a second pair: os/exec keeps
			// the LAST duplicate of a variable, so appending a per-leg value
			// (as this did) made the child read the smallest leg's window —
			// the opposite of the max computed above.
			idx, cur := -1, 0
			for i, kv := range env {
				if strings.HasPrefix(kv, "CLAUDE_CODE_MAX_CONTEXT_TOKENS=") {
					idx = i
					cur, _ = strconv.Atoi(strings.TrimPrefix(kv, "CLAUDE_CODE_MAX_CONTEXT_TOKENS="))
					break
				}
			}
			switch {
			case idx < 0:
				s := strconv.Itoa(v)
				env = append(env,
					"CLAUDE_CODE_MAX_CONTEXT_TOKENS="+s,
					"CLAUDE_CODE_AUTO_COMPACT_WINDOW="+s,
				)
			case v > cur:
				s := strconv.Itoa(v)
				env[idx] = "CLAUDE_CODE_MAX_CONTEXT_TOKENS=" + s
				// Update the paired AUTO_COMPACT_WINDOW at the same index +1.
				if idx+1 < len(env) && strings.HasPrefix(env[idx+1], "CLAUDE_CODE_AUTO_COMPACT_WINDOW=") {
					env[idx+1] = "CLAUDE_CODE_AUTO_COMPACT_WINDOW=" + s
				}
			}
		}
	}
	return env
}

// Run launches Claude Code against the plan: one local translation proxy,
// routing per request model id.
func (c *Claude) Run(model string, models []LaunchModel, args []string) error {
	// Decide wizard eligibility BEFORE the extractors below strip
	// --sonnet-model/--haiku-model/--oversize/--route-policy/--plan/... from
	// args: those flags are exactly what suppresses the wizard (tierWizardFlags),
	// so asking afterwards always answered "eligible" and the wizard then
	// OVERWROTE the caller's explicit tiers with its own answers — Enter on a
	// step is "keep it" only relative to the wizard's own default, which is
	// "unset", so a typed --sonnet-model was replaced by "" and the launch ran
	// single-model at the primary's price.
	wizardEligible := tierWizardEligible(args)
	// Kept for flagPassed below: "the flag was passed with no value" is a
	// different thing from "the flag was not passed", and the extractors cannot
	// tell them apart once they have stripped it.
	rawArgs := args
	// Checked before the extractors strip them: a flag in a tier flag's value
	// slot is a missing value, not a value (see tierValueFlags).
	for _, name := range tierValueFlags {
		if got := flagSwallowedValue(args, name); got != "" {
			return fmt.Errorf("%s needs a value, got the flag %q — an argument beginning with \"--\" cannot be its value", name, got)
		}
	}
	forceTools, args := extractForceTools(args)
	sonnetModel, args := extractSonnetModel(args)
	haikuModel, args := extractHaikuModel(args)
	// Set below by standingTierModels: which tiers came from the saved config
	// rather than a flag/plan. See resolveTierPlan for the one use.
	savedSonnet, savedHaiku := false, false
	// Set by the wizard: "(same as primary)" is a deliberate CLEAR, and an
	// empty value that came from an answered step must not be refilled from
	// ~/.oaica/config.json (see the standingTierModels call below).
	clearedSonnet, clearedHaiku := false, false
	planName, args := extractPlanFlag(args)
	briefMode, args := extractBriefMode(args)
	policyArg, args := extractRoutePolicy(args)
	oversizeModel, args := extractOversizeModel(args)
	// A flag passed with nothing after it is not "not passed": reading it as
	// absent silently inherited a plan's stored value for `--oversize="$LEG"`
	// with LEG unset, and there was no way at all to drop a plan's oversize leg
	// — the plan refill (tier_plan_profiles.go) fills every empty value, and the
	// wizard's "none" row is unreachable with a --plan (2026-09-26 audit).
	if flagPassed(rawArgs, "--route-policy") && policyArg == "" {
		return fmt.Errorf("--route-policy needs a policy (local-first, remote-first, auto, local-only, remote-only, weighted)")
	}
	if flagPassed(rawArgs, "--oversize") && oversizeModel == "" {
		return fmt.Errorf("--oversize needs a model — use --oversize none to drop a plan's stored leg")
	}
	// The same rule for the tiers with no "none" spelling: an empty value
	// cannot say "clear this tier", and it used to be dropped before it ever
	// reached here, silently inheriting a plan's or the standing config's
	// model instead (2026-09-26 audit). Omit the flag to inherit; there is no
	// value that means clear, so say so rather than guess.
	if flagPassed(rawArgs, "--sonnet-model") && sonnetModel == "" {
		return fmt.Errorf("--sonnet-model needs a model id — omit the flag to keep the configured tier, or pass one to replace it")
	}
	if flagPassed(rawArgs, "--haiku-model") && haikuModel == "" {
		return fmt.Errorf("--haiku-model needs a model id — omit the flag to keep the configured tier, or pass one to replace it")
	}
	if flagPassed(rawArgs, "--plan") && planName == "" {
		return fmt.Errorf("--plan needs a plan name — omit the flag to launch without one")
	}
	// "--oversize none" is that drop, spelled rather than implied (an empty
	// value cannot say it: see above).
	oversizeDeclined := oversizeModel == oversizeNoneValue
	if oversizeDeclined {
		oversizeModel = ""
	}
	shardWeights, args := extractShardFlags(args)
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
	// Refused by name, not ignored — checked AFTER the unwrap above so both
	// spellings of "this launch uses a plan" are covered. The gate below skips
	// the wizard whenever a plan is present, so the combination did nothing at
	// all while docs/CLAUDE_TIERS.md promised --wizard forced the steps past
	// that gate. Honouring it is not the fix either: the wizard's first step
	// offers the LAST-USED plan (Enter there would replace this one), and its
	// tier steps lead with the standing ~/.oaica/config.json tiers rather than
	// the plan's, so their Enter-key defaults would drop the plan's tiers
	// (2026-09-26 audit).
	if wizardForced && planName != "" {
		return fmt.Errorf("launch wizard: --wizard cannot be combined with --plan %s — a plan supplies the tiers, and the wizard would offer to swap the plan out; drop --plan to walk them, or pick a plan from inside the wizard", planName)
	}
	// The wizard runs BEFORE the plan block (not after it) because its first
	// step can hand back a plan name: "reuse the plan I used here last time"
	// is only reachable from inside the wizard, and the answer has to take the
	// same resolution path a typed --plan does (resolvePlanModels below).
	if planName == "" && (wizardForced || wizardEligible) {
		// Sonnet/Haiku tier steps must offer the same "claude/*" and
		// "anthropic/*" native-passthrough entries the primary picker step
		// does (launch.go's selectSingleModelWithSelectorReady) — the
		// routing layer (nativeClaudeModelTier) already supports using one
		// as a secondary or haiku leg, the wizard just never listed them
		// (2026-09-05: "can't select an Anthropic plan for the tertiary
		// model"). Only this Claude-specific wizard needs them, so inject
		// here rather than in the shared LaunchModel inventory.
		wizardModels := append([]LaunchModel{}, models...)
		for _, m := range nativeClaudePickerModels {
			wizardModels = append(wizardModels, LaunchModel{Name: m.Name, Remote: true})
		}
		// The standing config feeds the wizard's DEFAULTS, not a ceiling on
		// its answers: read it here so each tier step can lead with a "keep
		// <saved model>" row (tierWizardTierItems). Enter then means "keep my
		// saved tier" — which is what a standing tier is FOR — while picking
		// any other row is a deliberate per-launch choice, exactly like a
		// flag, and must survive. Inverting that (config over the wizard's
		// answer) made a step the user answered look like it did nothing, and
		// made the preview and the saved plan name a tier the launch then
		// discarded (2026-09-26 audit).
		cfgSonnet, cfgHaiku, fromFileSonnet, fromFileHaiku := standingTierModels(sonnetModel, haikuModel)
		// The --oversize/--route-policy values this launch already has lead
		// their steps as "keep" rows: they win either way (below), so a step
		// defaulting anywhere else would make the wizard's closing preview name
		// a leg this launch is not going to use.
		//
		// For the policy that means the flag, or — with none typed — the
		// primary's own remotes.json route_policy, which line 1215 below is
		// what would otherwise apply. Handing the wizard only the typed flag
		// made its pre-selected "auto" the step's default, and the step's Enter
		// counts as an answer: a remote declaring local-only ran auto, which
		// escalates to remote legs on accumulated failures (2026-09-26 audit).
		policyForWizard := policyArg
		if policyForWizard == "" {
			policyForWizard = primaryRoutePolicy(model)
		}
		w, err := runTierWizard(wizardModels, model, cfgSonnet, cfgHaiku, oversizeModel, policyForWizard)
		if err != nil {
			return fmt.Errorf("launch wizard: %w", err)
		}
		if w.PlanName != "" {
			// Either a reused plan or one just saved; both resolve below
			// exactly like a typed --plan.
			planName = w.PlanName
		}
		if !w.PlanReused {
			// A REUSED plan supplies the tiers, so its steps were skipped and
			// there is nothing else to apply — not even the policy or the
			// oversize leg, which are unanswered on that path and must stay
			// unanswered so resolvePlanTier below reads them out of the plan
			// (pre-setting the wizard's "auto" here silently outranked a
			// plan's local-only; 2026-09-26 audit).
			//
			// Everything else — including the save path — applies the steps
			// the user actually answered. Gating this on PlanName == "" let a
			// save masquerade as a reuse, so an answered "(same as primary)"
			// clear was dropped and the standing ~/.oaica/config.json tier
			// came back (2026-09-26 audit).
			//
			// Only the steps the wizard actually ANSWERED may write the tier
			// slots: esc on the first step hands back an all-empty choice,
			// and assigning it unconditionally silently dropped a typed
			// --sonnet-model (2026-09-26).
			if w.SonnetAnswered {
				sonnetModel = w.SonnetModel
			}
			if w.HaikuAnswered {
				haikuModel = w.HaikuModel
			}
			// "(same as primary)" is an explicit CLEAR: an empty value from an
			// ANSWERED step must not be refilled from ~/.oaica/config.json, or
			// the wizard's only way to take a standing split back off undoes
			// itself right after the preview promised it was gone.
			clearedSonnet, clearedHaiku = w.SonnetCleared, w.HaikuCleared
			// A tier the user left at its saved value is still a saved value
			// for the stale-tier retry below (forgiven on failure); one they
			// picked is a choice made on this command line and never is.
			savedSonnet = w.SonnetAnswered && fromFileSonnet && w.SonnetModel == cfgSonnet && !w.SonnetCleared
			savedHaiku = w.HaikuAnswered && fromFileHaiku && w.HaikuModel == cfgHaiku && !w.HaikuCleared
		}
		// An ANSWERED step is a choice made in this wizard, and the user was
		// told to run it (--wizard) — so it applies even over a typed
		// --oversize/--route-policy, which supplies the step's default row
		// instead. Applying only the answered ones keeps an abandoned wizard
		// (esc) from wiping a typed flag, and keeps the closing preview from
		// naming a leg the launch is not going to use (2026-09-26 audit: a
		// non-Enter pick used to be discarded while the preview showed it).
		if w.OversizeAnswered {
			oversizeModel = w.OversizeModel
		}
		if w.PolicyAnswered {
			policyArg = w.RoutePolicy
		}
	}
	if planName != "" {
		resolvedModel, resolvedSonnet, resolvedHaiku, err := resolvePlanModels(planName, model, sonnetModel, haikuModel)
		if err != nil {
			return fmt.Errorf("--plan: %w", err)
		}
		model, sonnetModel, haikuModel = resolvedModel, resolvedSonnet, resolvedHaiku
		// Flags > plan > remotes.json route_policy: the plan only fills what
		// the flags left empty (tier_plan_profiles.go).
		policyArg, oversizeModel, err = resolvePlanTier(planName, policyArg, oversizeModel)
		if err != nil {
			return fmt.Errorf("--plan: %w", err)
		}
		if oversizeDeclined {
			// `--oversize none` is an ANSWER, the one the wizard's "none" row
			// gives: the refill just above must not bring the plan's leg back
			// (2026-09-26 audit).
			oversizeModel = ""
		}
	}
	// Standing user preference fills what neither a flag nor a plan set:
	// ~/.oaica/config.json sonnet_model / haiku_model (user_config.go).
	// Flag > plan > wizard > config > same-as-primary. A standing haiku_model is a real
	// request for a distinct haiku tier, so it takes a native primary off the
	// untouched runNative path (which has no env at all and would run its own
	// built-in Haiku) — that is the intent, not a side effect.
	sonnetModel, haikuModel, cfgSonnet, cfgHaiku := standingTierModels(sonnetModel, haikuModel)
	// A wizard answer of "(same as primary)" is a CLEAR, and standingTierModels
	// cannot tell it apart from an unanswered step: it fills every empty value,
	// so the standing split came straight back — after the wizard's own preview
	// had already said it was gone (2026-09-26 audit). Clearing the source flag
	// along with the value keeps savedSonnet/savedHaiku honest too: a key that
	// is no longer in use is not a stale key the retry may blame.
	if clearedSonnet {
		sonnetModel, cfgSonnet = "", false
	}
	if clearedHaiku {
		haikuModel, cfgHaiku = "", false
	}
	// OR, not assignment: the wizard branch above may already have recorded
	// that a tier came from the file, and this call sees a non-empty value (so
	// it reports false) — overwriting would lose the attribution the stale-tier
	// retry needs to forgive a saved value.
	savedSonnet, savedHaiku = savedSonnet || cfgSonnet, savedHaiku || cfgHaiku
	if briefMode {
		// Claude Code's own flag, not a bespoke mechanism — see
		// briefModeSystemPrompt's doc for why this exact wording and why
		// not "--compact".
		args = append(args, "--append-system-prompt", briefModeSystemPrompt)
	}

	// "claude/<tier>" picker entries with NO tier split requested take the
	// fully-untouched native path: the REAL Claude Code binary, clean env,
	// zero proxy involvement — the strongest privacy/simplicity guarantee,
	// kept as the default for the common case (a plain native launch).
	// Checked here, after --plan/wizard/config have all had a chance to
	// fill in sonnetModel/haikuModel, not at entry — a native primary
	// reached via `--plan` with a sonnet leg must still get the split.
	// Requesting a split on a native primary instead routes through the
	// local proxy (nativeAnthropicPassthrough, anthropic_openai_proxy.go):
	// a native leg forwards straight to api.anthropic.com with the user's
	// own credential and incurs no OAICA billing either way — see
	// proxyRoute.NativePassthrough's doc for what that skips (2026-09-02).
	//
	// --oversize/--route-policy/--shard must keep the launch on the plan path
	// even with no tier split: runNative execs Claude Code with the passthrough
	// args only, so those flags would be dropped silently — and
	// `--oversize <bigger-remote>` alongside a native primary is a request to
	// ignore it, not a typo to swallow silently. The plan path is where the
	// launch can at least REPORT what the leg will do: the crossover is only
	// reached for a leg whose window is known, so against a native primary it
	// covers that split's overflow and never the primary's own requests (see
	// the oversize block below).
	// A policy the user typed is validated even when the launch turns out to
	// need no proxy at all: a single native leg has nothing to fall back to, so
	// the policy is inert, but a typo must not pass unnoticed — this is the same
	// message the plan path returns, and the same one a --plan value gets.
	if policyArg != "" {
		if _, err := parseRoutePolicy(policyArg); err != nil {
			return fmt.Errorf("route_policy %q (remotes.json or --route-policy) is not one of local-first, remote-first, auto, local-only, remote-only, weighted", policyArg)
		}
	}
	nativeOnly := func(sonnet, haiku string) (string, bool) {
		return nativeTierOnly(model, sonnet, haiku, oversizeModel)
	}
	if tier, ok := nativeOnly(sonnetModel, haikuModel); ok {
		return c.runNative(tier, args)
	}

	claudePath, err := ensureClaudeInstalled()
	if err != nil {
		return err
	}

	plan, effSonnet, effHaiku, err := resolveTierPlan(model, sonnetModel, haikuModel, forceTools, savedSonnet, savedHaiku)
	if err != nil {
		return err
	}
	// The saved-config fallback above may have dropped the very tiers that
	// made this a split — with both gone, a native primary is a plain native
	// launch again and belongs on the untouched path the earlier check could
	// not choose (it saw the un-forgiven config values). Only reachable when
	// a saved value was actually dropped: the earlier check returns runNative
	// for every other empty-tier native launch.
	if tier, ok := nativeOnly(effSonnet, effHaiku); ok {
		return c.runNative(tier, args)
	}
	// Policy precedence: --route-policy flag > primary remote's
	// route_policy (remotes.json) > local-first (parseRoutePolicy default).
	if policyArg == "" {
		policyArg = plan.Primary.RoutePolicy
	}
	policy, err := parseRoutePolicy(policyArg)
	if err != nil {
		return fmt.Errorf("route_policy %q (remotes.json or --route-policy) is not one of local-first, remote-first, auto, local-only, remote-only, weighted", policyArg)
	}
	plan.Routes.Policy = policy
	// The --oversize and --shard flags rewrite the plan's legs in place; see
	// applyRouteOverrides for why the oversize leg is resolved FIRST.
	if err := applyRouteOverrides(&plan, oversizeModel, shardWeights, forceTools); err != nil {
		return err
	}
	// Real context window from the upstreams' /models metadata (claude.go's
	// cloud-alias map covers :cloud; this covers user remotes and the
	// router). Probed here, not in buildTierPlan: unit tests build plans
	// against fake/unroutable URLs and must not wait on network.
	//
	// Checked on ALL THREE legs, not just Primary (2026-09-02): a native
	// primary (sourceNativeAnthropic) is neither a user remote nor router,
	// so this gate always skipped probing entirely for a native-primary +
	// OAICA-split launch — SecondaryContext/HaikuContext stayed 0 no
	// matter what fixes touched withContextWindows/
	// applyContextWindowsToRoutes, and both envVars' native-primary
	// context-window fallback and the proxy's context-fit clamp had
	// nothing to work with for that combination.
	probeSource := func(s endpointSource) bool { return s == sourceUserRemote || s == sourceRouter }
	if probeSource(plan.Primary.Source) || probeSource(plan.Secondary.Source) || probeSource(plan.Haiku.Source) {
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

	if plan.SecondaryName != plan.PrimaryName || plan.HaikuName != plan.PrimaryName {
		fmt.Fprintf(os.Stderr, "tiers: opus -> %s (%s)  sonnet/subagents -> %s (%s)  haiku -> %s (%s)\n",
			plan.PrimaryName, plan.Primary.Source, plan.SecondaryName, plan.Secondary.Source, plan.HaikuName, plan.Haiku.Source)
	}
	if len(plan.Routes.Fallbacks) > 1 {
		fmt.Fprintf(os.Stderr, "route policy: %s (fallback legs: %d)\n", policy, len(plan.Routes.Fallbacks)-1)
	}
	if plan.Routes.Oversize.BaseURL != "" || plan.Routes.Oversize.NativePassthrough {
		// The crossover compares the oversize leg with the window of the leg
		// that would otherwise serve, and each tier route carries its own
		// (applyContextWindowsToRoutes) — so the primary's window is NOT "the"
		// threshold: a small split tier crosses over far below it, and a leg no
		// larger than the route it would replace never crosses at all
		// (oversizeSwap requires strictly larger, and bails outright when the
		// serving route's window is 0, as a native or unprobed remote primary's
		// always is). Name the leg, then say so when the PRIMARY's own requests
		// are outside its reach — rather than printing ">0k-token requests" or
		// a number that is only right when the primary is the leg overflowing.
		//
		// A NATIVE oversize leg carries no BaseURL by construction
		// (proxyRoute.NativePassthrough), so gating on BaseURL alone made
		// `--oversize claude/<tier>` print NOTHING at all — not even the
		// `tiers:`/`route policy:` lines, which have their own gates — leaving
		// the user unable to tell whether the flag had any effect (2026-09-26).
		over := plan.Routes.Oversize
		switch {
		// BaseURL, not NativePassthrough: routeFor marks an anthropic-WIRE
		// remote passthrough too (a vendor coding plan, e.g. zai/glm-5.3), and
		// that leg is neither served by the user's Anthropic login nor
		// unprobed — claiming both was wrong on every count. A truly native
		// leg is the only one with no base URL (2026-09-26 audit).
		case over.BaseURL == "":
			fmt.Fprintf(os.Stderr, "oversize: requests past the serving leg's window -> claude/%s (your own Anthropic login; Anthropic enforces its real window upstream and we never probe it, so this leg needs no size comparison)\n",
				over.UpstreamModel)
		case over.ContextWindow > 0:
			fmt.Fprintf(os.Stderr, "oversize: requests past the serving leg's window -> %s (%s, %dk window)\n",
				over.Label, over.UpstreamModel, over.ContextWindow/1024)
		default:
			fmt.Fprintf(os.Stderr, "oversize: requests past the serving leg's window -> %s (%s; its own window is unprobed, and the crossover needs it)\n",
				over.Label, over.UpstreamModel)
		}
		// The leg is still a breaker fallback for the primary (the route table
		// appends it), so this warns about the crossover only, never about the
		// leg being useless.
		switch {
		case plan.PrimaryContext <= 0:
			fmt.Fprintf(os.Stderr, "oversize: the primary's own requests cannot cross over — its context window is unknown (nothing answered the probe; a native primary is never probed at all)\n")
		case over.ContextWindow > 0 && over.ContextWindow <= plan.PrimaryContext:
			fmt.Fprintf(os.Stderr, "oversize: the primary's own requests cannot cross over — this leg is not larger than the primary's %dk window\n",
				plan.PrimaryContext/1024)
		}
	}

	// Default model mode: with a tier split configured, launch straight
	// into opusplan — Claude Code plans on the opus tier (the primary) and
	// executes on the sonnet tier (the secondary), no /model opusplan
	// needed after every relaunch. An explicit --model in the user's args
	// wins (c.args skips ours), and a single-model launch (no secondary)
	// keeps pinning the primary as before.
	// A NATIVE primary especially needs this: ANTHROPIC_DEFAULT_OPUS_MODEL
	// carries our own picker-namespaced string ("claude/fable"), which the
	// real Claude Code binary does not recognize as a --model value on its
	// own -- opusplan mode is a fixed built-in preset that Claude Code
	// resolves its opus/haiku slots from internally, bypassing that env
	// var for those two tiers (only the sonnet slot is actually overridden
	// in opusplan mode). Without opusplan, a haiku-only split (sonnet ==
	// primary, haiku != primary) sent claude/fable straight to --model and
	// failed ("issue with the selected model (claude/fable)", 2026-09-02) —
	// this condition previously checked SecondaryName only, so it never
	// caught that case.
	claudeModel := claudeCodeModelAlias(plan.PrimaryName)
	if (plan.SecondaryName != plan.PrimaryName || plan.HaikuName != plan.PrimaryName) && !hasClaudeModelFlag(args) {
		claudeModel = "opusplan"
		fmt.Fprintf(os.Stderr, "model mode: opusplan (plan with %s, sonnet %s, haiku %s)\n",
			plan.PrimaryName, plan.SecondaryName, plan.HaikuName)
	}

	cmd := exec.Command(claudePath, c.args(claudeModel, args)...)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.Env = plan.childEnv(anthropicBaseURL, clientToken)
	if err := runChild(cmd); err != nil {
		claudeResumeHint(err, args)
		return err
	}
	return nil
}

// applyRouteOverrides applies the two flags that rewrite the plan's legs in
// place: --oversize (resolve the named model and install it as the plan's
// oversize leg, appending it as a fallback too) and --shard (stamp a Weight
// onto every leg on a named model's base URL).
//
// Order matters (2026-09-26 audit): --shard runs AFTER --oversize, because the
// oversize leg is a leg of this launch. Stamping first meant `--shard X:7
// --oversize X` found no route on X's base URL yet — X's leg was appended to
// the fallbacks a few lines later — so the weight was silently dropped and the
// leg kept whatever remotes.json's `weight` (0 by default) gave it, while
// docs/CLAUDE_TIERS.md promises --shard "overrides weight for a single launch
// without editing the file". The loop below stamps the Oversize leg as well as
// Default and Fallbacks for the same reason.
func applyRouteOverrides(plan *tierPlan, oversizeModel string, shardWeights map[string]int, forceTools bool) error {
	if oversizeModel != "" {
		over, err := resolveLaunchEndpoint(oversizeModel)
		if err != nil {
			return fmt.Errorf("--oversize: %w", err)
		}
		// Two native-passthrough endpoints (both BaseURL=="") would compare
		// equal here even when they're genuinely different tiers -- e.g.
		// --oversize claude/fable alongside a native claude/fable PRIMARY
		// is exactly the intended setup (2026-09-02: swap the OAICA
		// sonnet/haiku leg to the native primary's own real 1M+ window when
		// it overflows), not a same-backend error. Only reject the
		// same-backend case for two ORDINARY (non-native) endpoints, where
		// an identical BaseURL really does mean "no larger leg exists."
		if over.Source != sourceNativeAnthropic && over.BaseURL == plan.Primary.BaseURL {
			// redactBaseURL: resolveRemoteEndpoint deliberately keeps a
			// Basic userinfo (user:password@) in the URL it returns, and
			// this text goes out through cobra.CheckErr to stderr — a
			// password printed for a routing mistake (2026-09-26 audit).
			return fmt.Errorf("--oversize %q resolves to the same backend as the primary (%s): the oversize leg exists to serve requests the primary cannot hold, so it must be a different base URL (a larger-context remote)", oversizeModel, redactBaseURL(over.BaseURL))
		}
		if err := gateRemoteToolsEndpoint(over.RemoteEndpoint, toolWireAnthropic, forceTools); err != nil {
			return fmt.Errorf("--oversize: %w", err)
		}
		plan.Routes.Oversize = routeFor(over)
		if oversizeSource := over.Source; oversizeSource != sourceNativeAnthropic {
			// The oversize leg is a full route leg: it can also serve as the
			// breaker fallback for the other legs (and it gets
			// health-probed). A native leg has no BaseURL to probe/dedup on
			// this way — nativeOversizeBreakerKey (route_policy.go) is its
			// own separate breaker identity, checked directly in
			// oversizeSwap instead.
			urlSeen := map[string]bool{}
			for _, f := range plan.Routes.Fallbacks {
				urlSeen[f.BaseURL] = true
			}
			if !urlSeen[plan.Routes.Oversize.BaseURL] {
				plan.Routes.Fallbacks = append(plan.Routes.Fallbacks, plan.Routes.Oversize)
			}
		}
	}
	// --shard <model>:<weight>: resolve each named model to its BaseURL and
	// stamp that Weight onto every existing route (Default/Fallbacks/Oversize)
	// on that base URL. Applied AFTER Fallbacks are built (buildTierPlan) and
	// after the oversize block above, so this only ever weights legs the plan
	// already has — it cannot introduce a new leg on its own, matching
	// --oversize's model (resolve, don't invent). An id that doesn't resolve to
	// any existing leg is silently a no-op: --route-policy weighted's own doc
	// already covers "fewer than 2 weighted legs" degrading to plain failover,
	// so a typo'd --shard target just leaves you in that same safe state rather
	// than failing the launch.
	for shardModel, weight := range shardWeights {
		ep, err := resolveLaunchEndpoint(shardModel)
		if err != nil || ep.BaseURL == "" {
			continue
		}
		if plan.Routes.Default.BaseURL == ep.BaseURL {
			plan.Routes.Default.Weight = weight
		}
		for i := range plan.Routes.Fallbacks {
			if plan.Routes.Fallbacks[i].BaseURL == ep.BaseURL {
				plan.Routes.Fallbacks[i].Weight = weight
			}
		}
		if plan.Routes.Oversize.BaseURL == ep.BaseURL {
			plan.Routes.Oversize.Weight = weight
		}
	}
	return nil
}

// directLaunchEnv is the child environment for a launch door that runs the agent
// directly and hands it ONE key explicitly (extra, appended after the scrub).
//
// `oaica launch claude` and `oaica launch openclaw` keep every real key oaica could
// have read out of the child's environment, because the agent's Bash tool, or a
// command a prompt injection wrote, can read it. The doors that started from
// os.Environ() and appended their own key — codex, copilot, kimi's env-config CLI,
// the codex app's terminal path — passed every OTHER configured remote's key and the
// router's OAICA_API_KEY through, so an injected session could read and spend
// accounts it was never launched against (2026-09-29 audit, round 110, F110-L2-2).
// The scrubbed names are the same derived set openclaw uses, so the paths cannot
// drift; everything else the user exported is untouched. Doors that do NOT hand the
// child a key (a native run under the user's own login, and tools that may resolve
// a key from a variable named in their own config) are deliberately not routed here.
func directLaunchEnv(extra ...string) []string {
	return append(scrubCredentialEnv(os.Environ(), openclawCredentialEnvNames()), extra...)
}
