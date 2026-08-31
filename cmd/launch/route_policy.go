package launch

// route_policy.go — route policies + a health circuit breaker for the launch
// translation proxy's routing table (2026-08-31 design; Task #28).
//
// Until now a proxyRouteTable was static: the model id selected one route and
// that route's upstream either worked or the request failed (after the
// proxy's own SDK-style retries). With two backends in a plan (primary +
// --sonnet-model on DIFFERENT base URLs, e.g. a remote OpenRouter leg and a
// local daemon), there is a second backend that could serve the turn while
// the first is down — but nothing used it.
//
// A route POLICY says what the proxy may do when the selected route's
// upstream is failing, and a circuit BREAKER says when it is failing.
//
//   - local-first (default): on failure, prefer a local backend; else any
//     healthy alternate. Existing single-remote launches are unaffected —
//     with no Fallbacks there is nothing to fall back to.
//   - remote-first: on failure, prefer a remote backend.
//   - auto: currently an alias of local-first (per-request task escalation
//     is the v1.1 follow-up; accepted now so configs don't churn then).
//   - local-only / remote-only: never leave that locality; if its leg is
//     down, fail the request rather than silently cross over (a user who
//     says "local-only" has a reason — data residency, cost cap — and a
//     silent crossover violates it).
//
// Failure detection is feedback-driven plus probed: every forwarded request
// that exhausts its retries (transport error, or a retryable status AFTER
// the retry budget) records a failure, and a 30s background probe of each
// distinct base URL records recovery. 3 consecutive failures open the
// breaker for 90s; any success (probe or real request) closes it. While a
// route is OPEN, new requests go to the fallback immediately instead of
// burning the caller's retry budget against a dead upstream — but never
// mid-stream: once a response has started, the route is frozen for that
// request (the handler only consults the breaker before forwarding).

import (
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

const (
	RouteLocalFirst  routePolicy = "local-first"
	RouteRemoteFirst routePolicy = "remote-first"
	RouteAuto        routePolicy = "auto"
	RouteLocalOnly   routePolicy = "local-only"
	RouteRemoteOnly  routePolicy = "remote-only"
)

type routePolicy string

// parseRoutePolicy normalizes a --route-policy flag / remotes.json field.
// Empty means the default (local-first). Invalid values fail loudly — a
// typo'd policy silently degrading to the default would be worse than a
// failed launch.
func parseRoutePolicy(s string) (routePolicy, error) {
	switch s {
	case "":
		return RouteLocalFirst, nil
	case string(RouteLocalFirst), string(RouteRemoteFirst), string(RouteAuto),
		string(RouteLocalOnly), string(RouteRemoteOnly):
		return routePolicy(s), nil
	}
	return "", os.ErrInvalid
}

func (p routePolicy) String() string {
	if p == "" {
		return string(RouteLocalFirst)
	}
	return string(p)
}

// pinned reports whether the policy forbids leaving that locality entirely.
func (p routePolicy) pinned() string {
	switch p {
	case RouteLocalOnly:
		return "local"
	case RouteRemoteOnly:
		return "remote"
	}
	return ""
}

// preferred returns the locality the policy would rather fall back to, or ""
// when it has no preference.
func (p routePolicy) preferred() string {
	switch p {
	case RouteLocalFirst, RouteAuto:
		return "local"
	case RouteRemoteFirst:
		return "remote"
	}
	return ""
}

// routeLocality classifies a route's base URL as local (loopback — a local
// daemon, a local `oaica serve`) or remote. The label source prefix is not
// enough (a "remote:" source can still be a LAN box), so this is decided on
// the actual endpoint, same rule the gateway's large-context admission uses.
func routeLocality(baseURL string) string {
	if h := hostOf(baseURL); h == "127.0.0.1" || h == "localhost" || h == "::1" || h == "[::1]" {
		return "local"
	}
	return "remote"
}

func hostOf(baseURL string) string {
	s := baseURL
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	}
	if i := strings.IndexAny(s, "/?#"); i >= 0 {
		s = s[:i]
	}
	if i := strings.LastIndex(s, "@"); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.Index(s, ":"); i >= 0 {
		s = s[:i]
	}
	return s
}

// routeBreaker is the per-base-URL circuit breaker: 3 consecutive failures
// (real requests or health probes) open it for breakerOpenFor; any success
// closes it. Consecutive failures, not a sliding window, so one bad request
// amid healthy traffic never trips it — the failure mode that matters is a
// leg that is actually DOWN, and that state produces nothing but failures.
const breakerFailsToOpen = 3

var breakerOpenFor = 90 * time.Second

type routeBreaker struct {
	fails     atomic.Int32
	openUntil atomic.Int64
}

func (b *routeBreaker) recordFail() {
	if b.fails.Add(1) >= breakerFailsToOpen {
		b.openUntil.Store(time.Now().Add(breakerOpenFor).UnixNano())
	}
}

func (b *routeBreaker) recordOK() {
	b.fails.Store(0)
	b.openUntil.Store(0)
}

func (b *routeBreaker) open() bool {
	return time.Now().UnixNano() < b.openUntil.Load()
}

