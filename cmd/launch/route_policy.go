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
	"hash/fnv"
	"io"
	"net"
	"net/http"
	"os"
	"sort"
	"strconv"
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
	// RouteWeighted splits HEALTHY traffic across every route (base +
	// Fallbacks) that carries a nonzero Weight, instead of leaving
	// Fallbacks idle until the base route's breaker opens. Session-sticky
	// (a consistent hash of SessionID picks the leg), so one conversation's
	// prefix-cache reuse on its replica isn't broken by weighting — only
	// the split across DIFFERENT sessions follows the weights. A route
	// with Weight 0 (every route, unless the user opts it in) is excluded
	// from the ring; with no weighted routes this degrades to ordinary
	// failover, same as every other policy.
	RouteWeighted routePolicy = "weighted"
)

type routePolicy string

// parseRoutePolicy normalizes a --route-policy flag / remotes.json field.
// Empty means the default (local-first). Invalid values fail loudly — a
// typo'd policy silently degrading to the default would be worse than a
// failed launch.
// RoutePolicyNames is every policy parseRoutePolicy accepts, in the order
// user-facing text lists them — the single source for both flag help lines and
// the refusal below. It exists because the three had already drifted:
// `oaica plan set --help` denied "weighted" while the command accepted it and
// two documents shipped it, so the one line a user reads when setting a plan
// was the one line that was wrong (2026-09-26).
func RoutePolicyNames() []string {
	return []string{
		string(RouteLocalFirst), string(RouteRemoteFirst), string(RouteAuto),
		string(RouteLocalOnly), string(RouteRemoteOnly), string(RouteWeighted),
	}
}

// RoutePolicyList renders RoutePolicyNames for a help string or an error.
func RoutePolicyList() string { return strings.Join(RoutePolicyNames(), ", ") }

