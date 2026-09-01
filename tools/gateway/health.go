package main

// health.go — per-upstream health gating for /v1/models (2026-09-01).
//
// /v1/models is served from CONFIG so it stays stable for OpenRouter's
// poller, but a model whose backend is dead was advertised as fully
// available: clients picked it, burned a launch, and got a 5xx. Now each
// entry carries "status" ("healthy" | "unhealthy") from a cheap cached
// probe of its own upstream, so the client picker can badge dead models
// and a dashboard can read health straight off the catalog.
//
// Probes are NOT on the request path: at most one per DISTINCT upstream
// per probeTTL, 1 token, and a failed probe only labels the model — it
// never blocks serving (a momentary probe timeout must not take a live
// model out of the picker; the completion path is the real judge).

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

const (
	probeTTL       = 60 * time.Second
	probeTimeout   = 5 * time.Second
	probeMaxTokens = 1
)

type upstreamProbe struct {
	at     time.Time
	health bool
}

var upstreamProbes struct {
	sync.Mutex
	m map[string]upstreamProbe
}

// probeUpstreamHealth answers whether addr is serving right now: a real
// 1-token chat completion against the first model bound to it, with the
// gateway's upstream credential. Cached probeTTL per address.
func (g *gateway) probeUpstreamHealth(addr, upstreamID string) bool {
	now := time.Now()
	upstreamProbes.Lock()
	if upstreamProbes.m == nil {
		upstreamProbes.m = map[string]upstreamProbe{}
	}
	if p, ok := upstreamProbes.m[addr]; ok && now.Sub(p.at) < probeTTL {
		upstreamProbes.Unlock()
		return p.health
	}
	upstreamProbes.Unlock()

	healthy := g.probeUpstreamHealthUncached(addr, upstreamID)

	upstreamProbes.Lock()
	upstreamProbes.m[addr] = upstreamProbe{at: now, health: healthy}
	upstreamProbes.Unlock()
	return healthy
}

func (g *gateway) probeUpstreamHealthUncached(addr, upstreamID string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()
	body := `{"model":` + fmt.Sprintf("%q", upstreamID) + `,"messages":[{"role":"user","content":"ping"}],"max_tokens":1,"temperature":0}`
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, strings.TrimRight(addr, "/")+"/v1/chat/completions", strings.NewReader(body))
	if err != nil {
		return false
	}
	req.Header.Set("Content-Type", "application/json")
	if k := os.Getenv("OAICA_GATEWAY_UPSTREAM_KEY"); k != "" {
		req.Header.Set("Authorization", "Bearer "+k)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false
	}
	defer resp.Body.Close()
	return resp.StatusCode == http.StatusOK
}

// annotateHealth adds "status" to a /v1/models entry: "unhealthy" when the
// model's upstream fails its cached probe, "healthy" otherwise. The key is
// omitted entirely when the model is healthy so the common case costs
// nothing and the catalog stays byte-stable for pollers.
func (g *gateway) annotateHealth(m gwModel, entry map[string]any) {
	if !g.probeUpstreamHealth(m.upstreamAddr(g.cfg.UpstreamAddr), m.upstreamID()) {
		entry["status"] = "unhealthy"
	}
}