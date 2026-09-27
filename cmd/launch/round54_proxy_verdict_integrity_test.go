package launch

// round54_proxy_verdict_integrity_test.go — round 54, the client proxy leg.
//
// Four findings, one shape: the verdict this leg gives the client must depend on
// what the upstream SAID, not on which wire or which arm of this leg answered
// it, and must agree with the two sibling legs.
//
//  1. A1 — a whole completion whose only payload is a call the upstream never
//     named, carrying arguments, was refused 502 as an empty completion while
//     the byte-identical STREAM answered it 200 with the arguments as prose.
//     Both arms of the metered gateway relay such a fragment as text, and so
//     does this leg's own streaming path; the non-stream arm alone dropped it.
//  2. A2 — the route health probe asked <base>/models for every leg, ignoring a
//     row's declared models_path, so a leg serving completions fine answered
//     404 to its own health probe and had its breaker opened (selectRoute then
//     retires it from fallback and oversize selection).
//  3. A3 — mapToolChoice switched on the raw type string, so {"type":"Any"}
//     and {"type":"Tool","name":…} fell to the default arm and stated nothing,
//     while the converter and the metered gateway both read the type
//     case-insensitively and force a call.
//  4. A4 — an upstream 401/403 on the passthrough wire was relayed to the
//     client as the vendor's own authentication_error, sending Claude Code into
//     its login flow; the translated path maps that same refusal to a 502
//     because the credential that was refused is OURS.

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/ollama/ollama/anthropic"
)

// round54PostMessage sends a /v1/messages request to the proxy and returns the
// status and body, streaming or not.
func round54PostMessage(t *testing.T, proxyURL, model string, stream bool) (int, string) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": model, "max_tokens": 16, "stream": stream,
		"messages": []map[string]any{{"role": "user", "content": "ping"}},
	})
	req, err := http.NewRequest(http.MethodPost, proxyURL+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("x-api-key", "proxy-client-token")
	req.Header.Set("anthropic-version", "2023-06-01")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, string(b)
}

// round54Route builds a user-remote route pointing at baseURL on the given wire.
func round54Route(baseURL, model, wire string) proxyRoute {
	name := "zai"
	if wire == "anthropic" {
		name = "zai-coding-plan"
	}
	return routeFor(launchEndpoint{Source: sourceUserRemote, RemoteEndpoint: RemoteEndpoint{
		Name: name, BaseURL: baseURL, Token: "sk-ours", UpstreamModel: model, Wire: wire,
	}})
}

// A1: the same document, answered by both arms of this leg, must relay the
// unnamed fragment's arguments to the client as text.
func TestANamelessCallsArgumentsReachTheClientAsTextOnBothPaths(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	nonStream := `{"id":"c","object":"chat.completion","choices":[{"index":0,"finish_reason":"tool_calls","message":{"role":"assistant","tool_calls":[{"id":"call_1","type":"function","function":{"name":"","arguments":"{\"a\":1}"}}]}}],"usage":{"prompt_tokens":7,"completion_tokens":3}}`
	stream := []string{
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{"role":"assistant","tool_calls":[{"index":0,"id":"call_1","type":"function","function":{"name":"","arguments":"{\"a\":1}"}}]},"finish_reason":null}]}`,
		`data: {"id":"c","object":"chat.completion.chunk","choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
		`data: [DONE]`,
	}

	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(nonStream))
	}))
	defer up.Close()
	route := round54Route(up.URL, "glm-5.3", "openai")
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", false)

	if code != http.StatusOK {
		t.Fatalf("a whole completion whose only payload is a NAMELESS call with arguments was answered %d, want 200 with those arguments as text — the gateway leg relays the identical document that way on both of its arms, and this leg's own streaming path does too, so refusing it here made the verdict depend on which shape the upstream sent\nbody: %s", code, body)
	}
	if !strings.Contains(body, `\"a\":1`) && !strings.Contains(body, `{"a":1}`) {
		t.Errorf("the arguments the upstream wrote and billed reached the client as %s — they are the model's only output in this turn, and the streaming path relays them as a text block", body)
	}
	if strings.Contains(body, `"type":"tool_use"`) {
		t.Errorf("the client was handed a tool_use block for a call the upstream never named — content_block_start is the only event that carries a name, so Claude Code reports such a block as pending forever and can never run it\nbody: %s", body)
	}

	ups := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		for _, f := range stream {
			_, _ = w.Write([]byte(f + "\n\n"))
		}
	}))
	defer ups.Close()
	routeS := round54Route(ups.URL, "glm-5.3", "openai")
	proxyS := startEntitlementTestProxy(t, routeS, map[string]proxyRoute{"glm-5.3": routeS})
	codeS, bodyS := round54PostMessage(t, proxyS, "glm-5.3", true)

	if codeS != http.StatusOK {
		t.Fatalf("the streamed twin of that document answered %d, want 200", codeS)
	}
	if !strings.Contains(bodyS, `\"a\":1`) {
		t.Errorf("the streaming arm did not relay the unnamed fragment's arguments as text\nbody: %s", bodyS)
	}
}

