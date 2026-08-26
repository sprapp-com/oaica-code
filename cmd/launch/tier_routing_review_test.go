package launch

// Cases from the 2026-08-26 adversarial review of tier routing.

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func writeLocalRegistry(t *testing.T, entries []oaicaLocalServersRegistryEntry) {
	t.Helper()
	home, _ := os.UserHomeDir()
	if err := os.MkdirAll(filepath.Join(home, ".oaica"), 0o700); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(entries)
	if err := os.WriteFile(filepath.Join(home, ".oaica", "local_servers.json"), b, 0o600); err != nil {
		t.Fatal(err)
	}
}

func remoteBox(t *testing.T, index map[string][]string) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"},{"name":"box2","base_url":"http://box2:8080/v1","api_key":"k2","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, index)
}

// Regression (review, high): a bare --sonnet-model that the primary's remote
// does not enumerate must still pass through to that remote, as it always
// did ("muse-spark-1.2" on opencode-go). Same for OpenRouter "vendor/id"
// forms that are not a remote prefix.
func TestSecondary_BareIDPassesThroughToPrimaryRemote(t *testing.T) {
	remoteBox(t, map[string][]string{"kat-awq": {"box/kat-awq"}})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)

	for _, sonnet := range []string{"muse-spark-1.2", "openai/gpt-5"} {
		plan, err := buildTierPlan("box/kat-awq", sonnet, false)
		if err != nil {
			t.Fatalf("%s: %v", sonnet, err)
		}
		r, m := plan.Routes.resolve(sonnet)
		if r.BaseURL != "http://box:8080/v1" || r.Key != "k" || m != sonnet {
			t.Fatalf("%s -> %+v %q, want primary's remote with the id passed through", sonnet, r, m)
		}
	}
}

// Regression (review, medium): a bare secondary that another remote or the
// router ALSO serves must not silently move to a different backend/key.
func TestSecondary_BareIDNeverSilentlyLeavesPrimaryRemote(t *testing.T) {
	remoteBox(t, map[string][]string{
		"kat-awq":       {"box/kat-awq"},
		"deepseek-chat": {"box/deepseek-chat", "box2/deepseek-chat"}, // ambiguous
		"only-box2":     {"box2/only-box2"},                          // unique elsewhere
	})
	stubCloudFetch(t, []oaicaModelEntry{{ID: "deepseek-chat"}, {ID: "kat-awq"}}, nil) // router has them too
	stubDaemon(t, "deepseek-chat")
	t.Setenv("OAICA_HOST", "https://api.example.test")
	t.Setenv("OAICA_API_KEY", "sk-oaica")

	for _, sonnet := range []string{"deepseek-chat", "only-box2", "kat-awq"} {
		plan, err := buildTierPlan("box/kat-awq", sonnet, false)
		if err != nil {
			t.Fatalf("%s: %v", sonnet, err)
		}
		if r, _ := plan.Routes.resolve(sonnet); r.BaseURL != "http://box:8080/v1" || r.Key != "k" {
			t.Fatalf("%s silently rerouted to %+v", sonnet, r)
		}
	}
	// explicit forms DO leave the primary's remote
	plan, err := buildTierPlan("box/kat-awq", "box2/only-box2", false)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := plan.Routes.resolve("box2/only-box2"); r.BaseURL != "http://box2:8080/v1" || r.Key != "k2" {
		t.Fatalf("namespaced secondary = %+v", r)
	}
	plan, err = buildTierPlan("box/kat-awq", "router/deepseek-chat", false)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := plan.Routes.resolve("router/deepseek-chat"); r.BaseURL != "https://api.example.test/v1" || r.Key != "sk-oaica" {
		t.Fatalf("router/ secondary = %+v", r)
	}
	plan, err = buildTierPlan("box/kat-awq", "ollama/deepseek-chat", false)
	if err != nil {
		t.Fatal(err)
	}
	if r, _ := plan.Routes.resolve("ollama/deepseek-chat"); !strings.HasSuffix(r.BaseURL, "/v1") || r.Key != "ollama" {
		t.Fatalf("ollama/ secondary = %+v", r)
	}
}

// An ambiguous bare PRIMARY must be refused with the disambiguation hint,
// not fall through to the router.
func TestResolveLaunchEndpoint_AmbiguousBareIDIsAnError(t *testing.T) {
	remoteBox(t, map[string][]string{"deepseek-chat": {"box/deepseek-chat", "box2/deepseek-chat"}})
	stubCloudFetch(t, []oaicaModelEntry{{ID: "deepseek-chat"}}, nil)
	stubDaemon(t, "deepseek-chat")
	_, err := resolveLaunchEndpoint("deepseek-chat")
	if err == nil || !strings.Contains(err.Error(), "several remotes") || !strings.Contains(err.Error(), "box2/deepseek-chat") {
		t.Fatalf("err = %v", err)
	}
}

// The router rejecting the key must be reported as that, with the fix.
func TestResolveLaunchEndpoint_RouterAuthErrorNamesTheFix(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401, Host: "https://api.example.test"})
	stubDaemon(t)
	_, err := resolveLaunchEndpoint("kat-awq")
	if err == nil || !strings.Contains(err.Error(), "rejected the API key") || !strings.Contains(err.Error(), "oaica signin") {
		t.Fatalf("err = %v", err)
	}
}

