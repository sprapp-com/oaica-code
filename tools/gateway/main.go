// oaica-gateway is the public-facing OpenAI-compatible inference gateway for
// OpenRouter (and any other OpenAI-wire consumer). It exposes the pieces a
// provider must publish to be listed:
//
//	GET  /health                     -> unauthenticated readiness (200 only if upstream answers)
//	GET  /models, /v1/models         -> standardized OpenAI model list (from config)
//	POST /v1/chat/completions        -> proxied to the upstream
//	POST /v1/completions             -> proxied to the upstream
//
// Everything else is 404 BEFORE touching the proxy -- the upstream (vLLM via
// gatekeeper/katlb) exposes /metrics, /tokenize, dev-mode control endpoints
// etc. that must never be reachable from the public key.
//
// /models is served from a CONFIGURED list (not proxied), so it stays up even
// if a specific backend is momentarily down -- OpenRouter polls it and expects
// a stable answer. Per-model context_length, max_completion_tokens and
// per-token pricing are published because OpenRouter needs them to list and
// price the model.
//
// Auth: "Authorization: Bearer <key>" required. Keys live in the config as
// sha256 hex digests (never plaintext) and are compared in constant time.
// Unknown/missing key -> 401 in OpenAI's error shape.
//
// Metering: every completion is written to an append-only JSONL ledger
// (request id, key label, model, prompt/completion tokens, status, latency).
// This is the only record to reconcile OpenRouter payouts against. Streaming
// requests get stream_options.include_usage=true injected so vLLM emits a
// final usage chunk; without that, agent traffic (which is ~all streaming)
// would meter as zero output tokens.
//
// Config is a flat JSON file, reloaded on SIGHUP. A bad reload logs and keeps
// the previous config -- it never exits, because SIGHUP on a live public
// gateway must not be a kill switch. The upstream proxy is rebuilt on reload
// so changing upstream_addr actually takes effect.
//
//	{
//	  "upstream_addr": "http://127.0.0.1:30098",
//	  "listen_addr":   ":8081",
//	  "ledger_path":   "/workspace/oaica-gateway-ledger.jsonl",
//	  "api_keys": [ {"sha256": "<hex>", "label": "openrouter"} ],
//	  "models": [ {
//	    "id": "kat-awq", "upstream_id": "kat-awq", "owned_by": "oaica",
//	    "context_length": 262144, "max_completion_tokens": 32768,
//	    "pricing": {"prompt": "0.00000005", "completion": "0.00000012"}
//	  }, {
//	    "id": "glm-awq", "upstream_id": "glm-4.6-awq", "owned_by": "oaica",
//	    "upstream_addr": "http://127.0.0.1:30099",
//	    "pricing": {"prompt": "0.0000001", "completion": "0.0000003"}
//	  } ]
//	}
//
// A model may carry its own "upstream_addr" when it lives behind a different
// vLLM/gatekeeper than the rest; omitted, it inherits the top-level one.
//
// Hash a key for the config with: printf '%s' "$KEY" | sha256sum
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"math"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"reflect"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
	"unicode/utf8"
)

// Legal/status pages are embedded so they ship with the binary and are
// served unauthenticated at /privacy, /terms, /status -- OpenRouter's
// provider form needs public URLs for them. Content lives in legal/*.md and
// is rendered as text/markdown; edit the files and rebuild to change them.
//
//go:embed legal/PRIVACY.md legal/TERMS.md legal/STATUS.md
var legalFS embed.FS

func legalHandler(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b, err := legalFS.ReadFile("legal/" + name)
		if err != nil {
			writeErr(w, http.StatusNotFound, "not_found", "page unavailable")
			return
		}
		w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=3600")
		w.Write(b)
	}
}

// maxBodyBytes caps a completion request body. A 262k-token context of text
// is ~1 MB; 16 MB leaves ample headroom for tool schemas and base64 images
// while refusing the 512 MB the previous version would have buffered.
const maxBodyBytes = 16 << 20

// nonStreamMaxTokens bounds max_tokens on NON-streaming completions so the
// response can complete inside Cloudflare's ~100 s time-to-first-byte limit
// (the proxy's own ResponseHeaderTimeout is now 600 s, so the edge is the
// binding constraint). Streaming is not bounded by this (only by the
// model's max_completion_tokens).
//
// 8192 was wrong: at the ~80 tok/s a stream gets under the 32-way cap that
// is ~102 s, and the ledger showed eight real 504s, every one non-stream at
// latency_ms 90000-90012 (2026-08-25). 4096 completes in ~51 s worst case.
const nonStreamMaxTokens = 4096

type gwPricing struct {
	Prompt     string `json:"prompt"`     // USD per token, decimal string (OpenRouter shape)
	Completion string `json:"completion"` // USD per token, decimal string
	// CachedPrompt: USD per prefix-cache-HIT prompt token, separate from
	// (and normally cheaper than) Prompt -- the same asymmetric pricing
	// every competitor checked in docs/PRICING.md uses (OpenAI, DeepSeek,
	// MiniMax all charge less for cache-hit input, since it costs them
	// near-nothing to serve). Empty = no discount, cached tokens bill at
	// the same rate as fresh ones (today's behavior, unchanged unless
	// this is explicitly set). Applies to ledgerEntry.CachedTokens, which
	// depends on the upstream actually populating
	// prompt_tokens_details.cached_tokens -- see that field's doc for why
	// it currently reads 0 on this vLLM build.
	CachedPrompt string `json:"cached_prompt,omitempty"`
}

// gwPricingTier is one bracket of context-tiered INPUT pricing (see
// gwModel.PricingTiers). UpToPromptTokens is the inclusive upper bound of
// the bracket in real prompt tokens; the final entry omits it (0) and is the
// catch-all for everything above the last bound.
type gwPricingTier struct {
	UpToPromptTokens int    `json:"up_to_prompt_tokens,omitempty"`
	Prompt           string `json:"prompt"` // USD per UNCACHED prompt token, decimal string
}

// selectPricingTier returns the bracket a request of promptTokens falls in,
// plus that bracket's upper bound (0 = the unbounded catch-all), which is
// what lands in ledgerEntry.PriceTier. tiers must already have passed
// validatePricingTiers (ascending bounds, exactly one unbounded final
// entry), so the catch-all is guaranteed to be reachable and the loop always
// returns.
func selectPricingTier(tiers []gwPricingTier, promptTokens int) (gwPricingTier, int, bool) {
	for _, t := range tiers {
		// Bound is INCLUSIVE: a 32000-token prompt is still "up to 32000".
		if t.UpToPromptTokens == 0 || promptTokens <= t.UpToPromptTokens {
			return t, t.UpToPromptTokens, true
		}
	}
	return gwPricingTier{}, 0, false
}

// validatePricingTiers enforces the shape the billing split depends on:
// every rate a positive decimal, bounds strictly ascending, and exactly one
// unbounded entry which must be last. A config that violates any of these
// would silently misprice real traffic (an unreachable bracket, or a prompt
// size no bracket covers), so it is rejected at load rather than papered
// over at request time -- consistent with loadConfig refusing any other
// config that would produce wrong-but-plausible behavior.
func validatePricingTiers(tiers []gwPricingTier) error {
	if len(tiers) == 0 {
		return nil
	}
	prevBound := 0
	for i, t := range tiers {
		v, err := strconv.ParseFloat(t.Prompt, 64)
		if err != nil {
			return fmt.Errorf("pricing_tiers[%d].prompt %q is not a decimal number", i, t.Prompt)
		}
		if !(v > 0) {
			return fmt.Errorf("pricing_tiers[%d].prompt %q must be positive", i, t.Prompt)
		}
		if t.UpToPromptTokens == 0 {
			if i != len(tiers)-1 {
				return fmt.Errorf("pricing_tiers[%d]: only the LAST entry may omit up_to_prompt_tokens (it is the catch-all)", i)
			}
			continue
		}
		if t.UpToPromptTokens < 0 {
			return fmt.Errorf("pricing_tiers[%d].up_to_prompt_tokens must be positive", i)
		}
		if t.UpToPromptTokens <= prevBound {
			return fmt.Errorf("pricing_tiers[%d].up_to_prompt_tokens %d must be strictly greater than the previous bound %d", i, t.UpToPromptTokens, prevBound)
		}
		prevBound = t.UpToPromptTokens
	}
	if tiers[len(tiers)-1].UpToPromptTokens != 0 {
		return errors.New("pricing_tiers: the last entry must omit up_to_prompt_tokens (catch-all for prompts above the highest bound)")
	}
	return nil
}

type gwModel struct {
	ID                  string    `json:"id"`
	UpstreamID          string    `json:"upstream_id,omitempty"` // vLLM --served-model-name; defaults to ID
	OwnedBy             string    `json:"owned_by"`
	ContextLength       int       `json:"context_length,omitempty"`
	MaxCompletionTokens int       `json:"max_completion_tokens,omitempty"`
	Pricing             gwPricing `json:"pricing"`
	// PricingTiers: optional context-tiered INPUT pricing. The real cost
	// driver on this fleet is UNCACHED PREFILL GPU-seconds, and prefill is
	// superlinear in context: a >128k-token prompt monopolizes chunked
	// prefill slots for many seconds and pushes everyone else's p95 out
	// (658 of 1429 requests on 2026-08-31 were >128k, p95 172 s). A single
	// flat per-token input rate therefore has short prompts subsidizing
	// long ones. When set, the bracket is chosen by the request's REAL
	// total prompt_tokens and its rate applies to that request's UNCACHED
	// prompt tokens as a WHOLE-REQUEST rate (not marginal/progressive
	// brackets -- the customer-facing statement is "a 200k-token request
	// costs $X/M input", which is what an agentic client can actually
	// reason about). Cached prompt tokens keep Pricing.CachedPrompt's flat
	// rate (a cache hit skips prefill entirely, so context size does not
	// change what it costs us) and completion keeps Pricing.Completion.
	// Empty = today's behavior: Pricing.Prompt flat for all sizes. When
	// present it takes precedence over Pricing.Prompt; Pricing stays
	// required because cached_prompt/completion still come from it.
	PricingTiers        []gwPricingTier `json:"pricing_tiers,omitempty"`
	SupportedParameters []string        `json:"supported_parameters,omitempty"`
	// InputModalities is what the model can actually consume: "text" and
	// optionally "image". Empty means text only. kat-awq's config claims a
	// vision tower, but under AWQ an image request produces garbage
	// ("!!!!!!!!" in the reasoning field, verified live 2026-08-26); the
	// gateway must refuse images for such a model rather than bill a
	// customer for noise.
	InputModalities []string `json:"input_modalities,omitempty"`
	Created         int64    `json:"created,omitempty"`
	// UpstreamAddr lets one gateway front models that live behind DIFFERENT
	// backends (a second vLLM on another port/host, a third-party OpenAI
	// endpoint) while keeping a single public catalog, key set and ledger.
	// Empty inherits gwConfig.UpstreamAddr, which is what every existing
	// config does -- this field is purely additive.
	UpstreamAddr string `json:"upstream_addr,omitempty"`
	// UpstreamKeyEnv names the environment variable holding the credential for
	// THIS model's upstream_addr. OAICA_GATEWAY_UPSTREAM_KEY is the credential for
	// the gateway-wide default upstream and is never sent to another one, so a
	// model on a different upstream that needs a key names it here
	// (2026-09-29 audit, round 109, F109-L3-3).
	UpstreamKeyEnv string `json:"upstream_key_env,omitempty"`
}

// distinctUpstreams counts the backends this config actually fans out to --
// logged at startup/reload so a config typo that silently splits traffic
// across two upstreams is visible without diffing the JSON.
func distinctUpstreams(cfg gwConfig) int {
	seen := map[string]bool{cfg.UpstreamAddr: true}
	for _, m := range cfg.Models {
		seen[m.upstreamAddr(cfg.UpstreamAddr)] = true
	}
	return len(seen)
}

// upstreamAddr resolves the backend this model is served from, falling back
// to the gateway-wide default.
func (m gwModel) upstreamAddr(defaultAddr string) string {
	if m.UpstreamAddr != "" {
		return m.UpstreamAddr
	}
	return defaultAddr
}

// upstreamKeyFor is the credential to present to the upstream at addr. The
// gateway-wide OAICA_GATEWAY_UPSTREAM_KEY belongs to the DEFAULT upstream
// (gatekeeper) and goes nowhere else: it was set on every request and probe
// whichever upstream a model routed to, and upstream_addr is documented as able
// to name a third-party endpoint, so the gatekeeper credential went to someone
// else. A model on another upstream names its own key with upstream_key_env; one
// that names none is sent none (2026-09-29 audit, round 109, F109-L3-3).
func upstreamKeyFor(cfg gwConfig, addr string) string {
	if addr == cfg.UpstreamAddr {
		return os.Getenv("OAICA_GATEWAY_UPSTREAM_KEY")
	}
	for _, m := range cfg.Models {
		if m.UpstreamAddr == addr && m.UpstreamKeyEnv != "" {
			return os.Getenv(m.UpstreamKeyEnv)
		}
	}
	return ""
}

func (m gwModel) acceptsImages() bool {
	for _, x := range m.InputModalities {
		if x == "image" {
			return true
		}
	}
	return false
}

// partIsImage reports whether a chat content part carries an image: either by
// the type the schema names, or by the payload key alone, which is the way the
// walk in inlineImageBytes recognises one. The part's type is optional on this
// wire and clients do omit it; the gate and the meter have to answer the same
// question about the same part, or a body holding a 1 MB screenshot is charged
// the 4 KB image allowance by the walk and seen as text by the gate that
// decides whether a text-only model must refuse it (2026-09-27 audit, round
// 37, B-F7).
func partIsImage(p map[string]any) bool {
	switch p["type"] {
	case "image_url", "input_image", "image":
		return true
	}
	_, ok := p["image_url"]
	return ok
}

// hasImageContent reports whether any message carries an image part in the
// OpenAI chat schema ({"type":"image_url"} or {"type":"input_image"}, and the
// payload-key-only spelling partIsImage also accepts).
func hasImageContent(req map[string]any) bool {
	msgs, _ := req["messages"].([]any)
	for _, mi := range msgs {
		m, _ := mi.(map[string]any)
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, pi := range parts {
			if p, ok := pi.(map[string]any); ok && partIsImage(p) {
				return true
			}
		}
	}
	return false
}

func (m gwModel) upstreamID() string {
	if m.UpstreamID != "" {
		return m.UpstreamID
	}
	return m.ID
}

type gwKey struct {
	SHA256 string `json:"sha256"` // hex digest of the plaintext key
	Label  string `json:"label"`  // written to the ledger; never the key itself
	// Priority: this key's requests bypass large-context admission control
	// entirely (never queued behind or rejected by the semaphore -- see
	// gwConfig.LargeContextTokenThreshold). Admission control exists to stop
	// a pile of >128k prompts from taking the replica down, and it works,
	// but it does so by making SOMEBODY wait; latency is the thing worth
	// selling, so priority is the product: a priority key is the one that
	// never waits, and the non-priority pool absorbs the queueing. Default
	// false = today's behavior for every existing key.
	Priority bool `json:"priority,omitempty"`
	// MaxCompletionTokens: optional per-key ceiling on output tokens,
	// applied AFTER the model-level clamp and only ever downward (a key can
	// never raise the published model limit). Lets a cheap/free tier be
	// sold with a smaller output budget without minting a separate model
	// entry. 0 = unset = no per-key clamp (today's behavior).
	MaxCompletionTokens int `json:"max_completion_tokens,omitempty"`
	// MaxConcurrent: per-key cap on CONCURRENT completions in flight
	// (streaming counts until its final byte). Why a second knob next to
	// gatekeeper tiers: gatekeeper limits by TIER (free 2 / pro 10 /
	// team 50 / internal 200), which is a blunt instrument — one key is
	// a whole customer, and a plan change or a runaway agent loop needs
	// a number set on THE key, not a tier re-shuffle that hits every key
	// of that tier. Enforced with a fast 429 + Retry-After (cheap to
	// retry, like the large-context admission path), before any upstream
	// work starts or tokens are billed. 0 = unset = no per-key cap
	// (tier limit still applies at gatekeeper). Priority keys are NOT
	// exempt from this one: admission control protects the fleet from
	// load, concurrency protects an account's bill from itself.
	MaxConcurrent int `json:"max_concurrent,omitempty"`
}

// priorityKeyCount is logged on load/reload so a config that accidentally
// marks everyone (or nobody) priority is visible without diffing the JSON --
// same reasoning as distinctUpstreams.
func priorityKeyCount(cfg gwConfig) int {
	n := 0
	for _, k := range cfg.APIKeys {
		if k.Priority {
			n++
		}
	}
	return n
}

