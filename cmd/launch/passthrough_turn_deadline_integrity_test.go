package launch

// passthrough_turn_deadline_integrity_test.go — the Anthropic passthrough leg
// carried its own 10-minute response deadline, and a real completion that ran
// past it was cut off mid-stream, relayed incomplete, and counted against the
// leg: feedPassthroughRouteHealth reads an incomplete body after a 200 as a
// dead turn (correctly), so three long-but-healthy turns opened the circuit
// and the failover took the session off a working backend (2026-09-26 audit).
//
// The deadline cannot be the fix it was documented as. Its stated reason was
// that the child's own API_TIMEOUT_MS=600000 fires first, so this one only
// covers "the gap between the child giving up and its request being torn
// down" — but when the child gives up it CANCELS the request, and a cancelled
// request is already told apart from a dead leg (clientGone, on
// r.Context().Err()). The deadline adds nothing on that side and is actively
// wrong on the other one: its clock starts when this proxy receives the
// request and is not the child's clock, so a slower child, a retry loop, or a
// user who raised API_TIMEOUT_MS puts a healthy turn past it.
//
// What this leg uses instead is the same bounded transport every other
// proxied call in this package uses (proxyUpstreamClient): connection setup
// is bounded, the response is bounded only by the caller's context — which is
// exactly the "the client hung up" signal the health feed already reads.

import (
	"bytes"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// passthroughSlowFrameHold is long enough that a response deadline of any
// plausible size in a test would fire, and far too short to be the 10 minutes
// the real deadline was — the assertion here is not "10 minutes did not
// elapse", it is "this leg imposes NO response deadline of its own".
const passthroughSlowFrameHold = 250 * time.Millisecond

func TestAPassthroughLegImposesNoResponseDeadline(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	old := proxyUpstreamClient
	t.Cleanup(func() { proxyUpstreamClient = old })

	// The fake transport is the observation: this leg must go through the
	// shared proxied-upstream client, so a leg that builds its own
	// `&http.Client{Timeout: …}` — the shape this test is here to keep out —
	// never reaches it and fails below as "bypassed or cut the stream short".
	// (The deadline itself cannot be probed from in here: net/http implements
	// Client.Timeout with an internal timer rather than a context deadline, so
	// req.Context().Deadline() is empty even on a request that has one.)
	proxyUpstreamClient = &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		pr, pw := io.Pipe()
		go func() {
			defer pw.Close()
			// Two frames with a hold between them: the shape of a streaming
			// completion. A deadline long enough to matter for a real turn
			// would swallow the second frame, not the first.
			_, _ = pw.Write([]byte("event: message_start\n\n"))
			time.Sleep(passthroughSlowFrameHold)
			_, _ = pw.Write([]byte("event: message_stop\n\n"))
		}()
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       pr,
			Request:    req,
		}, nil
	})}

	body := []byte(`{"model":"fable","max_tokens":64,"stream":true,"messages":[{"role":"user","content":"hello"}]}`)
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))

	status, relayed := anthropicPassthrough(rec, req, body, "http://anthropic.example", "x-api-key", "sk-ant-notarealkey", "sess-deadline", true)

	if status != http.StatusOK || !relayed {
		t.Fatalf("anthropicPassthrough = (%d, relayed=%t), want (200, true) — this leg's own client was bypassed or it cut the stream short", status, relayed)
	}
	if got := rec.Body.String(); !strings.Contains(got, "message_start") || !strings.Contains(got, "message_stop") {
		t.Errorf("relayed body = %q, want both frames: the second one is what a deadline swallows, and a partial body after a 200 is indistinguishable from a leg that cannot finish a turn", got)
	}
}

// The same property on the shared client itself, so a deadline reintroduced
// there — where it would truncate every proxied streaming call, not just this
// leg — is caught by this test as well.
func TestTheSharedUpstreamClientBoundsSetupNotTheResponse(t *testing.T) {
	if proxyUpstreamClient.Timeout != 0 {
		t.Errorf("proxyUpstreamClient.Timeout = %s, want 0: this client bounds connection setup through its transport; a response deadline here truncates every streaming call it carries (the 5-minute one it used to have was removed for exactly that, review of 2026-08-26)", proxyUpstreamClient.Timeout)
	}
	tr, ok := proxyUpstreamClient.Transport.(*http.Transport)
	if !ok {
		t.Fatalf("proxyUpstreamClient.Transport is %T, want *http.Transport — the bounded setup (dial timeout, idle connections) is the half that must be KEPT", proxyUpstreamClient.Transport)
	}
	if tr.DialContext == nil {
		t.Error("proxyUpstreamClient's transport has no DialContext: an unbounded dial is a hang, not a long turn — the fix for a truncated response must not remove the bound on connection setup")
	}
}
