package launch

import (
	"bytes"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestNormalizeSystemFirst verifies the proxy folds every system message into a
// SINGLE leading system message. Some Anthropic→OpenAI translations place a
// second system message AFTER a user turn (e.g. Claude Code: [system, user,
// system]); strict chat templates (KAT-Coder's apex GGUF) raise
// "System message must be at the beginning" on that. Non-system messages keep
// their relative order.
func TestNormalizeSystemFirst(t *testing.T) {
	in := []openAIMessage{
		{Role: "system", Content: "sys-a"},
		{Role: "user", Content: "hello"},
		{Role: "system", Content: "sys-b"},
		{Role: "user", Content: "again"},
	}
	got := normalizeSystemFirst(in)
	want := []openAIMessage{
		{Role: "system", Content: "sys-a\n\nsys-b"},
		{Role: "user", Content: "hello"},
		{Role: "user", Content: "again"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("normalizeSystemFirst = %+v, want %+v", got, want)
	}

	// No system messages → unchanged.
	single := []openAIMessage{{Role: "user", Content: "x"}}
	if out := normalizeSystemFirst(single); !reflect.DeepEqual(out, single) {
		t.Errorf("normalizeSystemFirst(single) changed it: %+v", out)
	}

	// Empty system content is dropped, not emitted as a blank first message.
	blank := []openAIMessage{{Role: "system", Content: "  "}, {Role: "user", Content: "y"}}
	if out := normalizeSystemFirst(blank); len(out) != 1 || out[0].Role != "user" {
		t.Errorf("normalizeSystemFirst(blank system) = %+v, want just the user msg", out)
	}
}

// TestProxyHonorsPerRequestModel verifies the opusplan tier-split mechanism:
// the proxy forwards WHICHEVER model each Anthropic request carries
// (anthReq.Model), not just the fixed upstreamModel it was started with. This
// is what lets claude.go point ANTHROPIC_DEFAULT_OPUS_MODEL and
// ANTHROPIC_DEFAULT_SONNET_MODEL at two different bare ids through one proxy.
func TestProxyHonorsPerRequestModel(t *testing.T) {
	setLaunchTestHome(t, t.TempDir()) // request log must not land in the developer's real ~/.oaica
	var gotModels []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		gotModels = append(gotModels, req.Model)

		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":    "chatcmpl-1",
			"model": req.Model,
			"choices": []map[string]any{{
				"index":         0,
				"finish_reason": "stop",
				"message":       map[string]any{"role": "assistant", "content": "ok"},
			}},
		})
	}))
	defer upstream.Close()

	remote := userRemote{Name: "test", BaseURL: upstream.URL, APIKey: "test-key"}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token, err := StartAnthropicOpenAIProxy(ln, remote, "glm-5.3")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://" + ln.Addr().String()

	post := func(model string) {
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 10,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		})
		req2, _ := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(body))
		req2.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			b, _ := io.ReadAll(resp.Body)
			t.Fatalf("proxy returned %d: %s", resp.StatusCode, b)
		}
	}

	// Opus-tier request (the model the proxy was started with).
	post("glm-5.3")
	// Sonnet-tier request — a DIFFERENT model, as claude.go's --sonnet-model
	// would set via ANTHROPIC_DEFAULT_SONNET_MODEL.
	post("muse-spark-1.2")

	if len(gotModels) != 2 {
		t.Fatalf("upstream got %d requests, want 2", len(gotModels))
	}
	if gotModels[0] != "glm-5.3" {
		t.Errorf("first upstream model = %q, want glm-5.3", gotModels[0])
	}
	if gotModels[1] != "muse-spark-1.2" {
		t.Errorf("second upstream model = %q, want muse-spark-1.2 (tier split not honored)", gotModels[1])
	}
}

