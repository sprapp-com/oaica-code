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
	Keys         map[string]string `json:"keys"`          // key -> tier name
	UpstreamAddr string            `json:"upstream_addr"` // default "http://127.0.0.1:30099"
	ListenAddr   string            `json:"listen_addr"`   // default ":30098"
	// TrustedMeteredTiers are the tiers whose requests may carry X-Oaica-Metered upstream: the
	// marker means "the gateway already billed this turn", so oaicalb skips its own report. It
	// used to be forwarded from any key, and a free-tier key holder sending it erased the only
	// record of its own served turns (2026-09-29 audit, round 122, F122-L3-1). Default: internal.
	TrustedMeteredTiers []string `json:"trusted_metered_tiers"`
}

func defaultConfig() gkConfig {
	return gkConfig{
		Tiers:        map[string]int{"free": 2, "pro": 10, "team": 50},
		Keys:         map[string]string{},
		UpstreamAddr: "http://127.0.0.1:30099",
		ListenAddr:   ":30098",

		TrustedMeteredTiers: []string{"internal"},
	}
}

type gate struct {
	mu     sync.RWMutex
	cfg    gkConfig
	inuse  map[string]int // key -> current inflight count
	inuseM sync.Mutex
}

// parseConfig reads a config file. A missing path is the empty-key defaults (startup only); an
// unreadable or unparseable file is an error, never a fatal exit and never an empty config.
func parseConfig(path string) (gkConfig, error) {
	cfg := defaultConfig()
	if path == "" {
		return cfg, nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return cfg, err
	}
	if err := json.Unmarshal(b, &cfg); err != nil {
		return cfg, err
	}
	if cfg.UpstreamAddr == "" {
		cfg.UpstreamAddr = defaultConfig().UpstreamAddr
	}
	if cfg.ListenAddr == "" {
		cfg.ListenAddr = defaultConfig().ListenAddr
	}
	return cfg, nil
}

func loadConfig(path string) gkConfig {
	cfg, err := parseConfig(path)
	if err != nil {
		if os.IsNotExist(err) {
			log.Printf("gatekeeper: no config at %s (%v), using empty-key defaults (everything 401s until keys are added)", path, err)
			return defaultConfig()
		}
		log.Fatalf("gatekeeper: bad config %s: %v", path, err)
	}
	return cfg
}

// reload applies a new config on SIGHUP. A file that is missing (an editor's rename) or does not
// parse (mid-edit) keeps the config in force: it used to exit the process on bad JSON and to
// replace the key set with none on a missing file, taking every in-flight stream and the whole
// default upstream down during the documented key-add procedure (2026-09-29 audit, round 122,
// F122-L3-5).
func (g *gate) reload(path string) {
	cfg, err := parseConfig(path)
	if err != nil {
		log.Printf("gatekeeper: SIGHUP: config rejected, keeping the current one: %v", err)
		return
	}
	g.mu.Lock()
	g.cfg = cfg
	g.mu.Unlock()
	log.Printf("gatekeeper: config reloaded, %d keys, tiers=%v", len(cfg.Keys), cfg.Tiers)
}

// stripClientControlHeaders removes the request headers a client must never set on the hops
// behind this gate; the marker is kept only for a trusted tier.
func (g *gate) stripClientControlHeaders(h http.Header, tier string) {
	g.mu.RLock()
	trusted := false
	for _, t := range g.cfg.TrustedMeteredTiers {
		if t == tier {
			trusted = true
		}
	}
	g.mu.RUnlock()
	for k := range h {
		ck := http.CanonicalHeaderKey(k)
		if strings.HasPrefix(ck, "X-Gatekeeper-") || strings.HasPrefix(ck, "X-Katlb-") || strings.HasPrefix(ck, "X-Oaica-") {
			if ck == "X-Oaica-Metered" && trusted {
				continue
			}
			h.Del(k)
		}
	}
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

// handler is the gate's HTTP handler; main serves it and the tests drive it.
func (g *gate) handler() http.Handler {
	// Resolve the upstream for every request so a SIGHUP config reload changes
	// the live routing target as documented. NewSingleHostReverseProxy captures
	// its target at construction time, which previously made a reloaded
	// upstream_addr appear to succeed while traffic continued to use the old
	// backend.
	proxy := &httputil.ReverseProxy{Director: func(req *http.Request) {
		g.mu.RLock()
		upstreamAddr := g.cfg.UpstreamAddr
		g.mu.RUnlock()
		target, err := url.Parse(upstreamAddr)
		if err != nil {
			// loadConfig validated the initial value and configuration is
			// operator-owned; preserve the existing request on an invalid
			// reload rather than panicking a serving handler.
			return
		}
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.Host = target.Host
	}}

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
		g.stripClientControlHeaders(r.Header, tier)
		proxy.ServeHTTP(w, r)
	})
	return mux
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

	// Fail fast on a bad initial upstream_addr. The parsed value itself is not
	// kept: the Director below re-parses the address on every request so that
	// a SIGHUP reload takes effect, so this is a startup check only.
	if _, err := url.Parse(g.cfg.UpstreamAddr); err != nil {
		log.Fatalf("gatekeeper: bad upstream_addr %q: %v", g.cfg.UpstreamAddr, err)
	}
	mux := g.handler()
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