// routeBreakers maps a base URL to its breaker. Nil-safe: tables without
// fallbacks never allocate one, and a nil *routeBreakers simply records
// nothing (and reports everything healthy).
type routeBreakers struct {
	mu sync.Mutex
	m  map[string]*routeBreaker
}

func (rb *routeBreakers) forURL(baseURL string) *routeBreaker {
	if rb == nil {
		return nil
	}
	rb.mu.Lock()
	defer rb.mu.Unlock()
	if rb.m == nil {
		rb.m = map[string]*routeBreaker{}
	}
	if b, ok := rb.m[baseURL]; ok {
		return b
	}
	b := &routeBreaker{}
	rb.m[baseURL] = b
	return b
}

func (rb *routeBreakers) recordFail(baseURL string) {
	if b := rb.forURL(baseURL); b != nil {
		b.recordFail()
	}
}

func (rb *routeBreakers) recordOK(baseURL string) {
	if b := rb.forURL(baseURL); b != nil {
		b.recordOK()
	}
}

func (rb *routeBreakers) open(baseURL string) bool {
	if rb == nil {
		return false
	}
	rb.mu.Lock()
	b := rb.m[baseURL]
	rb.mu.Unlock()
	return b != nil && b.open()
}

// selectRoute resolves the requested model id and applies the policy:
// the ByModel/Default route is used whenever it has no OPEN breaker; only a
// failing route is replaced, by the first policy-ordered healthy fallback on
// a DIFFERENT base URL. Returns the route and whether a fallback leg (not
// the requested route) is being used — the handler reports that upstream via
// the X-Oaica-Route response header so anything in front (our gateway's
// ledger, user dashboards) can attribute the spend to the leg that actually
// served it.
func (t proxyRouteTable) selectRoute(requested string) (proxyRoute, string, bool) {
	base, model := t.resolve(requested)
	if len(t.Fallbacks) == 0 || !t.breakers.open(base.BaseURL) {
		return base, model, false
	}

	// Order the alternate legs per policy. A fallback on the same base URL
	// as the OPEN route is useless (same upstream, different model id) and
	// is skipped. A pinned policy (local-only/remote-only) restricts the
	// candidate set outright; if nothing qualifies, keep the base route and
	// let it fail visibly rather than crossing the locality line.
	type cand struct {
		r    proxyRoute
		rank int
	}
	var cands []cand
	pin, pref := t.Policy.pinned(), t.Policy.preferred()
	seen := map[string]bool{base.BaseURL: true}
	for _, f := range t.Fallbacks {
		if seen[f.BaseURL] {
			continue
		}
		seen[f.BaseURL] = true
		loc := routeLocality(f.BaseURL)
		if pin != "" && loc != pin {
			continue
		}
		rank := 1
		if pref != "" && loc == pref {
			rank = 0
		}
		cands = append(cands, cand{r: f, rank: rank})
	}
	for i := 1; i < len(cands); i++ { // stable sort by rank
		for j := i; j > 0 && cands[j].rank < cands[j-1].rank; j-- {
			cands[j], cands[j-1] = cands[j-1], cands[j]
		}
	}
	for _, c := range cands {
		if !t.breakers.open(c.r.BaseURL) {
			return c.r, c.r.UpstreamModel, true
		}
	}
	return base, model, false
}

// startRouteHealthPoll probes every distinct fallback base URL every
// pollInterval until ln closes, recording the outcome so breakers both OPEN
// proactively and RECOVER without waiting for a real request to prove the
// leg is back. GET /models: every OpenAI-compatible backend we route to
// (vLLM, Ollama, gateways, aggregators) serves it, and unlike a TCP dial it
// exercises the full HTTP path including any LB in front.
func (t proxyRouteTable) startRouteHealthPoll(ln <-chan struct{}, pollInterval time.Duration) {
	seen := map[string]bool{}
	var urls []string
	for _, r := range append([]proxyRoute{t.Default}, t.Fallbacks...) {
		if r.BaseURL != "" && !seen[r.BaseURL] {
			seen[r.BaseURL] = true
			urls = append(urls, r.BaseURL)
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	probe := func() {
		for _, u := range urls {
			resp, err := client.Get(strings.TrimRight(u, "/") + "/models")
			if err != nil {
				t.breakers.recordFail(u)
				continue
			}
			io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 500 {
				t.breakers.recordOK(u)
			} else {
				t.breakers.recordFail(u)
			}
		}
	}
	for {
		select {
		case <-ln:
			return
		case <-tick.C:
			probe()
		}
	}
}

// extractRoutePolicy pulls a launcher-level "--route-policy <p>" (or
// "--route-policy=<p>") out of the passthrough args, mirroring
// extractSonnetModel. Not forwarded to the child binary.
func extractRoutePolicy(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--route-policy" && i+1 < len(args):
			p := args[i+1]
			return p, append(args[:i:i], args[i+2:]...)
		case strings.HasPrefix(a, "--route-policy="):
			return strings.TrimPrefix(a, "--route-policy="), append(args[:i:i], args[i+1:]...)
		}
	}
	return "", args
}

