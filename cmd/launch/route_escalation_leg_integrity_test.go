package launch

// route_escalation_leg_integrity_test.go — `auto` escalation was a session-wide
// rule rather than a per-leg one, so it steered EVERY tier of an escalated
// session off its route, including tiers that had never failed, and could land
// them on the leg that was flapping (2026-09-26 audit).
//
// The state is per-session and holds one leg: the leg the session was last
// served on. resolveRoute consulted it without comparing it to the leg the
// request had just resolved to, and escalationTarget then picks the
// largest-window leg that is NOT the request's own route — the flapping primary
// qualifies for that as long as its breaker has not opened yet (the breaker
// needs 3 consecutive failures; escalation arms at 2). So a session with a
// flapping remote primary and a healthy 1M-window secondary sent its
// secondary-bound requests to the primary, and selectRoute's noteLeg then
// re-pointed the session's state at that leg — the escalation fed itself.

import (
	"testing"
)

func TestAutoEscalationLeavesOtherTiersAlone(t *testing.T) {
	primary := proxyRoute{BaseURL: "https://flapping.example/v1", UpstreamModel: "primary-m", Label: "remote:primary", ContextWindow: 262144}
	big := proxyRoute{BaseURL: "https://big.example/v1", UpstreamModel: "big-m", Label: "remote:big", ContextWindow: 1000000}
	local := proxyRoute{BaseURL: "http://127.0.0.1:11434/v1", UpstreamModel: "local-m", Label: "local", ContextWindow: 32768}

	table := proxyRouteTable{
		Policy:      RouteAuto,
		Default:     local,
		ByModel:     map[string]proxyRoute{"primary-model": primary, "big-model": big},
		Fallbacks:   []proxyRoute{primary, big},
		breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
	}
	table.SessionID = "s1"

	// The primary leg fails twice — the threshold — while its breaker is still
	// closed (that needs three).
	table.escalations.noteLeg(table.SessionID, primary.BaseURL)
	for i := 0; i < autoEscalateAfterFails; i++ {
		table.escalations.recordFail(table.SessionID, primary.BaseURL)
	}
	if !table.escalations.escalated(table.SessionID) {
		t.Fatalf("the session did not escalate at %d consecutive failures", autoEscalateAfterFails)
	}

	// A request that belongs on the healthy big leg must stay there.
	r, _, fb := table.selectRoute("big-model")
	if fb || r.BaseURL != big.BaseURL {
		t.Errorf("a request resolved to the healthy secondary %s was routed to %s (fallback=%v) — the escalation armed for the flapping primary was applied to a tier that never failed", big.BaseURL, r.BaseURL, fb)
	}

	// And a request for the local default tier must not cross the line either.
	if r, _, fb := table.selectRoute("local-m"); fb || r.BaseURL != local.BaseURL {
		t.Errorf("a request resolved to the local default was re-routed to %s (fallback=%v)", r.BaseURL, fb)
	}

	// The tier that DID fail still escalates.
	if r, _, fb := table.selectRoute("primary-model"); !fb || r.BaseURL != big.BaseURL {
		t.Errorf("the flapping leg's own requests must escalate to %s (got %s, fallback=%v)", big.BaseURL, r.BaseURL, fb)
	}
}

// The control: escalation armed on the leg a request resolves to still fires,
// so this cannot be passed by never escalating.
func TestAutoEscalationStillFiresForTheLegThatFailed(t *testing.T) {
	primary := proxyRoute{BaseURL: "https://flapping.example/v1", UpstreamModel: "primary-m", Label: "remote:primary", ContextWindow: 262144}
	big := proxyRoute{BaseURL: "https://big.example/v1", UpstreamModel: "big-m", Label: "remote:big", ContextWindow: 1000000}
	local := proxyRoute{BaseURL: "http://127.0.0.1:11434/v1", UpstreamModel: "local-m", Label: "local", ContextWindow: 32768}

	table := proxyRouteTable{
		Policy:      RouteAuto,
		Default:     local,
		ByModel:     map[string]proxyRoute{"primary-model": primary, "big-model": big},
		Fallbacks:   []proxyRoute{primary, big},
		breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
	}
	table.SessionID = "s2"

	// The BIG leg is the one failing this time; the primary is its fallback.
	table.escalations.noteLeg(table.SessionID, big.BaseURL)
	for i := 0; i < autoEscalateAfterFails; i++ {
		table.escalations.recordFail(table.SessionID, big.BaseURL)
	}

	r, _, fb := table.selectRoute("big-model")
	if !fb || r.BaseURL != primary.BaseURL {
		t.Errorf("the failing leg's own requests must escalate to %s (got %s, fallback=%v)", primary.BaseURL, r.BaseURL, fb)
	}
}
