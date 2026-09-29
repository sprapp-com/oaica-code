// meterhub is the central usage/billing service for oaica's inference
// fleet. Every region's oaica-gateway already writes a local, append-only
// JSONL ledger (see tools/gateway/main.go's writeLedger) — that stays the
// durable audit trail per box and never depends on this service being
// reachable. meterhub is the AGGREGATION layer on top: each gateway
// additionally reports the same entry here, async and best-effort, so
// "how many tokens has key X used across every region, right now" is one
// query instead of ssh-ing into N boxes and summing JSONL files by hand.
//
// This is the first piece of a real multi-region answer (see
// docs/MULTI_REGION_ROUTING.md for the rest): a single global source of
// truth for billing that does NOT sit on the request's critical path — a
// region keeps serving even if meterhub is unreachable, it just reports
// late (the reporter retries; nothing here can make a chat completion
// fail).
//
//	POST /ingest             -- one usage record (from a gateway's writeLedger)
//	GET  /usage?key=X        -- all records for one API key label
//	GET  /usage?model=X      -- all records for one model
//	GET  /usage/summary      -- global totals, grouped by key and model
//	GET  /health             -- unauthenticated liveness
//
// Storage: SQLite (modernc.org/sqlite, pure Go — no cgo, so this cross-
// compiles the same way every other tools/ binary does). Good enough for
// a single meterhub instance handling periodic async reports from a
// handful of gateways; if usage ever outgrows one SQLite file, the
// ingest/query API here doesn't change — only the storage backend would,
// same as gateway's own "flat JSON now, could be a DB later" design note.
//
// Auth: same shape as oaica-gateway -- "Authorization: Bearer <token>",
// sha256 digests in config, constant-time compare. A separate token from
// any gateway's own API keys: this token authenticates GATEWAYS reporting
// usage, not end users making inference calls.
package main

import (
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"math"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

type meterConfig struct {
	ListenAddr string `json:"listen_addr"`
	DBPath     string `json:"db_path"`
	// ReportTokens: sha256 hex digests of tokens gateways use to report
	// usage. Same never-plaintext convention as oaica-gateway's api_keys.
	ReportTokens []reportToken `json:"report_tokens"`
}

type reportToken struct {
	SHA256 string `json:"sha256"`
	Label  string `json:"label"` // which gateway/region this token belongs to — recorded per usage row
}

func defaultConfig() meterConfig {
	return meterConfig{
		ListenAddr: ":8095",
		DBPath:     "/workspace/meterhub.db",
	}
}

func loadConfig(path string) (meterConfig, error) {
	cfg := defaultConfig()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("parse %s: %w", path, err)
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = ":8095"
	}
	if cfg.DBPath == "" {
		cfg.DBPath = "/workspace/meterhub.db"
	}
	return cfg, nil
}