type gwConfig struct {
	UpstreamAddr string    `json:"upstream_addr"`
	ListenAddr   string    `json:"listen_addr"`
	LedgerPath   string    `json:"ledger_path"`
	APIKeys      []gwKey   `json:"api_keys"`
	Models       []gwModel `json:"models"`

	// PullCatalog / PullLicenseKeys drive the weights-distribution routes
	// (/v1/manifest, /v1/pull, /v1/catalog) used by `oaica pull` — see
	// pull.go. They are independent of Models/APIKeys on purpose: what you
	// can download locally is not what this gateway serves for inference,
	// and a chat API key must never double as a weights-download license.
	PullCatalog     []gwPullEntry `json:"pull_catalog,omitempty"`
	PullLicenseKeys []gwKey       `json:"pull_license_keys,omitempty"`

	// UpstreamErrorLogPath: every non-2xx response from upstream (excluding
	// SSE streams, which already 200 by the time an error could occur mid-
	// stream) gets one JSONL line here: the real upstream error message
	// (previously only ever surfaced to the client, normalized, then
	// discarded), plus request_id/session_id/estimated prompt tokens/
	// max_tokens/model/backend for correlation. Built 2026-08-29 after a
	// real incident needed manual packet-capture surgery on a100b to find
	// out WHY a session's large agentic requests kept 400ing (root cause:
	// prompt_tokens + max_tokens exceeding max_model_len is invisible
	// anywhere else — the client gets a generic normalized message, the
	// local ledger only has prompt_tokens=0 since the request never
	// reached generation). Empty disables (default via defaultConfig()'s
	// non-empty value matches every other *Path field's "safe by default"
	// convention — this is meant to always be on in production).
	UpstreamErrorLogPath string `json:"upstream_error_log_path,omitempty"`

	// MeterHubAddr, when set, makes this gateway ALSO report every ledger
	// entry to a central meterhub instance (tools/meterhub) — async,
	// best-effort, never on the request's critical path. The local JSONL
	// ledger (LedgerPath) stays the durable per-box audit trail
	// regardless; meterhub is purely an aggregation convenience so
	// "how many tokens has key X used across every region" doesn't
	// require ssh-ing into every box and summing files by hand. Empty =
	// disabled, byte-identical to before meterhub existed.
	MeterHubAddr  string `json:"meterhub_addr"`
	MeterHubToken string `json:"meterhub_token"`
	// Region names this gateway in meterhub's records — "a100b" today,
	// a real region name once there's more than one.
	Region string `json:"region"`

	// EntitlementEnabled turns on the subscriber-status check (blocking
	// canceled/suspended keys) — see entitlementCache's doc. False by
	// default: an unconfigured gateway must never start rejecting
	// requests it didn't before. Requires MeterHubAddr to be set (the
	// subscriber table lives in meterhub).
	EntitlementEnabled bool `json:"entitlement_enabled"`
	// EntitlementFailOpen decides what happens when meterhub is
	// unreachable or a key has no subscriber record at all: true = serve
	// anyway (never let an aggregation-layer outage block real traffic);
	// false = block anything not explicitly known-active (matches "block
	// unsubscribed users" literally — an unrecognized key is NOT a
	// subscriber). Default false (fail closed) once EntitlementEnabled is
	// true, since the whole point of enabling this is refusing unknown
	// keys.
	EntitlementFailOpen bool `json:"entitlement_fail_open"`
	// EntitlementCacheTTLSec bounds how stale a cached subscriber status
	// can be before the next request for that key re-checks meterhub.
	// Default 60. This is what keeps the per-request check fast — it
	// reads an in-memory map, not a network call, on every request
	// except the first (or first-after-expiry) for each key.
	EntitlementCacheTTLSec int `json:"entitlement_cache_ttl_sec"`
	// EntitlementOverageBilling: when true, a subscriber over their plan's
	// rolling-window cap (docs/PRICING.md's "real throttle" column) is let
	// through instead of blocked with 429 -- the request is served and
	// flagged Overage=true on the ledger (ledgerEntry.Overage) for a
	// billing job to charge at the overage rate. See
	// entitlementCache.overageBilling's doc. Default false: a hard block
	// stays the default behavior for anyone with EntitlementEnabled
	// already on, since flipping this silently would let a canceled-cap
	// key keep consuming without warning.
	EntitlementOverageBilling bool `json:"entitlement_overage_billing"`

	// LargeContextTokenThreshold / MaxConcurrentLargeContext: admission
	// control for the failure mode found 2026-08-29 — several 140K-190K
	// prompt-token requests landing on the same replica at once backed up
	// its scheduler badly enough that OTHER concurrent requests started
	// 502ing/504ing (real ledger evidence: 6x502 + 2x504 in a 24s window,
	// alongside 4 successful-but-46-72s-latency completions carrying
	// 140K-190K prompt tokens each). This is a coarse, cheap admission
	// gate: any request whose estimated prompt size (message content
	// bytes / 4, not a real tokenizer call — good enough to catch "this
	// is huge", not meant to be exact) is at or above the threshold
	// competes for a small bounded slot pool BEFORE it reaches the
	// scheduler, rather than piling in unbounded and taking down
	// unrelated concurrent requests with it. A request that can't get a
	// slot gets a fast 429 (cheap to retry) instead of a slow 502/504
	// (expensive: the upstream had already started real work). Normal
	// (non-large) requests are completely unaffected — this only gates
	// the specific request shape that caused the incident.
	// LargeContextTokenThreshold: default 50000 (0 uses the default; set
	// to a negative value to disable admission control entirely).
	LargeContextTokenThreshold int `json:"large_context_token_threshold"`
	// MaxConcurrentLargeContext: default 2 (0 uses the default).
	MaxConcurrentLargeContext int `json:"max_concurrent_large_context"`
	// RequestTimeoutSec bounds ONE completion's total wall clock, headers
	// through final stream byte. Default 900 (15 min); negative disables.
	// Why: 2026-08-31 ~03:27 UTC, one replica's oldest in-flight request
	// was 2337 s old — a wedged/stalled generation that pinned a seq slot
	// and KV for ~39 minutes until the replica watchdog killed the whole
	// engine, taking every other request with it. Individual generations
	// never legitimately run that long at our max output (32k tokens /
	// ~80 tok/s ≈ 7 min); anything past 15 min is a wedge the CLIENT has
	// abandoned or that will never finish. Aborting the request closes
	// its upstream connection, so vLLM preempts/finishes that sequence
	// and frees the slot in seconds instead of the engine-level kill.
	// Set above the worst legitimate case (slow decode under load),
	// which measured p95 is 180 s — 900 s is ~5x headroom.
	RequestTimeoutSec int `json:"request_timeout_sec"`
}

func defaultConfig() gwConfig {
	return gwConfig{
		UpstreamAddr:         "http://127.0.0.1:30098",
		ListenAddr:           ":8081",
		LedgerPath:           "/workspace/oaica-gateway-ledger.jsonl",
		UpstreamErrorLogPath: "/workspace/oaica-gateway-upstream-errors.jsonl",
	}
}

// loadConfig returns an error instead of exiting so a reload can refuse a bad
// file and keep serving with the previous config.
func loadConfig(path string) (gwConfig, error) {
	cfg := defaultConfig()
	if path == "" {
		return cfg, errors.New("no --config given")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.UpstreamAddr == "" {
		cfg.UpstreamAddr = defaultConfig().UpstreamAddr
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultConfig().ListenAddr
	}
	if cfg.LedgerPath == "" {
		cfg.LedgerPath = defaultConfig().LedgerPath
	}
	if cfg.UpstreamErrorLogPath == "" {
		cfg.UpstreamErrorLogPath = defaultConfig().UpstreamErrorLogPath
	}
	if len(cfg.APIKeys) == 0 {
		return cfg, errors.New("api_keys is empty: refusing a config that would accept nobody")
	}
	for i, k := range cfg.APIKeys {
		if strings.TrimSpace(k.Label) == "" {
			return cfg, fmt.Errorf("api_keys[%d]: label must be non-empty (empty labels authenticate by digest but then 401 and unmeter)", i)
		}
		if len(k.SHA256) != 64 {
			return cfg, fmt.Errorf("api_keys[%d]: sha256 must be 64 hex chars (got %d)", i, len(k.SHA256))
		}
		if _, err := hex.DecodeString(k.SHA256); err != nil {
			return cfg, fmt.Errorf("api_keys[%d]: sha256 is not hex: %w", i, err)
		}
	}
	if len(cfg.Models) == 0 {
		return cfg, errors.New("models is empty")
	}
	if _, err := url.Parse(cfg.UpstreamAddr); err != nil {
		return cfg, fmt.Errorf("upstream_addr %q: %w", cfg.UpstreamAddr, err)
	}
	for i, m := range cfg.Models {
		if m.UpstreamAddr == "" {
			continue
		}
		if _, err := url.Parse(m.UpstreamAddr); err != nil {
			return cfg, fmt.Errorf("models[%d].upstream_addr %q: %w", i, m.UpstreamAddr, err)
		}
	}
	for i, m := range cfg.Models {
		if err := validatePricingTiers(m.PricingTiers); err != nil {
			return cfg, fmt.Errorf("models[%d] (%s): %w", i, m.ID, err)
		}
	}
	if err := validatePullConfig(cfg); err != nil {
		return cfg, err
	}
	return cfg, nil
}

// ctxKeyBackend is the request-context key ModifyResponse uses to hand the
// serving replica's address back to completionHandler — see newProxy's
// ModifyResponse for why this indirection exists (ModifyResponse only sees
// *http.Response, not the ledger-building code in completionHandler).
type ctxKeyBackend struct{}

// ctxKeyErrCapture carries the request-side context an upstream error gets
// logged with (see gwConfig.UpstreamErrorLogPath) -- same indirection
// reason as ctxKeyBackend: ModifyResponse only sees *http.Response.
type ctxKeyErrCapture struct{}

// errCaptureInfo is deliberately NOT the full request body -- storing every
// large agentic prompt verbatim on every 400 would be its own liability
// (disk, secrets in tool args). What actually answers "why did this fail"
// is the upstream's own error message (captured separately, see
// onUpstreamError) correlated against these cheap, already-computed
// numbers -- e.g. prompt_tokens + max_tokens exceeding max_model_len shows
// up immediately as EstTokens+MaxTokens close to or past the model's
// context_length, without needing the message content at all.
type errCaptureInfo struct {
	RequestID string
	SessionID string
	Model     string
	EstTokens int
	MaxTokens int
	ReqBytes  int
	// Calibrate, when non-nil, records a REAL prompt-token count for this
	// request's session (see context_calibration.go). An upstream
	// context-overflow 400 states the true message-token count, which is
	// better ground truth than any estimate we can compute -- feeding it
	// back means the very next request of that session gets a measured
	// budget instead of a chars/4 guess.
	Calibrate func(promptTokens int)
}

// newProxy builds a reverse proxy with its own transport and explicit
// timeouts. No overall client timeout: streamed completions legitimately
// run for minutes.
//
// ResponseHeaderTimeout is 600 s, raised from 90 s after a real incident
// (2026-08-30 13:46-14:04 UTC): twelve consecutive 504s at ~90.6 s each on
// one 424 KB conversation. Nothing was wedged -- prefill of ~110k uncached
// tokens under load simply took longer than 90 s, and because every Claude
// Code retry re-prefilled from scratch, each retry timed out identically.
// The arithmetic makes 90 s indefensible as an upstream budget: prefill of
// a full 262k-token prompt at ~2k tok/s is ~130 s on its own, and queueing
// behind other replicas' prefills multiplies that. 600 s still bounds a
// genuinely stuck upstream, because it sits above the replica watchdog's
// own 300 s stall threshold -- the watchdog kills the wedged replica first,
// which closes the connection and fails the request well before 600 s.
//
// This does NOT help the public Cloudflare path, which imposes its own
// ~100 s time-to-first-byte limit that no server-side timeout can raise;
// LAN and tunnel clients hitting :8081 directly are the ones this fixes.
// Surviving the edge limit needs early SSE headers plus keepalive comments,
// which is a separate change.
// onUpstreamError is called for every non-2xx, non-SSE upstream response,
// with the real (untruncated-by-normalization) error message and the
// errCaptureInfo stashed in the request's context, if any (nil when the
// request never reached the point where completionHandler sets it, e.g. a
// request rejected before that point). nil disables logging entirely.
func newProxy(upstream string, onUpstreamError func(info *errCaptureInfo, status int, code, msg string)) (*httputil.ReverseProxy, error) {
	u, err := url.Parse(upstream)
	if err != nil {
		return nil, err
	}
	p := httputil.NewSingleHostReverseProxy(u)
	p.Transport = &http.Transport{
		DialContext:           (&net.Dialer{Timeout: 5 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		ResponseHeaderTimeout: 600 * time.Second,
		IdleConnTimeout:       90 * time.Second,
		MaxIdleConnsPerHost:   64,
	}
	p.FlushInterval = -1 // flush every write: required for SSE streaming
	p.ModifyResponse = func(resp *http.Response) error {
		// Capture which replica actually served this request (oaicalb sets
		// X-Katlb-Backend) into the request context BEFORE deleting it —
		// completionHandler reads it back out after ServeHTTP returns to
		// attribute the ledger row to a GPU. See ctxKeyBackend's doc.
		if b, ok := resp.Request.Context().Value(ctxKeyBackend{}).(*string); ok {
			*b = resp.Header.Get("X-Katlb-Backend")
		}
		// Internal topology must not leak to the public (audit L16: Via and
		// Server name internal hops too).
		resp.Header.Del("X-Katlb-Backend")
		resp.Header.Del("X-Gatekeeper-Tier")
		resp.Header.Del("X-Gatekeeper-Limit")
		resp.Header.Del("Via")
		resp.Header.Del("Server")
		// The five above were the hops this gateway knew of on the day it was
		// written; every header an upstream adds afterwards reached callers, on
		// both doors. A cookie a model's own upstream sets landed on the public
		// origin, and X-Powered-By, an X-Upstream-* or X-Gatekeeper-* label, an
		// Alt-Svc, or a redirect to an internal address named the inside
		// (2026-09-29 audit, round 109, F109-L3-1). A denylist by name and by
		// prefix rather than an allowlist, because clients read headers this
		// gateway cannot enumerate (rate-limit families, request ids, content
		// negotiation) and a dropped one breaks them silently.
		for _, h := range []string{"Set-Cookie", "X-Powered-By", "Alt-Svc", "Location"} {
			resp.Header.Del(h)
		}
		for k := range resp.Header {
			for _, prefix := range []string{"X-Gatekeeper-", "X-Katlb-", "X-Upstream-"} {
				if strings.HasPrefix(http.CanonicalHeaderKey(k), prefix) {
					resp.Header.Del(k)
				}
			}
		}
		// gatekeeper (429/401) and katlb (503) answer with text/plain or a
		// non-OpenAI JSON; normalize so clients see {"error":{...}} and keep
		// Retry-After. Streaming bodies are never rewritten (status 200).
		if resp.StatusCode >= 400 && !strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
			raw, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			msg := strings.TrimSpace(string(raw))
			var probe struct {
				Error any `json:"error"`
			}
			if json.Unmarshal(raw, &probe) == nil {
				if em, ok := probe.Error.(map[string]any); ok {
					if s, ok := em["message"].(string); ok {
						msg = s
					}
				} else if s, ok := probe.Error.(string); ok {
					msg = s
				}
			}
			code := "upstream_error"
			switch resp.StatusCode {
			case http.StatusTooManyRequests:
				code = "rate_limit_exceeded"
				if resp.Header.Get("Retry-After") == "" {
					resp.Header.Set("Retry-After", "1")
				}
			case http.StatusServiceUnavailable, http.StatusBadGateway, http.StatusGatewayTimeout:
				code = "server_error"
				if resp.Header.Get("Retry-After") == "" {
					resp.Header.Set("Retry-After", "2")
				}
			case http.StatusBadRequest:
				code = "invalid_request_error"
			}
			var info *errCaptureInfo
			if v, ok := resp.Request.Context().Value(ctxKeyErrCapture{}).(*errCaptureInfo); ok {
				info = v
			}
			// An upstream context overflow ("This model's maximum context
			// length is N tokens. However, you requested M tokens (K in the
			// messages, ...)") is not an opaque backend failure -- it is the
			// exact condition the context-fit clamp tries to predict, and it
			// arrives with the REAL numbers. Rewrite it into Anthropic's
			// "prompt is too long: K tokens > N maximum" so an Anthropic-
			// shaped client (Claude Code, through cmd/launch's proxy) takes
			// its context-recovery path instead of retrying the identical
			// doomed request -- the 2026-08-29 compaction loop -- and feed K
			// back into this session's calibration. The error LOG keeps the
			// verbatim upstream text -- that sink exists for diagnosis, and
			// the raw numbers are the whole point of it.
			clientMsg := redactCredentialURLs(msg)
			if resp.StatusCode == http.StatusBadRequest {
				if promptTokens, maxTokens, ok := parseUpstreamContextOverflow(msg); ok {
					if info != nil && info.Calibrate != nil {
						info.Calibrate(promptTokens)
					}
					clientMsg = promptTooLongMessage(promptTokens, maxTokens)
				}
			}
			if onUpstreamError != nil {
				onUpstreamError(info, resp.StatusCode, code, msg)
			}
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": clientMsg, "type": code, "code": code}})
			resp.Body = io.NopCloser(bytes.NewReader(b))
			resp.ContentLength = int64(len(b))
			resp.Header.Set("Content-Type", "application/json")
			resp.Header.Set("Content-Length", fmt.Sprint(len(b)))
		}
		return nil
	}
	p.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		status := http.StatusBadGateway
		if errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "timeout") {
			status = http.StatusGatewayTimeout
		}
		// Real gap found 2026-08-29: a connection-level failure (dial
		// refused/timeout, no healthy backend) never reaches ModifyResponse
		// -- it's handled entirely here instead -- so it was invisible to
		// UpstreamErrorLogPath despite being a real upstream failure, only
		// findable by grepping the raw ledger for a bare status code with
		// no message. Log it the same way as a ModifyResponse-path error so
		// every upstream failure lands in one place.
		if onUpstreamError != nil {
			var info *errCaptureInfo
			if v, ok := r.Context().Value(ctxKeyErrCapture{}).(*errCaptureInfo); ok {
				info = v
			}
			onUpstreamError(info, status, "upstream_error", "upstream unavailable: "+err.Error())
		}
		w.Header().Set("Retry-After", "2")
		// Client body stays generic (2026-09-01 security audit M2): err.Error()
		// embeds the full internal upstream URL. The detail already went to
		// the operator sink above; echoing it to public callers leaks topology.
		writeErr(w, status, "upstream_error", "upstream unavailable")
	}
	return p, nil
}

