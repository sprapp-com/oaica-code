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
// Probes do not hold the request: at most one per DISTINCT upstream per
// probeTTL, 1 token, and a failed probe only labels the model — it never
// blocks serving (a momentary probe timeout must not take a live model out of
// the picker; the completion path is the real judge).
//
// "Not on the request path" used to be the claim while the probe ran inline:
// one /v1/models poll paid probeTimeout for EACH unreachable upstream, one
// after the other, so three dead upstreams held the roster for 15s and the
// launch picker's 800ms client gave up and silently dropped every row
// (2026-09-27 audit, round 27, B-A). A request now waits at most healthBudget
// for a probe it started or joined; a probe that has not landed by then is
// left running, the model is served with no status key at all, and the next
// request reads the answer from the cache.

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
	// healthBudget bounds what one /v1/models request spends waiting for cold
	// probe results, in total. It is short on purpose: the roster is a listing
	// and a listing that arrives late is a listing the client discards.
	healthBudget = 250 * time.Millisecond
)

type upstreamProbe struct {
	at     time.Time
	health bool
}

// probeFlight is the single-flight ticket for an in-flight upstream probe.
type probeFlight struct {
	done chan struct{}
}

var upstreamProbes struct {
	sync.Mutex
	m       map[string]upstreamProbe
	flights map[string]*probeFlight
}

// cachedUpstreamHealth is the probe result for addr when one is fresh enough to
// serve from (see probeTTL).
func cachedUpstreamHealth(addr string) (bool, bool) {
	now := time.Now()
	upstreamProbes.Lock()
	defer upstreamProbes.Unlock()
	p, ok := upstreamProbes.m[addr]
	if !ok || now.Sub(p.at) >= probeTTL {
		return false, false
	}
	return p.health, true
}

// startUpstreamProbe returns the in-flight probe for addr, starting one if
// there is none. Single-flight (2026-09-01 audit M3): the check-then-probe gap
// let N concurrent /v1/models requests on TTL expiry all fire a real (billable)
// completion at the upstream simultaneously. The in-flight map pins one probe
// per address; every other caller joins it and shares the result.
func (g *gateway) startUpstreamProbe(addr, upstreamID string) *probeFlight {
	upstreamProbes.Lock()
	if upstreamProbes.m == nil {
		upstreamProbes.m = map[string]upstreamProbe{}
	}
	if upstreamProbes.flights == nil {
		upstreamProbes.flights = map[string]*probeFlight{}
	}
	if existing, ok := upstreamProbes.flights[addr]; ok {
		upstreamProbes.Unlock()
		return existing
	}
	flight := &probeFlight{done: make(chan struct{})}
	upstreamProbes.flights[addr] = flight
	upstreamProbes.Unlock()

	go func() {
		healthy := g.probeUpstreamHealthUncached(addr, upstreamID)
		upstreamProbes.Lock()
		upstreamProbes.m[addr] = upstreamProbe{at: time.Now(), health: healthy}
		delete(upstreamProbes.flights, addr)
		upstreamProbes.Unlock()
		close(flight.done)
	}()
	return flight
}

// probeUpstreamHealthWithin answers whether addr is serving right now: a real
// 1-token chat completion against the first model bound to it, with the
// gateway's upstream credential. Cached probeTTL per address.
//
// It waits at most wait for a probe it had to start or join, and reports
// ok == false when the answer is not in by then — the probe keeps running, so
// the wait only decides whether THIS request can label the model. That is what
// keeps a slow upstream off the roster's critical path.
func (g *gateway) probeUpstreamHealthWithin(addr, upstreamID string, wait time.Duration) (bool, bool) {
	if health, ok := cachedUpstreamHealth(addr); ok {
		return health, true
	}
	flight := g.startUpstreamProbe(addr, upstreamID)
	if wait <= 0 {
		// No budget left for this request: the probe is running and the next
		// request will read its result from the cache.
		return false, false
	}
	timer := time.NewTimer(wait)
	defer timer.Stop()
	select {
	case <-flight.done:
		return cachedUpstreamHealth(addr)
	case <-timer.C:
		return false, false
	}
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
// omitted entirely when the model is healthy — and when no probe answer
// arrived within budget — so the common case costs nothing and the catalog
// stays byte-stable for pollers.
func (g *gateway) annotateHealth(m gwModel, entry map[string]any, deadline time.Time) {
	// Read under the lock: the caller (modelsHandler) released its RLock before
	// this loop, and apply() writes g.cfg under the write lock on every reload —
	// so this read raced a SIGHUP against a live /v1/models poll
	// (2026-09-27 audit, round 25, -race proof in the round's test file).
	g.mu.RLock()
	addr := g.cfg.UpstreamAddr
	g.mu.RUnlock()
	if health, ok := g.probeUpstreamHealthWithin(m.upstreamAddr(addr), m.upstreamID(), time.Until(deadline)); ok && !health {
		entry["status"] = "unhealthy"
	}
}
