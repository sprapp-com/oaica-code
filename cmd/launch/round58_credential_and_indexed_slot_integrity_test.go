package launch

// round58_credential_and_indexed_slot_integrity_test.go — round 58 on the
// client-proxy leg: three credential findings and the indexed half of the wire
// the gateway leg was fixed for.
//
//   - F58-L2-1: the `anthropic` catalog row declares api_key_env
//     ANTHROPIC_API_KEY, so a user with that variable set — the ordinary way
//     Claude Code is authenticated — has a key for the row. The passthrough
//     reported it as oaica's own because SOME key had been resolved, so
//     Anthropic's 401 under the user's own key was rewritten as a 502 and the
//     user was sent into a retry loop instead of a `claude /login`. Round 57
//     gave the row's KEEYLESS spelling the user's verdict; the keyed spelling
//     is the same credential on the same host and owed the same one.
//   - F58-L2-2: a credential injected as "Bearer <token>" was redacted by exact
//     match, so an upstream that names the token it refused, bare, had its
//     words handed to the client with the token in them.
//   - F58-L2-3: the startup context-window probe read route.Key alone, which is
//     empty for every spelling of that row (its key lives in api_key_env, or is
//     the user's own OAuth session), so the probe 401'd and the launch ran with
//     no real window — no CLAUDE_CODE_MAX_CONTEXT_TOKENS hint and no
//     context-fit clamp ceiling.
//   - F58-L3-2 (this leg's half): an index STATED and repeated for the turn's
//     next call folded that call into the first one's accumulator — round 36's
//     B-F2 with the index present instead of absent — dropping its id and
//     concatenating its arguments onto the first's. Both arms that hold a whole
//     list keep the two calls apart, and so does the gateway leg.
//
// The controls are pinned beside each: a key oaica stored for the row is still
// its own and still earns the 502, a short placeholder is still left alone, a
// keyless row on any other vendor still probes bare, and the restatement round
// 56 pinned is still one call.
//
// One reported candidate is NOT fixed, deliberately: the armed entitlement gate
// discriminates "anthropic-wire remote" from "native claude/* leg" by
// BaseURL != "", so the `anthropic` catalog row — api.anthropic.com under the
// user's own credential, which the same comments call the class that stays
// ungated — is gated where a byte-identical native leg is not. Nothing
// observable turns on it today: the gate is inert unless a deployment sets
// OAICA_ENTITLEMENT_CHECK=1 and registers a real check, and that check is handed
// the route LABEL ("remote:anthropic") and the model, so it can exempt the row
// itself. Moving the discriminator now would hide a remote row from a check
// that may need to see it, against no verified harm.

import (
	"bytes"
	"io"
	"net/http"
	"strings"
	"testing"
)

// --- F58-L2-1 ---------------------------------------------------------------

// TestAKeyTheUsersOwnVariableNamesIsTheUsersOwnJudges the verdict on the row
// shape the catalog actually produces.
func TestAKeyTheUsersOwnVariableNamesIsTheUsersOwn(t *testing.T) {
	// The catalog row: api_key_env resolves to the user's own variable.
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-user-key")
	userOwn := proxyRoute{BaseURL: "https://api.anthropic.com/v1", Wire: "anthropic", KeyEnv: "ANTHROPIC_API_KEY", UpstreamModel: "claude-opus-5"}
	upstream, headerName, headerValue, ours, ok := userOwn.anthropicPassthroughTarget()
	if !ok {
		t.Fatal("the row resolved no target")
	}
	if headerValue != "sk-ant-user-key" || headerName != "x-api-key" {
		t.Fatalf("the premise moved: the row sent %s=%q, want the user's own key", headerName, headerValue)
	}
	if upstream != "https://api.anthropic.com/v1/messages" {
		t.Errorf("upstream = %q", upstream)
	}
	if ours {
		t.Errorf("a row whose credential IS %s reported it as oaica's own\nthe 401 under it reaches Claude Code as a 502 api_error — a broken proxy and a retried prompt — while the same key on the native claude/* leg and on this row's keyless spelling relays the 401 and its `claude /login` instruction (2026-09-28 audit, round 58, F58-L2-1)", nativeAnthropicKeyEnv)
	}

	// The same row when the variable is NOT set: resolveKey falls through to the
	// key oaica stored for it, which IS the row's own credential.
	t.Setenv("ANTHROPIC_API_KEY", "")
	stored := proxyRoute{BaseURL: "https://api.anthropic.com/v1", Wire: "anthropic", Key: "sk-oaica-stored", KeyEnv: "ANTHROPIC_API_KEY", UpstreamModel: "claude-opus-5"}
	if _, _, _, ours, ok := stored.anthropicPassthroughTarget(); !ok || !ours {
		t.Errorf("a row running on the key oaica stored for it reported credentialIsOurs=%v (ok=%v), want oaica's own — this is the case the 502 exists for", ours, ok)
	}

	// A row with no key env at all: unchanged.
	raw := proxyRoute{BaseURL: "https://plan.example/v1", Wire: "anthropic", Key: "sk-plan", UpstreamModel: "m"}
	if _, _, _, ours, ok := raw.anthropicPassthroughTarget(); !ok || !ours {
		t.Errorf("a row with no key env reported credentialIsOurs=%v (ok=%v)", ours, ok)
	}
}