// TestProxyFallsBackToFixedModelWhenRequestOmitsIt covers the
// byte-identical-for-non-split-launches guarantee: a request with no model
// field (or an empty one) still resolves to the proxy's fixed upstreamModel.
func TestProxyFallsBackToFixedModelWhenRequestOmitsIt(t *testing.T) {
	setLaunchTestHome(t, t.TempDir()) // request log must not land in the developer's real ~/.oaica
	var gotModel string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Model string `json:"model"`
		}
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &req)
		gotModel = req.Model
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id":      "chatcmpl-1",
			"model":   req.Model,
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer upstream.Close()

	remote := userRemote{Name: "test", BaseURL: upstream.URL, APIKey: "test-key"}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	token, err := StartAnthropicOpenAIProxy(ln, remote, "deepseek-v4-flash")
	if err != nil {
		t.Fatal(err)
	}
	proxyURL := "http://" + ln.Addr().String()

	body, _ := json.Marshal(map[string]any{
		"max_tokens": 10,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	req2, _ := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(body))
	req2.Header.Set("Authorization", "Bearer "+token)
	resp, err := http.DefaultClient.Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		b, _ := io.ReadAll(resp.Body)
		t.Fatalf("proxy returned %d: %s", resp.StatusCode, b)
	}
	if gotModel != "deepseek-v4-flash" {
		t.Errorf("upstream model = %q, want fixed upstreamModel deepseek-v4-flash (regression: non-split launch changed)", gotModel)
	}
}