func parseRoutePolicy(s string) (routePolicy, error) {
	switch s {
	case "":
		return RouteLocalFirst, nil
	case string(RouteLocalFirst), string(RouteRemoteFirst), string(RouteAuto),
		string(RouteLocalOnly), string(RouteRemoteOnly), string(RouteWeighted):
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
	h := hostOf(baseURL)
	if h == "localhost" {
		return "local"
	}
	// By ADDRESS, not by string: 127.0.0.1, ::1 and their spelled-out forms
	// (0:0:0:0:0:0:0:1) are all this machine, and only net.ParseIP knows that
	// (2026-09-26 audit).
	if ip := net.ParseIP(h); ip != nil && ip.IsLoopback() {
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
	// A bracketed IPv6 literal: its colons are the ADDRESS, not a port
	// separator, so cutting at the first ":" here returned "[" and made
	// routeLocality call loopback remote (2026-09-26 audit). Return the
	// address without its brackets, the form routeLocality also accepts.
	if strings.HasPrefix(s, "[") {
		if i := strings.Index(s, "]"); i >= 0 {
			return s[1:i]
		}
		return s
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
// zero or past, means "not escalated"). The signal is PER-LEG: only results
// from the leg selectRoute CHOSE count — a success on some other leg must not
// clear the primary's failure streak, and a failure on a fallback must not
// feed the escalation counter toward it.
//
// One leg, not "whichever leg served the request": the oversize crossover
// serves a leg on another host by construction (oversizeSwap), and its caller
// therefore feeds the breaker only — see crossoverEscalationLeg for why that
// result is deliberately not an escalation signal (2026-09-26 audit).
// The count is per LEG, not "the leg this session was last served on". A
// single current-leg slot meant every request for another tier re-pointed the
// state and RESET the streak: Claude Code interleaves tiers (haiku titling,
// subagents), and in a plan those tiers resolve to different base URLs, so the
// primary's consecutive-failure count was wiped before it could ever reach
// autoEscalateAfterFails and the escalation was unreachable no matter how
// often the leg failed (2026-09-26 audit, tenth round). Each leg's own streak
// is the honest signal; a success on one leg still cannot clear another's.
type routeEscalation struct {
	mu             sync.Mutex
	fails          map[string]int
	escalatedLeg   string
	escalatedUntil atomic.Int64
}

// expireLocked applies what the hold window MEANS: an escalation that ran out
// with no further failures escalated away from a leg that has now been
// quiet-healthy for autoEscalateHoldFor, so that leg is considered recovered
// and its streak starts over. Without this, a leg's count would survive the
// window and re-arm on the very next single failure — the "reset" the old
// per-request leg change used to provide as a side effect (2026-09-26 audit,
// tenth round). Only the leg the escalation was armed on is cleared: every
// other leg keeps its own independent streak.
//
// Callers hold e.mu.
func (e *routeEscalation) expireLocked() {
	if e.escalatedLeg == "" || time.Now().UnixNano() < e.escalatedUntil.Load() {
		return
	}
	delete(e.fails, e.escalatedLeg)
	e.escalatedLeg = ""
	e.escalatedUntil.Store(0)
}

func (e *routeEscalation) recordFail(baseURL string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	if e.fails == nil {
		e.fails = map[string]int{}
	}
	e.fails[baseURL]++
	if e.fails[baseURL] >= autoEscalateAfterFails {
		e.escalatedLeg = baseURL
		e.escalatedUntil.Store(time.Now().Add(autoEscalateHoldFor).UnixNano())
	}
}

func (e *routeEscalation) recordOK(baseURL string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	delete(e.fails, baseURL)
	if e.escalatedLeg == baseURL {
		// The leg we were escalating away from served a request: nothing left
		// to escalate from.
		e.escalatedLeg = ""
		e.escalatedUntil.Store(0)
	}
}

func (e *routeEscalation) escalated() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	return e.escalatedLeg != ""
}

// escalatedFor reports whether this session is currently escalated AWAY FROM
// baseURL. The leg matters: escalation exists to move a session off a leg that
// is failing, and applying it to every tier of the session sent requests that
// resolved to a healthy secondary onto the flapping primary — a request whose
// own leg has never failed must be left where it resolved
// (2026-09-26 audit).
func (e *routeEscalation) escalatedFor(baseURL string) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.expireLocked()
	return e.escalatedLeg == baseURL && baseURL != ""
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

// escalatedFor reports whether this session is escalated away from baseURL —
// the only form of escalation the router acts on.
func (re *routeEscalations) escalatedFor(sessionID, baseURL string) bool {
	if re == nil {
		return false
	}
	re.mu.Lock()
	e := re.m[sessionID]
	re.mu.Unlock()
	return e != nil && e.escalatedFor(baseURL)
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

// weightedRingVpointsPerUnit sets the granularity of the hash ring: each
// route gets Weight*weightedRingVpointsPerUnit points on the ring, so a
// weight-3 route gets ~3x the ring coverage (and therefore ~3x the traffic
// share) of a weight-1 route. Consistent-hash arc lengths are the gaps
// between adjacent SORTED points, not the point count directly — with too
// few points those gaps have high variance and the traffic split drifts
// well off the nominal weight ratio (observed ~2x error at 100/unit in
// testing). 1000 keeps that variance small even for a 2-3 route ring,
// without making ring construction (O(routes*vpoints), rebuilt only when
// the healthy set changes) meaningfully expensive.
const weightedRingVpointsPerUnit = 1000

// maxShardWeight bounds one leg's `--shard model:weight`. The weight is a
// relative share, so any ratio a user actually wants is expressible at small
// numbers (3:1, 100:1); a value in the millions buys no reachable split but
// costs Weight*weightedRingVpointsPerUnit ring points PER REQUEST, all of them
// allocated and sorted before the pick. `--shard box/big:1000000` was ~149 GiB
// of ring per request (2026-09-26 audit, tenth round). Out-of-range weights are
// skipped like every other malformed value this flag accepts (a non-numeric
// weight, a weight <= 0), so the flag never silently pretends to have applied a
// split it did not.
const maxShardWeight = 10000

// maxRingPoints bounds the whole ring regardless of the weights, so the memory
// a single request can allocate does not depend on a number in a flag. A ring
// larger than this has its points apportioned proportionally, keeping ratios
// close (each candidate keeps at least one point) without the arithmetic above
// being able to reach the allocator at all.
const maxRingPoints = 20000

// weightedRingPoint is one virtual node on a weightedPick hash ring.
type weightedRingPoint struct {
	route proxyRoute
	hash  uint32
}

// weightedPick selects a leg for RouteWeighted: build the ring from base +
// every Fallback (plus Oversize) that carries Weight>0 and whose breaker is
// currently closed, pinned-policy-permitting, then hash SessionID onto it.
// Same SessionID always lands on the same leg while the healthy/weighted
// set doesn't change (session-sticky, for prefix-cache reuse — see
// RouteWeighted's doc), but the split ACROSS sessions follows the weights.
// Returns false when fewer than 2 distinct base URLs qualify — nothing to
// weight, caller falls through to ordinary failover.
// weightedRing builds the hash ring for weightedPick. Each route gets
// Weight*weightedRingVpointsPerUnit points when the total fits maxRingPoints;
// otherwise the budget is apportioned by weight (at least one point per route),
// so the size of the ring — and the work one request does to build and sort it —
// is bounded by maxRingPoints no matter what weights reached here.
//
// It is rebuilt per request, not cached: the candidate set is "the weighted
// legs whose breaker is currently closed, pinned-policy-permitting", and the
// cost of asking is a map lookup per leg, while a cache would have to key on
// that whole set and invalidate on every breaker transition. What makes that
// affordable is the bound, not reuse.
func weightedRing(candidates []proxyRoute) []weightedRingPoint {
	points := make([]int, len(candidates))
	totalWeight := int64(0)
	for i, r := range candidates {
		totalWeight += int64(r.Weight)
		points[i] = r.Weight * weightedRingVpointsPerUnit
	}
	if int64(maxRingPoints) < totalWeight*weightedRingVpointsPerUnit {
		for i, r := range candidates {
			n := int(int64(r.Weight) * maxRingPoints / totalWeight)
			if n < 1 {
				n = 1
			}
			points[i] = n
		}
	}

	var ring []weightedRingPoint
	for i, r := range candidates {
		for n := 0; n < points[i]; n++ {
			h := fnv.New32a()
			h.Write([]byte(r.BaseURL + "#" + strconv.Itoa(n)))
			ring = append(ring, weightedRingPoint{route: r, hash: h.Sum32()})
		}
	}
	sort.Slice(ring, func(i, j int) bool { return ring[i].hash < ring[j].hash })
	return ring
}

func (t proxyRouteTable) weightedPick(base proxyRoute) (proxyRoute, bool) {
	var candidates []proxyRoute
	seen := map[string]bool{}
	add := func(rs ...proxyRoute) {
		for _, r := range rs {
			if r.BaseURL == "" || r.Weight <= 0 || seen[r.BaseURL] {
				continue
			}
			if pin := t.Policy.pinned(); pin != "" && routeLocality(r.BaseURL) != pin {
				continue
			}
			if t.breakers.open(r.BaseURL) {
				continue
			}
			seen[r.BaseURL] = true
			candidates = append(candidates, r)
		}
	}
	add(base)
	add(t.Fallbacks...)
	add(t.Oversize)
	if len(candidates) < 2 {
		return proxyRoute{}, false
	}

	ring := weightedRing(candidates)

	kh := fnv.New32a()
	kh.Write([]byte(t.SessionID))
	key := kh.Sum32()
	idx := sort.Search(len(ring), func(i int) bool { return ring[i].hash >= key })
	if idx == len(ring) {
		idx = 0 // wrap past the last point to the first
	}
	return ring[idx].route, true
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
	// No per-request "note the leg" step: the escalation state counts each
	// leg's own consecutive failures, so which tier a request happened to
	// resolve to cannot reset another leg's streak (see routeEscalation).
	return route, model, usedFallback
}

func (t proxyRouteTable) resolveRoute(requested string) (proxyRoute, string, bool) {
	base, model := t.resolve(requested)

	// `auto` escalation: when this session's failures have accumulated past
	// autoEscalateAfterFails, skip ahead to the strongest healthy secondary
	// leg immediately (the breaker only speaks up at 3, and only for its own
	// per-leg signal). Degrades to the normal path when no secondary
	// qualifies.
	if t.Policy == RouteAuto && t.escalations.escalatedFor(t.SessionID, base.BaseURL) {
		if r, ok := t.escalationTarget(base); ok {
			return r, r.UpstreamModel, true
		}
	}

	// `weighted`: unlike every other policy, this one can steer HEALTHY
	// traffic away from base — Fallbacks are not failover-only here, they're
	// live capacity. Only kicks in when at least 2 distinct base URLs carry
	// a nonzero Weight; otherwise there is nothing to split and this falls
	// through to the ordinary failover path below.
	if t.Policy == RouteWeighted {
		if r, ok := t.weightedPick(base); ok {
			return r, r.UpstreamModel, r.BaseURL != base.BaseURL
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
		// An anthropic-wire REMOTE oversize leg (BaseURL set — the plan rows)
		// is breaker-keyed on its own base URL, not the native constant: the
		// two are different upstreams and one being down says nothing about
		// the other.
		key := nativeOversizeBreakerKey
		if t.Oversize.BaseURL != "" {
			key = t.Oversize.BaseURL
		}
		if t.breakers.open(key) {
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

// startRouteHealthPoll probes every distinct fallback leg every pollInterval
// until ctx is cancelled, recording the outcome so breakers both OPEN
// proactively and RECOVER without waiting for a real request to prove the leg
// is back. GET /models: every OpenAI-compatible backend we route to (vLLM,
// Ollama, gateways, aggregators) serves it, and unlike a TCP dial it exercises
// the full HTTP path including any LB in front.
//
// The probe carries the leg's OWN credential, and only a status that actually
// proves the completion path works counts (2026-09-26 audit, tenth round,
// auditor B — HIGH). Both halves were wrong before:
//
//   - Unauthenticated, a key-protected leg answered 401/403 to a probe that
//     never sent its key — and any status below 500 was read as recovery.
//     recordOK zeroes BOTH openUntil and fails, so an OPEN breaker was closed
//     one poll later and a leg failing real requests more slowly than the poll
//     interval never accumulated breakerFailsToOpen at all: the circuit never
//     opened, the documented failover never happened, and every 30s window
//     bounced the session back onto the dead leg.
//   - For the same reason a 429 from a shedding leg read healthy forever.
//
// So: 2xx is the only evidence of recovery; 401/403 is evidence of NOTHING
// (the leg answered, but this probe's authorization says nothing about the
// completion path) and is left as no signal — it must not erase a real failure
// count; every other answer, and a transport error, is a leg failure.
func (t proxyRouteTable) startRouteHealthPoll(ctx context.Context, pollInterval time.Duration) {
	seen := map[string]bool{}
	var legs []proxyRoute
	for _, r := range append(append([]proxyRoute{t.Default}, t.Fallbacks...), t.Oversize) {
		// An anthropic-wire remote leg is NOT probed: GET <base>/models is an
		// OpenAI-shaped probe, and a vendor that serves /messages there need
		// not serve /models at all (the plan rows' base carries an /anthropic
		// path — z.ai answers 404 for anything but the Anthropic surface). A
		// 404 would open the breaker and silently retire a working leg from
		// both fallback and oversize selection. Unprobed legs read as healthy
		// (routeBreakers.open is false for an unknown key), matching how a
		// native-anthropic leg is treated (it has no BaseURL to probe at all).
		if r.Wire == "anthropic" {
			continue
		}
		if r.BaseURL != "" && !seen[r.BaseURL] {
			seen[r.BaseURL] = true
			legs = append(legs, r)
		}
	}
	client := &http.Client{Timeout: 5 * time.Second}
	tick := time.NewTicker(pollInterval)
	defer tick.Stop()
	probe := func() {
		for _, leg := range legs {
			u := leg.BaseURL
			// ctx-bound request: shutdown cancels an in-flight probe instead
			// of letting shutdown lag behind it (up to the 5s client timeout
			// per URL under the old channel-only signal).
			req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(u, "/")+"/models", nil)
			if err != nil {
				continue
			}
			// The same credential the leg's own requests carry (KeyEnv
			// re-read per probe, exactly as resolveKey does per request).
			if key := leg.resolveKey(); key != "" {
				req.Header.Set("Authorization", "Bearer "+key)
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
			switch {
			case resp.StatusCode >= 200 && resp.StatusCode < 300:
				t.breakers.recordOK(u)
			case resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden:
				// Alive but unauthorized for THIS probe: not evidence the
				// completion path works, and not evidence it is broken.
				// Recording nothing is the point — recordOK here is what
				// kept an OPEN breaker from ever staying open.
			default:
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
// extractSonnetModel — including its shape: EVERY occurrence is stripped and
// the last one wins. Returning at the first match left a repeated flag in the
// passthrough args, where Claude Code — which has no such flag — failed the
// launch while the launcher ran the earlier value (2026-09-26 audit). Not
// forwarded to the child binary.
func extractRoutePolicy(args []string) (policy string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--route-policy" && i+1 < len(args):
			// Trimmed: the policy is matched exactly, so a padded value
			// (" local-only") failed with a message quoting surrounding
			// whitespace the user cannot see they typed.
			policy = strings.TrimSpace(args[i+1])
			i++
		case strings.HasPrefix(a, "--route-policy="):
			policy = strings.TrimSpace(strings.TrimPrefix(a, "--route-policy="))
		default:
			rest = append(rest, a)
		}
	}
	return policy, rest
}

// primaryRoutePolicy is the route_policy a user remote declares for the
// primary in remotes.json, or "" when the primary is not a user remote
// (router, daemon, ":local", native).
//
// It is the policy this launch would use with no --route-policy flag, because
// tier_routing.go fills the empty flag from plan.Primary.RoutePolicy — so
// anything that "answers" the policy on the user's behalf must lead with this
// value rather than with a default of its own. The wizard's policy step did
// the latter, and its Enter (which answers the step) silently turned a remote's
// local-only into auto (2026-09-26 audit).
//
// Only the "<remote>/<id>" spelling is looked up: a bare id needs the
// bareRemoteModelIndex sweep (a per-process network sweep this path must not
// pay for a menu label), and bare ownership is ambiguous by definition —
// resolveLaunchEndpoint refuses the ambiguous ones outright.
func primaryRoutePolicy(model string) string {
	if !strings.Contains(model, "/") {
		return ""
	}
	r, _, ok := findUserRemoteForModel(model)
	if !ok {
		return ""
	}
	return r.RoutePolicy
}

// extractOversizeModel pulls "--oversize <model>" (or "--oversize=<model>"),
// the larger-context leg that serves requests the current leg cannot hold
// (the auto-compaction call near a ceiling, mostly). Same picker vocabulary
// as --sonnet-model: "<remote>/<id>", "router/<id>", "<id>:local", bare id.
// Like extractRoutePolicy, every occurrence is stripped and the last one wins:
// a repeat used to be forwarded to the child binary, which does not know the
// flag. Not forwarded to the child binary.
func extractOversizeModel(args []string) (model string, rest []string) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--oversize" && i+1 < len(args):
			// Trimmed for the same reason as --sonnet-model: a padded value
			// silently routes the leg to the primary's remote.
			model = strings.TrimSpace(args[i+1])
			i++
		case strings.HasPrefix(a, "--oversize="):
			model = strings.TrimSpace(strings.TrimPrefix(a, "--oversize="))
		default:
			rest = append(rest, a)
		}
	}
	return model, rest
}

// extractShardFlags pulls every repeatable "--shard <model>:<weight>" (or
// "--shard=<model>:<weight>") out of the passthrough args — the CLI surface
// for RouteWeighted (route_policy.go): sets a route's Weight for THIS
// launch without editing remotes.json. <model> uses the same picker
// vocabulary as --sonnet-model ("<remote>/<id>", "router/<id>", bare id);
// <weight> is a positive integer. Repeat the flag once per leg, e.g.
// `--shard gateway46/oaica-35b-a3b-vision:3 --shard kat-91:1
// --route-policy weighted`. A malformed entry (no ":weight", non-positive,
// non-integer) is dropped rather than failing the launch — a typo here
// should degrade to "that leg gets no shard weight", not block the whole
// launch, since --route-policy weighted already degrades gracefully to
// plain failover when fewer than 2 legs end up weighted. Not forwarded to
// the child binary.
func extractShardFlags(args []string) (map[string]int, []string) {
	var shards map[string]int
	var rest []string
	for i := 0; i < len(args); i++ {
		a := args[i]
		var spec string
		switch {
		case a == "--shard" && i+1 < len(args):
			spec = args[i+1]
			i++
		case strings.HasPrefix(a, "--shard="):
			spec = strings.TrimPrefix(a, "--shard=")
		default:
			rest = append(rest, a)
			continue
		}
		// The LAST colon separates the weight: this flag's vocabulary is the
		// picker's, and picker ids carry colons of their own ("llama3.2:3b",
		// "<name>:cloud", "<name>:local"). Cutting at the first one read the
		// weight as "cloud:3", failed to parse it and dropped the entry with no
		// message, so the split the user asked for silently degraded to plain
		// failover (2026-09-27 audit, round 22).
		cut := strings.LastIndex(spec, ":")
		if cut < 0 {
			continue
		}
		model, weightStr := spec[:cut], spec[cut+1:]
		// Both halves trimmed: a padded model id matches no existing route
		// and this flag's documented behaviour for a non-matching id is a
		// SILENT no-op, so padding would look like the weight was ignored.
		model = strings.TrimSpace(model)
		weight, err := strconv.Atoi(strings.TrimSpace(weightStr))
		if err != nil || weight <= 0 || weight > maxShardWeight {
			continue
		}
		if shards == nil {
			shards = map[string]int{}
		}
		shards[model] = weight
	}
	return shards, rest
}