type gateway struct {
	mu    sync.RWMutex
	cfg   gwConfig
	proxy *httputil.ReverseProxy // the default (top-level upstream_addr) proxy
	// proxies holds one reverse proxy per DISTINCT upstream address, so a
	// model with its own upstream_addr reuses connections/transport state
	// per backend instead of getting a fresh proxy per request. Always
	// contains the default address; rebuilt wholesale on every apply().
	proxies map[string]*httputil.ReverseProxy
	// byID indexes models by public id AND by "<owned_by>/<id>" so callers
	// may use either "kat-awq" or "oaica/kat-awq".
	byID map[string]gwModel

	ledgerMu sync.Mutex
	ledger   *os.File

	// errLog is the upstream-error JSONL sink -- see
	// gwConfig.UpstreamErrorLogPath's doc. nil (path empty) disables it,
	// though defaultConfig() always fills a path in practice.
	errLogMu sync.Mutex
	errLog   *os.File

	// largeContextThreshold / largeContextSem: see gwConfig's doc on the
	// same fields. -1 threshold means admission control is disabled
	// (largeContextSem is nil in that case too). Guarded by mu since
	// apply() can rebuild them on a config reload.
	// largeContextThreshold / largeContextSem: see gwConfig's doc on the
	// same fields. -1 threshold means admission control is disabled
	// (largeContextSem is nil in that case too). Guarded by mu since
	// apply() can rebuild them on a config reload.
	largeContextThreshold int
	largeContextSem       chan struct{}

	// requestTimeout bounds a single completion's wall clock; see
	// gwConfig.RequestTimeoutSec's doc. 0 = disabled. Guarded by mu
	// (reload can change it via apply()).
	requestTimeout time.Duration

	// keyInflight tracks each key's concurrent completions for
	// gwKey.MaxConcurrent enforcement; label -> *atomic.Int32,
	// created on first sight. Labels come only from config keys, so the
	// map is bounded and never needs eviction.
	keyInflight sync.Map

	// calib holds per-session (request bytes -> real prompt_tokens) pairs
	// for the context-fit clamp; see context_calibration.go. Lazily built
	// via calibrator() because &gateway{} is constructed directly in a few
	// places (main, tests) with no constructor to hook.
	calibOnce sync.Once
	calib     *promptCalibrator

	// /health probe cache; see healthCacheTTL.
	healthMu   sync.Mutex
	healthAt   time.Time
	healthLast healthResult
	// lastSuccessAt: last time a real routed completion returned 200 with
	// usage (set where the calibrator records; see completionHandler). Lets
	// /health answer "ok (recent traffic)" even when the probe ITSELF
	// starves: under a giant-prefill storm the 1-token probe queues behind
	// 18 real seqs and can time out at ANY timeout (25s tested, measured
	// storms run 3+ min) while customer completions keep succeeding —
	// reporting "down" then is crying wolf exactly when the fleet is at
	// capacity. Serving proven working within healthTrafficFreshness is
	// stronger evidence of health than a synthetic probe. Guarded by
	// healthMu.
	lastOKAt atomic.Int64 // unix seconds of the last successful completion

	// meterCh feeds the background reporter goroutine (see
	// startMeterReporter); nil when MeterHubAddr is unset. Buffered and
	// non-blocking on send — a full channel means meterhub is unreachable
	// for a while, and the right response is to drop that report (the
	// local JSONL ledger already has it) rather than let a billing
	// side-channel add latency or backpressure to real chat completions.
	meterCh chan usageReport
	// meterDone stops the reporter goroutine that drains meterCh — see the
	// reload comment in apply(): runMeterReporter ranges over meterCh, so
	// replacing the channel without closing it leaked one goroutine per reload.
	meterDone chan struct{}

	// entitlement is the subscriber-status cache — see entitlementCache's
	// doc. nil when EntitlementEnabled is false.
	entitlement *entitlementCache
}

func (g *gateway) apply(cfg gwConfig) error {
	// Build every distinct upstream up front: a reload that names a bad
	// address must be rejected as a whole, not discovered per-request.
	proxies := make(map[string]*httputil.ReverseProxy, 1+len(cfg.Models))
	newFor := func(addr string) error {
		if _, ok := proxies[addr]; ok {
			return nil
		}
		p, err := newProxy(addr, g.logUpstreamError)
		if err != nil {
			return err
		}
		proxies[addr] = p
		return nil
	}
	if err := newFor(cfg.UpstreamAddr); err != nil {
		return err
	}
	for _, m := range cfg.Models {
		if err := newFor(m.upstreamAddr(cfg.UpstreamAddr)); err != nil {
			return err
		}
	}
	p := proxies[cfg.UpstreamAddr]
	byID := make(map[string]gwModel, len(cfg.Models)*2)
	for _, m := range cfg.Models {
		byID[m.ID] = m
		if m.OwnedBy != "" {
			byID[m.OwnedBy+"/"+m.ID] = m
		}
	}
	f, err := os.OpenFile(cfg.LedgerPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("open ledger %s: %w", cfg.LedgerPath, err)
	}
	var ef *os.File
	if cfg.UpstreamErrorLogPath != "" {
		ef, err = os.OpenFile(cfg.UpstreamErrorLogPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
		if err != nil {
			f.Close() // don't leak the already-opened ledger (audit L15)
			return fmt.Errorf("open upstream error log %s: %w", cfg.UpstreamErrorLogPath, err)
		}
	}
	g.mu.Lock()
	g.cfg = cfg
	g.proxy = p
	g.proxies = proxies
	g.byID = byID
	g.mu.Unlock()
	g.ledgerMu.Lock()
	if g.ledger != nil {
		g.ledger.Close()
	}
	g.ledger = f
	g.ledgerMu.Unlock()
	g.errLogMu.Lock()
	if g.errLog != nil {
		g.errLog.Close()
	}
	g.errLog = ef
	g.errLogMu.Unlock()

	// (Re)start the meter reporter on every apply — a reload that changes
	// MeterHubAddr must take effect without a process restart, same as
	// every other config field here. Each call gets its own channel and
	// goroutine, and the OLD one is STOPPED here: runMeterReporter ranges over
	// its channel, so it exits when that channel closes — and nothing closed
	// it. The comment this replaces claimed the old goroutine "keeps draining
	// its own now-orphaned channel until it empties and exits on its own",
	// which it cannot: nobody sends on that channel any more and nobody closes
	// it, so every reload parked one more goroutine on a receive that never
	// returns (2026-09-27 audit, round 25). meterDone is that stop signal, and
	// the channel itself is deliberately NOT closed: a sender that read it
	// under RLock can still be in its send when the lock is dropped, and a send
	// on a closed channel panics — the reporter's select on done and ch keeps
	// the exit prompt without making the send unsafe.
	//
	// meterCh and entitlement are read under RLock by reportUsage and the
	// completion path, so the writes happen under the same lock (2026-09-01
	// security audit M4): a SIGHUP reload raced live requests before.
	g.mu.Lock()
	if g.meterDone != nil {
		close(g.meterDone)
		g.meterDone = nil
	}
	// Whatever the retired reporter had not yet delivered is taken out of its
	// channel and handed to the reporter that replaces it. It used to be
	// dropped in silence: a reload with reports queued retired the consumer and
	// left them in a channel nobody would ever read, so the aggregated view at
	// meterhub was missing records the local ledger had, and nothing said so
	// (2026-09-27 audit, round 27, B-C). Closing the channel is not the
	// alternative — a sender that read it under RLock can still be in its send,
	// and that panics.
	stranded := drainMeterChannel(g.meterCh)
	if cfg.MeterHubAddr != "" {
		g.meterCh = make(chan usageReport, 256)
		g.meterDone = make(chan struct{})
		go runMeterReporter(g.meterCh, g.meterDone, cfg.MeterHubAddr, cfg.MeterHubToken, meterReporterBackoff)
	} else {
		g.meterCh = nil
	}
	droppedReports := requeueMeterReports(g.meterCh, stranded)

	if cfg.EntitlementEnabled && cfg.MeterHubAddr != "" {
		ttl := time.Duration(cfg.EntitlementCacheTTLSec) * time.Second
		if ttl <= 0 {
			ttl = 60 * time.Second
		}
		g.entitlement = newEntitlementCache(cfg.MeterHubAddr, cfg.MeterHubToken, ttl, cfg.EntitlementFailOpen, cfg.EntitlementOverageBilling)
	} else {
		g.entitlement = nil
	}
	g.mu.Unlock()

	// Said OUTSIDE the lock, which is the point: a log write is an unbounded
	// write to whatever stderr points at, and under the WRITE lock it froze
	// every completion on the box — a pending writer blocks new readers
	// outright (2026-09-27 audit, round 30, B1; the same shape round 29 fixed in
	// reportUsage, one function away). The count is a plain int, so nothing is
	// needed to carry it out.
	if droppedReports > 0 {
		log.Printf("oaica-gateway: reload dropped %d queued usage report(s) that the reporter it replaced had not delivered (the local ledger still has them)", droppedReports)
	}

	// Large-context admission control — see gwConfig.LargeContextTokenThreshold's
	// doc. A negative threshold disables it; everything else gets sane
	// defaults. Rebuilt on every apply so a config reload changes the pool
	// size immediately — any request already holding a slot keeps it
	// (the old channel drains naturally), matching the meter reporter's
	// own reload pattern above.
	threshold := cfg.LargeContextTokenThreshold
	if threshold == 0 {
		threshold = 50_000
	}
	maxConcurrent := cfg.MaxConcurrentLargeContext
	if maxConcurrent <= 0 {
		maxConcurrent = 2
	}
	g.mu.Lock()
	if threshold < 0 {
		g.largeContextThreshold = -1
		g.largeContextSem = nil
	} else {
		g.largeContextThreshold = threshold
		g.largeContextSem = make(chan struct{}, maxConcurrent)
	}
	// Per-request wall-clock cap — see gwConfig.RequestTimeoutSec's doc.
	// 0 = default (15 min); negative = disabled.
	if cfg.RequestTimeoutSec > 0 {
		g.requestTimeout = time.Duration(cfg.RequestTimeoutSec) * time.Second
	} else if cfg.RequestTimeoutSec < 0 {
		g.requestTimeout = 0
	} else {
		g.requestTimeout = 900 * time.Second
	}
	g.mu.Unlock()
	return nil
}

// entitlementSnapshot reads the current entitlement cache under the lock.
//
// The completion path asked for this field directly, outside every lock scope,
// while apply() replaces it under the write lock on every reload — a SIGHUP
// flipping EntitlementEnabled raced a live request (2026-09-27 audit, round 25,
// -race proof in the round's test file). A snapshot is the whole fix: the cache
// is immutable once built, so the handler can use it after releasing the lock.
func (g *gateway) entitlementSnapshot() *entitlementCache {
	g.mu.RLock()
	defer g.mu.RUnlock()
	return g.entitlement
}

// usageReport is what gets sent to meterhub's POST /ingest — same shape as
// ledgerEntry plus the region label meterhub needs to tell gateways apart.
type usageReport struct {
	ledgerEntry
	Region string `json:"region"`
}

// meterReporterBackoff is the delay before retry N (1-indexed); a package
// var so tests can shrink it to keep a deliberately-unreachable-meterhub
// test from leaving a real multi-second retry loop running past the test
// function's return (it was measurably making unrelated wall-clock-
// sensitive tests flakier under `go test ./...` before this existed).
//
// It is read ONCE, by the caller that starts the reporter, and passed to the
// goroutine as an argument (runMeterReporter's backoff parameter). Read inside
// the goroutine instead, it was a package var written by a test's t.Cleanup
// while that goroutine was still retrying, which is a real data race the
// round-25 `-race` run caught (2026-09-27 audit, round 25).
var meterReporterBackoff = func(attempt int) time.Duration { return time.Duration(attempt) * time.Second }

// runMeterReporter drains ch and POSTs each report to meterhub, retrying
// a failed send a bounded number of times with backoff before giving up on
// that one record (the local JSONL ledger already has it durably — a
// meterhub outage can never lose billing data, only delay the aggregated
// view). Exits when ch is closed and drained, or when done is closed — which
// is how apply() retires the reporter of the config it is replacing; see the
// reload comment there for why ch is not the stop signal.
func runMeterReporter(ch <-chan usageReport, done <-chan struct{}, addr, token string, backoff func(int) time.Duration) {
	client := &http.Client{Timeout: 5 * time.Second}
	for {
		var rep usageReport
		var ok bool
		select {
		case <-done:
			return
		case rep, ok = <-ch:
			if !ok {
				return
			}
		}
		body, err := json.Marshal(rep)
		if err != nil {
			// The record is in the local ledger either way; this is the one
			// failure that would otherwise leave no trace at all here.
			log.Printf("oaica-gateway: could not encode the meterhub report for %s (%v); it stays in the local ledger", rep.RequestID, err)
			continue
		}
		if err := deliverMeterReport(client, done, addr, token, body, backoff); err != nil {
			// Giving up used to be silent: the retry loop exhausted its attempts
			// and moved on, so a meterhub outage longer than the backoff left a
			// record in the local ledger and NOTHING else — the aggregate at
			// meterhub was short of it and no line said so, which is the silence
			// round 27 removed from the reload path (2026-09-27 audit, round 29,
			// B2). The ledger still has it, so this is a delay in the aggregate
			// view and the operator is the one who can act on it.
			log.Printf("oaica-gateway: gave up on the meterhub report for %s: %v (it stays in the local ledger, so only the aggregate view is without it)", rep.RequestID, err)
		}
	}
}

// deliverMeterReport POSTs one already-encoded report to meterhub, retrying a
// bounded number of times with backoff, and returns the reason it was not
// delivered (nil when it was).
//
// Retrying stops promptly when done fires: the reporter is retired by a reload
// that has already replaced the config it was built from, so sleeping out the
// rest of the backoff only delays the line the caller writes about it. The
// record itself is not lost — the local ledger has it.
func deliverMeterReport(client *http.Client, done <-chan struct{}, addr, token string, body []byte, backoff func(int) time.Duration) error {
	const maxAttempts = 3
	lastErr := errors.New("no attempt was made")
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		req, err := http.NewRequest(http.MethodPost, strings.TrimRight(addr, "/")+"/ingest", bytes.NewReader(body))
		if err != nil {
			return err
		}
		req.Header.Set("Content-Type", "application/json")
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		resp, err := client.Do(req)
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
				return nil
			}
			lastErr = fmt.Errorf("meterhub answered %s", resp.Status)
		} else {
			lastErr = err
		}
		if attempt == maxAttempts {
			break
		}
		select {
		case <-done:
			return fmt.Errorf("%w; the gateway reloaded while it was being retried", lastErr)
		case <-time.After(backoff(attempt)):
		}
	}
	return fmt.Errorf("after %d attempts: %w", maxAttempts, lastErr)
}

// drainMeterChannel takes every report currently queued on ch, in order, and
// returns them. Called while apply() holds the write lock, so no sender is
// between its read of g.meterCh and its send: the reports it returns are the
// whole of what the retired reporter had left.
func drainMeterChannel(ch chan usageReport) []usageReport {
	if ch == nil {
		return nil
	}
	var out []usageReport
	for {
		select {
		case rep := <-ch:
			out = append(out, rep)
		default:
			return out
		}
	}
}

// requeueMeterReports moves reports onto the reporter that replaced the one
// that queued them, in order and without blocking (a full channel is the
// condition reportUsage already drops for). It returns how many could NOT be
// handed over.
func requeueMeterReports(ch chan usageReport, reports []usageReport) int {
	if len(reports) == 0 {
		return 0
	}
	dropped := 0
	for _, rep := range reports {
		if ch == nil {
			dropped++
			continue
		}
		select {
		case ch <- rep:
		default:
			dropped++
		}
	}
	return dropped
}

// reportUsage sends e to the meter reporter's channel, non-blocking. A full
// channel (meterhub down/slow for a while) drops the report rather than
// stalling the request that triggered it — see meterCh's doc.
//
// The read of g.meterCh and the send are ONE critical section, which is what
// drainMeterChannel's doc claims of them. apply() drains the channel it is
// about to retire while holding the write lock, so a sender that dropped the
// read lock in between had its report land in the retired channel: never
// delivered by any reporter, and never counted as dropped either, so the only
// trace was a record missing from meterhub's aggregate — the exact silence
// round 27 set out to remove (2026-09-27 audit, round 28, B-F1). The send is a
// non-blocking select, so the read lock is held for a bounded time.
func (g *gateway) reportUsage(e ledgerEntry) {
	// The read of g.meterCh, the send, and the reading of g.cfg.Region are ONE
	// critical section, and the log line is NOT: a log write is an unbounded
	// write to whatever sink stderr is pointed at, and holding the read lock
	// across it — which the line below used to be written under — let a slow
	// sink stall every completion on the box for as long as the write took
	// (2026-09-27 audit, round 29, B1).
	queued, hasReporter := func() (bool, bool) {
		g.mu.RLock()
		defer g.mu.RUnlock()
		ch := g.meterCh
		if ch == nil {
			return false, false
		}
		if meterReportSendHook != nil {
			meterReportSendHook()
		}
		select {
		case ch <- usageReport{ledgerEntry: e, Region: g.cfg.Region}:
			return true, true
		default:
			return false, true
		}
	}()
	if !queued && hasReporter {
		log.Printf("oaica-gateway: meterhub report channel full, dropping report for %s (local ledger still has it)", e.RequestID)
	}
}

// meterReportSendHook, when non-nil, runs inside reportUsage's critical
// section, immediately before the send. A test seam, same shape as
// meterReporterBackoff: it lets a test hold a report between its read of
// g.meterCh and its send and observe that a reload's swap cannot proceed in
// that window.
var meterReportSendHook func()

// calibrator returns the per-session prompt-size calibrator, building it on
// first use. Bounded at maxCalibratedSessions -- the gateway sees every
// client's sessions, so this map must never grow without limit.
func (g *gateway) calibrator() *promptCalibrator {
	g.calibOnce.Do(func() { g.calib = newPromptCalibrator(maxCalibratedSessions) })
	return g.calib
}

