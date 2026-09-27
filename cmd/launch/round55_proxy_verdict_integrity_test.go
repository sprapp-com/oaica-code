package launch

// round55_proxy_verdict_integrity_test.go — round 55's findings on the client
// proxy leg (leg 2: cmd/launch/anthropic_openai_proxy.go).
//
// One shape, four places: a verdict that should follow what the UPSTREAM said
// (and what the sibling leg says about the same body) instead followed which
// arm of this proxy answered, whose tokenizer was measured, or which credential
// the request went out under.
//
//  1. B1 — the oversize crossover judged the DESTINATION leg's window with the
//     SOURCE leg's measured tokens-per-byte. A session whose primary leg was
//     calibrated at the band's floor (0.1 tokens/byte) crossed to a leg whose
//     own encoder costs 0.25 tokens/byte, so the destination was handed a
//     prompt four times the size it was judged to be and the round trip was
//     spent on a request that could not fit: the check exists to refuse BEFORE
//     the hop, and it has to ask the leg that will serve the tokens.
//
//  2. B2 — a whole completion whose only payload is a call the upstream never
//     named, carrying arguments, was relayed with those arguments as text on
//     the non-stream arm and dropped on the adopt arm — where the round-45 pin
//     still expected a 502. The metered gateway counts such a document as
//     something on both of its arms (documentSaysSomething) and relays the
//     arguments, and this leg's own streaming path does the same, so the
//     verdict turned on which shape the upstream sent.
//
//  3. B3 — an upstream 401/403 on the passthrough wire was rewritten to a 502
//     for BOTH classes of leg. That is right for a plan row, whose key oaica
//     resolved and whose refusal means OUR key is wrong (round 54), and wrong
//     for a native claude/* leg, where the credential is the user's own:
//     native_anthropic_auth.go's documented contract is that Anthropic's 401
//     reaches the client untouched, "exactly what would happen running Claude
//     Code natively with that same stale token — the user re-runs `claude
//     /login` as normal". The 502 replaced that instruction with "the proxy is
//     broken".
//
//  4. B4 — a truncated call the upstream never named was dropped before the
//     flush could relay its arguments, so a turn whose ONLY output was that
//     fragment reached the client as an empty end_turn. A nameless fragment is
//     not an executable call (the reason a truncated NAMED call is dropped) —
//     it is the model's raw text, which the non-stream arm relays and so does
//     the metered gateway.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// round55Post sends a /v1/messages request and returns the status and body.
func round55Post(t *testing.T, proxy string, body []byte) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, proxy+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "client-token")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// round55Body is a request whose prompt is contentLen bytes of text.
func round55Body(t *testing.T, contentLen, maxTokens int, stream bool) []byte {
	t.Helper()
	b, err := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": maxTokens, "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": strings.Repeat("x", contentLen)}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// B1: the crossover must judge the destination leg with the DESTINATION's own
// tokens-per-byte, not the source's. Getting this wrong is both a spent round
// trip (a hop the destination cannot hold is taken anyway) and a false refusal
// (a hop it CAN hold is turned away), so the scenario below is built to see
// both: the primary leg is taught to cost 1.0 token/byte and the destination
// 0.1, and the same 60 KB probe is 60030 tokens by one count and 6003 by the
// other against the destination's 55000 window.
func TestTheOversizeDestinationIsJudgedByItsOwnTokens(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	// A leg's upstream that STATES whatever the test tells it to state for the
	// prompt — which is how a session's measured tokens-per-byte for THAT leg
	// is set to anything inside the calibration band (0.1 … 1.0).
	legUpstream := func(hits *int, stated *int) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			*hits++
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"c","object":"chat.completion","model":"m","choices":[{"index":0,"message":{"role":"assistant","content":"PONG"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%d,"completion_tokens":1,"total_tokens":%d}}`, *stated, *stated+1)
		}))
	}

	// Two independent wires: one for each phase of the scenario, so a failure
	// in an earlier phase cannot be mistaken for the probe's verdict.
	t.Run("a hop the destination can hold is taken", func(t *testing.T) {
		var baseStated, bigStated int
		baseHits, bigHits := 0, 0
		base := legUpstream(&baseHits, &baseStated)
		defer base.Close()
		over := legUpstream(&bigHits, &bigStated)
		defer over.Close()

		proxy := startProxyWithOversize(t, proxyRouteTable{
			Default:  routeAt(base.URL, "small-m", 30000),
			ByModel:  map[string]proxyRoute{"kat-awq": routeAt(base.URL, "small-m", 30000)},
			Oversize: routeAt(over.URL, "big-m", 55000),
		})

		// Phase 1 — the primary leg's own measurement: a 40 KB prompt it states
		// as 40000 tokens, the top of the calibration band.
		warm := round55Body(t, 40000, 16, false)
		baseStated = clientPromptBytes(warm)
		if code, body := round55Post(t, proxy, warm); code != 200 {
			t.Fatalf("phase 1 (measure the primary) answered %d: %s", code, body)
		}

		// Phase 2 — the destination leg's own measurement. This request cannot
		// fit the primary by its measured count (40030 tokens of a 30000
		// window) and is served on the oversize leg, where the upstream states
		// 0.1 token/byte for it. That turn is what teaches this session what
		// the DESTINATION costs.
		mid := round55Body(t, 40000, 16, false)
		baseStated = clientPromptBytes(mid)
		bigStated = clientPromptBytes(mid) / 10
		if code, body := round55Post(t, proxy, mid); code != 200 || bigHits != 1 {
			t.Fatalf("phase 2 (measure the destination) answered %d with %d hit(s) on the oversize leg, want 200 and 1: %s", code, bigHits, body)
		}
		bigHits = 0

		// Phase 3 — the probe. By the SOURCE's measured 1.0 token/byte it is
		// 60030 tokens with a 6000-token margin: 66030 of the destination's
		// 55000 window, i.e. no room at all, and refusing here turns away a
		// compaction call the destination holds with 48000 to spare. By the
		// destination's OWN measured 0.1 it is 6003 tokens with a 600 margin.
		probe := round55Body(t, 60000, 16, false)
		baseStated = clientPromptBytes(probe)
		bigStated = clientPromptBytes(probe) / 10
		code, body := round55Post(t, proxy, probe)

		if bigHits != 1 {
			t.Errorf("the oversize leg was hit %d time(s): the swap decision used the SOURCE leg's measured 1.0 tokens/byte (%d tokens + margin) against the destination's 55000 window, while that leg's own measured ratio is 0.1 (%d tokens) — one of the two halves of this judgement read the wrong leg's numbers. status=%d\n%s",
				bigHits, clientPromptBytes(probe), clientPromptBytes(probe)/10, code, body)
		}
		if code != http.StatusOK {
			t.Errorf("status=%d, want 200: the destination holds this request with ~48000 tokens of room by its own count\n%s", code, body)
		}
	})

	// The other direction: a hop the destination CANNOT hold must be refused
	// before the round trip carries the whole prompt to a leg that will 400 on
	// it. The destination here has never served this session, so its plan is
	// the default bytes/4 while the primary has measured the band's floor.
	t.Run("a hop the destination cannot hold is refused", func(t *testing.T) {
		var baseStated, bigStated int
		baseHits, bigHits := 0, 0
		base := legUpstream(&baseHits, &baseStated)
		defer base.Close()
		over := legUpstream(&bigHits, &bigStated)
		defer over.Close()

		proxy := startProxyWithOversize(t, proxyRouteTable{
			Default:  routeAt(base.URL, "small-m", 30000),
			ByModel:  map[string]proxyRoute{"kat-awq": routeAt(base.URL, "small-m", 30000)},
			Oversize: routeAt(over.URL, "big-m", 55000),
		})

		warm := round55Body(t, 40000, 16, false)
		baseStated = clientPromptBytes(warm) / 10
		if code, body := round55Post(t, proxy, warm); code != 200 {
			t.Fatalf("warm-up answered %d: %s", code, body)
		}
		bigHits = 0

		probe := round55Body(t, 400000, 16, false)
		probePB := clientPromptBytes(probe)
		baseStated = probePB / 10
		code, body := round55Post(t, proxy, probe)

		if bigHits != 0 {
			t.Errorf("the request was forwarded to the oversize leg (%d hit(s)) judged by the SOURCE leg's measured 0.1 tokens/byte: that leg's own default estimate is %d tokens against its 55000 window, so the hop was guaranteed to fail upstream and the round trip, with the whole prompt in it, was spent to find out. status=%d\n%s",
				bigHits, probePB/4*13/10, code, body)
		}
		if code != http.StatusBadRequest {
			t.Errorf("status=%d, want 400: the destination's own token count cannot fit its window, so the proxy must refuse before spending the round trip\n%s", code, body)
		}
	})
}

// routeAt is one leg of the table above, on the OpenAI wire.
func routeAt(baseURL, model string, window int) proxyRoute {
	return proxyRoute{Label: "remote:" + model, BaseURL: baseURL + "/v1", Key: "k",
		UpstreamModel: model, ContextWindow: window, Wire: "openai"}
}

// B2: the same whole-completion document, on both arms of this leg — and its
// arguments are the model's output, so both must relay them.
func TestANamelessCallsArgumentsInAWholeCompletionReachBothArms(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	doc := `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"","arguments":"{\"a\":1}"}}]}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(doc))
	}))
	defer up.Close()
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})

	nsCode, nsBody := round54PostMessage(t, proxy, "glm-5.3", false)
	sCode, sBody := round54PostMessage(t, proxy, "glm-5.3", true)

	if nsCode != http.StatusOK {
		t.Fatalf("premise: the non-stream arm answers %d for this document\n%s", nsCode, nsBody)
	}
	if sCode != http.StatusOK {
		t.Errorf("the byte-identical document answered on the stream arm is %d and %d on the non-stream arm: the verdict depends on which arm answered, while the metered gateway relays this document the same way on both of its own (2026-09-28 audit, round 55)\nstream body: %s",
			sCode, nsCode, sBody)
	}
	if strings.Contains(sBody, `"error"`) {
		t.Errorf("the stream arm answered with an error event for a turn that carries the model's own output:\n%s", sBody)
	}
	if !strings.Contains(sBody, `\"a\":1`) && !strings.Contains(sBody, `{"a":1}`) {
		t.Errorf("the arguments the upstream wrote did not reach the client as text on the stream arm: %s", sBody)
	}
}

