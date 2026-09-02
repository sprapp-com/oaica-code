package launch

// Remote context-window discovery: an upstream's /v1/models metadata tells
// us the context the model is actually RUNNING with (vLLM `max_model_len`,
// OpenAI-style routers `context_length`). Claude Code assumes 200k for any
// model id it does not recognize and warns; passing the real window as
// CLAUDE_CODE_MAX_CONTEXT_TOKENS makes auto-compact honor it. Failure to
// probe is not fatal — we simply leave the env unset (previous behavior).

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
)

// remoteContextWindowFn is swappable so tests can stub the network probe.
var remoteContextWindowFn = cachedRemoteContextWindow

// probeTimeout is intentionally short: this runs on every launch, before the
// first token. A slow /models must not add seconds of startup latency.
const probeTimeout = 2 * time.Second

// probeCacheTTL bounds how long a probe result is reused. The wizard's
// oversize step probes EVERY candidate model, and withContextWindows then
// re-probes the same upstreams after the wizard — without a cache that's
// 2s per model per step, all sequential, on every launch.
const probeCacheTTL = 5 * time.Minute

type probeCacheEntry struct {
	window    int
	expiresAt time.Time
}

// probeCache memoizes defaultRemoteContextWindow per (base URL, model).
// A negative (0) result is cached too — a dead backend shouldn't cost its
// full timeout again for every candidate in the same launch.
var probeCache = struct {
	sync.Mutex
	m map[string]probeCacheEntry
}{m: map[string]probeCacheEntry{}}

func cachedRemoteContextWindow(route proxyRoute) int {
	key := route.BaseURL + "|" + route.UpstreamModel
	probeCache.Lock()
	e, ok := probeCache.m[key]
	if ok && time.Now().Before(e.expiresAt) {
		probeCache.Unlock()
		return e.window
	}
	probeCache.Unlock()

	w := defaultRemoteContextWindow(route)
	probeCache.Lock()
	probeCache.m[key] = probeCacheEntry{window: w, expiresAt: time.Now().Add(probeCacheTTL)}
	probeCache.Unlock()
	return w
}

type remoteModelMeta struct {
	ID            string `json:"id"`
	ContextLength int    `json:"context_length"`
	MaxModelLen   int    `json:"max_model_len"`
}

type remoteModelsResponse struct {
	Data []remoteModelMeta `json:"data"`
}

// defaultRemoteContextWindow asks route.BaseURL (…/v1) for its model list
// and returns the context window of the upstream model, 0 if unknown.
func defaultRemoteContextWindow(route proxyRoute) int {
	url := strings.TrimRight(route.BaseURL, "/") + "/models"
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0
	}
	if route.Key != "" {
		req.Header.Set("Authorization", "Bearer "+route.Key)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return 0
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0
	}
	var parsed remoteModelsResponse
	if err := json.NewDecoder(resp.Body).Decode(&parsed); err != nil {
		return 0
	}
	var meta *remoteModelMeta
	for i := range parsed.Data {
		if parsed.Data[i].ID == route.UpstreamModel {
			meta = &parsed.Data[i]
			break
		}
	}
	if meta == nil {
		if len(parsed.Data) == 1 {
			meta = &parsed.Data[0]
		} else {
			return 0
		}
	}
	// Routers report context_length; a bare vLLM reports max_model_len. A
	// router in front of vLLM may return both (identical). Prefer the
	// larger — never advertise less than the upstream says it serves.
	if meta.ContextLength >= meta.MaxModelLen {
		return meta.ContextLength
	}
	return meta.MaxModelLen
}

// withContextWindows probes each distinct upstream route for its real
// context window and records it on the plan. Never fails: an unreachable or
// non-enumerating upstream just leaves the fields at 0.
//
// The live probe wins when it succeeds — it reflects what's actually
// running right now, including an emergency downsized config (see the
// 2026-08-27 GPU2-crowding incident: --max-model-len dropped from 262144
// to 32768 while a co-tenant held VRAM, and the live probe alone caught
// that; a static manifest would have kept advertising the old number).
// The manifest (model_manifest.go) is only the fallback for when nothing
// answered — cold start, unreachable upstream, or an engine whose /models
// doesn't report context_length/max_model_len at all.
func (p *tierPlan) withContextWindows() *tierPlan {
	p.PrimaryContext = remoteContextWindowFn(p.Routes.Default)
	if p.PrimaryContext <= 0 {
		if v, ok := contextWindowFromManifest(p.PrimaryName); ok {
			p.PrimaryContext = v
		}
	}
	if p.SecondaryName != p.PrimaryName {
		if r, _ := p.Routes.resolve(p.SecondaryName); r.BaseURL != "" &&
			r.BaseURL != p.Routes.Default.BaseURL {
			p.SecondaryContext = remoteContextWindowFn(r)
		}
		if p.SecondaryContext <= 0 {
			if v, ok := contextWindowFromManifest(p.SecondaryName); ok {
				p.SecondaryContext = v
			}
		}
	}
	// HaikuContext was never probed here before 2026-09-02 -- a haiku-only
	// split (sonnet == primary, only haiku differs) always saw
	// HaikuContext == 0, so envVars' native-primary fallback (see its doc)
	// had nothing to fall back to for that specific combination even
	// though the haiku leg's real window IS knowable, same as secondary's.
	if p.HaikuName != p.PrimaryName && p.HaikuName != p.SecondaryName {
		if r, _ := p.Routes.resolve(p.HaikuName); r.BaseURL != "" &&
			r.BaseURL != p.Routes.Default.BaseURL {
			p.HaikuContext = remoteContextWindowFn(r)
		}
		if p.HaikuContext <= 0 {
			if v, ok := contextWindowFromManifest(p.HaikuName); ok {
				p.HaikuContext = v
			}
		}
	} else if p.HaikuName == p.SecondaryName {
		// Same leg as sonnet — reuse what was already probed instead of
		// hitting the upstream a second time for an identical answer.
		p.HaikuContext = p.SecondaryContext
	}
	if p.Routes.Oversize.BaseURL != "" && p.Routes.Oversize.BaseURL != p.Routes.Default.BaseURL {
		p.Routes.Oversize.ContextWindow = remoteContextWindowFn(p.Routes.Oversize)
	}
	return p
}