// TestProxy_SessionIDHeaderForwarded verifies proxyRouteTable.SessionID is
// sent upstream as X-Session-Id on every request — the piece a
// consistent-hash LB (e.g. oaicalb's session_hash_addr) needs to pin one
// launched conversation to the same backend replica for prefix-cache reuse.
// Two requests through the same proxy (same launch = same SessionID) must
// carry an identical header value; an empty SessionID must send none at all
// (older callers / non-session-aware backends stay byte-identical).
func TestProxy_SessionIDHeaderForwarded(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())

	var gotHeaders []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeaders = append(gotHeaders, r.Header.Get("X-Session-Id"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer upstream.Close()

	table := proxyRouteTable{
		ClientToken: "test-client-token",
		Default:     proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", Label: "test:kat-awq"},
		SessionID:   "oaica-session-abc123",
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	proxyURL := "http://" + ln.Addr().String()

	post := func() {
		body, _ := json.Marshal(map[string]any{
			"model": "kat-awq", "max_tokens": 10,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		req2, _ := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(body))
		req2.Header.Set("Authorization", "Bearer test-client-token")
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}
	post()
	post()

	if len(gotHeaders) != 2 {
		t.Fatalf("upstream got %d requests, want 2", len(gotHeaders))
	}
	for i, h := range gotHeaders {
		if h != "oaica-session-abc123" {
			t.Errorf("request %d X-Session-Id = %q, want %q", i, h, "oaica-session-abc123")
		}
	}

	// Empty SessionID (older callers / standalone proxy) sends no header.
	gotHeaders = nil
	table2 := proxyRouteTable{Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", Label: "test:kat-awq"}}
	ln2, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxyRoutes(ln2, table2)
	proxyURL2 := "http://" + ln2.Addr().String()
	body, _ := json.Marshal(map[string]any{
		"model": "kat-awq", "max_tokens": 10,
		"messages": []map[string]any{{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(proxyURL2+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if len(gotHeaders) != 1 || gotHeaders[0] != "" {
		t.Errorf("empty SessionID: got headers %v, want a single empty value", gotHeaders)
	}
}

// TestProxyResolveKey_ReReadsEnvVarLive is the regression for a production
// incident (2026-08-29): a client's OAICA_GATEWAY_KEY was exported to
// ~/.bashrc AFTER `oaica launch claude` had already built its proxy route
// table, and every request kept 401ing until the process was killed and
// relaunched — because Key was resolved once via remote.key() at table-build
// time and cached in the proxyRoute for the rest of the process's life.
// KeyEnv fixes this: resolveKey() re-reads the environment on every request.
func TestProxyResolveKey_ReReadsEnvVarLive(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	const envVar = "OAICA_TEST_LIVE_KEY"
	t.Setenv(envVar, "")

	var gotAuth []string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = append(gotAuth, r.Header.Get("Authorization"))
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"}}},
		})
	}))
	defer upstream.Close()

	// Table built BEFORE the env var is ever set, matching the incident:
	// the proxy process starts, THEN the key gets exported.
	table := proxyRouteTable{
		ClientToken: "test-client-token",
		Default:     proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", KeyEnv: envVar, Label: "test:kat-awq"},
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	proxyURL := "http://" + ln.Addr().String()

	post := func() {
		body, _ := json.Marshal(map[string]any{
			"model": "kat-awq", "max_tokens": 10,
			"messages": []map[string]any{{"role": "user", "content": "hi"}},
		})
		req2, _ := http.NewRequest("POST", proxyURL+"/v1/messages", bytes.NewReader(body))
		req2.Header.Set("Authorization", "Bearer test-client-token")
		resp, err := http.DefaultClient.Do(req2)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
	}

	// Env var still empty: no Authorization header sent upstream.
	post()
	if len(gotAuth) != 1 || gotAuth[0] != "" {
		t.Fatalf("before export: upstream auth = %v, want a single empty value", gotAuth)
	}

	// Export the key into the SAME already-running process's environment —
	// no relaunch, no route-table rebuild.
	t.Setenv(envVar, "sk-live-value")
	post()
	if len(gotAuth) != 2 || gotAuth[1] != "Bearer sk-live-value" {
		t.Fatalf("after export, same process: upstream auth = %v, want [.. \"Bearer sk-live-value\"]", gotAuth)
	}
}

// TestNewProxySessionID verifies the generator returns a non-empty, prefixed,
// and non-colliding value across calls — the same shape guarantee
// newProxyClientToken already has, since session IDs feed a consistent-hash
// bucket and a collision would silently merge two unrelated conversations
// onto one backend.
func TestNewProxySessionID(t *testing.T) {
	a, err := newProxySessionID()
	if err != nil {
		t.Fatal(err)
	}
	b, err := newProxySessionID()
	if err != nil {
		t.Fatal(err)
	}
	if a == "" || b == "" {
		t.Fatal("newProxySessionID returned an empty value")
	}
	if a == b {
		t.Fatal("newProxySessionID returned the same value twice")
	}
	if !strings.HasPrefix(a, "oaica-session-") {
		t.Errorf("newProxySessionID() = %q, want oaica-session- prefix", a)
	}
}

// TestContextFitClamp_ClientProxy_ReproducesRealIncident verifies the
// client-side translation proxy (RunAnthropicOpenAIProxyRoutes) clamps
// max_tokens the same way tools/gateway does server-side. Real 2026-08-29
// incident: this exact codepath (not the server-side gateway) forwarded a
// request with ~230145 estimated prompt tokens + max_tokens=32000 against a
// route with ContextWindow=262144 straight to the upstream, which 400'd
// with "one token over the ceiling" and no recovery path except /clear.
func TestContextFitClamp_ClientProxy_ClampsMaxTokensWhenRoomRemains(t *testing.T) {
	// A prompt with real room to spare after the 30%-margin reservation:
	// estimate ~100,000 tokens (400,000 chars), leaving a fitBudget well
	// above the request's own max_tokens=200000, so the proxy must clamp
	// down (not reject) and forward successfully.
	setLaunchTestHome(t, t.TempDir())
	var gotMaxTokens int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		gotMaxTokens = req.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
		})
	}))
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	table := proxyRouteTable{
		Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", ContextWindow: 262144},
	}
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	proxyURL := "http://" + ln.Addr().String()
	time.Sleep(50 * time.Millisecond)

	bigContent := strings.Repeat("x", 400000) // ~100,000 tokens at chars/4
	body, _ := json.Marshal(map[string]any{
		"model":      "kat-awq",
		"max_tokens": 200000,
		"messages":   []map[string]any{{"role": "user", "content": bigContent}},
	})
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("expected the proxy to clamp and forward successfully, got %d", resp.StatusCode)
	}
	if gotMaxTokens >= 200000 {
		t.Fatalf("expected max_tokens clamped below the original 200000, got %d", gotMaxTokens)
	}
	if gotMaxTokens <= 0 {
		t.Fatalf("expected a positive, still-useful max_tokens after clamping, got %d", gotMaxTokens)
	}
}

