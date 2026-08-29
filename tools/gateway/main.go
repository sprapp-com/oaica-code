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
//	  } ]
//	}
//
// Hash a key for the config with: printf '%s' "$KEY" | sha256sum
package main

import (
	"bufio"
	"bytes"
	"context"
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
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
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
// response can complete inside the proxy's 90 s ResponseHeaderTimeout (and
// Cloudflare's 100 s TTFB limit behind it). Streaming is not bounded by
// this (only by the model's max_completion_tokens).
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

type gwModel struct {
	ID                  string    `json:"id"`
	UpstreamID          string    `json:"upstream_id,omitempty"` // vLLM --served-model-name; defaults to ID
	OwnedBy             string    `json:"owned_by"`
	ContextLength       int       `json:"context_length,omitempty"`
	MaxCompletionTokens int       `json:"max_completion_tokens,omitempty"`
	Pricing             gwPricing `json:"pricing"`
	SupportedParameters []string  `json:"supported_parameters,omitempty"`
	// InputModalities is what the model can actually consume: "text" and
	// optionally "image". Empty means text only. kat-awq's config claims a
	// vision tower, but under AWQ an image request produces garbage
	// ("!!!!!!!!" in the reasoning field, verified live 2026-08-26); the
	// gateway must refuse images for such a model rather than bill a
	// customer for noise.
	InputModalities []string `json:"input_modalities,omitempty"`
	Created         int64    `json:"created,omitempty"`
}

func (m gwModel) acceptsImages() bool {
	for _, x := range m.InputModalities {
		if x == "image" {
			return true
		}
	}
	return false
}

// hasImageContent reports whether any message carries an image part in the
// OpenAI chat schema ({"type":"image_url"} or {"type":"input_image"}).
func hasImageContent(req map[string]any) bool {
	msgs, _ := req["messages"].([]any)
	for _, mi := range msgs {
		m, _ := mi.(map[string]any)
		parts, ok := m["content"].([]any)
		if !ok {
			continue
		}
		for _, pi := range parts {
			p, _ := pi.(map[string]any)
			switch p["type"] {
			case "image_url", "input_image", "image":
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
}

type gwConfig struct {
	UpstreamAddr string    `json:"upstream_addr"`
	ListenAddr   string    `json:"listen_addr"`
	LedgerPath   string    `json:"ledger_path"`
	APIKeys      []gwKey   `json:"api_keys"`
	Models       []gwModel `json:"models"`

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
}

// newProxy builds a reverse proxy with its own transport and explicit
// timeouts. ResponseHeaderTimeout returns a clean 504 before Cloudflare's
// 100s edge timeout would turn it into an opaque 524. No overall client
// timeout: streamed completions legitimately run for minutes.
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
		ResponseHeaderTimeout: 90 * time.Second,
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
		// Internal topology must not leak to the public.
		resp.Header.Del("X-Katlb-Backend")
		resp.Header.Del("X-Gatekeeper-Tier")
		resp.Header.Del("X-Gatekeeper-Limit")
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
			if onUpstreamError != nil {
				var info *errCaptureInfo
				if v, ok := resp.Request.Context().Value(ctxKeyErrCapture{}).(*errCaptureInfo); ok {
					info = v
				}
				onUpstreamError(info, resp.StatusCode, code, msg)
			}
			b, _ := json.Marshal(map[string]any{"error": map[string]any{"message": msg, "type": code, "code": code}})
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
		w.Header().Set("Retry-After", "2")
		writeErr(w, status, "upstream_error", "upstream unavailable: "+err.Error())
	}
	return p, nil
}

type gateway struct {
	mu    sync.RWMutex
	cfg   gwConfig
	proxy *httputil.ReverseProxy
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
	largeContextThreshold int
	largeContextSem       chan struct{}

	// /health probe cache; see healthCacheTTL.
	healthMu   sync.Mutex
	healthAt   time.Time
	healthLast healthResult

	// meterCh feeds the background reporter goroutine (see
	// startMeterReporter); nil when MeterHubAddr is unset. Buffered and
	// non-blocking on send — a full channel means meterhub is unreachable
	// for a while, and the right response is to drop that report (the
	// local JSONL ledger already has it) rather than let a billing
	// side-channel add latency or backpressure to real chat completions.
	meterCh chan usageReport

	// entitlement is the subscriber-status cache — see entitlementCache's
	// doc. nil when EntitlementEnabled is false.
	entitlement *entitlementCache
}

func (g *gateway) apply(cfg gwConfig) error {
	p, err := newProxy(cfg.UpstreamAddr, g.logUpstreamError)
	if err != nil {
		return err
	}
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
			return fmt.Errorf("open upstream error log %s: %w", cfg.UpstreamErrorLogPath, err)
		}
	}
	g.mu.Lock()
	g.cfg = cfg
	g.proxy = p
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
	// every other config field here. startMeterReporter is idempotent-safe
	// to call repeatedly: each call gets its own channel/goroutine: the OLD
	// goroutine (if any) keeps draining its own now-orphaned channel until
	// it empties and exits on its own — never killed mid-send, never leaks
	// past a few pending reports.
	if cfg.MeterHubAddr != "" {
		g.meterCh = make(chan usageReport, 256)
		go runMeterReporter(g.meterCh, cfg.MeterHubAddr, cfg.MeterHubToken)
	} else {
		g.meterCh = nil
	}

	if cfg.EntitlementEnabled && cfg.MeterHubAddr != "" {
		ttl := time.Duration(cfg.EntitlementCacheTTLSec) * time.Second
		if ttl <= 0 {
			ttl = 60 * time.Second
		}
		g.entitlement = newEntitlementCache(cfg.MeterHubAddr, cfg.MeterHubToken, ttl, cfg.EntitlementFailOpen, cfg.EntitlementOverageBilling)
	} else {
		g.entitlement = nil
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
	g.mu.Unlock()
	return nil
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
var meterReporterBackoff = func(attempt int) time.Duration { return time.Duration(attempt) * time.Second }

// runMeterReporter drains ch and POSTs each report to meterhub, retrying
// a failed send a bounded number of times with backoff before giving up on
// that one record (the local JSONL ledger already has it durably — a
// meterhub outage can never lose billing data, only delay the aggregated
// view). Exits when ch is closed AND drained.
func runMeterReporter(ch <-chan usageReport, addr, token string) {
	client := &http.Client{Timeout: 5 * time.Second}
	const maxAttempts = 3
	for rep := range ch {
		body, err := json.Marshal(rep)
		if err != nil {
			continue
		}
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			req, err := http.NewRequest(http.MethodPost, strings.TrimRight(addr, "/")+"/ingest", bytes.NewReader(body))
			if err != nil {
				break
			}
			req.Header.Set("Content-Type", "application/json")
			if token != "" {
				req.Header.Set("Authorization", "Bearer "+token)
			}
			resp, err := client.Do(req)
			if err == nil {
				resp.Body.Close()
				if resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK {
					break // delivered
				}
			}
			if attempt < maxAttempts {
				time.Sleep(meterReporterBackoff(attempt))
			}
		}
	}
}

// reportUsage sends e to the meter reporter's channel, non-blocking. A full
// channel (meterhub down/slow for a while) drops the report rather than
// stalling the request that triggered it — see meterCh's doc.
func (g *gateway) reportUsage(e ledgerEntry) {
	g.mu.RLock()
	ch := g.meterCh
	region := g.cfg.Region
	g.mu.RUnlock()
	if ch == nil {
		return
	}
	select {
	case ch <- usageReport{ledgerEntry: e, Region: region}:
	default:
		log.Printf("oaica-gateway: meterhub report channel full, dropping report for %s (local ledger still has it)", e.RequestID)
	}
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
	log.Printf("oaica-gateway: config reloaded: %d models, %d keys, upstream=%s",
		len(cfg.Models), len(cfg.APIKeys), cfg.UpstreamAddr)
}

// keyLabel returns the label for a valid Bearer key, or "" if unauthenticated.
// Constant-time compare of the presented key's digest against every stored
// digest so timing does not leak which key was closest.
func (g *gateway) keyLabel(r *http.Request) string {
	auth := r.Header.Get("Authorization")
	key := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
	if key == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(key))
	presented := []byte(hex.EncodeToString(sum[:]))
	g.mu.RLock()
	defer g.mu.RUnlock()
	label := ""
	for _, k := range g.cfg.APIKeys {
		if subtle.ConstantTimeCompare(presented, []byte(k.SHA256)) == 1 {
			label = k.Label
		}
	}
	return label
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
	if g.healthAt.IsZero() || time.Since(g.healthAt) >= healthCacheTTL {
		g.healthLast = g.probeHealth(r)
		g.healthAt = time.Now()
	}
	res := g.healthLast
	g.healthMu.Unlock()
	w.WriteHeader(res.code)
	json.NewEncoder(w).Encode(res.body)
}

