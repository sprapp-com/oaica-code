package launch

// route_health_probe_credential_integrity_test.go — the route health probe was
// unauthenticated, and any status below 500 was read as recovery (2026-09-26
// audit, tenth round, auditor B, HIGH).
//
// Two failures followed from the same two lines:
//
//   - The probe sent no credential, so a leg whose gateway requires a key
//     answered 401/403 — and `resp.StatusCode < 500` counted that as recovery.
//     recordOK zeroes BOTH openUntil and fails, so (i) an OPEN breaker was
//     closed again within one poll interval, bouncing the session back onto a
//     leg whose real requests were still failing, and (ii) worse, a leg that
//     failed real requests more slowly than the poll interval never
//     accumulated breakerFailsToOpen at all: the circuit never opened and the
//     documented failover never happened.
//   - A 429 from a shedding leg read healthy forever, for the same reason.
//
// The probe now carries the leg's own credential (resolveKey, re-read per
// probe exactly as the request path does), only 2xx counts as recovery, 401/403
// counts as nothing at all, and every other answer is a leg failure.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// probeLeg builds a one-leg table whose poll interval the test controls.
func probeLeg(t *testing.T, baseURL, key string) proxyRouteTable {
	t.Helper()
	return proxyRouteTable{
		Default: proxyRoute{Label: "remote:probe", BaseURL: baseURL, Key: key,
			UpstreamModel: "m", ContextWindow: 32768, Wire: "openai"},
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
}

// startPoll runs the health poll at interval until the test ends, returning a
// function that waits for at least one probe of the leg to have been answered.
func startPoll(t *testing.T, table proxyRouteTable, interval time.Duration, hits *atomic.Int64) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go table.startRouteHealthPoll(ctx, interval)
	waitForHits(t, hits, 1)
}

func waitForHits(t *testing.T, hits *atomic.Int64, n int64) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for hits.Load() < n {
		if time.Now().After(deadline) {
			t.Fatalf("the probe never reached the leg (%d hit(s) after 5s)", hits.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}

// keyedUpstream answers 401 unless the request carries want, and 2xx otherwise.
// want == "" means it always answers 200.
func keyedUpstream(t *testing.T, want string, status int, hits *atomic.Int64) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		if want != "" && r.Header.Get("Authorization") != "Bearer "+want {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		w.WriteHeader(status)
	}))
}

// (i) A leg that answers only 401 to the probe must not have its open circuit
// closed, nor its failure count erased.
func TestAnUnauthorizedProbeIsNotEvidenceOfRecovery(t *testing.T) {
	var hits atomic.Int64
	// No credential on the route: the probe cannot authenticate (the shape the
	// auditor proved, where the leg's real key comes from elsewhere).
	upstream := keyedUpstream(t, "the-real-key", http.StatusOK, &hits)
	defer upstream.Close()

	table := probeLeg(t, upstream.URL+"/v1", "")
	url := table.Default.BaseURL

	for i := 0; i < breakerFailsToOpen; i++ {
		table.breakers.recordFail(url)
	}
	if !table.breakers.open(url) {
		t.Fatalf("premise: %d failures did not open the breaker", breakerFailsToOpen)
	}
	failsWhileOpen := breakerFails(table, url)

	startPoll(t, table, 5*time.Millisecond, &hits)
	time.Sleep(60 * time.Millisecond)

	if !table.breakers.open(url) {
		t.Errorf("a 401 to an unauthenticated probe CLOSED the breaker on %s — the probe cannot tell a healthy leg from a key-protected one, and recordOK on that answer both retires the circuit and zeroes the failure count", url)
	}
	if got := breakerFails(table, url); got != failsWhileOpen {
		t.Errorf("the failure count moved from %d to %d on 401s alone — an unauthorized probe is evidence of nothing and must not erase real failures", failsWhileOpen, got)
	}
}

// (ii) The consequence that matters most: a leg failing real requests more
// slowly than the poll interval must still open its circuit. Before the fix the
// probe's 401-triggered recordOK reset the count every interval, so it never
// reached breakerFailsToOpen.
func TestAProbeCannotStarveTheBreakerOfRealFailures(t *testing.T) {
	var hits atomic.Int64
	upstream := keyedUpstream(t, "the-real-key", http.StatusOK, &hits)
	defer upstream.Close()

	table := probeLeg(t, upstream.URL+"/v1", "")
	url := table.Default.BaseURL

	startPoll(t, table, 5*time.Millisecond, &hits)

	// One real failure per probe: the leg is failing, slowly.
	for i := 0; i < breakerFailsToOpen; i++ {
		table.breakers.recordFail(url)
		before := hits.Load()
		waitForHits(t, &hits, before+1) // let a probe land between the failures
	}

	if !table.breakers.open(url) {
		t.Errorf("after %d real failures, each followed by a poll, the breaker on %s is still CLOSED — the probe's answer (401, no credential sent) is being recorded as a success, so fails never accumulates and the failover this breaker exists for never happens", breakerFailsToOpen, url)
	}
}

// The controls: the probe still recovers a leg that really is back — and it
// carries the leg's own credential to find that out — and still fails a leg
// that answers badly.
func TestTheProbeStillRecoversAHealthyKeyedLeg(t *testing.T) {
	var hits atomic.Int64
	upstream := keyedUpstream(t, "sk-realkey", http.StatusOK, &hits)
	defer upstream.Close()

	table := probeLeg(t, upstream.URL+"/v1", "sk-realkey")
	url := table.Default.BaseURL
	for i := 0; i < breakerFailsToOpen; i++ {
		table.breakers.recordFail(url)
	}
	if !table.breakers.open(url) {
		t.Fatalf("premise: the breaker did not open")
	}

	startPoll(t, table, 5*time.Millisecond, &hits)
	deadline := time.Now().Add(5 * time.Second)
	for table.breakers.open(url) {
		if time.Now().After(deadline) {
			t.Fatalf("a healthy, key-protected leg was never recovered — the probe reached it %d time(s) but its 2xx did not close the breaker (the probe must send the leg's own key: %s)", hits.Load(), url)
		}
		time.Sleep(2 * time.Millisecond)
	}
	if got := breakerFails(table, url); got != 0 {
		t.Errorf("a recovered leg still carries %d failure(s)", got)
	}
}

func TestTheProbeStillFailsALegThatAnswersBadly(t *testing.T) {
	var hits atomic.Int64
	upstream := keyedUpstream(t, "", http.StatusTooManyRequests, &hits)
	defer upstream.Close()

	table := probeLeg(t, upstream.URL+"/v1", "")
	url := table.Default.BaseURL

	startPoll(t, table, 5*time.Millisecond, &hits)
	deadline := time.Now().Add(5 * time.Second)
	for !table.breakers.open(url) {
		if time.Now().After(deadline) {
			t.Fatalf("a leg answering 429 to every probe never opened its breaker (%d hit(s)) — a rate-limited or shedding leg reads healthy forever", hits.Load())
		}
		time.Sleep(2 * time.Millisecond)
	}
}