func (g *gateway) reload(path string) {
	cfg, err := loadConfig(path)
	if err != nil {
		log.Printf("oaica-gateway: reload REJECTED, keeping previous config: %v", err)
		return
	}
	if err := g.apply(cfg); err != nil {
		log.Printf("oaica-gateway: reload REJECTED, keeping previous config: %v", err)
		return
	}
	log.Printf("oaica-gateway: config reloaded: %d models, %d keys (%d priority), upstream=%s (%d distinct upstreams)",
		len(cfg.Models), len(cfg.APIKeys), priorityKeyCount(cfg), cfg.UpstreamAddr, distinctUpstreams(cfg))
}

// keyLabel returns the label for a valid Bearer key, or "" if unauthenticated.
// Constant-time compare of the presented key's digest against every stored
// digest so timing does not leak which key was closest.
func (g *gateway) keyLabel(r *http.Request) string {
	k, _ := g.lookupKey(r)
	return k.Label
}

// redactCredentialUrlsRE matches URLs with embedded userinfo, the one shape
// in upstream error text that can carry a secret (audit 2026-09-01 L17).
var redactCredentialUrlsRE = regexp.MustCompile(`[a-zA-Z][a-zA-Z0-9+.-]*://[^/\s@]+:[^@\s/]+@`)

// redactCredentialUrls strips userinfo from any URL embedded in upstream
// error text before that text reaches the public client (audit L17: a
// misconfigured base_url with embedded credentials would otherwise echo
// them into the client's error). Everything else passes verbatim — the
// message is still the most useful explanation of the failure.
func redactCredentialURLs(text string) string {
	return redactCredentialUrlsRE.ReplaceAllStringFunc(text, func(m string) string {
		if i := strings.Index(m, "://"); i >= 0 {
			return m[:i+3] + "[redacted]@"
		}
		return m
	})
}

// presentedCredential is the credential the caller presented, from whichever
// header the wire they speak puts it in. An Authorization header is read
// case-insensitively (audit 2026-09-01 L13): some proxies/lambdas lowercase the
// scheme, and the old exact "Bearer " prefix rejected them.
//
// The Anthropic wire is not the Authorization wire. This endpoint answers
// /v1/messages, and an Anthropic client — the official SDK included — sends its
// key in `x-api-key` and no Authorization at all, which is exactly the header
// the strip below exists to keep away from a third-party upstream (audit
// 2026-09-01 H1). Reading it here and stripping it there are the two halves of
// serving those callers at all: without this half the SDK's own default gets a
// 401 from an endpoint built to answer it (2026-09-29 audit, round 89,
// F89-L3-4). Azure-style callers spell it `api-key`.
func presentedCredential(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); auth != "" {
		cred := strings.TrimSpace(auth)
		// The scheme word alone is the scheme with an EMPTY token — "Bearer "
		// reaches a server as "Bearer", because a header value's trailing
		// whitespace is stripped on the wire before any handler sees it — so it
		// carries no credential either, and must fall through like the case
		// below rather than be read as a bare token spelled "Bearer"
		// (2026-09-29 audit, round 90, F90-L3-3).
		if strings.EqualFold(cred, "Bearer") {
			cred = ""
		} else if len(cred) >= 7 && strings.EqualFold(cred[:7], "Bearer ") {
			cred = strings.TrimSpace(cred[7:])
		} else if strings.ContainsAny(cred, " \t") {
			// An Authorization that names a scheme this gateway does not read
			// ("Basic …", "Negotiate …", a digest challenge echoed back) is not
			// the caller's credential here — the two credentials this gateway
			// takes on that header are a Bearer token and a bare token, and a
			// token carries no space. Treating it as one made an intermediary's
			// header SHADOW the key the caller actually sent: an
			// `Authorization: Bearer ` (an empty token), a `Basic …` injected by
			// a load balancer or a corporate gateway, or a second Authorization
			// line whose value the first line's Get returns — each answered 401
			// for a request carrying a valid X-Api-Key, so the caller's own
			// credential was unreadable (2026-09-29 audit, round 90, F90-L3-3).
			cred = ""
		}
		if cred != "" {
			return cred
		}
	}
	for _, h := range []string{"X-Api-Key", "Api-Key"} {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return ""
}

// lookupKey resolves the presented Bearer key to its whole config entry (not
// just the label) so per-key policy -- Priority, MaxCompletionTokens -- is
// available on the request path. Same constant-time scan of every stored
// digest as before, and it deliberately does NOT break early on a match so
// the comparison count stays independent of which key was presented.
func (g *gateway) lookupKey(r *http.Request) (gwKey, bool) {
	key := presentedCredential(r)
	if key == "" {
		return gwKey{}, false
	}
	sum := sha256.Sum256([]byte(key))
	presented := []byte(hex.EncodeToString(sum[:]))
	g.mu.RLock()
	defer g.mu.RUnlock()
	var found gwKey
	ok := false
	for _, k := range g.cfg.APIKeys {
		if subtle.ConstantTimeCompare(presented, []byte(k.SHA256)) == 1 {
			found, ok = k, true
		}
	}
	return found, ok
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]any{
		"error": map[string]any{"message": msg, "type": code, "code": code},
	})
}

func (g *gateway) modelsHandler(w http.ResponseWriter, r *http.Request) {
	g.mu.RLock()
	models := g.cfg.Models
	g.mu.RUnlock()
	data := make([]map[string]any, 0, len(models))
	// One deadline for the whole roster, not one per model: the budget is what
	// this request is willing to spend on probe answers it does not have yet,
	// and spending it once per model is how a roster with several cold
	// upstreams held the caller for minutes (2026-09-27 audit, round 27, B-A).
	healthDeadline := time.Now().Add(healthBudget)
	for _, m := range models {
		created := m.Created
		if created == 0 {
			// OpenRouter polls /models; a "created" that changes on every
			// poll looks like a new model each time. Pin it to process
			// start when the config does not set one.
			created = processStart
		}
		entry := map[string]any{
			"id":       m.ID,
			"object":   "model",
			"created":  created,
			"owned_by": m.OwnedBy,
			"pricing":  m.Pricing,
		}
		if m.ContextLength > 0 {
			entry["context_length"] = m.ContextLength
		}
		if m.MaxCompletionTokens > 0 {
			entry["max_completion_tokens"] = m.MaxCompletionTokens
		}
		if len(m.SupportedParameters) > 0 {
			entry["supported_parameters"] = m.SupportedParameters
		}
		in := m.InputModalities
		if len(in) == 0 {
			in = []string{"text"}
		}
		entry["architecture"] = map[string]any{
			"input_modalities":  in,
			"output_modalities": []string{"text"},
		}
		g.annotateHealth(m, entry, healthDeadline)
		data = append(data, entry)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"object": "list", "data": data})
}

// healthHandler is unauthenticated so uptime monitors and OpenRouter can
// probe it. 200 only when the upstream actually answers; otherwise 503, so a
// down fleet is visible instead of hidden behind a static /models.
// healthCacheTTL bounds how often /health performs a real upstream chat
// probe. The probe is unauthenticated and runs on the customer ("openrouter")
// concurrency tier, so without a cache 32 parallel GETs could occupy every
// customer slot for a second at zero cost to the caller. Monitors poll at
// >= 30 s; a 10 s cache changes nothing for them.
const healthCacheTTL = 10 * time.Second

type healthResult struct {
	code int
	body map[string]any
}

func (g *gateway) healthHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	g.healthMu.Lock()
	fresh := !g.healthAt.IsZero() && time.Since(g.healthAt) < healthCacheTTL
	g.healthMu.Unlock()
	if !fresh {
		// Probe OUTSIDE the lock (2026-09-01 audit L9): the lock used to be
		// held across the whole <=25s probe, queuing every concurrent /health
		// GET behind it, and the probe ran on the FIRST requester's context —
		// that client disconnecting mid-probe cached a failure for everyone.
		// Concurrent refresher GETs may each probe once; the last write wins
		// and the cache means at most a handful of 1-token probes per TTL.
		res := g.probeHealth(r)
		g.healthMu.Lock()
		g.healthLast = res
		g.healthAt = time.Now()
		g.healthMu.Unlock()
	}
	g.healthMu.Lock()
	res := g.healthLast
	g.healthMu.Unlock()
	w.WriteHeader(res.code)
	json.NewEncoder(w).Encode(res.body)
}

// healthFallbackDown reports a probe failure, BUT reports 200 "ok" when a
// real completion succeeded through this gateway within
// healthTrafficFreshness — a fresh real success is stronger evidence of a
// healthy fleet than a synthetic 1-token probe that starves under load
// (measured: storms push probe latency past 25 s while customer streams
// keep completing). If the freshness window is also exhausted, it's a
// genuine outage: 503 down.
const healthTrafficFreshness = 300 * time.Second

func (g *gateway) healthFallbackDown(reason string) healthResult {
	if last := g.lastOKAt.Load(); last > 0 && time.Since(time.Unix(last, 0)) < healthTrafficFreshness {
		return healthResult{http.StatusOK, map[string]any{
			"status":            "ok",
			"detail":            "probe timed out, but a real completion succeeded " + fmt.Sprintf("%.0f", time.Since(time.Unix(last, 0)).Seconds()) + "s ago",
			"probe_context":     reason,
			"recent_traffic_ok": true,
		}}
	}
	return healthResult{http.StatusServiceUnavailable, map[string]any{"status": "down", "reason": reason}}
}

// probeHealth does the real check; healthHandler caches its result.
func (g *gateway) probeHealth(r *http.Request) healthResult {
	g.mu.RLock()
	up := g.cfg.UpstreamAddr
	models := g.cfg.Models
	cfgSnapshot := g.cfg
	g.mu.RUnlock()
	if len(models) == 0 {
		return healthResult{http.StatusServiceUnavailable, map[string]any{"status": "down", "reason": "no models configured"}}
	}
	// A real 1-token chat completion, authenticated with the gateway's own
	// upstream credential, through gatekeeper -> katlb -> a replica. This is
	// the only probe that proves a customer request would succeed. The old
	// unauthenticated GET /v1/models got a 401 from gatekeeper and reported
	// "ok" with every replica dead (audit 2026-08-25). Timeout 25 s (was
	// 8 s): under heavy real load the probe queues behind giant prefills
	// like any other request, and an 8 s budget reported "down" (with the
	// fleet actually serving 200s throughout) — the same busy-vs-wedged
	// confusion the LB and watchdog probe timeouts had to be raised for.
	// 600s ResponseHeaderTimeout is the real bound; 25s just avoids
	// crying wolf, matching oaicalb's probe_timeout_sec.
	// Background context, NOT the requester's (audit L9): a monitor that
	// disconnects must not fail the shared cached result for everyone.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	// Deliberately still ONE probe, of models[0] on its own upstream: the
	// gateway is "up" if the primary model serves. Probing every backend
	// would let a secondary model's dead upstream 503 the whole gateway
	// (and cost one real completion per backend per cache window).
	probeUp := models[0].upstreamAddr(up)
	body := `{"model":` + fmt.Sprintf("%q", models[0].upstreamID()) + `,"messages":[{"role":"user","content":"ping"}],"max_tokens":1,"temperature":0}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(probeUp, "/")+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if k := upstreamKeyFor(cfgSnapshot, probeUp); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		log.Printf("health probe failed: %v", err)
		return g.healthFallbackDown("upstream unavailable")
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return g.healthFallbackDown(fmt.Sprintf("upstream chat probe HTTP %d", resp.StatusCode))
	}
	// upstreams is a topology hint only (how many distinct backends this
	// gateway fronts), not a reachability report -- see probeUp above for
	// why the secondary backends are not probed.
	return healthResult{http.StatusOK, map[string]any{"status": "ok", "upstreams": distinctUpstreams(gwConfig{UpstreamAddr: up, Models: models})}}
}

// ledgerEntry is one metered completion. Appended as a JSON line so it can
// be tailed, grepped, and summed with jq without any database.
type ledgerEntry struct {
	TS               string `json:"ts"`
	RequestID        string `json:"request_id"`
	KeyLabel         string `json:"key"`
	Model            string `json:"model"`
	UpstreamModel    string `json:"upstream_model"`
	Path             string `json:"path"`
	Stream           bool   `json:"stream"`
	Status           int    `json:"status"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	// CachedTokens is the prefix-cache-hit portion of PromptTokens, from the
	// upstream's OpenAI-shaped prompt_tokens_details.cached_tokens. Zero
	// does not mean "no cache hit" -- it also means "upstream didn't
	// populate this field" (as of 2026-08-29, this vLLM build returns it
	// null on every response; the field is wired through end-to-end and
	// ready the moment an upstream starts sending real values, no further
	// code change needed). Cross-check against vLLM's own /metrics
	// (vllm:prefix_cache_hits_total/queries_total, token-level, aggregate
	// not per-request) if this stays zero and cache hits are suspected.
	CachedTokens int   `json:"cached_tokens"`
	LatencyMS    int64 `json:"latency_ms"`
	UsageSeen    bool  `json:"usage_seen"` // false = upstream sent no usage; do not trust zeros
	Aborted      bool  `json:"aborted"`    // client disconnected / upstream died mid-response
	// Backend is which replica actually served this request (oaicalb's
	// X-Katlb-Backend, e.g. "http://127.0.0.1:30106" = GPU0) — captured via
	// ctxKeyBackend before the gateway strips the header from the public
	// response. Empty if the request never reached a backend (blocked
	// before proxying, or the upstream error path never set the header).
	// This is the per-GPU usage attribution: group by Backend to see
	// GPU0-vs-GPU1 load, not just aggregate fleet totals.
	Backend string `json:"backend,omitempty"`
	// SessionID is the caller's X-Session-Id (set by cmd/launch's per-
	// launch proxy for LB session-hash affinity — see
	// anthropic_openai_proxy.go's newProxySessionID). Lets multiple
	// concurrent Claude Code sessions under the SAME api key (e.g.
	// internal-91 today represents 3 client machines combined) be told
	// apart without issuing each one a separate key. Empty for callers
	// that don't send one (older clients, direct API use).
	SessionID string `json:"session_id,omitempty"`
	// CostUSD: computed at write time from the model's pricing (including
	// CachedPrompt's discount, when set) -- see gwPricing.CachedPrompt's
	// doc. Informational only, same as the /models pricing fields
	// themselves: oaica-code has no billing/invoicing enforcement, this is
	// what a billing job would sum. 0 if the model's pricing couldn't be
	// parsed (e.g. empty/malformed decimal strings).
	CostUSD float64 `json:"cost_usd,omitempty"`
	// Overage: true if this request was allowed specifically because
	// EntitlementOverageBilling let it through despite exceeding the
	// subscriber's plan rolling-window cap -- see entitlementCache.check's
	// doc. A billing job should charge these at the (usually higher)
	// overage rate rather than the plan's included rate. Always false
	// when overage billing isn't enabled or the request was within cap.
	Overage bool `json:"overage,omitempty"`
	// PriceTier: the upper bound (in prompt tokens) of the context pricing
	// bracket this request billed at, or 0 for the unbounded catch-all --
	// and 0 too when the model has no pricing_tiers at all. Recorded so
	// revenue can be split by tier after the fact: the whole point of
	// tiering is to find out how much of the bill comes from the >128k
	// traffic that drives p95, and that question needs the bracket on the
	// row, not a re-derivation from prompt_tokens against a config that may
	// since have changed.
	PriceTier int `json:"price_tier,omitempty"`
}

func (g *gateway) writeLedger(e ledgerEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	g.ledgerMu.Lock()
	var writeErr error
	if g.ledger != nil {
		if _, err := g.ledger.Write(append(b, '\n')); err != nil {
			writeErr = err
		}
	}
	g.ledgerMu.Unlock()

	if writeErr != nil {
		// Audit L14: a silent failure here silently loses billing rows (disk
		// full, fd closed). One log line per failure is cheap; the request still
		// serves. Written after the lock for the reason round 30's B1 gives: the
		// sink is not ours and the lock is held by every other ledger writer
		// (2026-09-27 audit, round 30).
		log.Printf("ledger write failed: %v", writeErr)
	}

	// Best-effort central aggregation — see meterCh's doc. Never blocks:
	// reportUsage sends on a buffered channel with a non-blocking select.
	g.reportUsage(e)
}

// upstreamErrorLogLine is one JSONL row in gwConfig.UpstreamErrorLogPath.
type upstreamErrorLogLine struct {
	TS              string `json:"ts"`
	RequestID       string `json:"request_id,omitempty"`
	SessionID       string `json:"session_id,omitempty"`
	Model           string `json:"model,omitempty"`
	Status          int    `json:"status"`
	Code            string `json:"code"`
	Message         string `json:"message"`
	EstimatedPrompt int    `json:"estimated_prompt_tokens,omitempty"`
	MaxTokens       int    `json:"max_tokens,omitempty"`
	RequestBytes    int    `json:"request_bytes,omitempty"`
}