func TestContextFitClamp_ClientProxy_RejectsWhenPromptLeavesNoSafeRoom(t *testing.T) {
	// Real 2026-08-30 recurrence: a request whose REAL prompt (261,889
	// tokens) was already within 255 tokens of the 262,144 ceiling on its
	// own -- close enough that even flooring max_tokens to a small
	// positive number (the OLD behavior) still produced a request
	// guaranteed to 400 upstream. This fixture's estimate is deliberately
	// close enough to the ceiling that the 30% margin leaves no safe room
	// -- the proxy must reject client-side, never call upstream.
	setLaunchTestHome(t, t.TempDir())
	upstreamCalled := false
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalled = true
		w.WriteHeader(200)
	}))
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	table := proxyRouteTable{
		Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", ContextWindow: 262144},
	}
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	proxyURL := "http://" + ln.Addr().String()
	time.Sleep(50 * time.Millisecond)

	// ~230145 tokens at chars/4 -> ~920580 chars of message content.
	bigContent := strings.Repeat("x", 920580)
	body, _ := json.Marshal(map[string]any{
		"model":      "kat-awq",
		"max_tokens": 32000,
		"messages":   []map[string]any{{"role": "user", "content": bigContent}},
	})
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	respBody, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("expected 400 (no safe room to clamp into), got %d: %s", resp.StatusCode, respBody)
	}
	if upstreamCalled {
		t.Error("expected the proxy to reject client-side WITHOUT ever calling upstream")
	}
	// Anthropic's own wording (2026-08-30) -- Claude Code matches on it to
	// take its context-recovery path; see promptTooLongMessage.
	if !strings.Contains(string(respBody), "prompt is too long: ") {
		t.Errorf("expected Anthropic's 'prompt is too long: ' wording in the response, got %s", respBody)
	}
}

// TestContextFitClamp_ClientProxy_SmallPromptUnaffected mirrors the
// server-side test: a normal small request must not have max_tokens
// touched, and a route with ContextWindow=0 (unknown) must never clamp.
func TestContextFitClamp_ClientProxy_SmallPromptUnaffected(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var gotMaxTokens int
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			MaxTokens int `json:"max_tokens"`
		}
		b, _ := io.ReadAll(r.Body)
		json.Unmarshal(b, &req)
		gotMaxTokens = req.MaxTokens
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{
			"id": "chatcmpl-1", "model": "kat-awq",
			"choices": []map[string]any{{
				"index": 0, "finish_reason": "stop",
				"message": map[string]any{"role": "assistant", "content": "ok"},
			}},
		})
	}))
	defer upstream.Close()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	table := proxyRouteTable{
		Default: proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", ContextWindow: 262144},
	}
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	proxyURL := "http://" + ln.Addr().String()
	time.Sleep(50 * time.Millisecond)

	body, _ := json.Marshal(map[string]any{
		"model":      "kat-awq",
		"max_tokens": 2000,
		"messages":   []map[string]any{{"role": "user", "content": "hello"}},
	})
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	if gotMaxTokens != 2000 {
		t.Errorf("expected max_tokens to pass through unclamped for a small request, got %d", gotMaxTokens)
	}
}
