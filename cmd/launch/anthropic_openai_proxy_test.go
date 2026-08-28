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
	go RunAnthropicOpenAIProxy(ln, remote, "glm-5.3")
	proxyURL := "http://" + ln.Addr().String()

	post := func(model string) {
		body, _ := json.Marshal(map[string]any{
			"model":      model,
			"max_tokens": 10,
			"messages":   []map[string]any{{"role": "user", "content": "hi"}},
		})
		resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
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
	go RunAnthropicOpenAIProxy(ln, remote, "deepseek-v4-flash")
	proxyURL := "http://" + ln.Addr().String()

	body, _ := json.Marshal(map[string]any{
		"max_tokens": 10,
		"messages":   []map[string]any{{"role": "user", "content": "hi"}},
	})
	resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
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
		Default:   proxyRoute{BaseURL: upstream.URL, UpstreamModel: "kat-awq", Label: "test:kat-awq"},
		SessionID: "oaica-session-abc123",
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
		resp, err := http.Post(proxyURL+"/v1/messages", "application/json", bytes.NewReader(body))
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