// TestTheUsersOwnKeyRefusalIsRelayedOnTheKeyedRow is F58-L2-1 end to end: the
// verdict is only interesting for what the client is handed.
func TestTheUsersOwnKeyRefusalIsRelayedOnTheKeyedRow(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	t.Setenv("ANTHROPIC_API_KEY", "sk-ant-user-key")

	oldClient := proxyUpstreamClient
	proxyUpstreamClient = &http.Client{Transport: proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{
			StatusCode: http.StatusUnauthorized,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(bytes.NewReader([]byte(r57AnthropicAuthError))),
			Request:    req,
		}, nil
	})}
	t.Cleanup(func() { proxyUpstreamClient = oldClient })

	row := routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: "anthropic", BaseURL: "https://api.anthropic.com/v1", TokenEnv: "ANTHROPIC_API_KEY",
		UpstreamModel: "claude-opus-5", Wire: "anthropic",
	}})
	if row.KeyEnv != nativeAnthropicKeyEnv || row.resolveKey() != "sk-ant-user-key" {
		t.Fatalf("the premise moved: KeyEnv=%q resolveKey=%q — this test cannot judge the verdict", row.KeyEnv, row.resolveKey())
	}
	proxy := startProxyWithOversize(t, proxyRouteTable{Default: row})
	code, body, _ := postEntitlementTestMessage(t, proxy, row.UpstreamModel)

	if code != http.StatusUnauthorized {
		t.Errorf("api.anthropic.com under the user's own %s answered %d, want the upstream's own 401\nbody: %s\nthe credential is the user's, so its refusal means the user re-runs `claude /login` (2026-09-28 audit, round 58, F58-L2-1; round 57's F57-L2-1 for the keyless spelling)", nativeAnthropicKeyEnv, code, body)
	}
	if strings.Contains(body, "api_error") {
		t.Errorf("the user's own refused key was rewritten as an oaica-side fault:\n%s", body)
	}
}

// --- F58-L2-2 ---------------------------------------------------------------

func TestABareEchoOfASchemePrefixedCredentialIsRedacted(t *testing.T) {
	const token = "eyJhbGciOiJIUzI1NiJ9.body-signature-1234"
	for _, text := range []string{
		"invalid token " + token,
		"the credential " + token + " was rejected",
		"Authorization: Bearer " + token,
	} {
		got := redactSecret(text, "Bearer "+token)
		if strings.Contains(got, token) {
			t.Errorf("redacting %q left the credential in %q\nthe secret is the token, not the scheme: an upstream that names it bare was echoed to the client verbatim (2026-09-28 audit, round 58, F58-L2-2)", text, got)
		}
	}

	// The controls: what the guard is for.
	if got := redactSecret("nothing to hide here", "Bearer "+token); got != "nothing to hide here" {
		t.Errorf("unrelated text was rewritten: %q", got)
	}
	if got := redactSecret("x none y", "Bearer none"); strings.Contains(got, "REDACTED") {
		t.Errorf("a short placeholder was cut out of ordinary prose: %q", got)
	}
	if got := redactSecret("key sk-live-abcdefgh", "sk-live-abcdefgh"); strings.Contains(got, "sk-live-abcdefgh") {
		t.Errorf("a bare key stopped being redacted: %q", got)
	}
}

// --- F58-L2-3 ---------------------------------------------------------------

