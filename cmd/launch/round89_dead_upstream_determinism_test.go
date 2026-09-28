package launch

// round89_dead_upstream_determinism_test.go — leg 2 (2026-09-29 audit, round
// 89).
//
// Five tests in this package want an upstream that fails to connect, and they
// get one from deadUpstreamAddress. Until this round it worked by binding port
// 0, closing the listener, dialling the address to confirm it refuses, and
// handing that address out — with a comment recording that the window cannot be
// closed, because the port goes straight back to the ephemeral pool and any of
// this package's httptest servers can be given it a moment later. The 2026-09-26
// audit saw that once (TestADeadUpstreamStillFailsTheTranslatedLeg, a 4xx from
// somebody else's server counting as nothing).
//
// Measured this round, at HEAD ffc099225, the window is not rare: with every
// httptest listener in the package to compete with, the failure reproduced at
// -count=1 and -count=3 and in a full-package run —
//
//	full run: TestADeadUpstreamStillFailsAPassthroughLeg — "a refused
//	          passthrough connection did not count against
//	          http://127.0.0.1:46379" (a 4xx from a live sibling, exactly the
//	          symptom the 2026-09-26 note describes)
//	-count=20: the same test — "premise: a refused passthrough upstream
//	          answered 200" (a 200 from a live sibling)
//
// Both symptoms are one cause: the address answers from something that is not
// the upstream it claims to be. The fix is to stop asking for an ephemeral port
// at all. deadUpstreamPort is below the ephemeral range and below 1024, so no
// listener in this process can be handed it and an unprivileged test cannot bind
// it by asking; what remains of the old construction is the dial check, which is
// now a premise assertion rather than a search.
//
// The pin below holds the property and the mechanism behind it — every check
// asked of the port the address actually names, because a version of it written
// against the constant passed the very closed listener it exists to replace.

import (
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"
)

// deadUpstreamPort is a port nothing listens on and nothing in this package can
// be handed: below the ephemeral range (so no sibling :0 bind can get it) and
// below 1024 (so an unprivileged test cannot bind it by asking).
const deadUpstreamPort = 1

// deadUpstreamProblem reports why `addr` might not stay dead, or "" when it is
// the address deadUpstreamAddress promises: refused, unwritable by this
// process, and outside the range the kernel hands out.
func deadUpstreamProblem(addr string) string {
	u, err := url.Parse("http://" + addr)
	if err != nil {
		return fmt.Sprintf("%q is not an address: %v", addr, err)
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return fmt.Sprintf("%q names no port (%v)", addr, err)
	}
	if low := launchEphemeralLow(); port >= low {
		return fmt.Sprintf("port %d is inside the ephemeral range (>= %d), so a listener in this process can be given it once it comes free", port, low)
	}
	if port >= 1024 {
		return fmt.Sprintf("port %d is unprivileged, so a test could bind it", port)
	}
	conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
	if err == nil {
		conn.Close()
		return fmt.Sprintf("something is already listening on %s", addr)
	}
	return ""
}

// launchEphemeralLow is the bottom of the range the kernel allocates ephemeral
// ports from; a port below it cannot be handed to a `:0` bind. Falls back to
// 1024 — the privileged boundary, stricter than any ephemeral range — when the
// range cannot be read.
func launchEphemeralLow() int {
	b, err := os.ReadFile("/proc/sys/net/ipv4/ip_local_port_range")
	if err != nil {
		return 1024
	}
	f := strings.Fields(string(b))
	if len(f) < 2 {
		return 1024
	}
	low, err := strconv.Atoi(f[0])
	if err != nil || low < 1024 {
		return 1024
	}
	return low
}

// TestTheDeadUpstreamOfThisPackageStaysDead is the pin. It holds the property
// deadUpstreamAddress promises, and it holds the mechanism: a sibling `:0`
// listener really does get its port from the range the dead one is below.
func TestTheDeadUpstreamOfThisPackageStaysDead(t *testing.T) {
	addr := deadUpstreamAddress(t)
	if p := deadUpstreamProblem(addr); p != "" {
		t.Errorf("the address this package calls a dead upstream is not reliably dead: %s (2026-09-29 audit, round 89)", p)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := l.Addr().(*net.TCPAddr).Port
	l.Close()
	if port < launchEphemeralLow() {
		t.Errorf("a sibling listener got port %d, below the ephemeral range %d — the range this pin reasons about is not the one this kernel allocates from", port, launchEphemeralLow())
	}

	// And the property the callers rest on: a request against it reaches
	// nothing, so the failure they classify is the leg's, not a sibling's.
	if _, err := net.DialTimeout("tcp", addr, 2*time.Second); err == nil {
		t.Errorf("something answered %s — the tests that use it classify what they get as the upstream being dead, and a live sibling made that classification wrong twice this round (2026-09-29 audit, round 89)", addr)
	}
}