// probeHealth does the real check; healthHandler caches its result.
func (g *gateway) probeHealth(r *http.Request) healthResult {
	g.mu.RLock()
	up := g.cfg.UpstreamAddr
	models := g.cfg.Models
	g.mu.RUnlock()
	if len(models) == 0 {
		return healthResult{http.StatusServiceUnavailable, map[string]any{"status": "down", "reason": "no models configured"}}
	}
	// A real 1-token chat completion, authenticated with the gateway's own
	// upstream credential, through gatekeeper -> katlb -> a replica. This is
	// the only probe that proves a customer request would succeed. The old
	// unauthenticated GET /v1/models got a 401 from gatekeeper and reported
	// "ok" with every replica dead (audit 2026-08-25).
	ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
	defer cancel()
	body := `{"model":` + fmt.Sprintf("%q", models[0].upstreamID()) + `,"messages":[{"role":"user","content":"ping"}],"max_tokens":1,"temperature":0}`
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(up, "/")+"/v1/chat/completions", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if k := os.Getenv("OAICA_GATEWAY_UPSTREAM_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return healthResult{http.StatusServiceUnavailable, map[string]any{"status": "down", "reason": err.Error()}}
	}
	io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return healthResult{http.StatusServiceUnavailable, map[string]any{"status": "down", "reason": fmt.Sprintf("upstream chat probe HTTP %d", resp.StatusCode)}}
	}
	return healthResult{http.StatusOK, map[string]any{"status": "ok"}}
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
}

