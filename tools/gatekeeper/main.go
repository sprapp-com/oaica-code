// gatekeeper sits in front of katlb (or any single upstream) and adds the
// one thing katlb deliberately doesn't do: per-customer identity and
// concurrency limits. katlb load-balances/fails-over across replicas for
// everyone equally; gatekeeper decides whether a given caller is allowed to
// send another request at all right now, based on which API key they used
// and what tier that key is on.
//
// Auth: "Authorization: Bearer <key>" required. Unknown/missing key -> 401.
// Limit: each key has a max_concurrent from its tier. A request that would
// exceed it gets 429 immediately (no queueing -- queueing hides overload
// instead of signaling it, and a client-side retry-after loop is simpler to
// reason about than a black-box queue depth). Concurrency is tracked, not
// rate-per-second: matches how these tiers are actually meant to be sold
// ("N simultaneous sessions"), and is trivial to reason about/audit.
//
// Config is a flat JSON file, reloaded on SIGHUP so keys can be
// added/revoked without a restart:
//
//	{
//	  "tiers": {"free": 2, "pro": 10, "team": 50},
//	  "keys":  {"sk-abc123": "pro", "sk-def456": "free"}
//	}
package main

import (
	"encoding/json"
	"flag"
	"log"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
)

type gkConfig struct {
	Tiers        map[string]int    `json:"tiers"`
	Keys         map[string]string `json:"keys"`         // key -> tier name
	UpstreamAddr string            `json:"upstream_addr"` // default "http://127.0.0.1:30099"
	ListenAddr   string            `json:"listen_addr"`   // default ":30098"
}

func defaultConfig() gkConfig {
	return gkConfig{
		Tiers:        map[string]int{"free": 2, "pro": 10, "team": 50},
		Keys:         map[string]string{},
		UpstreamAddr: "http://127.0.0.1:30099",
		ListenAddr:   ":30098",
	}
}

type gate struct {
	mu     sync.RWMutex
	cfg    gkConfig
	inuse  map[string]int // key -> current inflight count
	inuseM sync.Mutex
}

func loadConfig(path string) gkConfig {
	cfg := defaultConfig()
	if path == "" {
		return cfg
	}
	b, err := os.ReadFile(path)
	if err != nil {
		log.Printf("gatekeeper: no config at %s (%v), using empty-key defaults (everything 401s until keys are added)", path, err)
		return cfg
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		log.Fatalf("gatekeeper: bad config %s: %v", path, err)
	}
	if cfg.UpstreamAddr == "" {
		cfg.UpstreamAddr = defaultConfig().UpstreamAddr
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultConfig().ListenAddr
	}
	return cfg
}

func (g *gate) reload(path string) {
	cfg := loadConfig(path)
	g.mu.Lock()
	g.cfg = cfg
	g.mu.Unlock()
	log.Printf("gatekeeper: config reloaded, %d keys, tiers=%v", len(cfg.Keys), cfg.Tiers)
}

// acquire returns (allowed, tierName, limit). Never blocks -- an over-limit
// caller gets 429 immediately, not a wait.
func (g *gate) acquire(key string) (bool, string, int) {
	g.mu.RLock()
	tier, ok := g.cfg.Keys[key]
	limit := g.cfg.Tiers[tier]
	g.mu.RUnlock()
	if !ok {
		return false, "", 0
	}
	g.inuseM.Lock()
	defer g.inuseM.Unlock()
	if g.inuse == nil {
		g.inuse = make(map[string]int)
	}
	if g.inuse[key] >= limit {
		return false, tier, limit
	}
	g.inuse[key]++
	return true, tier, limit
}

func (g *gate) release(key string) {
	g.inuseM.Lock()
	defer g.inuseM.Unlock()
	if g.inuse[key] > 0 {
		g.inuse[key]--
	}
}

func main() {
	configPath := flag.String("config", "", "path to gatekeeper JSON config (tiers, keys, upstream_addr, listen_addr)")
	flag.Parse()

	g := &gate{cfg: loadConfig(*configPath)}
	log.Printf("gatekeeper: %d keys, tiers=%v, upstream=%s, listen=%s",
		len(g.cfg.Keys), g.cfg.Tiers, g.cfg.UpstreamAddr, g.cfg.ListenAddr)

	// SIGHUP reload: rotate/add/revoke keys without dropping in-flight
	// requests or bouncing the process.
	sighup := make(chan os.Signal, 1)
	signal.Notify(sighup, syscall.SIGHUP)
	go func() {
		for range sighup {
			g.reload(*configPath)
		}
	}()

	upstreamURL, err := url.Parse(g.cfg.UpstreamAddr)
	if err != nil {
		log.Fatalf("gatekeeper: bad upstream_addr %q: %v", g.cfg.UpstreamAddr, err)
	}
	proxy := httputil.NewSingleHostReverseProxy(upstreamURL)

	mux := http.NewServeMux()
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		auth := r.Header.Get("Authorization")
		key := strings.TrimSpace(strings.TrimPrefix(auth, "Bearer "))
		if key == "" {
			http.Error(w, `{"error":"missing Authorization: Bearer <key>"}`, http.StatusUnauthorized)
			return
		}

		allowed, tier, limit := g.acquire(key)
		if !allowed && tier == "" {
			http.Error(w, `{"error":"invalid API key"}`, http.StatusUnauthorized)
			return
		}
		if !allowed {
			w.Header().Set("Retry-After", "1")
			w.Header().Set("X-Gatekeeper-Tier", tier)
			w.Header().Set("X-Gatekeeper-Limit", itoa(limit))
			http.Error(w, `{"error":"concurrency limit reached for your tier","tier":"`+tier+`","limit":`+itoa(limit)+`}`, http.StatusTooManyRequests)
			return
		}
		defer g.release(key)

		w.Header().Set("X-Gatekeeper-Tier", tier)
		proxy.ServeHTTP(w, r)
	})

	log.Fatal(http.ListenAndServe(g.cfg.ListenAddr, mux))
}

func itoa(n int) string {
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