// logUpstreamError is newProxy's onUpstreamError callback -- see
// gwConfig.UpstreamErrorLogPath's doc for why this exists. info is nil when
// the request never reached the point in completionHandler that fills it
// (rejected earlier, or something outside the normal completion path).
func (g *gateway) logUpstreamError(info *errCaptureInfo, status int, code, msg string) {
	g.errLogMu.Lock()
	f := g.errLog
	g.errLogMu.Unlock()
	if f == nil {
		return
	}
	line := upstreamErrorLogLine{
		TS: time.Now().UTC().Format(time.RFC3339Nano), Status: status, Code: code, Message: msg,
	}
	if info != nil {
		line.RequestID = info.RequestID
		line.SessionID = info.SessionID
		line.Model = info.Model
		line.EstimatedPrompt = info.EstTokens
		line.MaxTokens = info.MaxTokens
		line.RequestBytes = info.ReqBytes
	}
	b, err := json.Marshal(line)
	if err != nil {
		return
	}
	g.errLogMu.Lock()
	if g.errLog != nil {
		g.errLog.Write(append(b, '\n'))
	}
	g.errLogMu.Unlock()
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	// PromptCacheHitTokens is the sibling spelling some builds emit instead of
	// the details object (DeepSeek's). Declared so cachedTokens can fall back
	// to it rather than billing a stated hit at the fresh rate.
	PromptCacheHitTokens int `json:"prompt_cache_hit_tokens"`
	PromptTokensDetails  *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// cachedTokens is the prefix-cache hit count, and it is the ledger's copy of
// the function cmd/launch's client proxy already carries — the same three
// protections, for the same three failures (2026-09-27 audit, round 23):
//
//   - nil-safe: PromptTokensDetails is only present once an upstream actually
//     populates it (see ledgerEntry.CachedTokens's doc);
//   - the fallback is on the VALUE, not the pointer: a build that always emits
//     the details object, populated only when it has a hit, reports an uncached
//     prompt while its own sibling field states the hit — and here the number
//     is billed, so those tokens would pay the fresh rate;
//   - clamped at zero below and at the prompt size above: the cost math
//     subtracts this from the prompt, so a malformed negative count INFLATES
//     the fresh tokens (prompt - (-5000) = prompt + 5000), and the ledger row
//     would record a negative hit;
//   - reported as it stands when the prompt size was never stated, because
//     there is then no measurement to clamp against and "0" is silence, not a
//     measurement. A hit the upstream stated and a size it left out is a real
//     shape — a stream that narrates only the hit — and returning 0 for it
//     told the client the whole prompt was fresh input while the row for the
//     same turn recorded the hit: the two records of one request disagreeing
//     about the one number either of them had evidence for (2026-09-27 audit,
//     round 40, A40-3). Every caller that reports this against a prompt raises
//     that prompt to the hit (EstimatedUsage, promptSplit, and the non-stream
//     leg's own split), so no field can come out negative — which is why the
//     sibling function in cmd/launch's client proxy has carried exactly this
//     branch since round 39 (A-F4).
func (u usage) cachedTokens() int {
	c := u.statedCacheHit()
	if c < 0 {
		return 0
	}
	if u.PromptTokens <= 0 {
		// No measurement to clamp against, so the hit stands (see this
		// function's doc). A negative prompt is no statement either.
		return c
	}
	if c > u.PromptTokens {
		return u.PromptTokens
	}
	return c
}

// statedCacheHit is the cache hit the upstream stated, UNCLAMPED: details over
// sibling spelling, and zero when neither was stated. cachedTokens() clamps it
// to a stated prompt for the cost arithmetic, but every site that REPORTS the
// hit applies the round-40 rule instead -- a stated hit is evidence about the
// prompt, so the total is raised to the hit rather than the hit clamped down to
// a size the upstream never stated. The clamp inside cachedTokens() ran BEFORE
// the ledger row's own raise, which made that raise unreachable whenever the
// prompt was stated: the client read 5000 for a turn whose row recorded 1000,
// the exact disagreement the rule exists to prevent (2026-09-27 audit, round
// 41, B41-1).
func (u usage) statedCacheHit() int {
	c := u.detailsCachedTokens()
	// A malformed details value (<=0) must not suppress a sibling that does
	// state the hit: the clamp used to run AFTER this fallback, so a negative
	// details count short-circuited the sibling and then clamped to zero,
	// billing the whole prompt at the fresh rate -- the outcome the fallback
	// exists to prevent (2026-09-27 audit, round 24).
	if c <= 0 {
		if sibling := u.PromptCacheHitTokens; sibling > 0 {
			c = sibling
		}
	}
	if c < 0 {
		return 0
	}
	return c
}

// detailsCachedTokens is the prompt_tokens_details.cached_tokens count on its
// own, zero when the object carries no details block: one reading of the
// pointer for every site that resolves the two cache spellings.
func (u usage) detailsCachedTokens() int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
}

// merge folds a later chunk's usage into this one field by field. SSE usage
// objects are not guaranteed to be cumulative: a build may narrate the running
// counts per chunk and state only the fields it has just measured, so the
// whole-struct replace this used to be billed whichever chunk arrived last —
// a closing chunk that omits the cache fields turned a stated hit into zero on
// the ledger row, i.e. the prompt paid the fresh rate (2026-09-27 audit,
// round 24). A value the chunk does state wins, including one that replaces an
// earlier larger value; only silence preserves what an earlier chunk stated.
func (u *usage) merge(next usage) {
	// Only a POSITIVE count is a statement (2026-09-27 audit, round 33): a
	// malformed upstream reporting a negative one said nothing this gateway can
	// bill, and letting it win overwrote a stated count with a credit.
	if next.PromptTokens > 0 {
		u.PromptTokens = next.PromptTokens
	}
	if next.CompletionTokens > 0 {
		u.CompletionTokens = next.CompletionTokens
	}
	if next.PromptCacheHitTokens > 0 {
		u.PromptCacheHitTokens = next.PromptCacheHitTokens
	}
	if next.PromptTokensDetails != nil {
		if u.PromptTokensDetails == nil {
			u.PromptTokensDetails = next.PromptTokensDetails
		} else if next.PromptTokensDetails.CachedTokens > 0 {
			u.PromptTokensDetails.CachedTokens = next.PromptTokensDetails.CachedTokens
		}
	}
}

// fillEmpty folds a translating writer's own reading of a turn into the
// recorder's, field by field, and takes a field ONLY where the recorder has
// nothing: a count the upstream stated is the upstream's word for the turn, and
// an estimate exists to fill its silence, never to replace its statement
// (2026-09-27 audit, round 40, B40-2 — the substitution overwrote a stated
// prompt_tokens=11/completion_tokens=2 with the prompt estimate and a zero, so
// the row for a served turn stated counts the upstream never sent while the
// client read the real ones). Per field rather than per turn for merge's
// reason: an upstream may state one count and not the other.
func (u *usage) fillEmpty(next usage) {
	if next.PromptTokens > 0 && u.PromptTokens <= 0 {
		u.PromptTokens = next.PromptTokens
	}
	if next.CompletionTokens > 0 && u.CompletionTokens <= 0 {
		u.CompletionTokens = next.CompletionTokens
	}
	// The hit, read through cachedTokens() so "this row already states a hit"
	// is the same question the cost math asks, in either spelling.
	if u.cachedTokens() <= 0 {
		if d := next.PromptTokensDetails; d != nil && d.CachedTokens > 0 {
			u.PromptTokensDetails = d
		}
		if next.PromptCacheHitTokens > 0 {
			u.PromptCacheHitTokens = next.PromptCacheHitTokens
		}
	}
}

// nonNegative is the usage as a ledger row can state it. A negative count is
// not a count: the non-stream path assigns the upstream's usage object whole
// (usageRecorder.finish), and billing one recorded prompt_tokens=-400 with
// cost_usd=-2.06e-05 — a credit for tokens nobody served, in the one record
// kept of the request (2026-09-27 audit, round 33, sub-bar). Clamped here, at
// the row, so both the streaming merge and the whole-object path are covered.
func (u usage) nonNegative() usage {
	if u.PromptTokens < 0 {
		u.PromptTokens = 0
	}
	if u.CompletionTokens < 0 {
		u.CompletionTokens = 0
	}
	if u.PromptCacheHitTokens < 0 {
		u.PromptCacheHitTokens = 0
	}
	if d := u.PromptTokensDetails; d != nil && d.CachedTokens < 0 {
		u.PromptTokensDetails = &struct {
			CachedTokens int `json:"cached_tokens"`
		}{CachedTokens: 0}
	}
	return u
}

// ledgerStatusWriter lets a writer that TRANSLATES the upstream's answer state
// the status the client will see, which is not always the status the upstream
// sent: the /v1/messages bridge answers an untranslatable 200 with 502, and it
// decides that in finalize(), which messagesHandler calls AFTER
// completionHandler has already written the ledger row. Without this the row
// recorded the upstream's 200 for a turn the client read as a failure — the one
// record kept of what happened, claiming a success that never was
// (2026-09-27 audit, round 25).
type ledgerStatusWriter interface {
	LedgerStatus(upstream int) int
}

// usageRecorder wraps the ResponseWriter to (a) forward bytes immediately
// (streaming must not be buffered) and (b) scan them for the usage object.
// For non-streaming responses the whole body is one JSON document; for SSE
// each "data: {...}" line is a chunk and only the last one carries usage.
type usageRecorder struct {
	http.ResponseWriter
	status int
	stream bool
	usage  usage
	seen   bool
	tail   bytes.Buffer // last partial SSE line across writes
	body   bytes.Buffer // non-stream: accumulate (bounded) to parse usage once
}

func (u *usageRecorder) WriteHeader(code int) {
	u.status = code
	u.ResponseWriter.WriteHeader(code)
}

// clientStatus is the status the CLIENT will read for this turn: the writer that
// knows better than the upstream's own header may move it (the /v1/messages
// bridge answers an untranslatable 200 with 502, see ledgerStatusWriter), and it
// answers that question before the row is built, so every reader of this turn's
// verdict must ask it here rather than read u.status raw.
//
// It exists because the two readers had drifted: the row asked (entry) and the
// ground-truth guard below did not, so a turn the client was REFUSED — 502, row
// booked with nothing — still counted as a success for the next request of the
// session. Both halves of that were measured: /health reported a healthy
// upstream from a refused turn's upstream-stated usage, and one refused turn
// turned a later request of the same session from a 200 into a 400 "prompt is
// too long", using the refused turn's own prompt ratio as this session's
// calibration (2026-09-29 audit, round 92, F92-L3-1).
func (u *usageRecorder) clientStatus() int {
	status := u.status
	if reporter, ok := u.ResponseWriter.(ledgerStatusWriter); ok {
		status = reporter.LedgerStatus(status)
	}
	return status
}

func (u *usageRecorder) Write(p []byte) (int, error) {
	n, err := u.ResponseWriter.Write(p)
	if u.stream {
		// Cap the partial-line buffer (2026-09-01 audit M5): a wedged
		// upstream emitting one endless SSE line with no newline would grow
		// tail without limit and OOM the gateway. The trim happens INSIDE
		// scanSSE, which is also what drains the buffer — gating the call
		// itself made the cap a one-way door: once the buffer was over the
		// cap the drain never ran again, so every later line was dropped too
		// and a served 200 was metered as zero (2026-09-27 audit, round 31).
		u.scanSSE(p)
	} else if u.body.Len() < docBufferLimit {
		// docBufferLimit, not the 4 MiB this arm used to carry: the two arms are
		// two spellings of one turn, and a 5 MiB completion document was metered
		// with usage_seen=false when it arrived buffered and usage_seen=true when
		// the same bytes arrived as frames — the meter trusted a different size
		// on each arm (2026-09-29 audit, round 88, F88-L3-4).
		u.body.Write(p)
	}
	return n, err
}