func (g *gateway) writeLedger(e ledgerEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	g.ledgerMu.Lock()
	if g.ledger != nil {
		g.ledger.Write(append(b, '\n'))
	}
	g.ledgerMu.Unlock()

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
	PromptTokens        int `json:"prompt_tokens"`
	CompletionTokens    int `json:"completion_tokens"`
	PromptTokensDetails *struct {
		CachedTokens int `json:"cached_tokens"`
	} `json:"prompt_tokens_details"`
}

// cachedTokens is nil-safe: PromptTokensDetails is only present once an
// upstream actually populates it (see ledgerEntry.CachedTokens's doc).
func (u usage) cachedTokens() int {
	if u.PromptTokensDetails == nil {
		return 0
	}
	return u.PromptTokensDetails.CachedTokens
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

func (u *usageRecorder) Write(p []byte) (int, error) {
	n, err := u.ResponseWriter.Write(p)
	if u.stream {
		u.scanSSE(p)
	} else if u.body.Len() < 4<<20 {
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

func (u *usageRecorder) scanSSE(p []byte) {
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
			continue
		}
		payload := bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		if len(payload) == 0 || bytes.Equal(payload, []byte("[DONE]")) {
			continue
		}
		var chunk struct {
			Usage *usage `json:"usage"`
		}
		if json.Unmarshal(payload, &chunk) == nil && chunk.Usage != nil {
			u.usage = *chunk.Usage
			u.seen = true
		}
	}
}

func (u *usageRecorder) finish() {
	if u.stream || u.seen {
		return
	}
	var doc struct {
		Usage *usage `json:"usage"`
	}
	if json.Unmarshal(u.body.Bytes(), &doc) == nil && doc.Usage != nil {
		u.usage = *doc.Usage
		u.seen = true
	}
}

func newRequestID() string {
	var b [12]byte
	if _, err := io.ReadFull(randReader, b[:]); err != nil {
		return fmt.Sprintf("req_%d", time.Now().UnixNano())
	}
	return "req_" + hex.EncodeToString(b[:])
}

var randReader = mustRand()

// processStart is the fallback "created" timestamp for /models entries.
var processStart = time.Now().Unix()

func mustRand() io.Reader {
	f, err := os.Open("/dev/urandom")
	if err != nil {
		return bufio.NewReader(strings.NewReader(""))
	}
	return f
}

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
	msgs, ok := req["messages"]
	if !ok {
		return 0
	}
	b, err := json.Marshal(msgs)
	if err != nil {
		return 0
	}
	return len(b) / 4
}

