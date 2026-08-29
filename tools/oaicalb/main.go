// oaicalb (formerly katlb) is a 1-box simulation of session-hashed vs
// leastconn load balancing across a box's oaica-35b-a3b-vision vLLM
// replicas, to measure whether pinning a
// conversation to one replica (so its prefix cache actually gets reused)
// beats scattering requests round-robin/leastconn (each turn hits a
// cold-cache replica, reprocessing the whole prefix from scratch).
//
// Two listeners, same 6 backends:
//
//	:8090  leastconn (baseline — today's effective behavior)
//	:8091  consistent-hash on X-Session-Id header, leastconn fallback if the
//	       hashed backend is marked unhealthy
//
// Health checks: GET /v1/models every 3s, 2 consecutive failures marks a
// backend down, 2 consecutive successes marks it back up. A backend with
// stalled in-flight requests (older than stall_sec) is marked down on a
// SINGLE probe failure -- see lbConfig.StallSec.
package main

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"hash/fnv"
	"io"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// oaicalb is not model-specific -- it's a plain reverse proxy over any set of
// OpenAI-compatible backends. The backend list, health-check path, and the
// three listen ports are all config so any future multi-replica model (e.g.
// malay35b once it's scaled past one instance) gets the same leastconn +
// session-hash load balancing by running a second oaicalb with its own
// -config, not by touching this code.
type lbConfig struct {
	Backends      []string `json:"backends"`
	HealthPath    string   `json:"health_path"`       // default "/v1/models"
	LeastConnAddr string   `json:"leastconn_addr"`    // default ":8090"
	SessionAddr   string   `json:"session_hash_addr"` // default ":8091"
	StatusAddr    string   `json:"status_addr"`       // default ":8092"

	// ProbeModel turns the health check into a real 1-token
	// POST /v1/chat/completions for this served model name. GET /v1/models
	// only proves the HTTP server is up: vLLM answers it 200 while every
	// chat request 400s (e.g. tokenizer missing a chat_template -- the exact
	// outage hit on 2026-08-25), so oaicalb kept routing into errors with all
	// backends "UP". A chat probe fails the way a customer request fails.
	// Empty keeps the cheap GET probe.
	ProbeModel string `json:"probe_model"`
	// ProbeTimeoutSec bounds one chat probe (default 10s; a 1-token reply on
	// a healthy A100 replica is well under 1s, but a replica mid-startup can
	// stall while loading weights and must not be marked DOWN for that).
	ProbeTimeoutSec int `json:"probe_timeout_sec"`

	// StallSec is the hung-replica threshold (default 120; negative disables).
	// A replica that is LISTENING but hung -- accepts the connection and never
	// answers, or answers the cheap probe while real generations stall -- is
	// only caught by the probe when the probe itself fails twice in a row. A
	// replica whose probe flaps (ok, timeout, ok, ...) never trips that rule
	// while every real request on it sits forever. So: if a backend has at
	// least StallMinInflight in-flight requests each older than StallSec AND
	// its latest probe failed or timed out, it is marked DOWN on that single
	// failure and takes no new requests until the probe succeeds again (the
	// usual 2-consecutive-successes rule). A probe failure alone still needs
	// 2 in a row; old in-flight requests alone never mark a backend down (a
	// long legitimate generation on a replica whose probe passes is fine).
	StallSec int `json:"stall_sec"`
	// StallMinInflight is the N in "N stalled requests" above (default 1).
	StallMinInflight int `json:"stall_min_inflight"`

	// SessionOverflowFactor lets a session-hash pick escape an unevenly
	// loaded (but healthy) hashed backend, not just a DOWN one. Without
	// this, session affinity is sticky forever: one backend can sit idle
	// while another queues a growing backlog, because hashPick only ever
	// degrades to leastconn on an UNHEALTHY hash target (see hashPick).
	// When set > 0, a session is rerouted to the least-loaded healthy
	// backend if its hashed backend's inflight count exceeds
	// SessionOverflowFactor times the average inflight across all healthy
	// backends (0 average treated as 1 to avoid dividing by zero / always
	// tripping at zero load). This is a per-REQUEST decision, not
	// per-session: an overloaded backend draining back under the
	// threshold is naturally rejoined by the same session's next request
	// (no separate un-reroute logic needed). Default 0 disables the
	// check entirely -- session affinity stays sticky no matter the load
	// skew, matching pre-2026-08-29 behavior.
	SessionOverflowFactor float64 `json:"session_overflow_factor"`

	// MeterHubAddr/MeterHubToken/Region: report usage for every completion
	// this LB proxies, straight to meterhub, the same shape oaica-gateway
	// reports (see tools/meterhub, tools/gateway's writeLedger/reportUsage).
	// This closes a real gap found 2026-08-29: a client's local translation
	// proxy resolves its target address ONCE at `oaica launch claude` time,
	// so ANY session started before a remotes.json repoint (or anyone
	// pointed straight at oaicalb instead of the gateway, by config or by
	// mistake) bypasses the gateway's metering entirely -- 761 real
	// completions were served with only 40 ledger rows to show for it. The
	// gateway is still the RIGHT place for auth, entitlement, and per-key
	// billing; oaicalb is the one point every request passes through
	// regardless of entry path, so it is the right backstop for "did this
	// get counted at all" even when it can't attribute the request to a
	// specific customer key (oaicalb has no auth of its own). Requests that
	// already carry X-Oaica-Metered (set by the gateway before it forwards
	// through gatekeeper to here) are NOT reported again here -- see
	// requestAlreadyMetered -- so gateway-routed traffic is never
	// double-counted between the two reporters. Empty MeterHubAddr disables
	// this entirely (default; matches every existing deployment until this
	// is explicitly configured).
	MeterHubAddr  string `json:"meterhub_addr"`
	MeterHubToken string `json:"meterhub_token"`
	Region        string `json:"region"`
}