// Flush is required for httputil.ReverseProxy to stream (it type-asserts
// http.Flusher on the writer it is given).
func (u *usageRecorder) Flush() {
	if f, ok := u.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

// docBufferLimit bounds the buffer a whole-completion DOCUMENT may occupy while
// it arrives. It is the same 8 MiB bound the non-streaming arm already trusts
// (`bufCapOK`): one turn reaches the client as a document or as frames, and the
// two spellings of it may not be answered on different sizes (2026-09-28 audit,
// round 86, F86-L3-1).
//
// It is the bound for every buffer that has to hold a whole turn to read it,
// the meter's included. The meter's partial-line buffer was bounded by a 1 MiB
// `sseTailLimit`, so a turn whose body passed 1 MiB was metered with
// usage_seen=false on both streamed spellings and usage_seen=true on the plain
// one — the same body served, read two ways, billed with two different degrees
// of confidence (2026-09-28 audit, round 86, F86-L3-1's metering face).
const docBufferLimit = 8 << 20

// trimOverlongSSETail keeps that buffer bounded WITHOUT going deaf. When the
// buffer has reached the limit, the bytes up to and including the last
// newline are dropped so scanning resumes with the next whole line; a line
// still incomplete at the limit leaves nothing to resume from, and the bytes
// that continue it are dropped by this same check on later writes. Dropping
// the buffer outright — or skipping the scan that drains it — is what stopped
// usage extraction, and translation, for the whole rest of the stream
// (2026-09-27 audit, round 31).
func trimOverlongSSETail(buf *bytes.Buffer, limit int) {
	if buf.Len() < limit {
		return
	}
	if i := bytes.LastIndexByte(buf.Bytes(), '\n'); i >= 0 {
		buf.Next(i + 1)
		return
	}
	buf.Reset()
}

func (u *usageRecorder) scanSSE(p []byte) {
	trimOverlongSSETail(&u.tail, docBufferLimit)
	u.tail.Write(p)
	for {
		raw := u.tail.Bytes()
		i := bytes.IndexByte(raw, '\n')
		if i < 0 {
			return
		}
		line := bytes.TrimSpace(raw[:i])
		u.tail.Next(i + 1)
		if !bytes.HasPrefix(line, []byte("data:")) {
			// An upstream that ignores stream:true answers with one whole
			// completion document and no data: line at all, and the bridge adopts
			// it as the turn. Keeping the non-SSE lines (bounded by the same cap
			// the non-stream path uses) is what lets finish() read that document's
			// usage instead of booking the served turn as zero tokens
			// (2026-09-27 audit, round 39, B-F3).
			if u.body.Len() < docBufferLimit {
				u.body.Write(line)
				u.body.WriteByte('\n')
			}
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		// The frame is read with the BRIDGE's own struct, not a private lenient
		// one that models only `usage`: the meter and the client take one
		// reading of one body, and a frame the bridge cannot decode is a frame
		// the client never received, whose usage is not a usage to book. A
		// private struct that accepted what the bridge refused let one wire be
		// two turns — the client served 8/1 and the row billed 9000/500 for the
		// same bytes (2026-09-29 audit, round 90, F90-L3-1).
		var chunk oaStreamChunk
		if json.Unmarshal(payload, &chunk) != nil || chunk.Usage == nil {
			continue
		}
		u.usage.merge(*chunk.Usage)
		u.seen = true
	}
}

func (u *usageRecorder) finish() {
	// The bytes in body+tail are the non-`data:` bytes of the UPSTREAM's answer,
	// and they are read as the turn only when the writer that decided what to
	// serve says the turn WAS that document. On the stream arm that relayed
	// frames and then declined a whole completion, the document was never served:
	// booking its usage charged the client's session for an answer it never
	// received and told the row the upstream's word for counts the client was
	// never given — one body, two readings, and the row's was not the served one
	// (2026-09-29 audit, round 87, F87-L3-1).
	served := true
	if d, ok := u.ResponseWriter.(interface{ DocumentServed() bool }); ok {
		served = d.DocumentServed()
	}
	// The scanner above only consumes WHOLE lines, so a stream that ended without
	// a final newline leaves its last line — a frame or a document — in tail,
	// where `raw` would read it glued to the body: `{...document...}data: {...}`
	// is not JSON, so a turn whose client WAS served the document was metered from
	// the frame that preceded it: 9000/500 for a turn served as 7/3, and the row
	// kept the number that was not the turn (2026-09-29 audit, round 88,
	// F88-L3-1). Feeding the one pending line to the same scanner first lets it
	// leave the tail the way it would have had the upstream ended its last line:
	// a frame goes to the usage scan and stops gluing itself to the document.
	// Only a frame needs the feed — a pending document line is read out of the
	// tail by `raw` below exactly as it was before.
	if pending := bytes.TrimSpace(u.tail.Bytes()); bytes.HasPrefix(pending, []byte("data:")) {
		u.scanSSE([]byte("\n"))
	}
	raw := make([]byte, 0, u.body.Len()+u.tail.Len())
	raw = append(raw, u.body.Bytes()...)
	raw = append(raw, u.tail.Bytes()...)
	// A document with no trailing newline never leaves the scanner's
	// partial-line buffer — the loop above only consumes whole lines — so it is
	// the tail, not the body, that holds the last (here: only) line.
	var doc struct {
		Usage   *usage            `json:"usage"`
		Choices []json.RawMessage `json:"choices"`
	}
	// A document IS a turn only if it carries one. An object with usage and no
	// choices is what the bridge refuses — the client was answered 502 — so
	// booking its usage charged a session for an answer it never received, and
	// whether it was charged depended on which arm met the same bytes: the
	// buffered arm read the usage of the refused turn, the streamed arm did not
	// (2026-09-29 audit, round 88, F88-L3-3).
	document := json.Unmarshal(raw, &doc) == nil && doc.Usage != nil && len(doc.Choices) > 0
	if !served {
		return
	}
	if document {
		// The document IS the turn (an adopted stream document, or the
		// non-stream arm): its usage is the turn's, and it stands over a usage
		// statement an EARLIER frame made. Adoption copies the document's counts
		// into what the client is told, so a row that kept the frame's numbers
		// booked 9000/500 for a turn whose client read 7/3 — the two statements
		// were never reconciled and the row kept the one that was not the turn
		// (2026-09-29 audit, round 87, F87-L3-2). The scan above still wins
		// wherever no document was served (round 39, B-F3's other half).
		//
		// Merged, not assigned (2026-09-29 audit, round 88, F88-L3-2): the
		// document's word for the fields it states, and the frame's for the ones
		// it does not. Assigning the whole struct erased a cache hit an earlier
		// frame had stated — the frame's row booked 7/3 with cached=0 while its
		// client was read the same frame's hit and answered 2/3 with cached=5, and
		// the two readings of one turn disagreed exactly on whether the prompt
		// paid the cached rate.
		u.usage.merge(*doc.Usage)
		u.seen = true
		return
	}
	if u.seen {
		return
	}
	// Also for a STREAM request: an upstream that ignores stream:true answers
	// with one whole completion document and no data: lines at all, and the
	// bridge adopts it as the turn — so the body really does carry the usage,
	// and returning early here recorded prompt=0/completion=0/cost=0 for a turn
	// the client was served and the upstream billed (2026-09-27 audit, round 39,
	// B-F3).
}

// maxSessionIDLen bounds the X-Session-Id the gateway keeps.
const maxSessionIDLen = 128

// boundedSessionID returns the client's session id when it is a sane length and
// otherwise a bounded form of it that is still stable per id: its first 95 bytes,
// a "~", and 128 bits of its SHA-256. X-Session-Id is client-chosen and was stored
// at full length, up to the 64 KiB header limit, in the ledger row, the upstream
// error log and the calibrator's key: a holder of any valid key grew the ledger
// about 180x faster than an ordinary request does and pinned 4096 keys of that size
// in a map whose bound counts entries, not bytes. The id keeps its identity, so a
// session still groups (2026-09-29 audit, round 110, F110-L3-1).
func boundedSessionID(s string) string {
	if len(s) <= maxSessionIDLen {
		return s
	}
	sum := sha256.Sum256([]byte(s))
	return s[:maxSessionIDLen-33] + "~" + hex.EncodeToString(sum[:16])
}

func newRequestID() string {
	var b [12]byte
	// crypto/rand, not /dev/urandom-by-hand (2026-09-01 audit L10): the old
	// fallback on open failure read an empty string, making every request id
	// a collidable UnixNano. crypto/rand has no such failure mode (panics
	// only if the OS CSPRNG is irrecoverably broken).
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("req_%d", time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(b[:])
}

// processStart is the fallback "created" timestamp for /models entries.
var processStart = time.Now().Unix()

// completionHandler is the metered proxy path for /v1/chat/completions and
// /v1/completions. It reads the (capped) body once to: validate the model id
// and rewrite it to the upstream id, inject stream_options.include_usage on
// streaming requests, then forwards and meters the response.
// estimateMessageTokens is a coarse, cheap prompt-size estimate (marshal
// "messages" back to bytes, divide by 4) — NOT a real tokenizer call. It
// only needs to distinguish "this is a normal request" from "this is a
// 140K+ token monster" for admission control; being off by 20-30% in
// either direction doesn't change that classification for the threshold
// this gates at (see gwConfig.LargeContextTokenThreshold, default 50000).
func estimateMessageTokens(req map[string]any) int {
	return messagesBytes(req) / 4
}

// messagesBytes is the serialized size of the prompt a request carries --
// req["messages"] for the chat shape, req["prompt"] for the legacy
// completions shape both served by completionHandler. It is the unit BOTH the
// chars/4 estimate and the per-session calibration (context_calibration.go)
// are expressed in, so a calibrated tokens-per-byte ratio measured on one turn
// applies directly to the next.
//
// A legacy body was measured as zero (2026-09-27 audit, round 32, B-F3): the
// two prompt guards are in this one handler, but they read "messages" only, so
// a `{"prompt": ...}` request skipped admission control and the context-fit
// clamp entirely and reached the upstream as a raw prefill.
// A tool schema is prompt too (2026-09-27 audit, round 33, B-F1): the chat
// template renders req["tools"]/req["functions"] into the turn the replica
// prefills, and the upstream counts it in prompt_tokens. Measuring only the
// messages priced a tools-borne payload at zero, so it was admitted while the
// large-context pool was full, skipped the context-fit clamp, and its real
// measurement was discarded by the calibrator as bogus (a ratio above
// calibMaxRatio). Charged in the same byte unit as the messages, by length --
// no image and no inline payload can ride a tool schema.
func messagesBytes(req map[string]any) int {
	// The two prompt spellings are alternatives, so the LARGER of the two is
	// the bound: testing req["messages"] by presence let a body carrying an
	// empty messages array beside a real legacy prompt measure as 0 tokens and
	// walk past both guards — the same payload the prompt-only shape is
	// refused for (2026-09-27 audit, round 34, B-F5; round 33 covered the
	// shape where "messages" is absent, not where it is empty). Charging the
	// max can only over-charge a body that carries both, which no real client
	// sends, and never under-charges a prefill the upstream will do.
	total := 0
	if msgs, ok := req["messages"]; ok {
		if n, ok := jsonMeasuredBytes(msgs); ok {
			total = promptPayloadBytes(n, msgs)
		}
	}
	if p, ok := req["prompt"]; ok {
		if n, ok := jsonMeasuredBytes(p); ok && n > total {
			total = n
		}
	}
	for _, k := range []string{"tools", "functions"} {
		if v, ok := req[k]; ok {
			if n, ok := jsonMeasuredBytes(v); ok {
				total += n
			}
		}
	}
	return total
}

// jsonMeasuredBytes returns the size of a value in the unit the prompt-size
// estimate is expressed in: the bytes the upstream will DECODE, not the bytes
// Go spent encoding them. encoding/json writes `"`, `\` and the control
// characters as two-byte escapes and `<`, `>`, `&` and the line separators as
// six-byte ones; the upstream JSON-decodes the body before tokenizing anything,
// so none of that is prompt. Charged as prompt, the six bytes per `<` made a
// markup-heavy turn measure up to 6x its real size — a locally refused healthy
// turn, and the same unit feeds the calibration ratio and the fit clamp. The
// client leg stopped charging these escapes in round 35; the two legs of this
// product have to measure the same quantity, or a prompt one admits the other
// refuses (2026-09-27 audit, round 36, A-F4).
func jsonMeasuredBytes(v any) (int, bool) {
	b, err := json.Marshal(v)
	if err != nil {
		return 0, false
	}
	return len(b) - jsonEscapeOverhead(b), true
}

// jsonEscapeOverhead counts the bytes json.Marshal spends writing a character
// as an escape sequence. Every escape it writes decodes back to fewer bytes
// than it occupies — one fewer for the two-character forms, and as many as the
// rune it stands for for a `\uXXXX`.
//
// The scan follows backslashes the way a JSON reader does, so text holding the
// literal characters `\n` is not miscounted: its backslash is consumed as the
// two-character escape it is, and the `n` after it is ordinary text.
func jsonEscapeOverhead(b []byte) int {
	extra := 0
	for i := 0; i < len(b); i++ {
		if b[i] != '\\' || i+1 >= len(b) {
			continue
		}
		switch c := b[i+1]; c {
		case '\\', '"', '/', 'b', 'f', 'n', 'r', 't':
			extra++
			i++
		case 'u':
			if i+5 < len(b) && isHex4(b[i+2:i+6]) {
				extra += 6 - escapedRuneLen(b[i+2:i+6])
				i += 5
			}
		}
	}
	return extra
}

// escapedRuneLen is the UTF-8 length of what a `\uXXXX` escape decodes to. The
// escapes json.Marshal writes are for characters a JSON writer must escape —
// U+0000 to U+001F, U+2028 and U+2029 — and the last two are three bytes
// decoded: crediting every `\u` escape the one byte of a control escape
// measured a prompt made of line separators at a third of its size, so the fit
// clamp and the calibration ratio saw a prompt the upstream would have to
// refuse, or truncated the turn to fit a length it never had (2026-09-27
// audit, round 37, B-F6/A-F4). A lone surrogate decodes to U+FFFD, three bytes.
// This is the client leg's function too; the two legs have to measure the same
// quantity.
func escapedRuneLen(p []byte) int {
	var v rune
	for _, c := range p {
		v <<= 4
		switch {
		case c >= '0' && c <= '9':
			v |= rune(c - '0')
		case c >= 'a' && c <= 'f':
			v |= rune(c-'a') + 10
		default:
			v |= rune(c-'A') + 10
		}
	}
	if v >= 0xD800 && v <= 0xDFFF {
		return 3
	}
	if n := utf8.RuneLen(v); n > 0 {
		return n
	}
	return 3
}

// isHex4 reports whether a four-character `\u` payload is hex — every escape of
// that shape is a character json.Marshal escaped, whatever it is.
func isHex4(p []byte) bool {
	if len(p) != 4 {
		return false
	}
	for _, c := range p {
		if (c < '0' || c > '9') && (c < 'a' || c > 'f') && (c < 'A' || c > 'F') {
			return false
		}
	}
	return true
}

// imagePartByteAllowance is what one inline image is charged to the prompt-size
// estimate, in the same bytes-of-prompt unit messagesBytes returns. A data URI
// is a transport encoding, not prompt: leaving its base64 in the measurement
// billed a 1 MB screenshot as ~349563 prompt tokens and hard-rejected it on a
// model that publishes 262144 and accepts images, with no way for the session
// to recover (2026-09-27 audit, round 32, B-F1). ~1 MP is order 1e3 real
// tokens, so 4096 bytes (1024 tokens at chars/4) is the right order of
// magnitude for admission and for the fit clamp's estimate; a real usage
// report still corrects the per-session ratio either way.
const imagePartByteAllowance = 4096

// promptPayloadBytes turns a serialized prompt body into the byte count the
// estimate is expressed in: the serialized size, with any inline image's
// base64 payload replaced by imagePartByteAllowance.
func promptPayloadBytes(serialized int, v any) int {
	payload, images := inlineImageBytes(v)
	return serialized - payload + images*imagePartByteAllowance
}

// inlineImageBytes reports the total base64 payload length of the inline images
// found in a decoded request body and how many images there are. It recognises
// both shapes the gateway sees: the OpenAI part the Anthropic bridge itself
// writes (`{"type":"image_url","image_url":{"url":"data:..."}}`, messages.go)
// and an Anthropic `{"source":{"data":...}}` block a client sent straight
// through. Anything without inline data (a remote URL) is left alone -- there
// is no base64 to charge.
func inlineImageBytes(v any) (payload, images int) {
	switch t := v.(type) {
	case []any:
		for _, e := range t {
			p, n := inlineImageBytes(e)
			payload += p
			images += n
		}
	case map[string]any:
		for k, e := range t {
			if k == "image_url" {
				if m, ok := e.(map[string]any); ok {
					if u, ok := m["url"].(string); ok && strings.HasPrefix(u, "data:") {
						payload += len(u)
						images++
						continue
					}
				}
				// The part is also written with the data URI as a bare string
				// (`{"type":"image_url","image_url":"data:..."}`), which the
				// admission gate counts as an image and this walk did not
				// discount: the same 1 MB screenshot the map spelling charges
				// 4 KB against measured its full base64, so the gate admitted
				// the part and the meter that follows refused the turn —
				// locally, with "prompt is too long: N tokens > M maximum;
				// reduce the prompt or compact the conversation", the wording
				// Claude Code matches into a compaction path that cannot help
				// (2026-09-27 audit, round 36, B-F3).
				if u, ok := e.(string); ok && strings.HasPrefix(u, "data:") {
					payload += len(u)
					images++
					continue
				}
				// Anything else under this key is still an image the upstream
				// will fetch — a remote URL, or the string spelling of one. It
				// carries no base64 to discount, but it is not free, and the
				// other two measures in this product (anthropic.imageBlockBytes
				// and cmd/launch's clientPromptBytes) charge it the same
				// allowance. Counting it zero here left a URL-image turn
				// measuring smaller on this leg than on either of the others
				// (2026-09-27 audit, round 40, A40-9).
				switch x := e.(type) {
				case map[string]any:
					if u, _ := x["url"].(string); u != "" {
						// The URL text is REPLACED by the allowance, not
						// charged beside it: the other two measures in this
						// product charge a url-sourced image the allowance
						// alone, so counting the address as well made the same
						// body measure larger on this leg than on either of
						// the others (2026-09-27 audit, round 41, B41-4).
						payload += len(u)
						images++
						continue
					}
				case string:
					if x != "" {
						payload += len(x)
						images++
						continue
					}
				}
			}
			if k == "source" {
				if m, ok := e.(map[string]any); ok {
					// Only an IMAGE's source is a transport encoding. This
					// branch read any map keyed "source" holding a "data"
					// string, so a document block with a TEXT source — prompt
					// content, not an encoding — was discounted to the image
					// allowance, the same over-discount the client leg carried
					// until round 34 (A-F2, measured there as a 900 KB
					// attachment charged 4 KB). The block has to say it is an
					// image for the discount to apply.
					if bt, _ := t["type"].(string); bt == "image" {
						if d, ok := m["data"].(string); ok && d != "" {
							payload += len(d)
							images++
							continue
						}
						// A url-sourced image is the same image, fetched instead
						// of carried — the same allowance, and it is not
						// discounted because there is nothing encoded to discount
						// (2026-09-27 audit, round 40, A40-9).
						if st, _ := m["type"].(string); st == "url" {
							u, _ := m["url"].(string)
							r, _ := m["ref"].(string)
							if u != "" || r != "" {
								// The allowance replaces the reference, the
								// same rule the sibling measures apply
								// (2026-09-27 audit, round 41, B41-4).
								payload += len(u) + len(r)
								images++
								continue
							}
						}
					}
				}
			}
			p, n := inlineImageBytes(e)
			payload += p
			images += n
		}
	default:
		// A CONCRETE slice or array is walked exactly as []any is. The
		// Anthropic bridge (messages.go) builds its message list as
		// []map[string]any and hands the whole body to this walk as `any`:
		// the outer map matches the case above, but the message slice is
		// neither []any nor map[string]any, so every element under it fell
		// through to the end of this switch and returned (0, 0). The bridge
		// therefore charged a 1 MB inline image its whole base64 -- the
		// client was told input_tokens=250023 for a turn the other two legs
		// measure at ~1050 -- while the SAME body decoded from JSON, whose
		// arrays ARE []any, measured correctly; which is why every test that
		// built its body by unmarshalling JSON passed (2026-09-27 audit,
		// round 42, C42-1).
		rv := reflect.ValueOf(v)
		switch rv.Kind() {
		case reflect.Slice, reflect.Array:
			for i := 0; i < rv.Len(); i++ {
				p, n := inlineImageBytes(rv.Index(i).Interface())
				payload += p
				images += n
			}
		}
	}
	return payload, images
}

func (g *gateway) completionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	apiKey, _ := g.lookupKey(r)
	label := apiKey.Label
	if label == "" {
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
		return
	}
	// Per-key concurrency cap — see gwKey.MaxConcurrent's doc. Counted
	// for the WHOLE handler (including streams) and released on every
	// exit path via defer. sync.Map keyed by label; values are *int32
	// created on first sight and never deleted (label set is tiny and
	// bounded by config size).
	var inflight *atomic.Int32
	if apiKey.MaxConcurrent > 0 {
		v, _ := g.keyInflight.LoadOrStore(label, new(atomic.Int32))
		inflight = v.(*atomic.Int32)
		for {
			cur := inflight.Load()
			if cur >= int32(apiKey.MaxConcurrent) {
				w.Header().Set("Retry-After", "1")
				w.Header().Set("x-ratelimit-limit-requests", strconv.Itoa(apiKey.MaxConcurrent))
				w.Header().Set("x-ratelimit-remaining-requests", "0")
				writeErr(w, http.StatusTooManyRequests, "concurrency_limited",
					fmt.Sprintf("key %q has %d concurrent requests in flight (limit %d); wait for one to finish", label, cur, apiKey.MaxConcurrent))
				return
			}
			if inflight.CompareAndSwap(cur, cur+1) {
				defer inflight.Add(-1)
				// OpenAI-style rate-limit headers so professional SDKs /
				// agents can self-throttle BEFORE getting a 429 (they read
				// x-ratelimit-remaining-requests and pace themselves --
				// exactly the concurrency management the official SDKs
				// implement). Only sent when the key declares a cap.
				w.Header().Set("x-ratelimit-limit-requests", strconv.Itoa(apiKey.MaxConcurrent))
				w.Header().Set("x-ratelimit-remaining-requests", strconv.FormatInt(int64(apiKey.MaxConcurrent)-1-int64(cur), 10))
				break
			}
		}
	}
	var isOverage bool
	if ent := g.entitlementSnapshot(); ent != nil {
		allowed, reason, overage := ent.check(label)
		isOverage = overage
		if !allowed {
			if strings.HasPrefix(reason, "rate limit:") {
				// 429: the key IS entitled, just over its plan's rolling
				// window cap (checkWindowCap) — a distinct, temporary
				// condition from not being entitled at all.
				writeErr(w, http.StatusTooManyRequests, "rate_limited", reason)
				return
			}
			// 403, not 401: the key itself authenticated fine — this
			// is "who you are is known and not currently entitled",
			// a distinct condition from "we don't know who you are".
			writeErr(w, http.StatusForbidden, "subscription_required", reason)
			return
		}
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxBodyBytes))
	if err != nil {
		writeErr(w, http.StatusRequestEntityTooLarge, "request_too_large", "request body exceeds limit")
		return
	}
	var req map[string]any
	if err := json.Unmarshal(body, &req); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "body is not valid JSON")
		return
	}
	modelID, _ := req["model"].(string)
	g.mu.RLock()
	m, ok := g.byID[modelID]
	// Route to the model's own backend when it declares one. A model WITH a
	// distinct upstream_addr whose proxy is missing (config/reload skew,
	// audit 2026-09-01 L11) is a 503, not a silent fallback: the default
	// upstream may not host that upstream_id at all, and quietly forwarding
	// hides the skew behind a confusing "model not supported" from the wrong
	// backend.
	upstreamAddr := m.upstreamAddr(g.cfg.UpstreamAddr)
	upstreamKey := upstreamKeyFor(g.cfg, upstreamAddr)
	foreignUpstream := upstreamAddr != g.cfg.UpstreamAddr
	proxy, proxyOK := g.proxies[upstreamAddr]
	if !proxyOK {
		if m.upstreamAddr(g.cfg.UpstreamAddr) != g.cfg.UpstreamAddr {
			g.mu.RUnlock()
			writeErr(w, http.StatusServiceUnavailable, "server_error", "model upstream unavailable")
			return
		}
		proxy = g.proxy
	}
	threshold := g.largeContextThreshold
	sem := g.largeContextSem
	g.mu.RUnlock()
	if !ok {
		writeErr(w, http.StatusNotFound, "model_not_found", "unknown model "+fmt.Sprintf("%q", modelID))
		return
	}
	if hasImageContent(req) && !m.acceptsImages() {
		writeErr(w, http.StatusBadRequest, "invalid_request_error",
			fmt.Sprintf("model %q does not accept image input (text only)", m.ID))
		return
	}
	// estTokens is computed once and reused below: admission control, the
	// context-length-fit clamp, and error-log correlation all need it.
	msgBytes := messagesBytes(req)
	estTokens := msgBytes / 4
	// calibKey identifies the conversation for the per-session prompt-size
	// calibration used by the context-fit clamp below. X-Session-Id is what
	// cmd/launch's proxy sends (newProxySessionID) and what already lands in
	// the ledger's session_id, so both layers calibrate the same thing.
	// Without one we key by API-key label + model: coarser than a session
	// (several conversations from one key share a bucket) but still far
	// closer to reality than chars/4, and the sanity bounds in
	// promptCalibrator.record catch a mismatched pairing.
	// L8 (2026-09-01 audit): always namespace by the caller's key label —
	// X-Session-Id is client-controlled, and an un-namespaced map let one
	// valid key spray unique ids to evict every other key's calibrated
	// sessions. Same label+model fallback as before when no session header.
	sessionHeader := boundedSessionID(r.Header.Get("X-Session-Id"))
	calibKey := label + "\x00" + sessionHeader
	if sessionHeader == "" {
		calibKey = label + "\x00" + modelID
	}
	// Admission control for large-context requests — see
	// gwConfig.LargeContextTokenThreshold's doc for the incident this
	// closes. estimateMessageTokens is coarse on purpose (chars/4, no real
	// tokenizer call) — it only needs to catch "this is huge", not be
	// exact. threshold < 0 disables the check entirely.
	// Priority keys skip the gate entirely (see gwKey.Priority): the pool is
	// a latency tax, and not paying it is what a priority key buys. They
	// stay unbounded on purpose -- the pool still bounds everyone else, so
	// the pathological all-large-at-once shape from the 2026-08-29 incident
	// cannot be reproduced by the non-priority majority.
	if threshold >= 0 && sem != nil && estTokens >= threshold && !apiKey.Priority {
		select {
		case sem <- struct{}{}:
			defer func() { <-sem }()
		default:
			w.Header().Set("Retry-After", "2")
			writeErr(w, http.StatusTooManyRequests, "large_context_admission_limited",
				fmt.Sprintf("too many large-context requests (>=%d estimated tokens) in flight; retry shortly", threshold))
			return
		}
	}
	req["model"] = m.upstreamID()
	stream, _ := req["stream"].(bool)
	// Clamp the output budget to what is published in /models. Two reasons:
	// max_tokens above max_completion_tokens was accepted verbatim (audit),
	// and a NON-streaming reply must finish before Cloudflare's 100 s edge
	// timeout -- at ~80 tok/s per stream that is ~8k tokens. Above that the
	// client got a Cloudflare-branded text/plain 504 after the GPU had
	// already done the work. Streaming has no such ceiling (headers go out
	// immediately), so it keeps the full published limit.
	limit := m.MaxCompletionTokens
	if !stream && limit > nonStreamMaxTokens {
		limit = nonStreamMaxTokens
	}
	// Per-key output ceiling on top of the model-level clamp (see
	// gwKey.MaxCompletionTokens). Strictly downward: min() so a key can
	// tighten its own budget but never raise the published model limit.
	if apiKey.MaxCompletionTokens > 0 && (limit <= 0 || apiKey.MaxCompletionTokens < limit) {
		limit = apiKey.MaxCompletionTokens
	}
	if limit > 0 {
		// `n` and `best_of` fan out inside ONE request, so a ceiling on max_tokens
		// is a ceiling per choice: a key configured for 100 output tokens got ~6,400
		// on the OpenAI doors with n=64 (and more with best_of) while /v1/messages,
		// which builds its upstream body from scratch, could never carry either, and
		// MaxConcurrent counted the request once. Refused rather than narrowed to
		// one, because the client asked for n choices and would be answered with
		// one (2026-09-29 audit, round 108, F108-L3-2).
		for _, k := range []string{"n", "best_of"} {
			if choiceFanOut(req[k]) {
				writeErr(w, http.StatusBadRequest, "invalid_request_error",
					fmt.Sprintf("%s greater than 1 is not supported: output is capped per request", k))
				return
			}
		}
		// A body that does not state a positive numeric cap is held to the
		// ceiling all the same. The clamp below rewrites a key only when it holds
		// a positive number, so the field absent (the default shape of most
		// OpenAI SDK calls), null, a numeric string an upstream coerces, or a
		// non-positive number went upstream uncapped: a per-key
		// MaxCompletionTokens and the model's published limit were escaped by
		// omitting the field, the non-stream 8k clamp that keeps a reply under
		// Cloudflare's edge timeout never ran for the same body, and the ledger
		// row recorded a budget of 0. The Anthropic surface, where max_tokens is
		// required, could only ask for a huge cap and got the clamp. A field that
		// is not a positive number is dropped, and when nothing positive is
		// stated the ceiling is stated in its place; the clamps that follow then
		// tighten it to what fits (2026-09-29 audit, round 107, F107-L3-1).
		stated := false
		for _, k := range []string{"max_tokens", "max_completion_tokens"} {
			switch v := req[k].(type) {
			case float64:
				if v > 0 {
					stated = true
					continue
				}
			case int:
				if v > 0 {
					stated = true
					continue
				}
			}
			delete(req, k)
		}
		if !stated {
			req["max_tokens"] = limit
		}
		for _, k := range []string{"max_tokens", "max_completion_tokens"} {
			// Compare as float64, never through int(): a client asking for
			// max_tokens 1e19 wrapped int(v) to a negative, so the comparison
			// was false and the absurd ask was forwarded verbatim (2026-09-27
			// audit, round 32). The int case is the fit clamp below rewriting
			// the field, or any future caller that writes one.
			switch v := req[k].(type) {
			case float64:
				if v > float64(limit) {
					req[k] = limit
				}
			case int:
				if v > limit {
					req[k] = limit
				}
			}
		}
	}
	// Context-length-fit clamp — real 2026-08-29 incident: a Claude Code
	// session's own auto-compaction call itself failed with "maximum
	// context length is 262144 tokens... requested 230145 input + 32000
	// output = 262145" -- prompt_tokens + max_tokens exceeded the model's
	// context_length by exactly ONE token, a hard 400 from upstream with
	// no way for the client to recover except /clear (losing the whole
	// session). The client has no way to know the model's real
	// context_length or its own coarse prompt-size estimate the way we
	// do; the gateway does, and can just... not send a request doomed to
	// fail. Clamp max_tokens down to whatever fits instead of forwarding
	// a request that's already guaranteed to 400 -- a shorter real
	// completion beats a hard failure every time, especially for an
	// automatic compaction call the client can't retry with a smaller ask
	// on its own. estTokens is the same coarse chars/4 estimate used for
	// admission control.
	//
	// contextFitMarginRatio is NOT a flat token count -- a real 2026-08-29
	// recurrence (same session, 22x in a row) proved a fixed 2048-token
	// margin isn't remotely enough: this exact incident's own estimate was
	// 183,315 tokens against a REAL upstream count of 230,145 -- a 26%
	// underestimate, because dense code/tool-schema content tokenizes far
	// more compactly than chars/4 assumes. chars/4 is calibrated for
	// average English prose; it can miss badly on code-heavy payloads,
	// and the miss scales with prompt size, not a fixed amount. 30% is a
	// deliberate buffer above that observed 26% error, not a guess -- still
	// a heuristic, not a hard guarantee, but calibrated against a real
	// failure instead of an arbitrary round number.
	// minViableCompletion is the smallest max_tokens worth forwarding at
	// all. A real 2026-08-30 recurrence proved the OLD unconditional
	// "floor fitBudget at 256" rule was itself unsafe: that request's real
	// prompt was 261,889 tokens -- already 255 tokens short of the
	// 262,144 ceiling on its own -- so flooring max_tokens to 256 still
	// produced prompt+output=262,145, one over, the exact failure this
	// clamp exists to prevent. When the real prompt leaves less room than
	// this, there is no safe positive max_tokens to force -- reject the
	// request client-side with a clear reason instead of forwarding one
	// still guaranteed to 400 upstream.
	//
	// 2026-08-30 UPDATE: the 30%/4096 pair below is now only the
	// UNCALIBRATED path -- the first request of a session, where we have
	// nothing better. It is unchanged because it is the well-tested safe
	// default, but the same day proved it also errs the OTHER way: on the
	// client proxy's identical clamp, an ~806 KB Claude Code body estimated
	// at 201,670 tokens x 1.30 = 262,171 tripped this rejection against a
	// 262,144 window, while the REAL prompt was ~243,000 -- ~19,000 tokens
	// of headroom thrown away. And the request rejected was Claude Code's
	// own auto-compaction call, the one request that would have shrunk the
	// session, so the session could not recover on its own.
	// contextFitPlan therefore prefers a per-session tokens-per-byte ratio
	// measured from a real usage.prompt_tokens (context_calibration.go),
	// with a 3%/512 margin, and only falls back to these constants.
	const minViableCompletion = 16
	const contextFitMarginRatio = uncalibratedMarginRatio
	const contextFitMarginFloor = uncalibratedMarginFloor
	if m.ContextLength > 0 {
		estFit, margin, _ := contextFitPlan(g.calibrator(), calibKey, msgBytes)
		fitBudget := m.ContextLength - estFit - margin
		if fitBudget < minViableCompletion {
			// Anthropic's exact "prompt is too long: N tokens > M maximum"
			// wording -- see promptTooLongMessage for why it is load-bearing.
			writeErr(w, http.StatusBadRequest, "invalid_request_error",
				promptTooLongMessage(estFit, m.ContextLength-minViableCompletion))
			return
		}
		for _, k := range []string{"max_tokens", "max_completion_tokens"} {
			// Both shapes: JSON unmarshalling only ever produces float64, but
			// the output-budget clamp above rewrites the field with a plain
			// int, and a float64-only assertion silently skipped this clamp on
			// exactly the requests that ask for more than the window can hold
			// (2026-09-27 audit, round 31). Same two cases as the maxTokens
			// read further down, which was fixed the same way.
			// Compared as float64, never through int(): int(1e19) wraps
			// negative, so the comparison was false and an absurd budget was
			// forwarded verbatim on the one path where this clamp is the only
			// guard — a model that publishes no max_completion_tokens, because
			// the output-budget clamp above runs only when one is published
			// (2026-09-27 audit, round 34, B-F4). Same conversion the
			// output-budget clamp lost in round 32, and the same wrap.
			switch v := req[k].(type) {
			case float64:
				if v > float64(fitBudget) {
					req[k] = fitBudget
				}
			case int:
				if v > fitBudget {
					req[k] = fitBudget
				}
			}
		}
	}
	if stream {
		so, _ := req["stream_options"].(map[string]any)
		if so == nil {
			so = map[string]any{}
		}
		// Always on. A client sending include_usage:false produced a 200 with
		// zero metered tokens -- a billing bypass. OpenAI clients tolerate the
		// trailing usage-only chunk.
		so["include_usage"] = true
		req["stream_options"] = so
	}
	nb, err := json.Marshal(req)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid_request_error", "could not re-encode request")
		return
	}
	r.Body = io.NopCloser(bytes.NewReader(nb))
	r.ContentLength = int64(len(nb))
	r.Header.Set("Content-Length", fmt.Sprint(len(nb)))
	// Never forward the caller's public key upstream; gatekeeper (if it is
	// the upstream) has its own keys. Replace with the gateway's upstream
	// credential when one is configured via env.
	if upstreamKey != "" {
		r.Header.Set("Authorization", "Bearer "+upstreamKey)
	} else {
		r.Header.Del("Authorization")
	}
	// Also strip every other caller-credential header (2026-09-01 security
	// audit H1): Anthropic-wire clients send the key in X-Api-Key, Azure-style
	// callers in api-key, and cookies pass through untouched. A model with its
	// own upstream_addr can point at a third-party endpoint — relaying the
	// caller's key there would leak it into someone else's logs.
	for _, h := range []string{"X-Api-Key", "Api-Key", "Cookie"} {
		r.Header.Del(h)
	}

	rid := newRequestID()
	w.Header().Set("X-Request-Id", rid)
	rec := &usageRecorder{ResponseWriter: w, status: http.StatusOK, stream: stream}
	start := time.Now()
	sessionID := sessionHeader
	// backend is filled in by ModifyResponse (see ctxKeyBackend) once the
	// upstream actually answers -- stays empty if the request never
	// reached a backend (blocked earlier, or the error path never set
	// oaicalb's header).
	backend := new(string)
	// The row exists to correlate "estimated prompt + output budget vs the
	// model's context_length", so record the budget the client asked for
	// under either spelling, and handle the types the clamps above leave
	// behind: a float64 (as unmarshaled from client JSON) or a plain int (if
	// the clamp rewrote it -- see that loop's req[k] = limit). A conversion
	// straight through int() wrapped a 1e19 ask to a negative and the row
	// recorded that instead of the clamped value (2026-09-27 audit, round 32).
	outputBudget := func(v any) int {
		switch n := v.(type) {
		case float64:
			if n >= math.MaxInt64 {
				return math.MaxInt64
			}
			if n <= 0 {
				return 0
			}
			return int(n)
		case int:
			return n
		}
		return 0
	}
	maxTokens := outputBudget(req["max_tokens"])
	if b := outputBudget(req["max_completion_tokens"]); b > maxTokens {
		maxTokens = b
	}
	errInfo := &errCaptureInfo{
		RequestID: rid, SessionID: sessionID, Model: modelID,
		EstTokens: estTokens, MaxTokens: maxTokens, ReqBytes: len(body),
		Calibrate: func(promptTokens int) { g.calibrator().record(calibKey, msgBytes, promptTokens) },
	}
	ctx := context.WithValue(r.Context(), ctxKeyBackend{}, backend)
	ctx = context.WithValue(ctx, ctxKeyErrCapture{}, errInfo)
	// Wall-clock cap — see gwConfig.RequestTimeoutSec's doc. The deadline
	// rides the request context, so ReverseProxy's outgoing request (and
	// the buffered body copy for non-stream) inherits it: at expiry the
	// transport closes the upstream connection, rec finishes with whatever
	// status it has, and the ledger row records Abort/timeouts like any
	// other mid-stream abort — no special-casing downstream. The deadline
	// must NOT wrap the ledger write itself, hence a fresh cancel not
	// deferred to handler exit in a way that races rec.finish (it can't:
	// cancel only kills the upstream transfer, writes here are local).
	g.mu.RLock()
	if g.requestTimeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, g.requestTimeout)
		defer cancel()
	}
	g.mu.RUnlock()
	r = r.WithContext(ctx)
	// The ledger write is DEFERRED so it runs on every exit path: normal
	// completion, a client that disconnects mid-stream (ReverseProxy panics
	// with http.ErrAbortHandler), or an upstream failure. Before this, an
	// aborted stream burned GPU time and left no row (audit 2026-08-25).
	// The panic is re-raised after logging so net/http still handles it.
	aborted := false
	defer func() {
		if p := recover(); p != nil {
			aborted = true
			rec.finish()
			g.writeLedger(g.entry(rec, m, label, rid, inboundPath(r), stream, start, aborted, *backend, sessionID, isOverage))
			panic(p)
		}
	}()
	// X-Oaica-Metered tells oaicalb (downstream through gatekeeper) that
	// this request is already being billed here -- see oaicalb's
	// meterAndServe/requestAlreadyMetered. Without this, a request routed
	// through both the gateway and oaicalb's own usage reporter (added
	// 2026-08-29 to catch traffic that bypasses the gateway entirely)
	// would be counted twice.
	//
	// Only the gateway's own upstream is downstream of oaicalb: a model on another
	// upstream is sent neither that marker nor the caller's address, which
	// ReverseProxy appends to X-Forwarded-For unless the header is present and nil
	// (2026-09-29 audit, round 109, F109-L3-3).
	if foreignUpstream {
		r.Header["X-Forwarded-For"] = nil
	} else {
		r.Header.Set("X-Oaica-Metered", "1")
	}
	proxy.ServeHTTP(rec, r)
	rec.finish()
	// Ground truth for the next request of this session: only a 200 whose
	// usage was actually seen (rec.seen covers the stream's final usage-only
	// chunk as well as a non-stream usage object) -- never an error, never a
	// zero. See context_calibration.go for the incident. The status asked is
	// the one the CLIENT was told, not the upstream's own: a turn the bridge
	// refused stated the upstream's 200 and stated usage, and reading that as
	// ground truth made a refused turn the session's calibration and its
	// recent success (2026-09-29 audit, round 92, F92-L3-1).
	if rec.clientStatus() == http.StatusOK && rec.seen && rec.usage.PromptTokens > 0 {
		g.lastOKAt.Store(time.Now().Unix())
		g.calibrator().record(calibKey, msgBytes, rec.usage.PromptTokens)
	}
	g.writeLedger(g.entry(rec, m, label, rid, inboundPath(r), stream, start, aborted, *backend, sessionID, isOverage))
}

