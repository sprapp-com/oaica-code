package launch

// escalation_alternation_starvation_integrity_test.go — `auto` escalation could
// never arm in an ordinary plan session, because alternating tiers reset the
// streak (2026-09-26 audit, tenth round, auditor B).
//
// The escalation counted "consecutive failures of the session's chosen leg",
// and selectRoute noted the leg of EVERY request just before resolving it.
// recordFail then treated a leg change as a fresh slate (the count was cleared
// whenever the resolved leg differed from the noted one). In a plan session the
// tiers do not share a base URL — the main model sits on one remote and the
// haiku/subagent tiers on another — so requests interleave between legs
// constantly. A failing primary could accumulate one failure, hand over to a
// subagent turn on the other base, and lose the count before the second failure
// landed: autoEscalateAfterFails (2) was arithmetically unreachable, silently,
// with no escalation ever firing for the session.
//
// The count now belongs to the leg, so an interleaved request for a different
// tier — even one that SUCCEEDS — says nothing about the failing leg's health.

import (
	"testing"
)

func TestAlternatingTiersDoNotStarveTheEscalation(t *testing.T) {
	primary := proxyRoute{BaseURL: "https://flapping.example/v1", UpstreamModel: "primary-m", Label: "remote:primary", ContextWindow: 262144}
	small := proxyRoute{BaseURL: "https://small.example/v1", UpstreamModel: "small-m", Label: "remote:small", ContextWindow: 32768}
	big := proxyRoute{BaseURL: "https://big.example/v1", UpstreamModel: "big-m", Label: "remote:big", ContextWindow: 1000000}
	local := proxyRoute{BaseURL: "http://127.0.0.1:11434/v1", UpstreamModel: "local-m", Label: "local", ContextWindow: 32768}

	table := proxyRouteTable{
		Policy:      RouteAuto,
		Default:     local,
		ByModel:     map[string]proxyRoute{"primary-model": primary, "small-model": small},
		Fallbacks:   []proxyRoute{primary, big},
		breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
	}
	table.SessionID = "s-starved"

	// Turn 1: the primary leg's request fails.
	if r, _, fb := table.selectRoute("primary-model"); fb || r.BaseURL != primary.BaseURL {
		t.Fatalf("premise: the primary-model request resolved to %s (fallback=%v), want its own leg", r.BaseURL, fb)
	}
	table.escalations.recordFail(table.SessionID, primary.BaseURL)

	// Between them, the session serves its other tier — a haiku/subagent turn
	// on a different host, which is exactly how a plan session alternates.
	if r, _, fb := table.selectRoute("small-model"); fb || r.BaseURL != small.BaseURL {
		t.Fatalf("premise: the small-model request resolved to %s (fallback=%v)", r.BaseURL, fb)
	}
	// And that turn succeeded. A 200 from another host is not evidence the
	// primary recovered.
	table.escalations.recordOK(table.SessionID, small.BaseURL)

	// Turn 2: the primary fails again. These are two consecutive failures OF
	// THAT LEG.
	table.escalations.recordFail(table.SessionID, primary.BaseURL)

	if !table.escalations.escalatedFor(table.SessionID, primary.BaseURL) {
		t.Errorf("%d failures of %s did not arm the escalation because a request for another tier resolved in between and wiped the count — in a session whose tiers sit on different hosts the threshold is unreachable and the failover never fires", autoEscalateAfterFails, primary.BaseURL)
	}
	if r, _, fb := table.selectRoute("primary-model"); !fb || r.BaseURL != big.BaseURL {
		t.Errorf("the flapping leg's own requests must escalate to the strongest healthy secondary %s (got %s, fallback=%v)", big.BaseURL, r.BaseURL, fb)
	}

	// The interleaved tier is still where it belongs: a per-leg fix must not
	// turn one leg's escalation into a session-wide reroute.
	if r, _, fb := table.selectRoute("small-model"); fb || r.BaseURL != small.BaseURL {
		t.Errorf("the healthy small tier was rerouted to %s (fallback=%v) by an escalation armed on another leg", r.BaseURL, fb)
	}
}

// Control: a leg that really does recover — its own failure streak ends on its
// own success — must not escalate on one later failure, so this cannot be
// passed by counting every failure forever.
func TestALegsOwnSuccessStillEndsItsStreak(t *testing.T) {
	primary := proxyRoute{BaseURL: "https://flapping.example/v1", UpstreamModel: "primary-m", Label: "remote:primary", ContextWindow: 262144}
	big := proxyRoute{BaseURL: "https://big.example/v1", UpstreamModel: "big-m", Label: "remote:big", ContextWindow: 1000000}
	local := proxyRoute{BaseURL: "http://127.0.0.1:11434/v1", UpstreamModel: "local-m", Label: "local", ContextWindow: 32768}

	table := proxyRouteTable{
		Policy:      RouteAuto,
		Default:     local,
		ByModel:     map[string]proxyRoute{"primary-model": primary},
		Fallbacks:   []proxyRoute{primary, big},
		breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
	}
	table.SessionID = "s-recovered"

	table.escalations.recordFail(table.SessionID, primary.BaseURL)
	table.escalations.recordOK(table.SessionID, primary.BaseURL)
	table.escalations.recordFail(table.SessionID, primary.BaseURL)

	if table.escalations.escalatedFor(table.SessionID, primary.BaseURL) {
		t.Errorf("one failure after the leg's own success armed the escalation — a leg that answers is not failing, and the counter must still say so")
	}
	if esc := escalationFailsFor(table, table.SessionID, primary.BaseURL); esc >= autoEscalateAfterFails {
		t.Errorf("the leg's own success left its count at %d (arms at %d)", esc, autoEscalateAfterFails)
	}
}