// applyContextWindowsToRoutes copies PrimaryContext/SecondaryContext (once
// known -- call after withContextWindows) onto the matching entries in
// p.Routes, so the proxy's own context-fit clamp (see the /v1/messages
// handler in anthropic_openai_proxy.go) has a real ceiling to enforce
// per request, not just the CLAUDE_CODE_AUTO_COMPACT_WINDOW env var hint
// below -- that hint is advisory (Claude Code's own token counting can
// drift a few tokens from ours, and the auto-compaction call itself is
// exactly the request most likely to land right on the edge, since it
// fires BECAUSE the session is already near the limit). The env var
// reduces how often this happens; the clamp guarantees it can't produce
// an outgoing request that's already doomed to fail, regardless.
func (p *tierPlan) applyContextWindowsToRoutes() *tierPlan {
	if p.PrimaryContext > 0 {
		p.Routes.Default.ContextWindow = p.PrimaryContext
		if r, ok := p.Routes.ByModel[p.PrimaryName]; ok {
			r.ContextWindow = p.PrimaryContext
			p.Routes.ByModel[p.PrimaryName] = r
		}
		if r, ok := p.Routes.ByModel[p.Primary.UpstreamModel]; ok && r.ContextWindow == 0 {
			r.ContextWindow = p.PrimaryContext
			p.Routes.ByModel[p.Primary.UpstreamModel] = r
		}
	}
	if p.SecondaryContext > 0 {
		if r, ok := p.Routes.ByModel[p.SecondaryName]; ok {
			r.ContextWindow = p.SecondaryContext
			p.Routes.ByModel[p.SecondaryName] = r
		}
		if r, ok := p.Routes.ByModel[p.Secondary.UpstreamModel]; ok && r.ContextWindow == 0 {
			r.ContextWindow = p.SecondaryContext
			p.Routes.ByModel[p.Secondary.UpstreamModel] = r
		}
	}
	// Haiku's route never got its ContextWindow set here before 2026-09-02
	// -- a haiku-only split's real proxy route had no clamp ceiling at all,
	// only the advisory env var (once withContextWindows also started
	// probing HaikuContext, same date). The clamp is what actually
	// prevents an outgoing request from exceeding the upstream's window;
	// without it a haiku-tier request near the edge would 400 instead of
	// auto-compacting first.
	if p.HaikuContext > 0 {
		if r, ok := p.Routes.ByModel[p.HaikuName]; ok {
			r.ContextWindow = p.HaikuContext
			p.Routes.ByModel[p.HaikuName] = r
		}
		if r, ok := p.Routes.ByModel[p.Haiku.UpstreamModel]; ok && r.ContextWindow == 0 {
			r.ContextWindow = p.HaikuContext
			p.Routes.ByModel[p.Haiku.UpstreamModel] = r
		}
	}
	return p
}

// maxOutputTokensReserve is Claude Code's fixed max_tokens request (32000,
// unaffected by how much input is already used). vLLM enforces
// input+output <= max_model_len, so advertising the raw window lets Claude
// Code fill it with input right up to the edge and then 400 on the next
// turn ("...262145 tokens" for a 262144 window, one over). Reserve the
// output budget up front so auto-compact triggers before that happens.
const maxOutputTokensReserve = 32000

// contextEnvVars is what Claude Code reads for the real window;
// AUTO_COMPACT_WINDOW is the older knob our cloud-alias path already used —
// keep both in sync rather than sending contradictory hints.
func (p tierPlan) contextEnvVars() []string {
	if p.PrimaryContext <= 0 {
		return nil
	}
	reserve := maxOutputTokensReserve
	if m, err := loadModelManifest(); err == nil {
		if e, ok := m.Get(p.PrimaryName); ok && e.DefaultMaxOutputTokens > 0 {
			reserve = e.DefaultMaxOutputTokens
		}
	}
	usable := p.PrimaryContext - reserve
	if usable <= 0 {
		usable = p.PrimaryContext
	}
	v := strconv.Itoa(usable)
	return []string{
		"CLAUDE_CODE_MAX_CONTEXT_TOKENS=" + v,
		"CLAUDE_CODE_AUTO_COMPACT_WINDOW=" + v,
	}
}
