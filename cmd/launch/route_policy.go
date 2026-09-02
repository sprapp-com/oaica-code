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
//   - auto: like local-first for ordinary failover, PLUS tier escalation:
//     autoEscalateAfterFails consecutive failures of a session's chosen leg
//     escalate that session's new requests to the strongest healthy
//     secondary leg without waiting for the breaker, resetting after
//     autoEscalateHoldFor of no failures. See "auto escalation" below.
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
	"context"
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
	// While OPEN the count is parked (further failures don't accumulate).
	// After expiry a stale count of breakerFailsToOpen lingers, so a fresh
	// failure that pushes past it RESTARTS the budget at 1: after an OPEN
	// cycle the leg needs its full budget of fresh failures to re-open — a
	// lone blip right after expiry must not re-open it on its own.
	// recordOK (probes or live traffic) resets the counter anyway.
	if b.open() {
		return
	}
	if n := b.fails.Add(1); n >= breakerFailsToOpen {
		if n > breakerFailsToOpen {
			b.fails.Store(1)
			n = 1
		}
		if n >= breakerFailsToOpen {
			b.openUntil.Store(time.Now().Add(breakerOpenFor).UnixNano())
		}
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

// auto escalation (route policy `auto`, the v1.1 design): a proxy counts
// per-session failures of the leg a session is currently being served on and
// ESCALATES the tier when the primary accumulates autoEscalateAfterFails
// consecutive failures — the same signals that feed the breaker (5xx after
// the retry budget, or a transport error; 4xx/429 are the leg working and
// never count). While escalated, that session's NEW requests go straight to
// the strongest healthy secondary leg (largest ContextWindow among the
// fallbacks, Oversize included when larger) instead of waiting for the
// breaker to open. Escalation ends autoEscalateHoldFor after the last
// failure — "minutes of healthy service", not one lucky 200: a single
// success on the secondary while the primary is still flapping must not
// bounce the session back onto the flapping leg, but a primary that has
// been quiet-healthy for this long is considered recovered. Like the
// breaker, escalation is only consulted in selectRoute — a response already
// streaming is never re-routed mid-stream.
const autoEscalateAfterFails = 2

// autoEscalateHoldFor is a package var (like breakerOpenFor) so tests can
// shorten the reset window instead of sleeping 10 minutes.
var autoEscalateHoldFor = 10 * time.Minute

// routeEscalation is the state for one SessionID: consecutive failures of
// its currently chosen leg, and the instant escalation expires (stale, i.e.
// zero or past, means "not escalated").
// routeEscalation is the state for one SessionID: consecutive failures of
// its currently chosen leg, and the instant escalation expires (stale, i.e.
// zero or past, means "not escalated"). The signal is PER-LEG: results from
// the leg the session is actually being served on count; a success on some
// other leg must not clear the primary's failure streak, and a failure on
// a fallback must not feed the escalation counter toward it.
type routeEscalation struct {
	mu             sync.Mutex
	leg            string
	fails          int
	escalatedUntil atomic.Int64
}

func (e *routeEscalation) noteLeg(baseURL string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.leg != baseURL {
		e.leg = baseURL
		e.fails = 0
	}
}

func (e *routeEscalation) recordFail(baseURL string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.leg != baseURL {
		return
	}
	e.fails++
	if e.fails >= autoEscalateAfterFails {
		e.escalatedUntil.Store(time.Now().Add(autoEscalateHoldFor).UnixNano())
	}
}

func (e *routeEscalation) recordOK(baseURL string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.leg != baseURL {
		return
	}
	e.fails = 0
}

func (e *routeEscalation) escalated() bool {
	return time.Now().UnixNano() < e.escalatedUntil.Load()
}

// routeEscalations maps a SessionID to its escalation state, keyed by
// proxyRouteTable.SessionID (one launched session per proxy process, so one
// bucket per launch). Nil-safe like routeBreakers: tables without
// fallbacks, or tests that never allocate one, simply never escalate.
type routeEscalations struct {
	mu sync.Mutex
	m  map[string]*routeEscalation
}

func (re *routeEscalations) forSession(sessionID string) *routeEscalation {
	if re == nil {
		return nil
	}
	re.mu.Lock()
	defer re.mu.Unlock()
	if re.m == nil {
		re.m = map[string]*routeEscalation{}
	}
	if e, ok := re.m[sessionID]; ok {
		return e
	}
	e := &routeEscalation{}
	re.m[sessionID] = e
	return e
}

func (re *routeEscalations) noteLeg(sessionID, baseURL string) {
	if e := re.forSession(sessionID); e != nil {
		e.noteLeg(baseURL)
	}
}

func (re *routeEscalations) recordFail(sessionID, baseURL string) {
	if e := re.forSession(sessionID); e != nil {
		e.recordFail(baseURL)
	}
}

func (re *routeEscalations) recordOK(sessionID, baseURL string) {
	if e := re.forSession(sessionID); e != nil {
		e.recordOK(baseURL)
	}
}

func (re *routeEscalations) escalated(sessionID string) bool {
	if re == nil {
		return false
	}
	re.mu.Lock()
	e := re.m[sessionID]
	re.mu.Unlock()
	return e != nil && e.escalated()
}

// escalationTarget picks the escalated-to leg: among the fallbacks plus the
// Oversize leg (all on base URLs different from the failing primary's), the
// largest-ContextWindow one that the breaker reports healthy and that a
// pinned policy permits. An unprobed window (0) still qualifies — the plan
// put the leg there as the stronger tier, and a 0 must not silently disable
// escalation. No qualifying leg (all OPEN, or everything pinned away)
// returns false and the caller keeps the base route, exactly like the
// breaker's own graceful fallback.
func (t proxyRouteTable) escalationTarget(base proxyRoute) (proxyRoute, bool) {
	var best proxyRoute
	seen := map[string]bool{base.BaseURL: true}
	add := func(rs ...proxyRoute) {
		for _, r := range rs {
			if r.BaseURL == "" || seen[r.BaseURL] {
				continue
			}
			seen[r.BaseURL] = true
			if pin := t.Policy.pinned(); pin != "" && routeLocality(r.BaseURL) != pin {
				continue
			}
			if t.breakers.open(r.BaseURL) {
				continue
			}
			if best.BaseURL == "" || r.ContextWindow > best.ContextWindow {
				best = r
			}
		}
	}
	add(t.Fallbacks...)
	add(t.Oversize)
	return best, best.BaseURL != ""
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
	route, model, usedFallback := t.resolveRoute(requested)
	// Track which leg the session is being served on: the escalation signal
	// only counts results from that leg (nil-safe — no state is a no-op).
	t.escalations.noteLeg(t.SessionID, route.BaseURL)
	return route, model, usedFallback
}

func (t proxyRouteTable) resolveRoute(requested string) (proxyRoute, string, bool) {
	base, model := t.resolve(requested)

	// `auto` escalation: when this session's failures have accumulated past
	// autoEscalateAfterFails, skip ahead to the strongest healthy secondary
	// leg immediately (the breaker only speaks up at 3, and only for its own
	// per-leg signal). Degrades to the normal path when no secondary
	// qualifies.
	if t.Policy == RouteAuto && t.escalations.escalated(t.SessionID) {
		if r, ok := t.escalationTarget(base); ok {
			return r, r.UpstreamModel, true
		}
	}

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

// minViableCompletionTokens mirrors the handler's minViableCompletion: below
// this leftover budget no safe positive max_tokens exists, so the request is
// already doomed.
const minViableCompletionTokens = 16

// oversizeSwap returns the Oversize leg when the request cannot fit on
// route: the leg's fit budget for THIS est/margin is already exhausted, the
// oversize leg is strictly larger, on a DIFFERENT base URL, breaker-healthy,
// and (for pinned policies like local-only) on the permitted side of the
// locality pin — crossover is an opt-out, not an opt-in, because a pinned
// user's reason (residency, cost cap) outranks a marginal success. Any
// condition failing returns the original route, whose caller then fails
// visibly with the existing "prompt is too long" 400.
//
// A NATIVE PASSTHROUGH oversize leg (t.Oversize.NativePassthrough) is a
// special case, checked first and separately: it carries no BaseURL/
// ContextWindow by design (proxyRoute.NativePassthrough's doc) — Anthropic
// enforces its own real window (1M+ for a modern model), which is always
// assumed large enough to be worth trying rather than failing the request
// outright. 2026-09-02: added because a native primary configured
// alongside a small-window OAICA sonnet/haiku split had NOTHING to swap to
// when that tier overflowed — --oversize only ever pointed at another
// OAICA/remote leg, so a genuinely large model already running (the native
// primary itself) was never reachable as a compaction leg. Breaker/pin
// checks use a fixed native-anthropic key (routeLocality has no meaning
// for an empty BaseURL) rather than being skipped outright.
func (t proxyRouteTable) oversizeSwap(route proxyRoute, estTokens, margin int) (proxyRoute, bool) {
	// Precondition: the current leg must actually be unable to hold the
	// request (the handler's only caller guarantees this; kept inside so the
	// function is honest standalone).
	if route.ContextWindow-estTokens-margin >= minViableCompletionTokens {
		return route, false
	}
	if t.Oversize.NativePassthrough {
		if route.NativePassthrough {
			return route, false // already on it — nothing to swap to
		}
		if pin := t.Policy.pinned(); pin != "" && pin != "remote" {
			// Native is always a remote call to api.anthropic.com — a
			// local-only pin must never cross to it, same reasoning as the
			// BaseURL-based check below for an ordinary oversize leg
			// (routeLocality returns the same "local"/"remote" strings).
			return route, false
		}
		if t.breakers.open(nativeOversizeBreakerKey) {
			return route, false
		}
		return t.Oversize, true
	}
	if route.ContextWindow <= 0 || t.Oversize.BaseURL == "" ||
		t.Oversize.BaseURL == route.BaseURL ||
		t.Oversize.ContextWindow <= route.ContextWindow {
		return route, false
	}
	if t.Oversize.ContextWindow-estTokens-margin < minViableCompletionTokens {
		return route, false // even the oversized leg can't hold it
	}
	if pin := t.Policy.pinned(); pin != "" && routeLocality(t.Oversize.BaseURL) != pin {
		return route, false
	}
	if t.breakers.open(t.Oversize.BaseURL) {
		return route, false
	}
	return t.Oversize, true
}

// nativeOversizeBreakerKey is the breaker identity for a native-Anthropic
// oversize leg — a stable string standing in for the (deliberately empty)
// BaseURL every other route's breaker state is keyed on.
const nativeOversizeBreakerKey = "native-anthropic-oversize"

// startRouteHealthPoll probes every distinct fallback base URL every
// pollInterval until ctx is cancelled, recording the outcome so breakers
// both OPEN proactively and RECOVER without waiting for a real request to
// prove the leg is back. GET /models: every OpenAI-compatible backend we
// route to (vLLM, Ollama, gateways, aggregators) serves it, and unlike a
// TCP dial it exercises the full HTTP path including any LB in front.
func (t proxyRouteTable) startRouteHealthPoll(ctx context.Context, pollInterval time.Duration) {
	seen := map[string]bool{}
	var urls []string
	legs := append([]proxyRoute{t.Default}, t.Fallbacks...)
	if t.Oversize.BaseURL != "" {
		legs = append(legs, t.Oversize)
	}
	for _, r := range legs {
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
			// ctx-bound request: shutdown cancels an in-flight probe instead
			// of letting shutdown lag behind it (up to the 5s client timeout
			// per URL under the old channel-only signal).
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u, "/")+"/models", nil)
			if err != nil {
				continue
			}
			resp, err := client.Do(req)
			if err != nil {
				if ctx.Err() != nil {
					return // shutdown, not a leg failure
				}
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
		case <-ctx.Done():
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

// extractOversizeModel pulls "--oversize <model>" (or "--oversize=<model>"),
// the larger-context leg that serves requests the current leg cannot hold
// (the auto-compaction call near a ceiling, mostly). Same picker vocabulary
// as --sonnet-model: "<remote>/<id>", "router/<id>", "<id>:local", bare id.
// Not forwarded to the child binary.
func extractOversizeModel(args []string) (string, []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--oversize" && i+1 < len(args):
			m := args[i+1]
			return m, append(args[:i:i], args[i+2:]...)
		case strings.HasPrefix(a, "--oversize="):
			return strings.TrimPrefix(a, "--oversize="), append(args[:i:i], args[i+1:]...)
		}
	}
	return "", args
}

