package launch

import (
	"bytes"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// TestParseRoutePolicy covers the accepted vocabulary, the empty default,
// and loud failure on a typo.
func TestParseRoutePolicy(t *testing.T) {
	for in, want := range map[string]routePolicy{
		"":              RouteLocalFirst,
		"local-first":   RouteLocalFirst,
		"remote-first":  RouteRemoteFirst,
		"auto":          RouteAuto,
		"local-only":    RouteLocalOnly,
		"remote-only":   RouteRemoteOnly,
		"Remote-First":  RouteRemoteFirst, // lenient on case? No — strict:
	} {
		if in == "Remote-First" {
			continue
		}
		got, err := parseRoutePolicy(in)
		if err != nil || got != want {
			t.Errorf("parseRoutePolicy(%q) = %v, %v; want %v", in, got, err, want)
		}
	}
	if _, err := parseRoutePolicy("locall-first"); err == nil {
		t.Error("typo'd policy must fail loudly, not degrade to the default")
	}
}

// TestLocalhostLocality pins the loopback rule the fallback ordering uses.
func TestLocalhostLocality(t *testing.T) {
	for url, want := range map[string]string{
		"http://127.0.0.1:11434/v1": "local",
		"http://localhost:8081/v1":  "local",
		"https://api.deepseek.com/v1": "remote",
	} {
		if got := routeLocality(url); got != want {
			t.Errorf("routeLocality(%q) = %q, want %q", url, got, want)
		}
	}
}

// End-to-end: the primary leg keeps answering 502 (post-retry), the breaker
// opens after breakerFailsToOpen failed requests, and subsequent traffic —
// under local-first — lands on the healthy local fallback with the
// X-Oaica-Route attribution header. Retries are cut to 1 so the test is fast.
func TestRoutePolicy_FallbackOnOpenBreaker(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	oldMax := proxyUpstreamMaxRetries
	proxyUpstreamMaxRetries = 1
	oldOpenFor := breakerOpenFor
	breakerOpenFor = time.Minute
	defer func() { proxyUpstreamMaxRetries, breakerOpenFor = oldMax, oldOpenFor }()

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("down"))
	}))
	defer down.Close()

	upOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// The 30s health poll also hits /models; answer everything.
		if r.URL.Path == "/v1/models" || r.URL.Path == "/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-2", "model": "local-m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "local ok"}}},
		})
	}))
	defer upOK.Close()

	table := proxyRouteTable{
		Policy: RouteLocalFirst,
		Default: proxyRoute{BaseURL: down.URL, UpstreamModel: "m", Label: "remote:down"},
		Fallbacks: []proxyRoute{{BaseURL: upOK.URL, UpstreamModel: "local-m", Label: "daemon:local"}},
		breakers: &routeBreakers{},
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, table) }()

	post := func() (string, http.Header) {
		body, _ := json.Marshal(map[string]any{
			"model": "m", "max_tokens": 4,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var out map[string]any
		_ = json.NewDecoder(resp.Body).Decode(&out)
		content, _ := out["content"].([]any)
		text := ""
		if len(content) > 0 {
			m, _ := content[0].(map[string]any)
			text, _ = m["text"].(string)
		}
		return text, resp.Header
	}

	// First breakerFailsToOpen requests hit the dead primary (that's the
	// breaker learning it's dead, correctly — no way around paying for the
	// detection window).
	for i := 0; i < breakerFailsToOpen; i++ {
		text, hdr := post()
		if text == "local ok" || hdr.Get("X-Oaica-Route") != "remote:down" {
			t.Fatalf("request %d should still be on the (failing) primary, got %q", i+1, hdr.Get("X-Oaica-Route"))
		}
	}
	// Breaker now OPEN for the primary: the next request must land on the
	// local fallback, with the attribution header naming the real leg.
	text, hdr := post()
	if text != "local ok" {
		t.Fatalf("expected fallback response, got: %v", text)
	}
	if hdr.Get("X-Oaica-Route") != "daemon:local" {
		t.Fatalf("X-Oaica-Route = %q, want daemon:local", hdr.Get("X-Oaica-Route"))
	}
}

// TestRoutePolicy_LocalOnlyDoesNotCross verifies the pin at the selection
// layer (no wire needed — selectRoute only consults breaker state): with
// local-only and only a remote fallback, an OPEN local breaker still returns
// the local route (the request then fails visibly) instead of silently
// crossing to the remote. remote-first, by contrast, picks the remote.
func TestRoutePolicy_LocalOnlyDoesNotCross(t *testing.T) {
	localDown := proxyRoute{BaseURL: "http://127.0.0.1:9206/v1", UpstreamModel: "m", Label: "daemon:local"}
	remoteUp := proxyRoute{BaseURL: "https://remote.example/v1", UpstreamModel: "m", Label: "remote:x"}
	for _, tc := range []struct {
		policy routePolicy
		want   proxyRoute
	}{{RouteLocalOnly, localDown}, {RouteRemoteFirst, remoteUp}} {
		table := proxyRouteTable{
			Policy:   tc.policy,
			Default:  localDown,
			Fallbacks: []proxyRoute{remoteUp},
			breakers: &routeBreakers{},
		}
		for i := 0; i < breakerFailsToOpen; i++ {
			table.breakers.recordFail(localDown.BaseURL)
		}
		got, _, _ := table.selectRoute("m")
		if got.BaseURL != tc.want.BaseURL {
			t.Errorf("policy %s with dead local leg: selected %s, want %s", tc.policy, got.BaseURL, tc.want.BaseURL)
		}
	}
}
// TestOversizeSwapRules pins the crossover gate: strictly-larger window,
// different base URL, healthy breaker, locality pin respected, and the
// oversized leg itself must be able to hold the request (else no point).
func TestOversizeSwapRules(t *testing.T) {
	small := proxyRoute{BaseURL: "http://127.0.0.1:9206/v1", UpstreamModel: "m", Label: "local-serve:local", ContextWindow: 262144}
	big := proxyRoute{BaseURL: "https://big.example/v1", UpstreamModel: "big-m", Label: "remote:big", ContextWindow: 1000000}
	mk := func(policy routePolicy) *routeBreakers {
		table := proxyRouteTable{
			Policy:   policy,
			Default:  small,
			Oversize: big,
		}
		b := &routeBreakers{}
		table.breakers = b
		return b
	}
	// 230k-est prompt against 262k with a margin: no fit on the small leg…
	est, margin := 262000, 1000
	if got, swapped := (&proxyRouteTable{Oversize: big, breakers: &routeBreakers{}}).oversizeSwap(small, est, margin); !swapped || got.UpstreamModel != "big-m" {
		t.Errorf("overflow request should swap to the big leg (got %v, %v)", got, swapped)
	}
	// …but one that fits must stay on the small leg.
	if _, swapped := (&proxyRouteTable{Oversize: big, breakers: &routeBreakers{}}).oversizeSwap(small, 1000, 1000); swapped {
		t.Error("fitting request must not crossover")
	}
	// Same base URL = same leg, no swap.
	if _, swapped := (&proxyRouteTable{Oversize: small, breakers: &routeBreakers{}}).oversizeSwap(small, 262000, 1000); swapped {
		t.Error("same-base-URL oversize is a no-op")
	}
	// local-only pin must block the remote crossover.
	if _, swapped := (&proxyRouteTable{Policy: RouteLocalOnly, Oversize: big, breakers: &routeBreakers{}}).oversizeSwap(small, 262000, 1000); swapped {
		t.Error("local-only must not crossover to a remote oversize leg")
	}
	// OPEN breaker on the oversize leg blocks the swap (visible 400 instead).
	rb := mk(RouteLocalFirst)
	for i := 0; i < breakerFailsToOpen; i++ {
		rb.recordFail(big.BaseURL)
	}
	if _, swapped := (&proxyRouteTable{Oversize: big, breakers: rb}).oversizeSwap(small, 262000, 1000); swapped {
		t.Error("OPEN oversize breaker must block the swap")
	}
}

// TestOversizeCrossOverEndToEnd: primary claims a 300-token window, the
// request needs more, and the --oversize leg (real, healthy) serves it
// instead of a 400 — with the attribution header naming the new leg.
func TestOversizeCrossOverEndToEnd(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	// A leg whose /models reports max_model_len 300; the chat call always
	// 501s to prove it never sees the request.
	tiny := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"tiny","context_length":300,"max_model_len":300}]}`))
			return
		}
		w.WriteHeader(http.StatusNotImplemented)
	}))
	defer tiny.Close()

	big := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"data":[{"id":"big-m","context_length":1000000,"max_model_len":1000000}]}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-3", "model": "big-m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "compacted on big"}}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3},
		})
	}))
	defer big.Close()

	// Stub the 2s launch-time probe to real values via the swappable fn.
	oldProbe := remoteContextWindowFn
	remoteContextWindowFn = func(route proxyRoute) int {
		if route.BaseURL == tiny.URL {
			return 300
		}
		return 1000000
	}
	defer func() { remoteContextWindowFn = oldProbe }()

	table := proxyRouteTable{
		Default:  proxyRoute{BaseURL: tiny.URL, UpstreamModel: "tiny", Label: "remote:small", ContextWindow: 300},
		Oversize: proxyRoute{BaseURL: big.URL, UpstreamModel: "big-m", Label: "remote:big", ContextWindow: 1000000},
		breakers: &routeBreakers{},
		// 10KB of prompt text: clearly beyond 300 tokens, trivially inside 1M.
	}
	// (SessionID empty: calib keys per-label, fine for a one-shot test.)

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, table) }()

	body, _ := json.Marshal(map[string]any{
		"model": "tiny", "max_tokens": 8,
		"messages": []map[string]any{{"role": "user", "content": "please compact this: " + string(make([]byte, 10240))}},
	})
	resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var out map[string]any
	_ = json.NewDecoder(resp.Body).Decode(&out)
	if out["error"] != nil {
		t.Fatalf("expected oversize crossover to serve the request, got error: %v", out["error"])
	}
	if got := resp.Header.Get("X-Oaica-Route"); got != "remote:big" {
		t.Fatalf("X-Oaica-Route = %q, want remote:big", got)
	}
}

// TestAutoPolicy_EscalatesToStrongerLeg end-to-end: under `auto`, two
// consecutive 5xx on the primary escalate the session to the larger
// secondary leg WITHOUT the breaker being open (2 < breakerFailsToOpen).
// Under local-first the identical traffic stays on the primary.
func TestAutoPolicy_EscalatesToStrongerLeg(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	oldMax := proxyUpstreamMaxRetries
	proxyUpstreamMaxRetries = 1
	defer func() { proxyUpstreamMaxRetries = oldMax }()

	down := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusBadGateway)
	}))
	defer down.Close()

	upOK := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-4", "model": "big-m",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "escalated ok"}}},
		})
	}))
	defer upOK.Close()

	tbl := func(policy routePolicy) proxyRouteTable {
		return proxyRouteTable{
			Policy:     policy,
			Default:    proxyRoute{BaseURL: down.URL, UpstreamModel: "m", Label: "remote:down", ContextWindow: 262144},
			Fallbacks:  []proxyRoute{{BaseURL: upOK.URL, UpstreamModel: "big-m", Label: "remote:big", ContextWindow: 1000000}},
			breakers:   &routeBreakers{},
			escalations: &routeEscalations{},
		}
	}

	// auto: requests 1..N stay on the (failing) primary; request N+1 has
	// escalated to the bigger leg, with the attribution header naming it.
	post := func(table proxyRouteTable) http.Header {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		go func() { _ = RunAnthropicOpenAIProxyRoutes(ln, table) }()
		body, _ := json.Marshal(map[string]any{
			"model": "m", "max_tokens": 4,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", bytes.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		return resp.Header
	}

	runN := func(policy routePolicy) http.Header {
		table := tbl(policy)
		for i := 0; i < autoEscalateAfterFails; i++ {
			post(table)
		}
		return post(table)
	}

	// auto: after autoEscalateAfterFails failures, the next request lands on
	// the escalated (bigger) leg. Note the breaker is NOT open here (2 < 3)
	// — this is escalation, not ordinary failover.
	if got := runN(RouteAuto).Get("X-Oaica-Route"); got != "remote:big" {
		t.Fatalf("auto: request %d should land on the escalated leg, X-Oaica-Route = %q", autoEscalateAfterFails+1, got)
	}

	// local-first: WITHOUT the breaker open there is no failover, so the
	// identical traffic stays on the primary.
	if got := runN(RouteLocalFirst).Get("X-Oaica-Route"); got != "remote:down" {
		t.Fatalf("local-first must NOT escalate (X-Oaica-Route = %q, want remote:down)", got)
	}
}

// TestAutoPolicy_ResetAndSignals pins the escalation state machine at the
// selection layer: N consecutive failures escalate; a 200 resets the
// consecutive counter; an active escalation survives that success but decays
// after autoEscalateHoldFor; an OPEN breaker on the target leg degrades back
// to the base route; other policies never escalate.
func TestAutoPolicy_ResetAndSignals(t *testing.T) {
	oldHold := autoEscalateHoldFor
	autoEscalateHoldFor = 20 * time.Millisecond
	defer func() { autoEscalateHoldFor = oldHold }()

	base := proxyRoute{BaseURL: "https://primary.example/v1", UpstreamModel: "m", Label: "remote:primary", ContextWindow: 262144}
	big := proxyRoute{BaseURL: "https://big.example/v1", UpstreamModel: "big-m", Label: "remote:big", ContextWindow: 1000000}

	table := proxyRouteTable{
		Policy: RouteAuto, Default: base,
		Fallbacks:   []proxyRoute{big},
			breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
	}
	table.escalations.noteLeg(table.SessionID, base.BaseURL)

	for i := 0; i < autoEscalateAfterFails-1; i++ {
		table.escalations.recordFail(table.SessionID, base.BaseURL)
	}
	if _, _, fb := table.selectRoute("m"); fb {
		t.Error("1 failure below the threshold must not escalate")
	}
	table.escalations.recordFail(table.SessionID, base.BaseURL)
	if r, _, fb := table.selectRoute("m"); !fb || r.BaseURL != big.BaseURL {
		t.Errorf("consecutive failures at the threshold must escalate to the big leg (got %s, fallback=%v)", r.BaseURL, fb)
	}
	// A success ON THE SERVING LEG clears the consecutive counter, but an
	// escalation already earned stands until the hold window decays (one
	// lucky 200 on the secondary must not bounce the session back onto a
	// flapping primary — and per-leg signals mean it doesn't even clear the
	// primary's failure streak).
	table.escalations.recordOK(table.SessionID, big.BaseURL)
	if r, _, fb := table.selectRoute("m"); !fb || r.BaseURL != big.BaseURL {
		t.Errorf("active escalation must survive a success (got %s, fallback=%v)", r.BaseURL, fb)
	}
	time.Sleep(2 * autoEscalateHoldFor)
	if r, _, fb := table.selectRoute("m"); fb || r.BaseURL != base.BaseURL {
		t.Errorf("escalation must reset after the hold window (got %s, fallback=%v)", r.BaseURL, fb)
	}
	// After the reset, ONE failure alone can't re-escalate (the counter was
	// cleared by the OK above).
	table.escalations.recordFail(table.SessionID, base.BaseURL)
	if _, _, fb := table.selectRoute("m"); fb {
		t.Error("single post-reset failure must not escalate")
	}

	// OPEN breaker on the target leg degrades to the base route gracefully.
	table2 := proxyRouteTable{
		Policy: RouteAuto, Default: base,
		Fallbacks:   []proxyRoute{big},
			breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
	}
	table.escalations.noteLeg(table.SessionID, base.BaseURL)
	for i := 0; i < autoEscalateAfterFails; i++ {
		table2.escalations.recordFail(table2.SessionID, base.BaseURL)
	}
	for i := 0; i < breakerFailsToOpen; i++ {
		table2.breakers.recordFail(big.BaseURL)
	}
	if r, _, fb := table2.selectRoute("m"); fb || r.BaseURL != base.BaseURL {
		t.Errorf("OPEN breaker on the escalation target must keep the base route (got %s, fallback=%v)", r.BaseURL, fb)
	}

	// local-first never escalates, no matter how many failures accumulate.
	table3 := proxyRouteTable{
		Policy: RouteLocalFirst, Default: base,
		Fallbacks:   []proxyRoute{big},
			breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
	}
	table.escalations.noteLeg(table.SessionID, base.BaseURL)
	for i := 0; i < autoEscalateAfterFails*3; i++ {
		table3.escalations.recordFail(table3.SessionID, base.BaseURL)
	}
	if r, _, fb := table3.selectRoute("m"); fb || r.BaseURL != base.BaseURL {
		t.Errorf("non-auto policy must not escalate (got %s, fallback=%v)", r.BaseURL, fb)
	}

	// No escalation state allocated -> route unchanged (nil-safety).
	table4 := proxyRouteTable{
		Policy: RouteAuto, Default: base,
		Fallbacks: []proxyRoute{big},
			breakers:  &routeBreakers{},
	}
	if r, _, fb := table4.selectRoute("m"); fb || r.BaseURL != base.BaseURL {
		t.Errorf("nil escalations must not escalate (got %s, fallback=%v)", r.BaseURL, fb)
	}
}

// TestWeightedPolicy_SplitsBySessionAndWeight builds a 3-weight/1-weight
// pair of healthy routes and checks that (a) many distinct sessions split
// roughly 3:1 across them, and (b) any one session ID always lands on the
// same leg across repeated resolves.
func TestWeightedPolicy_SplitsBySessionAndWeight(t *testing.T) {
	heavy := proxyRoute{BaseURL: "http://heavy", UpstreamModel: "m", Weight: 3}
	light := proxyRoute{BaseURL: "http://light", UpstreamModel: "m", Weight: 1}

	counts := map[string]int{}
	const sessions = 2000
	for i := 0; i < sessions; i++ {
		table := proxyRouteTable{
			Policy:    RouteWeighted,
			Default:   heavy,
			Fallbacks: []proxyRoute{light},
			SessionID: "session-" + strconv.Itoa(i),
			breakers:  &routeBreakers{},
		}
		r, _, _ := table.selectRoute("m")
		counts[r.BaseURL]++
	}

	ratio := float64(counts["http://heavy"]) / float64(counts["http://light"])
	if ratio < 2.0 || ratio > 4.5 {
		t.Errorf("weighted split ratio = %.2f, want roughly 3.0 (heavy=%d light=%d)",
			ratio, counts["http://heavy"], counts["http://light"])
	}

	// Same session ID must always resolve to the same leg.
	table := proxyRouteTable{
		Policy:    RouteWeighted,
		Default:   heavy,
		Fallbacks: []proxyRoute{light},
		SessionID: "sticky-session",
		breakers:  &routeBreakers{},
	}
	first, _, _ := table.selectRoute("m")
	for i := 0; i < 20; i++ {
		r, _, _ := table.selectRoute("m")
		if r.BaseURL != first.BaseURL {
			t.Fatalf("session stickiness broke: got %s, want %s", r.BaseURL, first.BaseURL)
		}
	}
}

// TestWeightedPolicy_ExcludesOpenBreakerAndUnweighted checks that a leg
// with an OPEN breaker is never picked even if weighted, and that a leg
// with Weight 0 never enters the ring — falling through to ordinary
// failover behavior when fewer than 2 weighted legs remain healthy.
func TestWeightedPolicy_ExcludesOpenBreakerAndUnweighted(t *testing.T) {
	heavy := proxyRoute{BaseURL: "http://heavy", UpstreamModel: "m", Weight: 3}
	unweighted := proxyRoute{BaseURL: "http://unweighted", UpstreamModel: "m"} // Weight 0

	breakers := &routeBreakers{}
	breakers.recordFail("http://heavy")
	breakers.recordFail("http://heavy")
	breakers.recordFail("http://heavy")
	if !breakers.open("http://heavy") {
		t.Fatal("test setup: expected heavy's breaker to be open")
	}

	table := proxyRouteTable{
		Policy:    RouteWeighted,
		Default:   heavy,
		Fallbacks: []proxyRoute{unweighted},
		SessionID: "s1",
		breakers:  breakers,
	}
	// heavy is OPEN, unweighted has Weight 0: nothing qualifies for the
	// ring, and unweighted also isn't a candidate for ordinary failover
	// (Weight doesn't gate failover) — but here it's the only Fallback, so
	// it should still be used via the pre-existing failover path.
	r, _, fb := table.selectRoute("m")
	if r.BaseURL != "http://unweighted" || !fb {
		t.Errorf("expected failover to unweighted fallback when weighted ring is empty, got %s fallback=%v", r.BaseURL, fb)
	}
}