// A2: the health probe must ask the URL the leg's model list actually lives at.
func TestTheHealthProbeAsksTheLegsDeclaredModelsURL(t *testing.T) {
	var mu sync.Mutex
	var paths []string
	var hits atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		paths = append(paths, r.URL.Path)
		mu.Unlock()
		hits.Add(1)
		if r.URL.Path == "/v1/models" {
			w.WriteHeader(http.StatusOK)
			return
		}
		w.WriteHeader(http.StatusNotFound) // this vendor's list is not at <base>/models
	}))
	defer upstream.Close()

	table := proxyRouteTable{
		Default: proxyRoute{
			Label: "remote:modelsurl", BaseURL: upstream.URL,
			ModelsURL:     upstream.URL + "/v1/models",
			UpstreamModel: "m", ContextWindow: 32768, Wire: "openai",
		},
		breakers: &routeBreakers{}, escalations: &routeEscalations{},
	}
	startPoll(t, table, 5*time.Millisecond, &hits)
	time.Sleep(60 * time.Millisecond)

	mu.Lock()
	asked := append([]string(nil), paths...)
	mu.Unlock()
	if len(asked) == 0 {
		t.Fatal("the probe never reached the leg")
	}
	for i, p := range asked {
		if p != "/v1/models" {
			t.Fatalf("probe %d asked %q, want the row's declared models_path %q — the 404 that a vendor without <base>/models answers is read as a leg failure, and the breaker then retires a leg that serves completions fine from fallback and oversize selection", i+1, p, "/v1/models")
		}
	}
	if table.breakers.open(upstream.URL) {
		t.Errorf("the breaker opened for a HEALTHY leg: %d probes of the wrong endpoint read as failures", len(asked))
	}
}

// A3: the type is normalized before it is read, as both sibling legs normalize
// it.
func TestToolChoiceTypeIsNormalizedAsTheSiblingLegsNormalizeIt(t *testing.T) {
	// The converter reads "none" with EqualFold over a trimmed value
	// (anthropic.go, dropTools), and the metered gateway lowercases and trims
	// before its switch — so the two sibling legs answer all of these.
	for _, tc := range []struct {
		spelling string
		want     any
	}{
		{"auto", "auto"}, {"AUTO", "auto"}, {" Auto ", "auto"},
		{"any", "required"}, {"Any", "required"}, {"ANY", "required"},
		{"none", nil}, {"NONE", nil}, {" none ", nil},
	} {
		if got := mapToolChoice(&anthropic.ToolChoice{Type: tc.spelling}); got != tc.want {
			t.Errorf("tool_choice.type=%q maps to %#v on this leg; the sibling legs read the type case-insensitively and trimmed, so they answer %#v for it — one body then forces a tool call on one leg and leaves the model free on the other",
				tc.spelling, got, tc.want)
		}
	}
	for _, spelling := range []string{"tool", "Tool", "TOOL", " tool "} {
		got, ok := mapToolChoice(&anthropic.ToolChoice{Type: spelling, Name: "read_file"}).(map[string]any)
		if !ok {
			t.Errorf("tool_choice.type=%q with a name maps to %#v, want the named function the gateway leg states for it", spelling, mapToolChoice(&anthropic.ToolChoice{Type: spelling, Name: "read_file"}))
			continue
		}
		fn, _ := got["function"].(map[string]any)
		if got["type"] != "function" || fn["name"] != "read_file" {
			t.Errorf("tool_choice.type=%q maps to %#v, want the named function", spelling, got)
		}
	}
}

