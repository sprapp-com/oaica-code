package launch

// round56_oversize_destination_fit_integrity_test.go — round 56's finding on
// the oversize crossover's NATIVE-passthrough branch (F56-6).
//
// oversizeSwap's ordinary branch judges the destination leg with the
// destination's OWN measured tokens-per-byte before crossing over (round 55),
// and refuses when even that leg cannot hold the request. The native-passthrough
// branch was written before there were anthropic-wire REMOTE legs to swap to:
// it skipped that check because the native leg carries no BaseURL and no
// ContextWindow of its own (api.anthropic.com enforces its real window, so a
// crossover is worth trying). When --oversize points at a PLAN ROW instead —
// an anthropic-wire remote leg, which DOES carry a BaseURL and a ContextWindow
// this launch probed from that leg's own /models — the exemption was inherited
// by a leg whose window is known: the request crossed over, the vendor
// answered a context overflow after the round trip, and the client paid for a
// prompt nothing there could hold.
//
// Only the native constant is exempt now (BaseURL empty, ContextWindow always
// 0), and a leg whose probed window is 0 — a probe that failed — is left to
// the worth-trying rule as before, since the check may only refuse a window the
// leg itself stated.

import "testing"

// r56OversizeTable is a small primary with the given native-passthrough
// oversize destination.
func r56OversizeTable(oversize proxyRoute) proxyRouteTable {
	return proxyRouteTable{Oversize: oversize, breakers: &routeBreakers{}}
}

func TestANativeCrossoverIsRefusedByTheDestinationsOwnCount(t *testing.T) {
	const est, margin = 990, 10
	primary := proxyRoute{BaseURL: "http://127.0.0.1:11434", ContextWindow: 1000}

	// The destination states a window, and its own measured ratio says this
	// request is past it.
	tooSmall := proxyRoute{BaseURL: "https://plan.example", ContextWindow: 200_000, NativePassthrough: true}
	planFor := func(proxyRoute) (int, int) { return 250_000, 1000 }
	if got, swapped := r56OversizeTable(tooSmall).oversizeSwap(primary, est, margin, planFor); swapped {
		t.Errorf("the request crossed over to %q, whose own probed window is %d and whose own tokens-per-byte read it as %d tokens: the round trip is spent on a prompt that leg cannot hold, and the client is billed for it — the ordinary branch has refused exactly this since round 55", got.BaseURL, tooSmall.ContextWindow, 250_000)
	}

	// The same destination, with numbers that fit.
	planFits := func(proxyRoute) (int, int) { return 20_000, 1000 }
	if _, swapped := r56OversizeTable(tooSmall).oversizeSwap(primary, est, margin, planFits); !swapped {
		t.Error("a destination whose own count says it holds the request was not crossed to")
	}

	// The caller's numbers stand when no per-leg planner is supplied.
	if _, swapped := r56OversizeTable(tooSmall).oversizeSwap(primary, 250_000, 1000, nil); swapped {
		t.Error("with no planner the destination is judged by the caller's numbers, and 250000 + 1000 does not fit a 200000 window")
	}
}

func TestAnUnprobedNativeDestinationIsStillWorthTrying(t *testing.T) {
	const est, margin = 990, 10
	primary := proxyRoute{BaseURL: "http://127.0.0.1:11434", ContextWindow: 1000}

	// A probed window of 0 is a probe that failed, not a small window: the
	// anthropic wire enforces its own real one, and this check may only refuse
	// a window the leg itself stated.
	unprobed := proxyRoute{BaseURL: "https://plan.example", ContextWindow: 0, NativePassthrough: true}
	if _, swapped := r56OversizeTable(unprobed).oversizeSwap(primary, est, margin, func(proxyRoute) (int, int) { return 999_999, 999_999 }); !swapped {
		t.Error("a native-wire destination that stated no window was refused: 0 means the probe failed, and the vendor's own window is the one that answers")
	}

	// The native constant itself — no BaseURL, no window — stays exempt, which
	// is the exemption this branch's doc gives a reason for.
	native := proxyRoute{NativePassthrough: true, ContextWindow: 1_000_000}
	if _, swapped := r56OversizeTable(native).oversizeSwap(primary, est, margin, func(proxyRoute) (int, int) { return 2_000_000, 0 }); !swapped {
		t.Error("a native passthrough leg with no BaseURL was refused by a count: api.anthropic.com is reached with the client's own credential and enforces its real window itself")
	}
}