// usageRecord is what a gateway POSTs to /ingest — a superset of
// tools/gateway's ledgerEntry (same field names/JSON tags on the shared
// fields, so a gateway can marshal its own ledgerEntry with one added
// "region" field and send it as-is). RequestID is the idempotency key: a
// gateway retrying a failed report after a network blip must not double-
// count usage.
type usageRecord struct {
	TS               string `json:"ts"`
	RequestID        string `json:"request_id"`
	Region           string `json:"region"` // which gateway/box reported this — "a100b", future region names
	KeyLabel         string `json:"key"`
	Model            string `json:"model"`
	UpstreamModel    string `json:"upstream_model"`
	Path             string `json:"path"`
	Stream           bool   `json:"stream"`
	Status           int    `json:"status"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	// CachedTokens: prefix-cache-hit portion of PromptTokens, threaded
	// through from the upstream's OpenAI-shaped
	// prompt_tokens_details.cached_tokens by both reporters (tools/gateway,
	// tools/oaicalb). Usually 0 today, not because caching is off (it's
	// on, verified ~50-60% hit rate via vLLM's own /metrics endpoint,
	// vllm:prefix_cache_hits_total/queries_total) but because this vLLM
	// build doesn't populate the per-request field yet (2026-08-29). Column
	// exists so the value starts flowing the moment an upstream does.
	CachedTokens int   `json:"cached_tokens"`
	LatencyMS    int64 `json:"latency_ms"`
	UsageSeen    bool  `json:"usage_seen"`
	Aborted      bool  `json:"aborted"`
	// Backend: which replica actually served this request (e.g.
	// "http://127.0.0.1:30106" = GPU0 on a100b) — see tools/gateway's
	// ledgerEntry.Backend doc. Group by (region, backend) for per-GPU
	// usage, not just aggregate fleet totals. Empty if the request never
	// reached a backend.
	Backend string `json:"backend,omitempty"`
	// SessionID: the caller's X-Session-Id, letting multiple concurrent
	// sessions under the SAME api key be told apart without issuing
	// separate keys — see tools/gateway's ledgerEntry.SessionID doc.
	SessionID string `json:"session_id,omitempty"`
	// CostUSD/Overage: see tools/gateway's ledgerEntry doc for both --
	// computed cache-aware cost and whether this request was allowed only
	// via overage billing (over a plan's rolling-window cap, let through
	// and flagged rather than blocked). Informational, same as everything
	// else pricing-related here: no invoicing enforcement exists yet.
	CostUSD float64 `json:"cost_usd,omitempty"`
	Overage bool    `json:"overage,omitempty"`
}

type meterHub struct {
	db     *sql.DB
	tokens map[string]string // sha256 hex -> label
}

// sqliteURIPath escapes a file path for use in a `file:` URI: SQLite decodes %XX and stops at ? and #, so a
// configured path containing them opened a different database (2026-09-29 audit, round 129, F129-L3-3).
func sqliteURIPath(p string) string {
	return strings.NewReplacer("%", "%25", "?", "%3f", "#", "%23").Replace(p)
}

func newMeterHub(cfg meterConfig) (*meterHub, error) {
	// WAL and a busy timeout: on the default rollback journal any reader (the summary scans the whole table)
	// makes a concurrent report fail at once with SQLITE_BUSY, and a reporter that gives up after three
	// tries has lost the row for good (oaicalb keeps no ledger) (2026-09-29 audit, round 128, F128-L3-1).
	dsn := (&url.URL{Scheme: "file", Opaque: sqliteURIPath(cfg.DBPath), RawQuery: "_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db %s: %w", cfg.DBPath, err)
	}
	if _, err := db.Exec(`
		CREATE TABLE IF NOT EXISTS usage (
			request_id TEXT PRIMARY KEY,
			ts TEXT NOT NULL,
			region TEXT NOT NULL,
			key_label TEXT NOT NULL,
			model TEXT NOT NULL,
			upstream_model TEXT NOT NULL,
			path TEXT NOT NULL,
			stream INTEGER NOT NULL,
			status INTEGER NOT NULL,
			prompt_tokens INTEGER NOT NULL,
			completion_tokens INTEGER NOT NULL,
			cached_tokens INTEGER NOT NULL DEFAULT 0,
			latency_ms INTEGER NOT NULL,
			usage_seen INTEGER NOT NULL,
			aborted INTEGER NOT NULL,
			received_at TEXT NOT NULL,
			backend TEXT NOT NULL DEFAULT '',
			session_id TEXT NOT NULL DEFAULT '',
			cost_usd REAL NOT NULL DEFAULT 0,
			overage INTEGER NOT NULL DEFAULT 0
		);
		CREATE INDEX IF NOT EXISTS idx_usage_key ON usage(key_label);
		CREATE INDEX IF NOT EXISTS idx_usage_model ON usage(model);
		CREATE INDEX IF NOT EXISTS idx_usage_ts ON usage(ts);
		-- idx_usage_backend / idx_usage_session are created further below,
		-- AFTER the ALTER TABLE migrations that add those columns to a
		-- pre-existing DB -- CREATE TABLE IF NOT EXISTS is a no-op against
		-- an already-existing table, so indexing a column that doesn't
		-- exist YET on this table would fail here (hit in production
		-- 2026-08-29: "no such column: backend").

		CREATE TABLE IF NOT EXISTS subscribers (
			key_label TEXT PRIMARY KEY,
			status TEXT NOT NULL,
			plan TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'manual',
			external_id TEXT NOT NULL DEFAULT '',
			updated_at TEXT NOT NULL,
			note TEXT NOT NULL DEFAULT '',
			reset_at TEXT NOT NULL DEFAULT ''
		);
	`); err != nil {
		return nil, fmt.Errorf("create schema: %w", err)
	}
	// Migration for pre-existing DBs from before reset_at existed: CREATE
	// TABLE IF NOT EXISTS above only applies the column to a fresh table.
	// "duplicate column name" (a fresh DB that already has it via the
	// CREATE TABLE) is expected and ignored; any other error is real.
	if _, err := db.Exec(`ALTER TABLE subscribers ADD COLUMN reset_at TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("migrate reset_at: %w", err)
	}
	// event_created: the Stripe event time of the last pushed change, so a late, older push cannot win
	// (2026-09-29 audit, round 133, F133-L3-4).
	if _, err := db.Exec(`ALTER TABLE subscribers ADD COLUMN event_created INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("migrate event_created: %w", err)
	}
	// Same pattern for cached_tokens on usage (added 2026-08-29).
	if _, err := db.Exec(`ALTER TABLE usage ADD COLUMN cached_tokens INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("migrate cached_tokens: %w", err)
	}
	// Same pattern for backend/session_id (added 2026-08-29). Indexes are
	// created above via CREATE INDEX IF NOT EXISTS, which is safe to
	// re-run against a table that already has the columns.
	if _, err := db.Exec(`ALTER TABLE usage ADD COLUMN backend TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("migrate backend: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE usage ADD COLUMN session_id TEXT NOT NULL DEFAULT ''`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("migrate session_id: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_backend ON usage(backend)`); err != nil {
		return nil, fmt.Errorf("migrate idx_usage_backend: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_usage_session ON usage(session_id)`); err != nil {
		return nil, fmt.Errorf("migrate idx_usage_session: %w", err)
	}
	// Same pattern for cost_usd/overage (added 2026-08-29).
	if _, err := db.Exec(`ALTER TABLE usage ADD COLUMN cost_usd REAL NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("migrate cost_usd: %w", err)
	}
	if _, err := db.Exec(`ALTER TABLE usage ADD COLUMN overage INTEGER NOT NULL DEFAULT 0`); err != nil &&
		!strings.Contains(err.Error(), "duplicate column") {
		return nil, fmt.Errorf("migrate overage: %w", err)
	}

	tokens := make(map[string]string, len(cfg.ReportTokens))
	for _, t := range cfg.ReportTokens {
		tokens[strings.ToLower(t.SHA256)] = t.Label
	}
	return &meterHub{db: db, tokens: tokens}, nil
}

// authed checks "Authorization: Bearer <token>" against the configured
// report tokens, constant-time, same pattern as oaica-gateway.
func (h *meterHub) authed(r *http.Request) (label string, ok bool) {
	auth := r.Header.Get("Authorization")
	const prefix = "Bearer "
	if !strings.HasPrefix(auth, prefix) {
		return "", false
	}
	token := strings.TrimSpace(strings.TrimPrefix(auth, prefix))
	if token == "" {
		return "", false
	}
	sum := sha256.Sum256([]byte(token))
	hexSum := hex.EncodeToString(sum[:])
	for known, lbl := range h.tokens {
		if subtle.ConstantTimeCompare([]byte(known), []byte(hexSum)) == 1 {
			return lbl, true
		}
	}
	return "", false
}

// maxIngestBytes bounds one /ingest body.
const maxIngestBytes = 64 << 10

func (h *meterHub) ingestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := h.authed(r); !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	// A report is a few hundred bytes; a row the reporter wrote from a client's model name or session
	// id was 15 MB (2026-09-29 audit, round 122, F122-L3-2).
	var rec usageRecord
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBytes)).Decode(&rec); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if rec.RequestID == "" {
		http.Error(w, `{"error":"request_id is required"}`, http.StatusBadRequest)
		return
	}
	// A count outside any real turn's range poisons every aggregate that sums it (5e18 overflowed a key's group
	// and dropped or failed the summary) (2026-09-29 audit, round 131, F131-L3-2). The ts is left free-form:
	// existing readers and tests pin that.
	const maxTokens = 1_000_000_000_000
	for _, n := range []int64{int64(rec.PromptTokens), int64(rec.CompletionTokens), int64(rec.CachedTokens)} {
		if n < 0 || n > maxTokens {
			http.Error(w, `{"error":"token count out of range"}`, http.StatusBadRequest)
			return
		}
	}
	// cost_usd and latency_ms feed the same aggregates: 1e308 twice sums to +Inf, the summary fails to encode and
	// answers an empty 200 for every key (2026-09-29 audit, round 132, F132-L3-1).
	if math.IsNaN(rec.CostUSD) || math.IsInf(rec.CostUSD, 0) || rec.CostUSD < 0 || rec.CostUSD > 1e9 || rec.LatencyMS < 0 {
		http.Error(w, `{"error":"cost or latency out of range"}`, http.StatusBadRequest)
		return
	}
	// INSERT OR IGNORE: request_id is the idempotency key. A gateway
	// retrying a report after a timeout must not double-count usage — the
	// second (identical) insert silently no-ops instead of erroring, so
	// the reporter's retry logic can stay dumb (always retry on any
	// non-2xx, never worry about "did the first attempt actually land").
	_, err := h.db.Exec(`
		INSERT OR IGNORE INTO usage
			(request_id, ts, region, key_label, model, upstream_model, path,
			 stream, status, prompt_tokens, completion_tokens, cached_tokens,
			 latency_ms, usage_seen, aborted, received_at, backend, session_id,
			 cost_usd, overage)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		rec.RequestID, rec.TS, rec.Region, rec.KeyLabel, rec.Model, rec.UpstreamModel,
		rec.Path, rec.Stream, rec.Status, rec.PromptTokens, rec.CompletionTokens, rec.CachedTokens,
		rec.LatencyMS, rec.UsageSeen, rec.Aborted, time.Now().UTC().Format(time.RFC3339),
		rec.Backend, rec.SessionID, rec.CostUSD, rec.Overage,
	)
	if err != nil {
		log.Printf("meterhub: insert failed: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

type usageRow struct {
	RequestID        string  `json:"request_id"`
	TS               string  `json:"ts"`
	Region           string  `json:"region"`
	KeyLabel         string  `json:"key"`
	Model            string  `json:"model"`
	PromptTokens     int     `json:"prompt_tokens"`
	CompletionTokens int     `json:"completion_tokens"`
	CachedTokens     int     `json:"cached_tokens"`
	Status           int     `json:"status"`
	LatencyMS        int64   `json:"latency_ms"`
	Backend          string  `json:"backend,omitempty"`
	SessionID        string  `json:"session_id,omitempty"`
	CostUSD          float64 `json:"cost_usd,omitempty"`
	Overage          bool    `json:"overage,omitempty"`
}

func (h *meterHub) usageHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authed(r); !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	q := r.URL.Query()
	where := []string{"1=1"}
	args := []any{}
	if v := q.Get("key"); v != "" {
		where = append(where, "key_label = ?")
		args = append(args, v)
	}
	if v := q.Get("model"); v != "" {
		where = append(where, "model = ?")
		args = append(args, v)
	}
	if v := q.Get("region"); v != "" {
		where = append(where, "region = ?")
		args = append(args, v)
	}
	if v := q.Get("since"); v != "" {
		where = append(where, "ts >= ?")
		args = append(args, v)
	}
	if v := q.Get("backend"); v != "" {
		where = append(where, "backend = ?")
		args = append(args, v)
	}
	if v := q.Get("session_id"); v != "" {
		where = append(where, "session_id = ?")
		args = append(args, v)
	}
	limit := 1000
	if v := q.Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 && n <= 10000 {
			limit = n
		}
	}
	query := fmt.Sprintf(`SELECT request_id, ts, region, key_label, model, prompt_tokens, completion_tokens, cached_tokens, status, latency_ms, backend, session_id, cost_usd, overage
		FROM usage WHERE %s ORDER BY ts DESC LIMIT ?`, strings.Join(where, " AND "))
	args = append(args, limit)

	rows, err := h.db.Query(query, args...)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := make([]usageRow, 0, limit)
	for rows.Next() {
		var u usageRow
		if err := rows.Scan(&u.RequestID, &u.TS, &u.Region, &u.KeyLabel, &u.Model, &u.PromptTokens, &u.CompletionTokens, &u.CachedTokens, &u.Status, &u.LatencyMS, &u.Backend, &u.SessionID, &u.CostUSD, &u.Overage); err != nil {
			log.Printf("meterhub: usage row scan failed: %v", err)
			continue
		}
		out = append(out, u)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"records": out, "count": len(out)})
}

type summaryRow struct {
	KeyLabel         string `json:"key"`
	Model            string `json:"model"`
	Requests         int    `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	CachedTokens     int64  `json:"cached_tokens"`
	Errors           int    `json:"errors"`
	// CostUSD/OverageRequests: see tools/gateway's ledgerEntry doc.
	// Informational -- no invoicing enforcement exists, this is what a
	// billing job would read.
	CostUSD         float64 `json:"cost_usd"`
	OverageRequests int     `json:"overage_requests"`
}

// summaryHandler answers "global usage right now" -- every key x model
// combination with request counts and token totals, the shape a billing
// job or a dashboard actually wants (not a raw row dump).
func (h *meterHub) summaryHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authed(r); !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	where := []string{"1=1"}
	args := []any{}
	if v := r.URL.Query().Get("since"); v != "" {
		where = append(where, "ts >= ?")
		args = append(args, v)
	}
	query := fmt.Sprintf(`
		SELECT key_label, model,
		       COUNT(*) as requests,
		       COALESCE(SUM(prompt_tokens), 0) as prompt_tokens,
		       COALESCE(SUM(completion_tokens), 0) as completion_tokens,
		       COALESCE(SUM(cached_tokens), 0) as cached_tokens,
		       SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END) as errors,
		       COALESCE(SUM(cost_usd), 0) as cost_usd,
		       SUM(CASE WHEN overage != 0 THEN 1 ELSE 0 END) as overage_requests
		FROM usage WHERE %s
		GROUP BY key_label, model
		ORDER BY key_label, model`, strings.Join(where, " AND "))

	rows, err := h.db.Query(query, args...)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []summaryRow{}
	for rows.Next() {
		var s summaryRow
		if err := rows.Scan(&s.KeyLabel, &s.Model, &s.Requests, &s.PromptTokens, &s.CompletionTokens, &s.CachedTokens, &s.Errors, &s.CostUSD, &s.OverageRequests); err != nil {
			log.Printf("meterhub: summary row scan failed: %v", err)
			continue
		}
		out = append(out, s)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"summary": out})
}

type backendSummaryRow struct {
	Region           string `json:"region"`
	Backend          string `json:"backend"`
	Requests         int    `json:"requests"`
	PromptTokens     int64  `json:"prompt_tokens"`
	CompletionTokens int64  `json:"completion_tokens"`
	Errors           int    `json:"errors"`
	AvgLatencyMS     int64  `json:"avg_latency_ms"`
}

// backendSummaryHandler answers "how loaded is each GPU replica" -- the
// per-GPU usage tracking question docs/PRICING.md's capacity math has been
// doing by hand all session (grep vLLM logs, ssh in, count manually).
// Empty backend rows (requests blocked before reaching a replica -- 401,
// 403, 429, or the large-context admission gate) are excluded: they never
// touched a GPU, so they'd be noise in a per-replica load view.
func (h *meterHub) backendSummaryHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authed(r); !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	where := []string{"backend != ''"}
	args := []any{}
	if v := r.URL.Query().Get("since"); v != "" {
		where = append(where, "ts >= ?")
		args = append(args, v)
	}
	if v := r.URL.Query().Get("region"); v != "" {
		where = append(where, "region = ?")
		args = append(args, v)
	}
	query := fmt.Sprintf(`
		SELECT region, backend,
		       COUNT(*) as requests,
		       COALESCE(SUM(prompt_tokens), 0) as prompt_tokens,
		       COALESCE(SUM(completion_tokens), 0) as completion_tokens,
		       SUM(CASE WHEN status >= 400 THEN 1 ELSE 0 END) as errors,
		       CAST(COALESCE(AVG(latency_ms), 0) AS INTEGER) as avg_latency_ms
		FROM usage WHERE %s
		GROUP BY region, backend
		ORDER BY region, backend`, strings.Join(where, " AND "))

	rows, err := h.db.Query(query, args...)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()

	out := []backendSummaryRow{}
	for rows.Next() {
		var s backendSummaryRow
		if err := rows.Scan(&s.Region, &s.Backend, &s.Requests, &s.PromptTokens, &s.CompletionTokens, &s.Errors, &s.AvgLatencyMS); err != nil {
			// A scan error here silently drops rows with no other signal --
			// hit this exact way 2026-08-29 (AVG() returns float64, scanned
			// into an int64 field, failed every row, endpoint looked like
			// "no data" instead of "broken"). Log so that never happens
			// silently again.
			log.Printf("meterhub: backend summary row scan failed: %v", err)
			continue
		}
		out = append(out, s)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"backends": out})
}

// --- Subscriber status: the entitlement source of truth every gateway
// checks before serving a request. ---
//
// This is deliberately shaped like the professional pattern (OpenAI/
// Anthropic/OpenRouter): a real payment processor (Stripe or similar) is
// the actual source of truth for billing, but you never call it
// synchronously on the request path — too slow, and an outage there must
// never take down inference. Instead the processor's webhooks write
// status HERE (via /subscribers/webhook, shaped for Stripe's event
// envelope but generic enough for any processor), and every gateway
// checks THIS fast local cache. Until a real processor is wired up,
// /subscribers/set is the manual equivalent — same table, same read
// path, so nothing about the gateway's check needs to change later.
//
// Status values: "active" (serve normally), "past_due" (still serve —
// matches Stripe's own grace-period semantics: a failed card is not an
// instant cutoff), "canceled" / "suspended" (block). Unknown key_label =
// treated as blocked by default (fail closed) UNLESS the gateway's
// entitlement check itself is disabled — see docs/BILLING_ENTITLEMENT.md.
type subscriberStatus struct {
	KeyLabel   string `json:"key_label"`
	Status     string `json:"status"`
	Plan       string `json:"plan,omitempty"`
	Source     string `json:"source,omitempty"`
	ExternalID string `json:"external_id,omitempty"`
	UpdatedAt  string `json:"updated_at,omitempty"`
	Note       string `json:"note,omitempty"`
	// EventCreated: optional (unix seconds) time of the upstream event this state comes from. A push older than the
	// stored one is ignored; 0/absent = an operator's manual write, always applied.
	EventCreated int64 `json:"event_created,omitempty"`
}

var validSubscriberStatuses = map[string]bool{
	"active": true, "past_due": true, "canceled": true, "suspended": true,
}

// subscriberSetHandler is the manual control surface: mark one key's
// status directly. This is what "easily block an unsubscribed user"
// means operationally today — POST here with status=canceled. Once a
// real payment processor is wired up, /subscribers/webhook does the same
// write automatically on subscription-lifecycle events.
func (h *meterHub) subscriberSetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := h.authed(r); !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var s subscriberStatus
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxIngestBytes)).Decode(&s); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if s.KeyLabel == "" {
		http.Error(w, `{"error":"key_label is required"}`, http.StatusBadRequest)
		return
	}
	if !validSubscriberStatuses[s.Status] {
		http.Error(w, fmt.Sprintf(`{"error":"status must be one of active, past_due, canceled, suspended, got %q"}`, s.Status), http.StatusBadRequest)
		return
	}
	if s.Source == "" {
		s.Source = "manual"
	}
	// An automated source (Stripe, via oaica-saas) must not undo an operator's manual suspension, and an older
	// event must not replace a newer one. Manual writes (source manual / no event time) always apply.
	_, err := h.db.Exec(`
		INSERT INTO subscribers (key_label, status, plan, source, external_id, updated_at, note, event_created)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(key_label) DO UPDATE SET
			status=excluded.status, plan=excluded.plan, source=excluded.source,
			external_id=excluded.external_id, updated_at=excluded.updated_at, note=excluded.note,
			event_created=excluded.event_created
		WHERE (excluded.source = 'manual' OR excluded.event_created = 0
		       OR (excluded.event_created >= subscribers.event_created
		           AND NOT (subscribers.source = 'manual' AND subscribers.status = 'suspended')))`,
		s.KeyLabel, s.Status, s.Plan, s.Source, s.ExternalID, time.Now().UTC().Format(time.RFC3339), s.Note, s.EventCreated,
	)
	if err != nil {
		log.Printf("meterhub: subscriber set failed: %v", err)
		http.Error(w, `{"error":"internal"}`, http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// subscriberGetHandler is the fast per-request check every gateway makes
// (see tools/gateway's entitlementCache). No auth beyond the same report
// token, matching /ingest — gateways are the only expected caller.
func (h *meterHub) subscriberGetHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authed(r); !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, `{"error":"key is required"}`, http.StatusBadRequest)
		return
	}
	var s subscriberStatus
	err := h.db.QueryRow(`SELECT key_label, status, plan, source, external_id, updated_at, note FROM subscribers WHERE key_label = ?`, key).
		Scan(&s.KeyLabel, &s.Status, &s.Plan, &s.Source, &s.ExternalID, &s.UpdatedAt, &s.Note)
	if err == sql.ErrNoRows {
		// No record at all: reported as status "unknown" rather than 404,
		// so the gateway's fail-closed/fail-open policy decision lives in
		// ONE place (the gateway config), not split across HTTP status
		// code handling here too.
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(subscriberStatus{KeyLabel: key, Status: "unknown"})
		return
	}
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(s)
}

// subscriberListHandler lists every known subscriber — the "update all"
// / audit view: see at a glance who is active/past_due/canceled/suspended
// without ssh-ing anywhere.
func (h *meterHub) subscriberListHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authed(r); !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	statusFilter := r.URL.Query().Get("status")
	query := `SELECT key_label, status, plan, source, external_id, updated_at, note FROM subscribers`
	args := []any{}
	if statusFilter != "" {
		query += ` WHERE status = ?`
		args = append(args, statusFilter)
	}
	query += ` ORDER BY key_label`
	rows, err := h.db.Query(query, args...)
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	defer rows.Close()
	out := []subscriberStatus{}
	for rows.Next() {
		var s subscriberStatus
		if err := rows.Scan(&s.KeyLabel, &s.Status, &s.Plan, &s.Source, &s.ExternalID, &s.UpdatedAt, &s.Note); err != nil {
			continue
		}
		out = append(out, s)
	}
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{"subscribers": out})
}

// planLimit is one tier's rolling-window caps, in REQUESTS, matching the
// tiers in docs/PRICING.md ("Our matching tiers" table — the column that
// doc calls the 5h window and the weekly cap). These are the load-bearing
// limits: the monthly token headline in that doc is marketing, this is
// what actually protects the fleet from one subscriber consuming more
// than a shared 2-GPU box can serve.
//
// Requests, not tokens, deliberately (2026-09-28): request count is what
// the comparable subscription products sell and what a coding agent
// actually consumes our GPU with — a prefill-heavy agent turn is ~120K
// tokens and the per-request marginal cost is what the pricing math in
// docs/PRICING.md is built on, while token sums let one subscriber's cheap
// long-context reads burn a cap an expensive-but-few-request user never
// touches. usage.request_id is the primary key, so one API call is exactly
// one row: COUNT(*) over the window IS the request count, with no
// double-count from a gateway retrying a report (INSERT OR IGNORE).
type planLimit struct {
	Requests5h int64
	Requests7d int64
}

// planLimits is deliberately a Go map, not a DB table: these are business
// decisions that change with a pricing doc edit + deploy, not per-tenant
// data. Empty/unknown plan = no cap (subscriberUsageHandler reports usage
// with caps omitted rather than guessing a limit that was never set).
//
// Slugs are the tier names lowercased; "pro+" is the one that is not a
// bare identifier, so it only ever reaches here as a stored string (the
// subscribers.plan column is free-form and written by the operator or the
// billing webhook, never derived from this map).
//
// Renamed 2026-09-28 from starter/pro/team — those were the token-cap slugs
// of a superseded rate card (docs/PRICING.md's single-price-per-size
// proposal) and no subscriber has ever been provisioned against them: the
// Stripe receiver is still pre-launch (docs/PRODUCTION_READINESS.md).
var planLimits = map[string]planLimit{
	"free": {Requests5h: 40, Requests7d: 500},
	"lite": {Requests5h: 400, Requests7d: 7_500},
	"pro":  {Requests5h: 900, Requests7d: 14_500},
	"pro+": {Requests5h: 1_300, Requests7d: 21_000},
	"max":  {Requests5h: 1_700, Requests7d: 27_500},
}

// usageWindow is one rolling window's reading. Tokens is reported without
// a cap: it is the billing/audit quantity (usage rows are what /usage and
// /usage/summary bill from) and the number a human needs to see to know
// whether a cap that counts requests is being gamed by outsized ones. Cap
// and Over are the request cap and whether it is exceeded.
type usageWindow struct {
	Tokens   int64 `json:"tokens"`
	Requests int64 `json:"requests"`
	Cap      int64 `json:"cap,omitempty"`
	Over     bool  `json:"over"`
}

// subscriberUsageHandler is the per-subscriber rolling-window instrument:
// how many requests (and, for the audit trail, how many tokens) this key
// has actually used in the trailing 5 hours and trailing 7 days, against
// its plan's request cap — the read side of the throttling
// docs/PRICING.md's tiers depend on. Nothing in the request
// path enforces this yet (see gateway's entitlementCache for the
// active/canceled/suspended enforcement point this could extend); this
// endpoint is the visibility a human or a future enforcement check needs
// before that gets wired up. Cumulative totals were already available via
// /usage and /usage/summary — this is specifically the sliding-window
// view those don't provide.
func (h *meterHub) subscriberUsageHandler(w http.ResponseWriter, r *http.Request) {
	if _, ok := h.authed(r); !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	key := r.URL.Query().Get("key")
	if key == "" {
		http.Error(w, `{"error":"key is required"}`, http.StatusBadRequest)
		return
	}
	var plan, resetAt string
	err := h.db.QueryRow(`SELECT plan, reset_at FROM subscribers WHERE key_label = ?`, key).Scan(&plan, &resetAt)
	if err != nil && err != sql.ErrNoRows {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}

	now := time.Now().UTC()
	// sum's actual cutoff is whichever is LATER: the window boundary, or an
	// explicit reset (see subscriberResetHandler) — a reset only ever
	// shrinks what counts, it can't extend a window back further than the
	// window itself already reaches.
	sum := func(since time.Time) (int64, int64, error) {
		cutoff := since
		if resetAt != "" {
			if t, err := time.Parse(time.RFC3339Nano, resetAt); err == nil && t.After(cutoff) {
				cutoff = t
			}
		}
		var tokens, requests sql.NullInt64
		err := h.db.QueryRow(
			`SELECT SUM(prompt_tokens + completion_tokens), COUNT(*) FROM usage WHERE key_label = ? AND ts >= ?`,
			key, cutoff.Format(time.RFC3339Nano),
		).Scan(&tokens, &requests)
		return tokens.Int64, requests.Int64, err
	}

	tok5h, req5h, err := sum(now.Add(-5 * time.Hour))
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}
	tok7d, req7d, err := sum(now.Add(-7 * 24 * time.Hour))
	if err != nil {
		http.Error(w, `{"error":"query failed"}`, http.StatusInternalServerError)
		return
	}

	w5h := usageWindow{Tokens: tok5h, Requests: req5h}
	w7d := usageWindow{Tokens: tok7d, Requests: req7d}
	if limit, ok := planLimits[plan]; ok {
		w5h.Cap, w5h.Over = limit.Requests5h, req5h > limit.Requests5h
		w7d.Cap, w7d.Over = limit.Requests7d, req7d > limit.Requests7d
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]any{
		"key_label": key, "plan": plan,
		"window_5h": w5h, "window_7d": w7d,
	})
}

// subscriberResetHandler zeroes a subscriber's rolling-window usage
// without touching the underlying usage rows or restarting anything: it
// sets reset_at to now, and subscriberUsageHandler's window sums treat
// that as their effective start point going forward (never further back
// than the window itself already reaches). The raw usage rows are left
// alone deliberately — they're the billing/audit trail (see /usage,
// /usage/summary), and this only affects the sliding-window view a plan
// cap is checked against. A subscriber's cumulative totals are
// unaffected by a reset.
func (h *meterHub) subscriberResetHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if _, ok := h.authed(r); !ok {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	var req struct {
		KeyLabel string `json:"key_label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, `{"error":"invalid json"}`, http.StatusBadRequest)
		return
	}
	if req.KeyLabel == "" {
		http.Error(w, `{"error":"key_label is required"}`, http.StatusBadRequest)
		return
	}
	res, err := h.db.Exec(
		`UPDATE subscribers SET reset_at = ? WHERE key_label = ?`,
		time.Now().UTC().Format(time.RFC3339Nano), req.KeyLabel,
	)
	if err != nil {
		http.Error(w, `{"error":"reset failed"}`, http.StatusInternalServerError)
		return
	}
	if n, _ := res.RowsAffected(); n == 0 {
		http.Error(w, `{"error":"no subscriber with that key_label"}`, http.StatusNotFound)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// The Stripe-shaped webhook that used to live here (POST /subscribers/webhook) is gone: it trusted the report
// token instead of Stripe's signature, so it could not face Stripe. Stripe now talks only to oaica-saas
// (POST /billing/webhook: signature-verified, idempotent, order-aware), which pushes each subscription's state to
// POST /subscribers/set and answers GET /entitlement/:key_label (2026-09-29, round 132).

func main() {
	configPath := flag.String("config", "", "path to a JSON config (listen_addr, db_path, report_tokens)")
	flag.Parse()

	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	hub, err := newMeterHub(cfg)
	if err != nil {
		log.Fatal(err)
	}
	log.Printf("meterhub: db=%s listen=%s report_tokens=%d", cfg.DBPath, cfg.ListenAddr, len(cfg.ReportTokens))

	mux := newMux(hub)
	log.Fatal(http.ListenAndServe(cfg.ListenAddr, mux))
}

// newMux registers every route. Split out of main so a test can assert which routes exist.
func newMux(hub *meterHub) *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		w.Write([]byte(`{"status":"ok"}`))
	})
	mux.HandleFunc("/ingest", hub.ingestHandler)
	mux.HandleFunc("/usage", hub.usageHandler)
	mux.HandleFunc("/usage/summary", hub.summaryHandler)
	mux.HandleFunc("/usage/by_backend", hub.backendSummaryHandler)
	mux.HandleFunc("/subscribers/set", hub.subscriberSetHandler)
	mux.HandleFunc("/subscribers/get", hub.subscriberGetHandler)
	mux.HandleFunc("/subscribers/list", hub.subscriberListHandler)
	mux.HandleFunc("/subscribers/usage", hub.subscriberUsageHandler)
	mux.HandleFunc("/subscribers/reset", hub.subscriberResetHandler)
	return mux
}