// A4: a refusal of OUR credential upstream is a 502 on both wires — never the
// vendor's authentication_error, which sends the client into its own login flow.
func TestAnUpstreamCredentialRefusalIsA502OnBothWires(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	for _, status := range []int{http.StatusUnauthorized, http.StatusForbidden} {
		for _, wire := range []string{"openai", "anthropic"} {
			t.Run(http.StatusText(status)+"/"+wire, func(t *testing.T) {
				var hits atomic.Int64
				upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
					hits.Add(1)
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(status)
					_, _ = w.Write([]byte(`{"type":"error","error":{"type":"authentication_error","message":"invalid api key"}}`))
				}))
				defer upstream.Close()

				route := round54Route(upstream.URL, "glm-5.3", wire)
				if wire == "anthropic" && !route.NativePassthrough {
					t.Fatalf("setup: an anthropic-wire remote is no longer a passthrough leg, so this arm no longer covers it")
				}
				proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
				code, body := round54PostMessage(t, proxy, "glm-5.3", false)

				if code != http.StatusBadGateway {
					t.Errorf("an upstream %d on the %s wire reached the client as %d, want 502: the credential that was refused is OURS, so an authentication_error tells Claude Code to log in again while the translated arm of this same proxy answers the identical refusal with a 502\nbody: %s",
						status, wire, code, body)
				}
				// The vendor's body may ride along as the 502's diagnosis; what
				// must not is its error TYPE. Claude Code branches on the type,
				// so an authentication_error envelope is what sends it into its
				// own login flow.
				var env struct {
					Error struct {
						Type string `json:"type"`
					} `json:"error"`
				}
				if err := json.Unmarshal([]byte(body), &env); err != nil {
					t.Fatalf("body is not an Anthropic error envelope: %v\nbody: %s", err, body)
				}
				if env.Error.Type == "authentication_error" {
					t.Errorf("the vendor's authentication_error type was relayed verbatim to the client on the %s wire — Claude Code reads that as its own login having expired, while the translated arm of this same proxy answers the identical refusal with an api_error 502\nbody: %s", wire, body)
				}
				if hits.Load() != 1 {
					t.Errorf("upstream hits = %d, want 1", hits.Load())
				}
			})
		}
	}
}

// The control for A4: a healthy passthrough leg still answers 200 byte-for-byte
// through the same branch the refusal above now leaves.
func TestAnUpstreamCredentialRefusalControl(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	const answer = `{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"text","text":"PONG"}],"usage":{"input_tokens":1,"output_tokens":1}}`
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(answer))
	}))
	defer upstream.Close()

	route := round54Route(upstream.URL, "glm-5.3", "anthropic")
	if !route.NativePassthrough {
		t.Fatalf("setup: an anthropic-wire remote is no longer a passthrough leg")
	}
	proxy := startEntitlementTestProxy(t, route, map[string]proxyRoute{"glm-5.3": route})
	code, body := round54PostMessage(t, proxy, "glm-5.3", false)
	if code != http.StatusOK || !strings.Contains(body, "PONG") {
		t.Fatalf("a healthy passthrough leg answered %d: %s", code, body)
	}
}
