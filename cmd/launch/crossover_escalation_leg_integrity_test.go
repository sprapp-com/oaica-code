package launch

// crossover_escalation_leg_integrity_test.go — every oversize-crossover path
// fed the `auto` policy's escalation a leg it can never accept
// (2026-09-26 audit).
//
// The escalation state tracks ONE leg per session: the one selectRoute noted,
// and recordFail/recordOK return early for any other. oversizeSwap only ever
// returns an Oversize leg on a DIFFERENT base URL (its own doc), so the four
// escalation records on the crossover paths — two inside
// feedPassthroughRouteHealth's callers, two written inline in the
// OpenAI-translation path — could not fire, ever. They read as signals while
// the per-leg guard silently dropped them, and the comments around them
// promised escalation ("`auto` never escalated the session off it") for a leg
// that state does not represent.
//
// The fix is to stop feeding it, and to name the leg explicitly at each call
// site so the code says what it does. The behaviour is deliberately unchanged:
// escalating off a healthy primary because a request did not fit its window
// would move the session's ordinary traffic for autoEscalateHoldFor, for a
// reason that leg had nothing to do with. A flapping crossover leg is handled
// by its OWN breaker, which oversizeSwap consults before swapping — the next
// oversize request then fails visibly with "prompt is too long".

import (
	"testing"
)

// The rule the call sites rely on: a crossover onto another host may feed no
// escalation at all, and a crossover onto the same host may feed that host.
func TestACrossoverToAnotherHostFeedsNoEscalation(t *testing.T) {
	cases := []struct {
		name             string
		selected, served string
		want             string
	}{
		{"another host", "https://a.example/v1", "https://b.example/v1", ""},
		{"the same host", "https://a.example/v1", "https://a.example/v1", "https://a.example/v1"},
		{"the native leg (its BaseURL is empty)", "https://a.example/v1", "", ""},
	}
	for _, c := range cases {
		if got := crossoverEscalationLeg(c.selected, c.served); got != c.want {
			t.Errorf("%s: crossoverEscalationLeg(%q, %q) = %q, want %q", c.name, c.selected, c.served, got, c.want)
		}
	}
}

// And the outcome, on the state the policy reads: a crossover that fails must
// open ITS breaker and must not escalate the session off the leg selectRoute
// chose — even after far more failures than autoEscalateAfterFails.
func TestACrossoverFailureEscalatesNothing(t *testing.T) {
	selected := proxyRoute{Label: "primary", BaseURL: "https://a.example/v1", UpstreamModel: "m"}
	over := proxyRoute{Label: "oversize", BaseURL: "https://b.example/v1", UpstreamModel: "m-big", ContextWindow: 1 << 20}

	table := proxyRouteTable{
		breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
		Policy:      RouteAuto,
		Default:     selected,
	}
	// selectRoute is what notes the leg the escalation may hear about.
	table.escalations.noteLeg("sess-1", selected.BaseURL)

	leg := crossoverEscalationLeg(selected.BaseURL, over.BaseURL)
	for i := 0; i < 5*autoEscalateAfterFails; i++ {
		feedPassthroughRouteHealth(table, over, "sess-1",
			passthroughBreakerKey(over, true), leg, 0, false, false)
	}

	if !table.breakers.open(passthroughBreakerKey(over, true)) {
		t.Error("the crossover leg's own breaker never opened — the health feed stopped recording the leg that actually failed")
	}
	if table.escalations.escalatedFor("sess-1", selected.BaseURL) {
		t.Errorf("%d failures of a crossover leg on another host escalated the session off %s — escalation moves the session off the leg selectRoute chose, and a request that did not fit that leg's window is not evidence about its health", 5*autoEscalateAfterFails, selected.BaseURL)
	}
}

// The control: the ordinary path — the leg selectRoute chose — still escalates.
// Without this, "never escalate" would pass the test above.
func TestAChosenLegFailureStillEscalates(t *testing.T) {
	route := proxyRoute{Label: "primary", BaseURL: "https://a.example/v1", UpstreamModel: "m"}
	table := proxyRouteTable{
		breakers:    &routeBreakers{},
		escalations: &routeEscalations{},
		Policy:      RouteAuto,
		Default:     route,
	}
	table.escalations.noteLeg("sess-1", route.BaseURL)

	for i := 0; i < autoEscalateAfterFails; i++ {
		feedPassthroughRouteHealth(table, route, "sess-1",
			passthroughBreakerKey(route, false), route.BaseURL, 0, false, false)
	}

	if !table.escalations.escalatedFor("sess-1", route.BaseURL) {
		t.Errorf("%d consecutive failures of the leg selectRoute chose did not escalate the session — the escalation signal is dead, which is the failure mode this file exists to prevent", autoEscalateAfterFails)
	}
}