// B3: the NATIVE leg's credential is the user's own, so its 401 is theirs to act
// on — the documented contract of native_anthropic_auth.go, and the difference
// between a 502 (the proxy is broken) and a 401 (run `claude /login`).
func TestTheNativeLegsOwn401IsRelayedToTheClient(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-user-key")

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`))
	}))
	defer up.Close()
	prev := nativeAnthropicUpstream
	nativeAnthropicUpstream = up.URL
	t.Cleanup(func() { nativeAnthropicUpstream = prev })

	body := []byte(`{"model":"claude-opus-5","max_tokens":16,"messages":[{"role":"user","content":"hi"}]}`)
	req := httptest.NewRequest(http.MethodPost, "/v1/messages", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	status, _, _ := nativeAnthropicPassthrough(rec, req, body, "sess-r55-native-401")

	if rec.Code != http.StatusUnauthorized {
		t.Errorf("the user's OWN credential was refused by the upstream and the client was told %d (the wrapper returned %d): native_anthropic_auth.go documents this case as Anthropic's own 401 passing through untouched, \"exactly what would happen running Claude Code natively with that same stale token\", and a 502 in its place tells the user the proxy is broken instead of sending them to `claude /login` (2026-09-28 audit, round 55)\n%s",
			rec.Code, status, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "authentication_error") {
		t.Errorf("the upstream's own authentication_error did not reach the client: %s", rec.Body.String())
	}

	// The control: the same upstream status under a credential oaica resolved
	// (a plan row) is still the 502 round 54 pinned — that refusal is OURS, and
	// an authentication_error there is what sends Claude Code into a login flow
	// over a key it never had.
	row := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`))
	}))
	defer row.Close()
	route := round54Route(row.URL, "glm-5.3", "anthropic")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	if code, body := round54PostMessage(t, proxy, "glm-5.3", false); code != http.StatusBadGateway {
		t.Errorf("a plan row's refused key reached the client as %d, want 502 — the credential that was refused there is the one oaica resolved\n%s", code, body)
	}
}

// B4: a truncated fragment the upstream never named is the model's raw output
// and reaches the client as text, on the streaming arm too.
func TestATruncatedUnnamedFragmentReachesTheClient(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	frames := []string{
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"","arguments":"{\"a\":"}}]},"finish_reason":null}]}`,
		`data: {"choices":[{"index":0,"delta":{},"finish_reason":"length"}]}`,
		`data: [DONE]`,
	}
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range frames {
			_, _ = w.Write([]byte(f + "\n\n"))
		}
	}))
	defer up.Close()
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", true)

	if code != http.StatusOK {
		t.Fatalf("the stream answered %d: %s", code, body)
	}
	if !strings.Contains(body, `{\"a\":`) && !strings.Contains(body, `{"a":`) {
		t.Errorf("the fragment the model was still writing did not reach the client at all: it is the turn's only output, it has no name (so it is never an executable call), and both the non-stream arm of this leg and the metered gateway relay it as text (2026-09-28 audit, round 55)\n%s", body)
	}
}
