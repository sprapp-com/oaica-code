// katlb is a 1-box simulation of session-hashed vs leastconn load balancing
// across the 6 kat-awq vLLM replicas, to measure whether pinning a
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
// backend down, 2 consecutive successes marks it back up.
package main

import (
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

// katlb is not kat-awq-specific -- it's a plain reverse proxy over any set of
// OpenAI-compatible backends. The backend list, health-check path, and the
// three listen ports are all config so any future multi-replica model (e.g.
// malay35b once it's scaled past one instance) gets the same leastconn +
// session-hash load balancing by running a second katlb with its own
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
	// outage hit on 2026-08-25), so katlb kept routing into errors with all
	// backends "UP". A chat probe fails the way a customer request fails.
	// Empty keeps the cheap GET probe.
	ProbeModel string `json:"probe_model"`
	// ProbeTimeoutSec bounds one chat probe (default 10s; a 1-token reply on
	// a healthy A100 replica is well under 1s, but a replica mid-startup can
	// stall while loading weights and must not be marked DOWN for that).
	ProbeTimeoutSec int `json:"probe_timeout_sec"`
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
		log.Printf("katlb: no config at %s (%v), using kat-awq defaults", path, err)
		return cfg, nil
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, fmt.Errorf("katlb: bad config %s: %w", path, err)
	}
	if cfg.ProbeTimeoutSec <= 0 {
		cfg.ProbeTimeoutSec = 10
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
	mu        sync.Mutex
}

func newBackend(raw string) *backend {
	u, err := url.Parse(raw)
	if err != nil {
		log.Fatal(err)
	}
	b := &backend{url: u, proxy: httputil.NewSingleHostReverseProxy(u)}
	b.healthy.Store(true)
	return b
}

// probeOnce runs ONE health probe against b. With probeModel set it is a
// real 1-token chat completion (see lbConfig.ProbeModel); otherwise a GET on
// healthPath. A backend is healthy only when the probe returns 200 -- a 400
// from a chat probe is the signal GET /v1/models can never give.
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
	if resp != nil {
		io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		resp.Body.Close()
	}
	return err == nil && resp != nil && resp.StatusCode == 200
}

func (b *backend) healthCheck(healthPath, probeModel string, timeout time.Duration) {
	client := &http.Client{Timeout: timeout}
	for {
		ok := b.probeOnce(client, healthPath, probeModel)
		b.mu.Lock()
		if ok {
			b.okCount++
			b.failCount = 0
			if b.okCount >= 2 {
				b.healthy.Store(true)
			}
		} else {
			b.failCount++
			b.okCount = 0
			if b.failCount >= 2 {
				b.healthy.Store(false)
			}
		}
		b.mu.Unlock()
		time.Sleep(3 * time.Second)
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

func hashPick(bs []*backend, key string) *backend {
	h := fnv.New32a()
	h.Write([]byte(key))
	idx := int(h.Sum32()) % len(bs)
	if idx < 0 {
		idx += len(bs)
	}
	if bs[idx].healthy.Load() {
		return bs[idx]
	}
	// fallback: hashed backend down, degrade to leastconn
	return leastConnPick(bs)
}

func serveWith(bs []*backend, pick func([]*backend) *backend) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		b := pick(bs)
		if b == nil {
			http.Error(w, "no healthy backend", http.StatusServiceUnavailable)
			return
		}
		atomic.AddInt64(&b.inflight, 1)
		defer atomic.AddInt64(&b.inflight, -1)
		w.Header().Set("X-Katlb-Backend", b.url.String())
		b.proxy.ServeHTTP(w, r)
	}
}

func main() {
	configPath := flag.String("config", "", "path to a JSON config (backends, health_path, listen addrs) -- see lbConfig. Empty uses the kat-awq 6-replica default.")
	flag.Parse()
	cfg, err := loadConfig(*configPath)
	if err != nil {
		log.Fatal(err)
	}
	if cfg.ProbeModel != "" {
		log.Printf("katlb: health probe = 1-token chat on %q (timeout %ds)", cfg.ProbeModel, cfg.ProbeTimeoutSec)
	}

	bs := make([]*backend, 0, len(cfg.Backends))
	for _, raw := range cfg.Backends {
		b := newBackend(raw)
		go b.healthCheck(cfg.HealthPath, cfg.ProbeModel, time.Duration(cfg.ProbeTimeoutSec)*time.Second)
		bs = append(bs, b)
	}
	time.Sleep(1 * time.Second) // let first health check land before serving

	leastconnMux := http.NewServeMux()
	leastconnMux.HandleFunc("/", serveWith(bs, leastConnPick))

	hashMux := http.NewServeMux()
	hashMux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("X-Session-Id")
		if key == "" {
			key = r.RemoteAddr // no session header -> degrade to per-client stickiness
		}
		serveWith(bs, func(bs []*backend) *backend { return hashPick(bs, key) })(w, r)
	})

	statusMux := http.NewServeMux()
	statusMux.HandleFunc("/status", func(w http.ResponseWriter, r *http.Request) {
		for _, b := range bs {
			healthy := "UP"
			if !b.healthy.Load() {
				healthy = "DOWN"
			}
			w.Write([]byte(b.url.String() + " " + healthy + " inflight=" + itoa(atomic.LoadInt64(&b.inflight)) + "\n"))
		}
	})

	go func() { log.Fatal(http.ListenAndServe(cfg.LeastConnAddr, leastconnMux)) }()
	go func() { log.Fatal(http.ListenAndServe(cfg.SessionAddr, hashMux)) }()
	log.Printf("katlb: %d backends, leastconn on %s, session-hash on %s, status on %s/status",
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
