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
}

func defaultConfig() gwConfig {
	return gwConfig{
		UpstreamAddr: "http://127.0.0.1:30098",
		ListenAddr:   ":8081",
		LedgerPath:   "/workspace/oaica-gateway-ledger.jsonl",
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

// newProxy builds a reverse proxy with its own transport and explicit
// timeouts. ResponseHeaderTimeout returns a clean 504 before Cloudflare's
// 100s edge timeout would turn it into an opaque 524. No overall client
// timeout: streamed completions legitimately run for minutes.
func newProxy(upstream string) (*httputil.ReverseProxy, error) {
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

	// /health probe cache; see healthCacheTTL.
	healthMu   sync.Mutex
	healthAt   time.Time
	healthLast healthResult
}

func (g *gateway) apply(cfg gwConfig) error {
	p, err := newProxy(cfg.UpstreamAddr)
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
	return nil
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
	LatencyMS        int64  `json:"latency_ms"`
	UsageSeen        bool   `json:"usage_seen"` // false = upstream sent no usage; do not trust zeros
	Aborted          bool   `json:"aborted"`    // client disconnected / upstream died mid-response
}

func (g *gateway) writeLedger(e ledgerEntry) {
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	g.ledgerMu.Lock()
	defer g.ledgerMu.Unlock()
	if g.ledger == nil {
		return
	}
	g.ledger.Write(append(b, '\n'))
}

type usage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
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
			g.writeLedger(g.entry(rec, m, label, rid, r.URL.Path, stream, start, aborted))
			panic(p)
		}
	}()
	proxy.ServeHTTP(rec, r)
	rec.finish()
	g.writeLedger(g.entry(rec, m, label, rid, r.URL.Path, stream, start, aborted))
}

// entry builds the ledger row for one completion.
func (g *gateway) entry(rec *usageRecorder, m gwModel, label, rid, path string, stream bool, start time.Time, aborted bool) ledgerEntry {
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
		LatencyMS:        time.Since(start).Milliseconds(),
		UsageSeen:        rec.seen,
		Aborted:          aborted,
	}
}

func (g *gateway) authed(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if g.keyLabel(r) == "" {
			writeErr(w, http.StatusUnauthorized, "invalid_api_key", "missing or invalid API key")
			return
		}
		h(w, r)
	}
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
	mux.HandleFunc("/models", g.authed(g.modelsHandler))
	mux.HandleFunc("/v1/models", g.authed(g.modelsHandler))
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