// inboundPathKey carries the path the client asked for across the /v1/messages
// bridge, which rewrites r.URL.Path to reach the upstream's OpenAI wire.
type inboundPathKey struct{}

// withInboundPath records the path the client asked for on the request
// context. Call it BEFORE any rewrite of r.URL.Path.
func withInboundPath(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), inboundPathKey{}, r.URL.Path))
}

// inboundPath returns the path the client asked for, falling back to the
// request's current path when nothing was recorded (every direct caller).
func inboundPath(r *http.Request) string {
	if p, ok := r.Context().Value(inboundPathKey{}).(string); ok && p != "" {
		return p
	}
	return r.URL.Path
}

// entry builds the ledger row for one completion.
func (g *gateway) entry(rec *usageRecorder, m gwModel, label, rid, path string, stream bool, start time.Time, aborted bool, backend, sessionID string, overage bool) ledgerEntry {
	u := rec.usage.nonNegative()
	// The status the CLIENT will read, when the writer knows better than the
	// upstream's own header: the /v1/messages bridge answers an untranslatable
	// 200 with 502 and that decision is made after this row is built (see
	// ledgerStatusWriter). Resolved by the recorder itself so the row and the
	// ground-truth guard above cannot drift apart again (2026-09-29 audit,
	// round 92, F92-L3-1).
	status := rec.clientStatus()
	// A translating writer may know the counts when the upstream stated none:
	// the recorder reads the UPSTREAM's bytes, and an upstream that sends no
	// usage object at all leaves the row recording zero for a turn the client was
	// served and the upstream billed. The /v1/messages bridge is the writer that
	// can say — the same estimate it sends the client — so the row and the client
	// agree (2026-09-27 audit, round 39, C-F3; per field since round 40, B40-2).
	// UsageSeen is deliberately left alone: it records whether the UPSTREAM
	// stated usage, and these counts are this gateway's own reading of the turn,
	// not the upstream's word for it.
	//
	// Only for a turn the client was told SUCCEEDED. A turn the upstream refused
	// (429) or that the bridge itself failed (502 for no answer, an unparseable
	// body, or an error frame mid-stream) was not served and not billed, and
	// booking it with the prompt estimate and a positive cost_usd charged the
	// overage accounting for an answer nobody received — the bridge's own
	// estimate of a turn that produced nothing (2026-09-27 audit, round 40,
	// B40-1).
	if status == http.StatusOK {
		if est, ok := rec.ResponseWriter.(interface{ EstimatedUsage() (usage, bool) }); ok {
			if eu, has := est.EstimatedUsage(); has {
				u.fillEmpty(eu.nonNegative())
			}
		}
	}
	// A hit the upstream stated is a statement about the prompt: tokens served
	// from its prefix cache are tokens OF this prompt, so a row that recorded
	// prompt_tokens below its own cached_tokens would record a fact that cannot
	// hold, and the fresh count the cost math derives from the two
	// (prompt - cached) would be negative. Raised here rather than left to the
	// estimate, so a path with no translating writer to ask still books a
	// coherent row (2026-09-27 audit, round 40, A40-3).
	//
	// Read through statedCacheHit() and not cachedTokens(): the latter clamps
	// the hit to a stated prompt, which made this raise unreachable for the
	// shape it is about — the one where the prompt WAS stated and the hit
	// exceeded it — so the client was told a 5000-token cache read for a turn
	// this row booked at 1000 (2026-09-27 audit, round 41, B41-1).
	if hit := u.statedCacheHit(); hit > u.PromptTokens {
		u.PromptTokens = hit
	}
	cached := u.cachedTokens()
	cost, tier := computeCostUSDTiered(m.Pricing, m.PricingTiers, u.PromptTokens, cached, u.CompletionTokens)
	// And a turn the client was told FAILED is not a turn this gateway may
	// charge for at all, whatever the upstream stated about it. The guard above
	// only ever suppressed this gateway's OWN estimate; the counts the upstream
	// stated entered u unconditionally, so a 502 row — refused before the client
	// received an answer — was still booked with a positive cost_usd, and the
	// overage accounting charges what this row costs: every refused turn was
	// charged once for nothing served and again for its retry. Zeroed here, at
	// the one place the row is built, rather than at each failing arm
	// (2026-09-27 audit, round 45, B45-14).
	// And neither may a turn the client was told FAILED carry counts or a claim
	// that the upstream stated them. A refused turn served the client no answer
	// and stated no usage to it, so the row that books one is a record of a turn
	// that did not happen: a stream cut between a usage frame and its terminator
	// (the gateway asks for stream_options.include_usage, so the usage frame IS
	// the last frame before the sentinel) reached the client as a 502 and was
	// booked as the upstream's own 9000/500 with usage_seen=true — the same
	// refused bytes as a body the buffered arm read as 0/0/false, one upstream
	// body and two rows. Zeroed at the row for cost's reason: at each failing arm
	// would be as many arms as there are spellings of a failure (2026-09-29
	// audit, round 89, F89-L3-1/F89-L3-3).
	seen := rec.seen
	if status != http.StatusOK {
		u, cached, seen = usage{}, 0, false
		cost, tier = 0, 0
	}
	return ledgerEntry{
		TS:               start.UTC().Format(time.RFC3339Nano),
		RequestID:        rid,
		KeyLabel:         label,
		Model:            m.ID,
		UpstreamModel:    m.upstreamID(),
		Path:             path,
		Stream:           stream,
		Status:           status,
		PromptTokens:     u.PromptTokens,
		CompletionTokens: u.CompletionTokens,
		CachedTokens:     cached,
		LatencyMS:        time.Since(start).Milliseconds(),
		UsageSeen:        seen,
		Aborted:          aborted,
		Backend:          backend,
		SessionID:        sessionID,
		CostUSD:          cost,
		Overage:          overage,
		PriceTier:        tier,
	}
}