// TestTheContextWindowProbeSendsTheCredentialTheRowSends is F58-L2-3: the probe
// is judged by the window it gets back from a host that requires the credential
// the row's own requests carry.
func TestTheContextWindowProbeSendsTheCredentialTheRowSends(t *testing.T) {
	const windowBody = `{"data":[{"id":"claude-opus-5","context_length":200000}]}`

	serve := func(t *testing.T, want func(*http.Request) bool, seen *http.Header) {
		t.Helper()
		rt := proxyRoundTripFunc(func(req *http.Request) (*http.Response, error) {
			if seen != nil && *seen == nil {
				*seen = req.Header.Clone()
			}
			if want != nil && !want(req) {
				return &http.Response{StatusCode: http.StatusUnauthorized,
					Header:  http.Header{"Content-Type": []string{"application/json"}},
					Body:    io.NopCloser(strings.NewReader(`{"type":"error","error":{"type":"authentication_error","message":"invalid x-api-key"}}`)),
					Request: req}, nil
			}
			return &http.Response{StatusCode: http.StatusOK,
				Header:  http.Header{"Content-Type": []string{"application/json"}},
				Body:    io.NopCloser(strings.NewReader(windowBody)),
				Request: req}, nil
		})
		old := http.DefaultClient
		http.DefaultClient = &http.Client{Transport: rt}
		t.Cleanup(func() { http.DefaultClient = old })
	}

	t.Run("the key the row names", func(t *testing.T) {
		setLaunchTestHome(t, t.TempDir())
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-user-key")
		var seen http.Header
		serve(t, func(req *http.Request) bool { return req.Header.Get("x-api-key") == "sk-ant-user-key" }, &seen)
		row := proxyRoute{BaseURL: "https://api.anthropic.com/v1", Wire: "anthropic", KeyEnv: "ANTHROPIC_API_KEY", UpstreamModel: "claude-opus-5"}
		if got := defaultRemoteContextWindow(row); got != 200000 {
			t.Errorf("the window came back as %d, want 200000\nthis row's requests carry the key its api_key_env names; the probe read route.Key alone, which is empty for every spelling of it, so it 401'd and the launch ran with no window (2026-09-28 audit, round 58, F58-L2-3)\nheaders sent: %v", got, seen)
		}
	})

	t.Run("the user's own session", func(t *testing.T) {
		setLaunchTestHome(t, t.TempDir())
		t.Setenv("ANTHROPIC_API_KEY", "")
		old := readClaudeOAuthAccessTokenFn
		readClaudeOAuthAccessTokenFn = func() (string, error) { return "sk-ant-oat-user-token", nil }
		t.Cleanup(func() { readClaudeOAuthAccessTokenFn = old })
		seen := http.Header{}
		serve(t, func(req *http.Request) bool {
			return strings.Contains(req.Header.Get("Authorization"), "sk-ant-oat-user-token")
		}, &seen)
		row := proxyRoute{BaseURL: "https://api.anthropic.com/v1", Wire: "anthropic", UpstreamModel: "claude-opus-5"}
		if got := defaultRemoteContextWindow(row); got != 200000 {
			t.Errorf("the keyless api.anthropic.com row probed to %d, want 200000\nthe passthrough sends the user's own OAuth session for this row, and its /models list is fetched with the same credential; the probe sent none (2026-09-28 audit, round 58, F58-L2-3)\nheaders sent: %v", got, seen)
		}
	})

	// The control: a keyless row on any OTHER vendor still goes out bare. The
	// native credential is valid for api.anthropic.com and for nothing else.
	t.Run("a keyless foreign row still probes bare", func(t *testing.T) {
		setLaunchTestHome(t, t.TempDir())
		t.Setenv("ANTHROPIC_API_KEY", "sk-ant-user-key")
		var seen http.Header
		serve(t, nil, &seen)
		row := proxyRoute{BaseURL: "https://plan.example/v1", Wire: "anthropic", UpstreamModel: "glm-5.3"}
		_ = defaultRemoteContextWindow(row)
		if seen.Get("x-api-key") != "" || seen.Get("Authorization") != "" {
			t.Errorf("a keyless row on another vendor was probed with %v: the user's Anthropic credential is valid on api.anthropic.com alone", seen)
		}
	})
}

// --- F58-L3-2, this leg's half ----------------------------------------------