// stallThreshold returns the effective hung-replica threshold, 0 if disabled.
func (c lbConfig) stallThreshold() time.Duration {
	if c.StallSec < 0 {
		return 0
	}
	return time.Duration(c.StallSec) * time.Second
}

func loadConfig(path string) (lbConfig, error) {
	cfg := lbConfig{
		Backends: []string{
			"http://127.0.0.1:30099",
			"http://127.0.0.1:30101",
			"http://127.0.0.1:30102",
			"http://127.0.0.1:30103",
			"http://127.0.0.1:30104",
			"http://127.0.0.1:30105",
		},
		HealthPath:    "/v1/models",
		LeastConnAddr: ":8090",
		SessionAddr:   ":8091",
		StatusAddr:    ":8092",
	}
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("oaicalb: no config at %s (%v), using built-in defaults", path, err)
		return cfg, nil
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("oaicalb: bad config %s: %w", path, err)
	}
	if cfg.ProbeTimeoutSec <= 0 {
		cfg.ProbeTimeoutSec = 10
	}
	if cfg.StallSec == 0 {
		cfg.StallSec = 120
	}
	if cfg.StallMinInflight <= 0 {
		cfg.StallMinInflight = 1
	}
	return cfg, nil
}

type backend struct {
	url       *url.URL
	proxy     *httputil.ReverseProxy
	inflight  int64
	healthy   atomic.Bool
	failCount int
	okCount   int
	// lastProbeOK is the result of the most recent probe, for /status.
	lastProbeOK atomic.Bool
	// starts holds the start time of every in-flight request proxied to this
	// backend, keyed by a per-backend sequence number, so the health loop can
	// see how long the oldest one has been waiting (see lbConfig.StallSec).
	// inflight stays a separate atomic so leastConnPick never takes mu.
	starts map[uint64]time.Time
	nextID uint64
	mu     sync.Mutex
}

func newBackend(raw string) *backend {
	u, err := url.Parse(raw)
	if err != nil {
		log.Fatal(err)
	}
	b := &backend{url: u, proxy: httputil.NewSingleHostReverseProxy(u), starts: map[uint64]time.Time{}}
	b.healthy.Store(true)
	b.lastProbeOK.Store(true)
	return b
}

// begin records a request being proxied to b; end(id) must follow.
func (b *backend) begin() uint64 {
	b.mu.Lock()
	id := b.nextID
	b.nextID++
	b.starts[id] = time.Now()
	b.mu.Unlock()
	atomic.AddInt64(&b.inflight, 1)
	return id
}