// computeCostUSD applies cache-hit-aware pricing: cachedTokens bill at
// CachedPrompt's rate (when set) instead of Prompt's, the rest of
// promptTokens plus completionTokens bill at their normal rates. See
// gwPricing.CachedPrompt's doc for why this split exists. Malformed or
// empty price strings return 0 rather than erroring -- pricing here is
// informational (no billing enforcement exists), a parse failure must
// never affect the response the caller actually gets.
func computeCostUSD(p gwPricing, promptTokens, cachedTokens, completionTokens int) float64 {
	cost, _ := computeCostUSDTiered(p, nil, promptTokens, cachedTokens, completionTokens)
	return cost
}

// computeCostUSDTiered is computeCostUSD plus optional context-tiered input
// pricing (see gwModel.PricingTiers). The bracket is chosen by the REAL
// total prompt_tokens reported by upstream -- including the cached ones,
// because what makes a request expensive to schedule is how long the context
// is, not how much of it happened to hit the prefix cache -- and the
// bracket's rate then applies to the UNCACHED tokens only, since the cached
// ones cost us near-nothing to serve and keep CachedPrompt's flat rate.
// Returns the cost and the bracket's upper bound for ledgerEntry.PriceTier
// (0 = the catch-all, or no tiers configured).
func computeCostUSDTiered(p gwPricing, tiers []gwPricingTier, promptTokens, cachedTokens, completionTokens int) (float64, int) {
	parse := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return v
	}
	promptRate := parse(p.Prompt)
	completionRate := parse(p.Completion)
	// cachedRate is resolved BEFORE the tier override on purpose: when a
	// model has no cached_prompt set, cached tokens fall back to the flat
	// Prompt rate (today's behavior), never to the tiered one -- otherwise
	// enabling tiers would silently reprice cache hits too.
	cachedRate := promptRate
	if p.CachedPrompt != "" {
		cachedRate = parse(p.CachedPrompt)
	}
	tier := 0
	if len(tiers) > 0 {
		if t, bound, ok := selectPricingTier(tiers, promptTokens); ok {
			promptRate = parse(t.Prompt)
			tier = bound
		}
	}
	if cachedTokens > promptTokens {
		cachedTokens = promptTokens // defensive: upstream data should never do this, but never bill negative fresh tokens if it does
	}
	freshPromptTokens := promptTokens - cachedTokens
	return float64(freshPromptTokens)*promptRate + float64(cachedTokens)*cachedRate + float64(completionTokens)*completionRate, tier
}

// entitlementCache is the fast local read-through cache in front of
// meterhub's subscriber table — see gwConfig.EntitlementEnabled's doc for
// why this exists as a cache rather than a synchronous per-request call.
// One entry per key label; refreshed on read when stale, never proactively
// polled (a key nobody is currently calling costs nothing to track).
type entitlementCache struct {
	addr     string
	token    string
	ttl      time.Duration
	failOpen bool
	// overageBilling: when true, exceeding a plan's rolling-window cap
	// (checkWindowCap) no longer blocks the request -- it's let through
	// and flagged Overage=true on the ledger row (see ledgerEntry.Overage
	// and computeCostUSD's caller) for a billing job to charge at the
	// overage rate, same pattern MiniMax's $5-100 credit top-ups use.
	// Canceled/suspended subscription status (the OTHER half of check())
	// is unaffected by this flag -- overage billing only ever applies to
	// an otherwise-active subscriber going over their window, never to
	// someone who isn't entitled at all. Default false: flipping this
	// silently would change existing 429-blocking behavior for anyone
	// who already has EntitlementEnabled on.
	overageBilling bool
	client         *http.Client

	mu      sync.Mutex
	entries map[string]entitlementCacheEntry
}

type entitlementCacheEntry struct {
	allowed   bool
	reason    string
	overage   bool
	fetchedAt time.Time
}

func newEntitlementCache(addr, token string, ttl time.Duration, failOpen, overageBilling bool) *entitlementCache {
	return &entitlementCache{
		addr: strings.TrimRight(addr, "/"), token: token, ttl: ttl, failOpen: failOpen, overageBilling: overageBilling,
		client:  &http.Client{Timeout: 3 * time.Second},
		entries: make(map[string]entitlementCacheEntry),
	}
}

// check returns whether label may proceed, a human-readable reason when
// it may not (or when it may but as billed overage), and whether this was
// an overage admission (see entitlementCache.overageBilling's doc). Reads
// the cache first; only reaches meterhub when the entry is missing or
// older than ttl, so a hot key never pays a network round trip on the
// request path.
func (c *entitlementCache) check(label string) (allowed bool, reason string, overage bool) {
	c.mu.Lock()
	e, ok := c.entries[label]
	c.mu.Unlock()
	if ok && time.Since(e.fetchedAt) < c.ttl {
		return e.allowed, e.reason, e.overage
	}

	allowed, reason, overage, authoritative := c.fetchAndDecide(label)
	c.mu.Lock()
	// A DEGRADED decision (meterhub unreachable) must not stick for the full
	// TTL (2026-09-01 security audit M6): in fail-open mode that was a
	// ttl-long billing bypass per key after recovery; in fail-closed mode it
	// refused real traffic for the same window. Degraded entries get a 5s
	// TTL so the next request re-probes meterhub.
	fetchedAt := time.Now()
	if !authoritative {
		fetchedAt = fetchedAt.Add(-c.ttl).Add(5 * time.Second)
	}
	c.entries[label] = entitlementCacheEntry{allowed: allowed, reason: reason, overage: overage, fetchedAt: fetchedAt}
	c.mu.Unlock()
	return allowed, reason, overage
}

// fetchAndDecide queries meterhub for label's subscriber status and
// applies the fail-open/fail-closed policy. Never blocks longer than the
// client's 3s timeout — a slow or unreachable meterhub degrades to
// whatever failOpen says, it never hangs the request.
// The 4th return, authoritative, is false when the answer came from a
// DEGRADED path (meterhub unreachable / malformed response) rather than an
// actual subscriber lookup — check() caches those for only 5s (see its doc).
func (c *entitlementCache) fetchAndDecide(label string) (bool, string, bool, bool) {
	req, err := http.NewRequest(http.MethodGet, c.addr+"/subscribers/get?key="+url.QueryEscape(label), nil)
	if err != nil {
		return c.failOpen, "entitlement check unavailable", false, false
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if c.failOpen {
			return true, "", false, false
		}
		return false, "entitlement service unreachable", false, false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if c.failOpen {
			return true, "", false, false
		}
		return false, "entitlement check failed", false, false
	}
	var s struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		if c.failOpen {
			return true, "", false, false
		}
		return false, "entitlement check failed", false, false
	}
	switch s.Status {
	case "active", "past_due":
		allowed, reason, capAuthoritative := c.checkWindowCap(label)
		// allowed=false with overageBilling on can only happen here if
		// checkWindowCap itself failed open/closed on an unreachable
		// meterhub (reason won't have the "rate limit:" prefix in that
		// case) -- overage is specifically "was over cap but let through
		// anyway", never "was blocked for an unrelated reason".
		overage := c.overageBilling && strings.HasPrefix(reason, "rate limit:")
		if overage {
			return true, reason, true, true
		}
		// The status lookup answered, but the cap lookup behind it may not
		// have -- it degrades exactly like the status probe does (meterhub
		// unreachable, non-200, undecodable), and that degradation has to
		// reach check()'s 5s TTL. Stamping it authoritative cached a paying
		// subscriber's 403 (or, fail-open, an over-cap key's admission) for
		// the whole EntitlementCacheTTLSec after a blip (2026-09-27 audit,
		// round 32, B-F2 -- the same M6 fix the status path already had).
		return allowed, reason, false, capAuthoritative
	case "canceled":
		return false, "subscription canceled", false, true
	case "suspended":
		return false, "account suspended", false, true
	default: // "unknown" — no subscriber record at all
		if c.failOpen {
			return true, "", false, false
		}
		return false, "no active subscription for this key", false, true
	}
}

// checkWindowCap is the enforcement side of meterhub's
// /subscribers/usage instrumentation (tools/meterhub's planLimits): an
// active/past_due subscriber can still be over their plan's rolling 5h or
// 7d REQUEST cap (docs/PRICING.md's tiers), which is a distinct condition
// from their subscription status. Only reached once status is already
// known active/past_due — a canceled/suspended key never gets this far.
// Same fail-open/fail-closed policy as the status check: a meterhub
// hiccup here degrades to c.failOpen rather than blocking (or silently
// admitting) every request while it's unreachable.
//
// The cap counts REQUESTS as of 2026-09-28, not tokens: the `over` flags
// this decodes are computed on the window's request count (usage rows are
// one per request — request_id is that table's primary key), which is
// what the rate card sells. The window's token total is still reported
// beside it for the audit trail and no longer gates anything, so an old
// binary of this gateway reading a current meterhub is capped by
// requests too — the flag, not this file, carries the semantics.
//
// The third return, authoritative, is fetchAndDecide's 4th: false when this
// answer came from a DEGRADED path rather than an actual usage lookup, so
// check() gives it the short TTL instead of the full one. Every degraded
// branch below says so.
func (c *entitlementCache) checkWindowCap(label string) (allowed bool, reason string, authoritative bool) {
	req, err := http.NewRequest(http.MethodGet, c.addr+"/subscribers/usage?key="+url.QueryEscape(label), nil)
	if err != nil {
		return c.failOpen, "usage check unavailable", false
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if c.failOpen {
			return true, "", false
		}
		return false, "usage check unreachable", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if c.failOpen {
			return true, "", false
		}
		return false, "usage check failed", false
	}
	var u struct {
		Window5h struct {
			Over bool `json:"over"`
		} `json:"window_5h"`
		Window7d struct {
			Over bool `json:"over"`
		} `json:"window_7d"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&u); err != nil {
		if c.failOpen {
			return true, "", false
		}
		return false, "usage check failed", false
	}
	if u.Window5h.Over {
		return false, "rate limit: 5-hour request cap exceeded, resets on a rolling window", true
	}
	if u.Window7d.Over {
		return false, "rate limit: weekly request cap exceeded, resets on a rolling window", true
	}
	return true, "", true
}

// mux builds the routing table. It lives here rather than inline in main
// so the tests exercise the exact same route set the binary serves -- the
// two used to be separate copies and drifted.
func mux(g *gateway) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("/health", g.healthHandler)
	m.HandleFunc("/privacy", legalHandler("PRIVACY.md"))
	m.HandleFunc("/terms", legalHandler("TERMS.md"))
	m.HandleFunc("/status", legalHandler("STATUS.md"))
	// /models is public (2026-08-26): it is served from in-memory config
	// (no upstream call) and contains only what the OpenRouter listing and
	// oaica.com already publish -- ids, context, limits, pricing, modalities.
	// Keeping it behind the key only risked OpenRouter's model poller not
	// sending one and the listing silently never appearing. Completions
	// stay authenticated; nothing about metering changes.
	m.HandleFunc("/models", g.modelsHandler)
	m.HandleFunc("/v1/models", g.modelsHandler)
	// Weights distribution for `oaica pull` (pull.go). Restored 2026-08-30:
	// the api.oaica.com cutover onto this gateway dropped these routes with
	// the old TypeScript router, so every pull 404'd. Unmetered, unledgered,
	// and independent of api_keys -- see pull.go's header comment.
	m.HandleFunc("/v1/manifest/", g.manifestHandler)
	m.HandleFunc("/v1/pull/", g.pullHandler)
	m.HandleFunc("/v1/catalog", g.catalogHandler)
	m.HandleFunc("/v1/chat/completions", g.completionHandler)
	// Anthropic Messages wire — translated to the OpenAI backend shape and
	// served through the same metered completion path (see messages.go).
	m.HandleFunc("/v1/messages", g.messagesHandler)
	m.HandleFunc("/v1/completions", g.completionHandler)
	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown route")
	})
	return m
}

func main() {
	configPath := flag.String("config", "", "path to oaica-gateway JSON config")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatalf("oaica-gateway: %v", err)
	}
	g := &gateway{}
	if err := g.apply(cfg); err != nil {
		log.Fatalf("oaica-gateway: %v", err)
	}
	log.Printf("oaica-gateway: %d models, %d keys (%d priority), upstream=%s (%d distinct upstreams), listen=%s, ledger=%s",
		len(cfg.Models), len(cfg.APIKeys), priorityKeyCount(cfg), cfg.UpstreamAddr, distinctUpstreams(cfg), cfg.ListenAddr, cfg.LedgerPath)

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			g.reload(*configPath)
		}
	}()

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux(g),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// No WriteTimeout: streamed completions run for minutes.
	}
	log.Fatal(srv.ListenAndServe())
}

// choiceFanOut reports whether a request field asks for more than one completion.
// The number may be spelled as a JSON number or as a numeric string an upstream
// coerces; anything else is left for the upstream to refuse.
func choiceFanOut(v any) bool {
	switch n := v.(type) {
	case float64:
		return n > 1
	case int:
		return n > 1
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(n), 64)
		return err == nil && f > 1
	}
	return false
}