// r58L2Calls posts one frame wire and returns this call's blocks in order.
func r58L2Calls(t *testing.T, frames ...string) []map[string]any {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	up := streamUpstream(t, strings.Join(frames, "\n\n")+"\n\n", false)
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", true)
	if code != http.StatusOK {
		t.Fatalf("status %d\n%s", code, body)
	}
	return sseToolUseBlocks(t, body)
}

const r58L2Fin = `data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`

func TestASlotStatedForTheTurnsSecondCallIsTheSecondCallOnThisLeg(t *testing.T) {
	for _, tc := range []struct {
		name   string
		frames []string
	}{
		{
			"a second id at one index",
			[]string{
				`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"a","function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
				`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"b","function":{"name":"Read"}}]}}]}`,
				`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"p\":1}"}}]}}]}`,
			},
		},
		{
			"a second name at one index",
			[]string{
				`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash","arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
				`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"Read"}}]}}]}`,
				`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"function":{"arguments":"{\"p\":1}"}}]}}]}`,
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			blocks := r58L2Calls(t, append(tc.frames, r58L2Fin, `data: [DONE]`)...)
			if len(blocks) != 2 {
				t.Fatalf("the turn's two calls reached the client as %d block(s):\n%v\nthe index identifies the slot the upstream is writing, and a call it states that slot for twice is the turn's next call: folded, the second call's id was discarded and its arguments concatenated onto the first's — round 36's B-F2 for the vendor that writes index 0 for every call, which keying by id could not cure because the index it repeats is STATED, not absent. The gateway leg's toolKey splits the same wire, and both arms that hold a whole list keep the two calls apart (2026-09-28 audit, round 58, F58-L3-2)", len(blocks), blocks)
			}
			if len(blocks) == 2 {
				if got := blocks[0]["input"]; !r58IsArgObject(got, "cmd", "ls") {
					t.Errorf("the first call's input is %v, want {\"cmd\":\"ls\"}", got)
				}
				if got := blocks[1]["input"]; !r58IsArgObject(got, "p", float64(1)) {
					t.Errorf("the second call's input is %v, want {\"p\":1}", got)
				}
				if blocks[0]["name"] != "Bash" || blocks[1]["name"] != "Read" {
					t.Errorf("the calls are named %v / %v, want Bash / Read", blocks[0]["name"], blocks[1]["name"])
				}
				if blocks[0]["id"] == blocks[1]["id"] {
					t.Errorf("both calls reached the client under one id %q: one tool_result answers two calls", blocks[0]["id"])
				}
			}
		})
	}
}

// r58IsArgObject reports whether input is exactly one key with the given value.
func r58IsArgObject(input any, key string, want any) bool {
	m, ok := input.(map[string]any)
	if !ok || len(m) != 1 {
		return false
	}
	v, ok := m[key]
	if !ok {
		return false
	}
	switch w := want.(type) {
	case string:
		s, ok := v.(string)
		return ok && s == w
	case float64:
		f, ok := v.(float64)
		return ok && f == w
	}
	return false
}

// The controls on this leg: the restatement round 56 pinned stays one call, and
// one call's fragments stated name-then-id-and-arguments stay one call.
func TestTheIndexedRestatementStaysOneCallOnThisLeg(t *testing.T) {
	blocks := r58L2Calls(t,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1","function":{"name":"Bash","arguments":"{\"a\":1}"}}]}}]}`,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_1"}]}}]}`,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"a\":1}"}}]}}]}`,
		r58L2Fin, `data: [DONE]`)
	if len(blocks) != 1 {
		t.Errorf("the call the upstream listed again at its own slot reached the client as %d blocks (round 56's F1):\n%v", len(blocks), blocks)
	}
	if len(blocks) == 1 && !r58IsArgObject(blocks[0]["input"], "a", float64(1)) {
		t.Errorf("the restated call's input is %v, want {\"a\":1}", blocks[0]["input"])
	}

	blocks = r58L2Calls(t,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash"}}]}}]}`,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"arguments":"{\"cmd\":\"ls\"}"}}]}}]}`,
		r58L2Fin, `data: [DONE]`)
	if len(blocks) != 1 || blocks[0]["id"] != "call_9" {
		t.Errorf("one call stated as name-then-id-and-arguments reached the client as %v", blocks)
	}
}