func (b *backend) end(id uint64) {
	atomic.AddInt64(&b.inflight, -1)
	b.mu.Lock()
	delete(b.starts, id)
	b.mu.Unlock()
}

// stalledLocked returns how many in-flight requests started more than stall
// ago (0 when stall detection is disabled), and the age of the oldest
// in-flight request. Caller holds b.mu.
func (b *backend) stalledLocked(now time.Time, stall time.Duration) (stalled int, oldest time.Duration) {
	for _, t := range b.starts {
		age := now.Sub(t)
		if age > oldest {
			oldest = age
		}
		if stall > 0 && age >= stall {
			stalled++
		}
	}
	return stalled, oldest
}

// oldestInflight is the age of the oldest request still proxied to b.
func (b *backend) oldestInflight() time.Duration {
	b.mu.Lock()
	defer b.mu.Unlock()
	_, oldest := b.stalledLocked(time.Now(), 0)
	return oldest
}

// probeBodyOK reports whether a 200 chat-probe body is a real completion:
// a JSON object with a non-empty "choices" array. A replica that is wedged
// after writing its headers (or an intermediary answering 200 with nothing)
// returns 200 with an empty or truncated body, which must count as a
// failure, exactly as the customer request would have failed. Content is
// deliberately not required to be non-empty: a 1-token reply can legally be
// "" (reasoning/tool-call opener).
func probeBodyOK(body []byte) bool {
	var v struct {
		Choices []json.RawMessage `json:"choices"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return false
	}
	return len(v.Choices) > 0
}

// probeBodyLimit bounds how much of a probe reply is read. A 1-token vLLM
// completion is well under 1 KB; anything past this is not a probe reply.
const probeBodyLimit = 1 << 20

// probeOnce runs ONE health probe against b. With probeModel set it is a
// real 1-token chat completion (see lbConfig.ProbeModel); otherwise a GET on
// healthPath. A backend is healthy only when the probe returns 200 -- a 400
// from a chat probe is the signal GET /v1/models can never give -- and, for
// the chat probe, only when the 200 carries an actual completion body.
func (b *backend) probeOnce(client *http.Client, healthPath, probeModel string) bool {
	if probeModel == "" {
		resp, err := client.Get(b.url.String() + healthPath)
		if resp != nil {
			resp.Body.Close()
		}
		return err == nil && resp != nil && resp.StatusCode == 200
	}
	body := `{"model":` + strconv.Quote(probeModel) + `,"messages":[{"role":"user","content":"ping"}],"max_tokens":1,"temperature":0}`
	resp, err := client.Post(b.url.String()+"/v1/chat/completions", "application/json", strings.NewReader(body))
	if err != nil || resp == nil {
		return false
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		io.Copy(io.Discard, io.LimitReader(resp.Body, probeBodyLimit))
		return false
	}
	// The body read shares the client's timeout, so a replica that sends
	// headers and then hangs fails here instead of counting as UP.
	reply, err := io.ReadAll(io.LimitReader(resp.Body, probeBodyLimit))
	if err != nil {
		return false
	}
	return probeBodyOK(reply)
}

// probeOpts is everything one backend's health loop needs. interval and
// stall are durations (not the config's integer seconds) so tests can run
// the loop at millisecond cadence.
type probeOpts struct {
	healthPath string
	probeModel string
	timeout    time.Duration // one probe
	interval   time.Duration // between probes; 0 = 3s
	stall      time.Duration // hung-replica threshold; 0 = disabled
	stallMin   int           // stalled requests needed; <1 = 1
}

func (o probeOpts) withDefaults() probeOpts {
	if o.interval <= 0 {
		o.interval = 3 * time.Second
	}
	if o.stallMin < 1 {
		o.stallMin = 1
	}
	return o
}

// healthCheck probes b every opts.interval until ctx is done. Down after 2
// consecutive failures, or after ONE failure while at least opts.stallMin
// in-flight requests are older than opts.stall; up again after 2
// consecutive successes.
func (b *backend) healthCheck(ctx context.Context, opts probeOpts) {
	opts = opts.withDefaults()
	client := &http.Client{Timeout: opts.timeout}
	for {
		ok := b.probeOnce(client, opts.healthPath, opts.probeModel)
		b.lastProbeOK.Store(ok)
		b.mu.Lock()
		if ok {
			b.okCount++
			b.failCount = 0
			if b.okCount >= 2 && !b.healthy.Load() {
				b.healthy.Store(true)
				log.Printf("oaicalb: %s UP (probe ok x%d)", b.url, b.okCount)
			}
		} else {
			b.failCount++
			b.okCount = 0
			stalled, oldest := b.stalledLocked(time.Now(), opts.stall)
			switch {
			case b.failCount >= 2:
				if b.healthy.Load() {
					b.healthy.Store(false)
					log.Printf("oaicalb: %s DOWN (probe failed x%d)", b.url, b.failCount)
				}
			case stalled >= opts.stallMin:
				if b.healthy.Load() {
					b.healthy.Store(false)
					log.Printf("oaicalb: %s DOWN (probe failed, %d in-flight request(s) stalled >= %s, oldest %s)",
						b.url, stalled, opts.stall, oldest.Round(time.Second))
				}
			}
		}
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-time.After(opts.interval):
		}
	}
}

// rrCounter breaks ties among equal-load backends. Sequential (non-
// overlapping) requests never build up real inflight counts -- every request
// sees the same 0 load on every healthy backend, so a pure "first minimum"
// scan always picks the same one (index 0) and the rest starve. Round-robin
// among the tied set fixes that without giving up real leastconn behavior
// once load actually differs (e.g. one replica running a slow generation).
var rrCounter atomic.Uint64

func leastConnPick(bs []*backend) *backend {
	var bestLoad int64 = -1
	tied := make([]*backend, 0, len(bs))
	for _, b := range bs {
		if !b.healthy.Load() {
			continue
		}
		load := atomic.LoadInt64(&b.inflight)
		switch {
		case bestLoad == -1 || load < bestLoad:
			bestLoad = load
			tied = tied[:0]
			tied = append(tied, b)
		case load == bestLoad:
			tied = append(tied, b)
		}
	}
	if len(tied) == 0 {
		return nil
	}
	idx := int(rrCounter.Add(1)) % len(tied)
	return tied[idx]
}

// averageHealthyLoad is the mean inflight count across healthy backends,
// with a floor of 1 so a 0-average deployment (nothing running yet) never
// makes the overflow check trip at zero load — a single new request on an
// otherwise-idle fleet must not immediately look "overflowing".
func averageHealthyLoad(bs []*backend) float64 {
	var total int64
	var n int
	for _, b := range bs {
		if !b.healthy.Load() {
			continue
		}
		total += atomic.LoadInt64(&b.inflight)
		n++
	}
	if n == 0 {
		return 1
	}
	avg := float64(total) / float64(n)
	if avg < 1 {
		avg = 1
	}
	return avg
}

func hashPick(bs []*backend, key string, overflowFactor float64) *backend {
	h := fnv.New32a()
	h.Write([]byte(key))
	idx := int(h.Sum32()) % len(bs)
	if idx < 0 {
		idx += len(bs)
	}
	target := bs[idx]
	if !target.healthy.Load() {
		// fallback: hashed backend down, degrade to leastconn
		return leastConnPick(bs)
	}
	if overflowFactor > 0 {
		load := float64(atomic.LoadInt64(&target.inflight))
		if load > overflowFactor*averageHealthyLoad(bs) {
			// Hashed backend is healthy but disproportionately loaded —
			// reroute THIS request to whichever healthy backend is least
			// loaded right now. Not sticky: the very next request from the
			// same session re-hashes to the same target and rejoins it
			// once load drains back under the threshold.
			if alt := leastConnPick(bs); alt != nil {
				return alt
			}
		}
	}
	return target
}

// metered is set from lbConfig.MeterHubAddr in main(); nil means metering
// is off (the default) and meterAndServe is a no-op wrapper.
var metered *meterHub

// meterHub is the fire-and-forget usage reporter to a central meterhub
// instance (tools/meterhub). Same shape/pattern as tools/gateway's
// runMeterReporter/reportUsage: a bounded channel, a background goroutine
// with capped retries, never on the request's critical path -- an
// unreachable meterhub cannot slow or fail a real completion here either.
type meterHub struct {
	ch     chan usageRecord
	addr   string
	token  string
	region string
}

type usageRecord struct {
	RequestID        string `json:"request_id"`
	TS               string `json:"ts"`
	Region           string `json:"region"`
	KeyLabel         string `json:"key"`
	Model            string `json:"model"`
	UpstreamModel    string `json:"upstream_model"`
	Path             string `json:"path"`
	Stream           bool   `json:"stream"`
	Status           int    `json:"status"`
	PromptTokens     int    `json:"prompt_tokens"`
	CompletionTokens int    `json:"completion_tokens"`
	// CachedTokens: see tools/gateway's ledgerEntry.CachedTokens doc for
	// what this is and why it's usually 0 today (vLLM doesn't populate
	// prompt_tokens_details.cached_tokens on responses as of 2026-08-29).
	CachedTokens int   `json:"cached_tokens"`
	LatencyMS    int64 `json:"latency_ms"`
	UsageSeen    bool  `json:"usage_seen"`
	Aborted      bool  `json:"aborted"`
	// Backend: which of this instance's own backends served the request
	// (its own X-Katlb-Backend value) -- see tools/gateway's
	// ledgerEntry.Backend doc.
	Backend string `json:"backend,omitempty"`
	// SessionID: the caller's X-Session-Id, if any (session-hash requests
	// on :8091 carry one; leastconn requests on :30099 usually don't).
	SessionID string `json:"session_id,omitempty"`
}

var meterReporterBackoff = func(attempt int) time.Duration { return time.Duration(attempt) * time.Second }

func newMeterHub(addr, token, region string) *meterHub {
	m := &meterHub{ch: make(chan usageRecord, 256), addr: strings.TrimRight(addr, "/"), token: token, region: region}
	go m.run()
	return m
}

func (m *meterHub) run() {
	client := &http.Client{Timeout: 5 * time.Second}
	for rec := range m.ch {
		body, err := json.Marshal(rec)
		if err != nil {
			continue
		}
		var ok bool
		for attempt := 0; attempt < 3 && !ok; attempt++ {
			if attempt > 0 {
				time.Sleep(meterReporterBackoff(attempt))
			}
			req, err := http.NewRequest(http.MethodPost, m.addr+"/ingest", bytes.NewReader(body))
			if err != nil {
				break
			}
			req.Header.Set("Content-Type", "application/json")
			if m.token != "" {
				req.Header.Set("Authorization", "Bearer "+m.token)
			}
			resp, err := client.Do(req)
			if err != nil {
				continue
			}
			resp.Body.Close()
			ok = resp.StatusCode == http.StatusNoContent || resp.StatusCode == http.StatusOK
		}
		if !ok {
			log.Printf("oaicalb: meterhub report dropped after retries (request_id=%s)", rec.RequestID)
		}
	}
}

// report is non-blocking: a full channel (meterhub unreachable/slow for a
// long stretch) drops the record and logs, it never blocks a real request.
func (m *meterHub) report(rec usageRecord) {
	select {
	case m.ch <- rec:
	default:
		log.Printf("oaicalb: meterhub channel full, dropping report (request_id=%s)", rec.RequestID)
	}
}

func newRequestID() string {
	var b [12]byte
	if _, err := io.ReadFull(rand.Reader, b[:]); err != nil {
		return fmt.Sprintf("oaicalb_req_%d", time.Now().UnixNano())
	}
	return "oaicalb_req_" + hex.EncodeToString(b[:])
}

// usageRecorder wraps the ResponseWriter to forward bytes immediately
// (streaming must not be buffered) while scanning them for the usage
// object -- same approach as tools/gateway's usageRecorder. For SSE each
// "data: {...}" line is a chunk and only the last one carries usage; for a
// non-streaming response the whole body is one JSON document.
type usageRecorder struct {
	http.ResponseWriter
	status           int
	stream           bool
	promptTokens     int
	completionTokens int
	cachedTokens     int
	seen             bool
	tail             bytes.Buffer
	body             bytes.Buffer
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
			Usage *struct {
				PromptTokens        int `json:"prompt_tokens"`
				CompletionTokens    int `json:"completion_tokens"`
				PromptTokensDetails *struct {
					CachedTokens int `json:"cached_tokens"`
				} `json:"prompt_tokens_details"`
			} `json:"usage"`
		}
		if json.Unmarshal(payload, &chunk) == nil && chunk.Usage != nil {
			u.promptTokens = chunk.Usage.PromptTokens
			u.completionTokens = chunk.Usage.CompletionTokens
			if chunk.Usage.PromptTokensDetails != nil {
				u.cachedTokens = chunk.Usage.PromptTokensDetails.CachedTokens
			}
			u.seen = true
		}
	}
}

func (u *usageRecorder) finish() {
	if u.stream || u.seen {
		return
	}
	var doc struct {
		Usage *struct {
			PromptTokens        int `json:"prompt_tokens"`
			CompletionTokens    int `json:"completion_tokens"`
			PromptTokensDetails *struct {
				CachedTokens int `json:"cached_tokens"`
			} `json:"prompt_tokens_details"`
		} `json:"usage"`
	}
	if json.Unmarshal(u.body.Bytes(), &doc) == nil && doc.Usage != nil {
		u.promptTokens = doc.Usage.PromptTokens
		u.completionTokens = doc.Usage.CompletionTokens
		if doc.Usage.PromptTokensDetails != nil {
			u.cachedTokens = doc.Usage.PromptTokensDetails.CachedTokens
		}
		u.seen = true
	}
}

// requestAlreadyMetered reports whether the gateway already billed this
// request before forwarding it through gatekeeper to here -- see
// lbConfig.MeterHubAddr's doc for why this header, not the request's
// presence on this LB at all, decides whether oaicalb reports it too.
func requestAlreadyMetered(r *http.Request) bool {
	return r.Header.Get("X-Oaica-Metered") != ""
}

// meterableCompletionPath: the same two paths tools/gateway meters.
func meterableCompletionPath(path string) bool {
	return path == "/v1/chat/completions" || path == "/v1/completions"
}

// keyLabelFor is best-effort: oaicalb has no auth of its own (that is the
// gateway's job), so a request that reaches oaicalb directly carries
// whatever Authorization header its own client happened to send -- often
// none. "direct" makes clear in meterhub that this traffic was NOT
// attributed to a specific customer key, which is still strictly better
// than the request being invisible to metering entirely.
func keyLabelFor(r *http.Request) string {
	if h := r.Header.Get("Authorization"); h != "" {
		return "direct:authenticated"
	}
	return "direct"
}

func meterAndServe(next http.HandlerFunc) http.HandlerFunc {
	if metered == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || !meterableCompletionPath(r.URL.Path) || requestAlreadyMetered(r) {
			next(w, r)
			return
		}
		start := time.Now()
		rawBody, err := io.ReadAll(io.LimitReader(r.Body, 8<<20))
		r.Body.Close()
		if err != nil {
			next(w, r)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(rawBody))
		var reqDoc struct {
			Model  string `json:"model"`
			Stream bool   `json:"stream"`
		}
		json.Unmarshal(rawBody, &reqDoc)

		rec := &usageRecorder{ResponseWriter: w, stream: reqDoc.Stream}
		next(rec, r)
		rec.finish()

		// serveWith sets X-Katlb-Backend on rec (the ResponseWriter it was
		// handed) before proxying -- read it back here rather than
		// threading the picked *backend through meterAndServe's signature.
		// Unlike the gateway (which strips this header from what the
		// public client sees), oaicalb leaves it on the response as-is
		// today for anyone hitting it directly -- an existing, separate,
		// lower-priority concern not addressed by this change.
		backend := rec.Header().Get("X-Katlb-Backend")

		metered.report(usageRecord{
			RequestID: newRequestID(), TS: time.Now().UTC().Format(time.RFC3339Nano),
			Region: metered.region, KeyLabel: keyLabelFor(r), Backend: backend, SessionID: r.Header.Get("X-Session-Id"),
			Model: reqDoc.Model, UpstreamModel: reqDoc.Model, Path: r.URL.Path,
			Stream: reqDoc.Stream, Status: rec.status,
			PromptTokens: rec.promptTokens, CompletionTokens: rec.completionTokens, CachedTokens: rec.cachedTokens,
			LatencyMS: time.Since(start).Milliseconds(), UsageSeen: rec.seen,
		})
	}
}

func serveWith(bs []*backend, pick func([]*backend) *backend) http.HandlerFunc {
	return meterAndServe(func(w http.ResponseWriter, r *http.Request) {
		b := pick(bs)
		if b == nil {
			http.Error(w, "no healthy backend", http.StatusServiceUnavailable)
			return
		}
		id := b.begin()
		defer b.end(id)
		w.Header().Set("X-Katlb-Backend", b.url.String())
		b.proxy.ServeHTTP(w, r)
	})
}

// sessionHandler pins a request to the backend hashed from its X-Session-Id
// (per-client address when absent), degrading to leastconn when that
// backend is DOWN or (if overflowFactor > 0) disproportionately loaded —
// see hashPick.
func sessionHandler(bs []*backend, overflowFactor float64) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Session-Id")
		if key == "" {
			key = r.RemoteAddr // no session header -> degrade to per-client stickiness
		}
		serveWith(bs, func(bs []*backend) *backend { return hashPick(bs, key, overflowFactor) })(w, r)
	}
}

func main() {
	configPath := flag.String("config", "", "path to a JSON config (backends, health_path, listen addrs) -- see lbConfig. Empty uses the kat-awq 6-replica default.")
	flag.Parse()
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.MeterHubAddr != "" {
		metered = newMeterHub(cfg.MeterHubAddr, cfg.MeterHubToken, cfg.Region)
		log.Printf("oaicalb: reporting usage to meterhub at %s (region=%q, skips requests already metered upstream via X-Oaica-Metered)", cfg.MeterHubAddr, cfg.Region)
	}
	if cfg.ProbeModel != "" {
		log.Printf("oaicalb: health probe = 1-token chat on %q (timeout %ds)", cfg.ProbeModel, cfg.ProbeTimeoutSec)
	}
	if stall := cfg.stallThreshold(); stall > 0 {
		log.Printf("oaicalb: hung-replica detection = %d in-flight request(s) stalled >= %s + probe failure", cfg.StallMinInflight, stall)
	} else {
		log.Printf("oaicalb: hung-replica detection disabled (stall_sec < 0)")
	}
	if cfg.SessionOverflowFactor > 0 {
		log.Printf("oaicalb: session-hash overflow reroute enabled (>%.1fx average healthy load reroutes to leastconn)", cfg.SessionOverflowFactor)
	} else {
		log.Printf("oaicalb: session-hash overflow reroute disabled (session_overflow_factor <= 0) — affinity is fully sticky")
	}
	opts := probeOpts{
		healthPath: cfg.HealthPath,
		probeModel: cfg.ProbeModel,
		timeout:    time.Duration(cfg.ProbeTimeoutSec) * time.Second,
		stall:      cfg.stallThreshold(),
		stallMin:   cfg.StallMinInflight,
	}

	bs := make([]*backend, 0, len(cfg.Backends))
	for _, raw := range cfg.Backends {
		b := newBackend(raw)
		go b.healthCheck(context.Background(), opts)
		bs = append(bs, b)
	}
	time.Sleep(1 * time.Second) // let first health check land before serving

	leastconnMux := http.NewServeMux()
	leastconnMux.HandleFunc("/", serveWith(bs, leastConnPick))

	hashMux := http.NewServeMux()
	hashMux.HandleFunc("/", sessionHandler(bs, cfg.SessionOverflowFactor))

	statusMux := http.NewServeMux()
	statusMux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		for _, b := range bs {
			healthy := "UP"
			if !b.healthy.Load() {
				healthy = "DOWN"
			}
			probe := "ok"
			if !b.lastProbeOK.Load() {
				probe = "fail"
			}
			w.Write([]byte(b.url.String() + " " + healthy + " inflight=" + itoa(atomic.LoadInt64(&b.inflight)) +
				" oldest_inflight_sec=" + itoa(int64(b.oldestInflight()/time.Second)) + " probe=" + probe + "\n"))
		}
	})

	go func() { log.Fatal(http.ListenAndServe(cfg.LeastConnAddr, leastconnMux)) }()
	go func() { log.Fatal(http.ListenAndServe(cfg.SessionAddr, hashMux)) }()
	log.Printf("oaicalb: %d backends, leastconn on %s, session-hash on %s, status on %s/status",
		len(bs), cfg.LeastConnAddr, cfg.SessionAddr, cfg.StatusAddr)
	log.Fatal(http.ListenAndServe(cfg.StatusAddr, statusMux))
}

func itoa(n int64) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}