func (g *gateway) completionHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		writeErr(w, http.StatusMethodNotAllowed, "method_not_allowed", "use POST")
		return
	}
	label := g.keyLabel(r)
	if label == "" {
		writeErr(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
		return
	}
	var isOverage bool
	if g.entitlement != nil {
		allowed, reason, overage := g.entitlement.check(label)
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
	proxy := g.proxy
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
	estTokens := estimateMessageTokens(req)
	// Admission control for large-context requests — see
	// gwConfig.LargeContextTokenThreshold's doc for the incident this
	// closes. estimateMessageTokens is coarse on purpose (chars/4, no real
	// tokenizer call) — it only needs to catch "this is huge", not be
	// exact. threshold < 0 disables the check entirely.
	if threshold >= 0 && sem != nil && estTokens >= threshold {
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
	if limit > 0 {
		for _, k := range []string{"max_tokens", "max_completion_tokens"} {
			if v, ok := req[k].(float64); ok && int(v) > limit {
				req[k] = limit
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
	// admission control -- a margin absorbs its inaccuracy so this clamp
	// undershoots slightly rather than still barely exceeding the limit.
	const contextFitMargin = 2048
	if m.ContextLength > 0 {
		fitBudget := m.ContextLength - estTokens - contextFitMargin
		if fitBudget < 256 {
			fitBudget = 256 // never clamp to something too small to be a useful reply
		}
		for _, k := range []string{"max_tokens", "max_completion_tokens"} {
			if v, ok := req[k].(float64); ok && int(v) > fitBudget {
				req[k] = fitBudget
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
	if up := os.Getenv("OAICA_GATEWAY_UPSTREAM_KEY"); up != "" {
		r.Header.Set("Authorization", "Bearer "+up)
	} else {
		r.Header.Del("Authorization")
	}

	rid := newRequestID()
	w.Header().Set("X-Request-Id", rid)
	rec := &usageRecorder{ResponseWriter: w, status: http.StatusOK, stream: stream}
	start := time.Now()
	sessionID := r.Header.Get("X-Session-Id")
	// backend is filled in by ModifyResponse (see ctxKeyBackend) once the
	// upstream actually answers -- stays empty if the request never
	// reached a backend (blocked earlier, or the error path never set
	// oaicalb's header).
	backend := new(string)
	// req["max_tokens"] may be a float64 (as unmarshaled from client JSON) or
	// a plain int (if the non-stream clamp above rewrote it -- see that
	// loop's req[k] = limit) -- handle both rather than silently reading 0
	// for every clamped non-streaming request.
	maxTokens := 0
	switch mt := req["max_tokens"].(type) {
	case float64:
		maxTokens = int(mt)
	case int:
		maxTokens = mt
	}
	errInfo := &errCaptureInfo{
		RequestID: rid, SessionID: sessionID, Model: modelID,
		EstTokens: estTokens, MaxTokens: maxTokens, ReqBytes: len(body),
	}
	ctx := context.WithValue(r.Context(), ctxKeyBackend{}, backend)
	ctx = context.WithValue(ctx, ctxKeyErrCapture{}, errInfo)
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
			g.writeLedger(g.entry(rec, m, label, rid, r.URL.Path, stream, start, aborted, *backend, sessionID, isOverage))
			panic(p)
		}
	}()
	// X-Oaica-Metered tells oaicalb (downstream through gatekeeper) that
	// this request is already being billed here -- see oaicalb's
	// meterAndServe/requestAlreadyMetered. Without this, a request routed
	// through both the gateway and oaicalb's own usage reporter (added
	// 2026-08-29 to catch traffic that bypasses the gateway entirely)
	// would be counted twice.
	r.Header.Set("X-Oaica-Metered", "1")
	proxy.ServeHTTP(rec, r)
	rec.finish()
	g.writeLedger(g.entry(rec, m, label, rid, r.URL.Path, stream, start, aborted, *backend, sessionID, isOverage))
}

// entry builds the ledger row for one completion.
func (g *gateway) entry(rec *usageRecorder, m gwModel, label, rid, path string, stream bool, start time.Time, aborted bool, backend, sessionID string, overage bool) ledgerEntry {
	cached := rec.usage.cachedTokens()
	return ledgerEntry{
		TS:               start.UTC().Format(time.RFC3339Nano),
		RequestID:        rid,
		KeyLabel:         label,
		Model:            m.ID,
		UpstreamModel:    m.upstreamID(),
		Path:             path,
		Stream:           stream,
		Status:           rec.status,
		PromptTokens:     rec.usage.PromptTokens,
		CompletionTokens: rec.usage.CompletionTokens,
		CachedTokens:     cached,
		LatencyMS:        time.Since(start).Milliseconds(),
		UsageSeen:        rec.seen,
		Aborted:          aborted,
		Backend:          backend,
		SessionID:        sessionID,
		CostUSD:          computeCostUSD(m.Pricing, rec.usage.PromptTokens, cached, rec.usage.CompletionTokens),
		Overage:          overage,
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
	parse := func(s string) float64 {
		v, err := strconv.ParseFloat(s, 64)
		if err != nil {
			return 0
		}
		return v
	}
	promptRate := parse(p.Prompt)
	completionRate := parse(p.Completion)
	cachedRate := promptRate
	if p.CachedPrompt != "" {
		cachedRate = parse(p.CachedPrompt)
	}
	if cachedTokens > promptTokens {
		cachedTokens = promptTokens // defensive: upstream data should never do this, but never bill negative fresh tokens if it does
	}
	freshPromptTokens := promptTokens - cachedTokens
	return float64(freshPromptTokens)*promptRate + float64(cachedTokens)*cachedRate + float64(completionTokens)*completionRate
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

	allowed, reason, overage = c.fetchAndDecide(label)
	c.mu.Lock()
	c.entries[label] = entitlementCacheEntry{allowed: allowed, reason: reason, overage: overage, fetchedAt: time.Now()}
	c.mu.Unlock()
	return allowed, reason, overage
}

// fetchAndDecide queries meterhub for label's subscriber status and
// applies the fail-open/fail-closed policy. Never blocks longer than the
// client's 3s timeout — a slow or unreachable meterhub degrades to
// whatever failOpen says, it never hangs the request.
func (c *entitlementCache) fetchAndDecide(label string) (bool, string, bool) {
	req, err := http.NewRequest(http.MethodGet, c.addr+"/subscribers/get?key="+url.QueryEscape(label), nil)
	if err != nil {
		return c.failOpen, "entitlement check unavailable", false
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if c.failOpen {
			return true, "", false
		}
		return false, "entitlement service unreachable", false
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if c.failOpen {
			return true, "", false
		}
		return false, "entitlement check failed", false
	}
	var s struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&s); err != nil {
		if c.failOpen {
			return true, "", false
		}
		return false, "entitlement check failed", false
	}
	switch s.Status {
	case "active", "past_due":
		allowed, reason := c.checkWindowCap(label)
		// allowed=false with overageBilling on can only happen here if
		// checkWindowCap itself failed open/closed on an unreachable
		// meterhub (reason won't have the "rate limit:" prefix in that
		// case) -- overage is specifically "was over cap but let through
		// anyway", never "was blocked for an unrelated reason".
		overage := c.overageBilling && strings.HasPrefix(reason, "rate limit:")
		if overage {
			return true, reason, true
		}
		return allowed, reason, false
	case "canceled":
		return false, "subscription canceled", false
	case "suspended":
		return false, "account suspended", false
	default: // "unknown" — no subscriber record at all
		if c.failOpen {
			return true, "", false
		}
		return false, "no active subscription for this key", false
	}
}

// checkWindowCap is the enforcement side of meterhub's
// /subscribers/usage instrumentation (tools/meterhub's planLimits): an
// active/past_due subscriber can still be over their plan's rolling 5h or
// 7d token cap (docs/PRICING.md's "real throttle" column), which is a
// distinct condition from their subscription status. Only reached once
// status is already known active/past_due — a canceled/suspended key
// never gets this far. Same fail-open/fail-closed policy as the status
// check: a meterhub hiccup here degrades to c.failOpen rather than
// blocking (or silently admitting) every request while it's unreachable.
func (c *entitlementCache) checkWindowCap(label string) (bool, string) {
	req, err := http.NewRequest(http.MethodGet, c.addr+"/subscribers/usage?key="+url.QueryEscape(label), nil)
	if err != nil {
		return c.failOpen, "usage check unavailable"
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	resp, err := c.client.Do(req)
	if err != nil {
		if c.failOpen {
			return true, ""
		}
		return false, "usage check unreachable"
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		if c.failOpen {
			return true, ""
		}
		return false, "usage check failed"
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
			return true, ""
		}
		return false, "usage check failed"
	}
	if u.Window5h.Over {
		return false, "rate limit: 5-hour token cap exceeded, resets on a rolling window"
	}
	if u.Window7d.Over {
		return false, "rate limit: weekly token cap exceeded, resets on a rolling window"
	}
	return true, ""
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
	log.Printf("oaica-gateway: %d models, %d keys, upstream=%s, listen=%s, ledger=%s",
		len(cfg.Models), len(cfg.APIKeys), cfg.UpstreamAddr, cfg.ListenAddr, cfg.LedgerPath)

	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			g.reload(*configPath)
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/health", g.healthHandler)
	mux.HandleFunc("/privacy", legalHandler("PRIVACY.md"))
	mux.HandleFunc("/terms", legalHandler("TERMS.md"))
	mux.HandleFunc("/status", legalHandler("STATUS.md"))
	// /models is public (2026-08-26): it is served from in-memory config
	// (no upstream call) and contains only what the OpenRouter listing and
	// oaica.com already publish -- ids, context, limits, pricing, modalities.
	// Keeping it behind the key only risked OpenRouter's model poller not
	// sending one and the listing silently never appearing. Completions
	// stay authenticated; nothing about metering changes.
	mux.HandleFunc("/models", g.modelsHandler)
	mux.HandleFunc("/v1/models", g.modelsHandler)
	mux.HandleFunc("/v1/chat/completions", g.completionHandler)
	mux.HandleFunc("/v1/completions", g.completionHandler)
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not_found", "unknown route")
	})

	srv := &http.Server{
		Addr:              cfg.ListenAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
		// No WriteTimeout: streamed completions run for minutes.
	}
	log.Fatal(srv.ListenAndServe())
}