// The real daemon probe: POST /api/show (so ":cloud" aliases count), and an
// unreachable daemon is reported as such.
func TestDaemonProbe_LiveShowAndUnreachable(t *testing.T) {
	noRemotes(t)
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	d := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/show" || r.Method != http.MethodPost {
			http.NotFound(w, r)
			return
		}
		b, _ := io.ReadAll(r.Body)
		if strings.Contains(string(b), `"x:cloud"`) {
			w.Write([]byte(`{"modelfile":""}`))
			return
		}
		w.WriteHeader(404)
	}))
	defer d.Close()
	t.Setenv("OLLAMA_HOST", d.URL)
	if ep, err := resolveLaunchEndpoint("x:cloud"); err != nil || ep.Source != sourceDaemon || ep.BaseURL != d.URL+"/v1" {
		t.Fatalf("cloud alias via /api/show: %+v, %v", ep, err)
	}
	if _, err := resolveLaunchEndpoint("y"); err == nil || !strings.Contains(err.Error(), "not pulled on the local daemon") {
		t.Fatalf("unknown model: %v", err)
	}
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:1")
	if _, err := resolveLaunchEndpoint("y"); err == nil || !strings.Contains(err.Error(), "no local daemon at") {
		t.Fatalf("unreachable daemon: %v", err)
	}
}

// A ":local" server started with --api-key gets that key as the bearer.
func TestResolveLaunchEndpoint_LocalServeUsesRegisteredKey(t *testing.T) {
	noRemotes(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer origin.Close()
	writeLocalRegistry(t, []oaicaLocalServersRegistryEntry{{Model: "bonsai", Origin: origin.URL, PID: 1, APIKey: "K"}})
	ep, err := resolveLaunchEndpoint("bonsai:local")
	if err != nil || ep.Token != "K" {
		t.Fatalf("ep = %+v, %v", ep, err)
	}
}

// Security (review): the loopback proxy must refuse callers that do not
// present the per-launch token, so no other local process can spend the
// launcher's upstream keys. No token configured = old behaviour (open).
func TestProxyRoutes_ClientTokenRequired(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	var upstreamHits int
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamHits++
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"x","model":"m","choices":[{"index":0,"finish_reason":"stop","message":{"role":"assistant","content":"ok"}}]}`))
	}))
	defer up.Close()
	table := proxyRouteTable{Default: proxyRoute{BaseURL: up.URL + "/v1", Key: "real-key", UpstreamModel: "m"}, ClientToken: "tok-123"}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	defer ln.Close()
	base := "http://" + ln.Addr().String()
	body := `{"model":"m","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
	call := func(path, method, auth string) int {
		req, _ := http.NewRequest(method, base+path, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := call("/v1/messages", "POST", ""); c != 401 {
		t.Fatalf("no token: %d", c)
	}
	if c := call("/v1/messages", "POST", "wrong"); c != 401 {
		t.Fatalf("wrong token: %d", c)
	}
	if c := call("/v1/models", "GET", ""); c != 401 {
		t.Fatalf("models without token: %d", c)
	}
	if upstreamHits != 0 {
		t.Fatalf("upstream reached %d times without a valid token", upstreamHits)
	}
	if c := call("/v1/messages", "POST", "tok-123"); c != 200 {
		t.Fatalf("valid token: %d", c)
	}
	// x-api-key is the other header Claude Code may use
	req, _ := http.NewRequest("POST", base+"/v1/messages", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("x-api-key", "tok-123")
	resp, _ := http.DefaultClient.Do(req)
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("x-api-key: %d", resp.StatusCode)
	}
}

func TestPlanEnv_NoRealKeyInChildEnvironment(t *testing.T) {
	remoteBox(t, map[string][]string{"kat-awq": {"box/kat-awq"}})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t)
	plan, err := buildTierPlan("box/kat-awq", "", false)
	if err != nil {
		t.Fatal(err)
	}
	for _, kv := range plan.envVars("http://x", "oaica-proxy-abc") {
		if strings.Contains(kv, "=k") && strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") {
			t.Fatalf("remote key leaked into child env: %s", kv)
		}
		if strings.HasPrefix(kv, "ANTHROPIC_AUTH_TOKEN=") && !strings.HasSuffix(kv, "=oaica-proxy-abc") {
			t.Fatalf("auth token must be the proxy token: %s", kv)
		}
	}
}

// Caught on .91 2026-08-27: `--model router/kat-awq` passed --sonnet-model
// resolution but the launch pre-flight (showOrPullWithPolicy) treated the
// prefixed name as a local model and tried to pull it.
func TestShowOrPull_SourcePrefixedModelNeverPulls(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	t.Setenv("OAICA_API_KEY", "sk")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "kat-awq"}}, nil)
	stubDaemon(t, "qwen2.5:7b")

	for _, m := range []string{"router/kat-awq", "oaica/kat-awq", "ollama/qwen2.5:7b"} {
		if err := showOrPullWithPolicy(context.Background(), deadClient(t), m, missingModelFail, false); err != nil {
			t.Fatalf("%s: %v", m, err)
		}
	}
	err := showOrPullWithPolicy(context.Background(), deadClient(t), "router/nope", missingModelFail, false)
	if err == nil || strings.Contains(err.Error(), "pull") || !strings.Contains(err.Error(), "api.example.test") {
		t.Fatalf("unknown prefixed model must fail without a pull attempt: %v", err)
	}
}

func TestRedactURL(t *testing.T) {
	if got := redactURL("http://user:s3cret@box:8080/v1"); strings.Contains(got, "s3cret") || got != "http://box:8080/v1" {
		t.Fatalf("got %q", got)
	}
	if got := redactURL("https://api.oaica.com/v1"); got != "https://api.oaica.com/v1" {
		t.Fatalf("got %q", got)
	}
}
