package launch

import (
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

func stubDaemon(t *testing.T, models ...string) {
	t.Helper()
	old := daemonHasModel
	daemonHasModel = func(m string) (bool, bool) {
		for _, x := range models {
			if x == m {
				return true, true
			}
		}
		return false, true
	}
	t.Cleanup(func() { daemonHasModel = old })
}

func noRemotes(t *testing.T) {
	t.Helper()
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[]}`)
	stubBareIndex(t, map[string][]string{})
}

// A router model (what a fresh customer has) must go through the
// translation proxy against <router>/v1 with the OAICA key -- before this,
// Claude Code was pointed at the router directly and the public gateway has
// no /v1/messages ("unrecognized_model", 2026-08-26).
func TestResolveLaunchEndpoint_RouterModel(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	t.Setenv("OAICA_API_KEY", "sk-cust")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "kat-awq"}}, nil)
	stubDaemon(t)

	ep, err := resolveLaunchEndpoint("kat-awq")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Source != sourceRouter || ep.BaseURL != "https://api.example.test/v1" || ep.Token != "sk-cust" || ep.UpstreamModel != "kat-awq" {
		t.Fatalf("router endpoint = %+v", ep)
	}
	// "+lora" composites are router syntax: readiness by base id, full id upstream
	ep, err = resolveLaunchEndpoint("kat-awq+fitness")
	if err != nil || ep.Source != sourceRouter || ep.UpstreamModel != "kat-awq+fitness" {
		t.Fatalf("composite = %+v, %v", ep, err)
	}
}

func TestResolveLaunchEndpoint_DaemonModelIncludingCloud(t *testing.T) {
	noRemotes(t)
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401, Host: "https://api.oaica.com"})
	stubDaemon(t, "deepseek-v4-flash:0731-cloud", "qwen2.5:7b-instruct")

	ep, err := resolveLaunchEndpoint("deepseek-v4-flash:0731-cloud")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Source != sourceDaemon || ep.BaseURL != "http://127.0.0.1:11434/v1" || ep.Token != "ollama" || ep.UpstreamModel != "deepseek-v4-flash:0731-cloud" {
		t.Fatalf("daemon endpoint = %+v", ep)
	}
}

func TestResolveLaunchEndpoint_LocalServe(t *testing.T) {
	noRemotes(t)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(200) }))
	defer origin.Close()
	home, _ := os.UserHomeDir()
	os.MkdirAll(filepath.Join(home, ".oaica"), 0o700)
	reg, _ := json.Marshal([]oaicaLocalServersRegistryEntry{{Model: "bonsai", Origin: origin.URL, PID: 1}})
	os.WriteFile(filepath.Join(home, ".oaica", "local_servers.json"), reg, 0o600)

	ep, err := resolveLaunchEndpoint("bonsai:local")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Source != sourceLocalServe || ep.BaseURL != origin.URL+"/v1" || ep.UpstreamModel != "bonsai" || ep.Token != "" {
		t.Fatalf("local endpoint = %+v", ep)
	}
	if _, err := resolveLaunchEndpoint("other:local"); err == nil || !strings.Contains(err.Error(), "oaica serve") {
		t.Fatalf("missing local server must be named: %v", err)
	}
}

func TestResolveLaunchEndpoint_UserRemoteWins(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{"kat-awq": {"box/kat-awq"}})
	stubCloudFetch(t, []oaicaModelEntry{{ID: "kat-awq"}}, nil) // router also has it; remote must win
	stubDaemon(t, "kat-awq")

	// the BARE id: every source claims it; the unique user remote must win
	ep, err := resolveLaunchEndpoint("kat-awq")
	if err != nil || ep.Source != sourceUserRemote || ep.BaseURL != "http://box:8080/v1" || ep.Token != "k" || ep.UpstreamModel != "kat-awq" {
		t.Fatalf("bare remote = %+v, %v", ep, err)
	}
	ep, err = resolveLaunchEndpoint("box/kat-awq")
	if err != nil || ep.Source != sourceUserRemote || ep.UpstreamModel != "kat-awq" {
		t.Fatalf("namespaced remote = %+v, %v", ep, err)
	}
	// explicit prefixes override the remote for the same bare id
	if ep, err := resolveLaunchEndpoint("router/kat-awq"); err != nil || ep.Source != sourceRouter {
		t.Fatalf("router/ prefix = %+v, %v", ep, err)
	}
	if ep, err := resolveLaunchEndpoint("ollama/kat-awq"); err != nil || ep.Source != sourceDaemon {
		t.Fatalf("ollama/ prefix = %+v, %v", ep, err)
	}
}

func TestResolveLaunchEndpoint_NotFoundNamesEverySource(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "kat-awq"}}, nil)
	stubDaemon(t)

	_, err := resolveLaunchEndpoint("nope")
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"remotes.json", "api.example.test", "local daemon"} {
		if !strings.Contains(err.Error(), want) {
			t.Fatalf("error %q should mention %q", err, want)
		}
	}
}

// The feature: primary on the local daemon (an Ollama cloud model),
// --sonnet-model on a user remote. Two different backends behind one proxy.
func TestBuildTierPlan_CrossSourceTiers(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubCloudFetch(t, nil, &oaicaRouterError{Status: 401})
	stubDaemon(t, "deepseek-v4-flash:0731-cloud")
	t.Setenv("OLLAMA_HOST", "http://127.0.0.1:11434")

	plan, err := buildTierPlan("deepseek-v4-flash:0731-cloud", "box/kat-awq", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Primary.Source != sourceDaemon || plan.Secondary.Source != sourceUserRemote {
		t.Fatalf("sources = %s / %s", plan.Primary.Source, plan.Secondary.Source)
	}
	r1, m1 := plan.Routes.resolve("deepseek-v4-flash:0731-cloud")
	r2, m2 := plan.Routes.resolve("box/kat-awq")
	if r1.BaseURL != "http://127.0.0.1:11434/v1" || m1 != "deepseek-v4-flash:0731-cloud" {
		t.Fatalf("primary route = %+v %q", r1, m1)
	}
	if r2.BaseURL != "http://box:8080/v1" || r2.Key != "k" || m2 != "kat-awq" {
		t.Fatalf("secondary route = %+v %q", r2, m2)
	}
	// bare upstream id of the secondary also routes there
	if r, _ := plan.Routes.resolve("kat-awq"); r.BaseURL != "http://box:8080/v1" {
		t.Fatalf("bare secondary id route = %+v", r)
	}
	env := map[string]string{}
	for _, kv := range plan.envVars("http://127.0.0.1:1", "tok") {
		k, v, _ := strings.Cut(kv, "=")
		env[k] = v
	}
	if env["ANTHROPIC_DEFAULT_OPUS_MODEL"] != "deepseek-v4-flash:0731-cloud" || env["ANTHROPIC_DEFAULT_HAIKU_MODEL"] != "deepseek-v4-flash:0731-cloud" {
		t.Fatalf("opus/haiku env wrong: %v", env)
	}
	if env["ANTHROPIC_DEFAULT_SONNET_MODEL"] != "box/kat-awq" || env["CLAUDE_CODE_SUBAGENT_MODEL"] != "box/kat-awq" {
		t.Fatalf("sonnet/subagent env wrong: %v", env)
	}
	if env["ANTHROPIC_AUTH_TOKEN"] == "" || env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:1" {
		t.Fatalf("token/base env wrong: %v", env)
	}
}

func TestBuildTierPlan_SingleModelPinsEveryTier(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	t.Setenv("OAICA_API_KEY", "sk")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "kat-awq"}}, nil)
	stubDaemon(t)

	plan, err := buildTierPlan("kat-awq", "", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SecondaryName != "kat-awq" || plan.Routes.Default.BaseURL != "https://api.example.test/v1" {
		t.Fatalf("plan = %+v", plan)
	}
	for _, kv := range plan.envVars("http://x", "tok") {
		if strings.HasPrefix(kv, "ANTHROPIC_DEFAULT_") && !strings.HasSuffix(kv, "=kat-awq") {
			t.Fatalf("tier not pinned: %s", kv)
		}
	}
}

func TestBuildTierPlan_SecondaryNotFound(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "kat-awq"}}, nil)
	stubDaemon(t)
	_, err := buildTierPlan("kat-awq", "ghost", "", false)
	if err == nil || !strings.Contains(err.Error(), "--sonnet-model") || !strings.Contains(err.Error(), "ghost") {
		t.Fatalf("err = %v", err)
	}
}

// The proxy must send each request to the upstream its model id maps to,
// with that upstream's own key and model id; unknown ids fall back to the
// default with the id passed through.
func TestProxyRoutes_PerModelUpstream(t *testing.T) {
	type seen struct{ model, auth string }
	var a, b []seen
	mk := func(sink *[]seen) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			var req struct {
				Model string `json:"model"`
			}
			body, _ := io.ReadAll(r.Body)
			json.Unmarshal(body, &req)
			*sink = append(*sink, seen{req.Model, r.Header.Get("Authorization")})
			w.Header().Set("Content-Type", "application/json")
			json.NewEncoder(w).Encode(map[string]any{"id": "x", "model": req.Model,
				"choices": []map[string]any{{"index": 0, "finish_reason": "stop", "message": map[string]any{"role": "assistant", "content": "ok"}}}})
		}))
	}
	upA, upB := mk(&a), mk(&b)
	defer upA.Close()
	defer upB.Close()
	setLaunchTestHome(t, t.TempDir()) // request log goes under a temp HOME

	table := proxyRouteTable{
		Default: proxyRoute{BaseURL: upA.URL + "/v1", Key: "key-a", UpstreamModel: "model-a"},
		ByModel: map[string]proxyRoute{
			"primary":   {BaseURL: upA.URL + "/v1", Key: "key-a", UpstreamModel: "model-a"},
			"secondary": {BaseURL: upB.URL + "/v1", Key: "", UpstreamModel: "model-b"},
		},
	}
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	go RunAnthropicOpenAIProxyRoutes(ln, table)
	defer ln.Close()
	post := func(model string) {
		body := `{"model":"` + model + `","max_tokens":5,"messages":[{"role":"user","content":"hi"}]}`
		resp, err := http.Post("http://"+ln.Addr().String()+"/v1/messages", "application/json", strings.NewReader(body))
		if err != nil {
			t.Fatal(err)
		}
		io.ReadAll(resp.Body)
		resp.Body.Close()
		if resp.StatusCode != 200 {
			t.Fatalf("%s: HTTP %d", model, resp.StatusCode)
		}
	}
	post("primary")
	post("secondary")
	post("unknown-id")
	if len(a) != 2 || a[0].model != "model-a" || a[0].auth != "Bearer key-a" || a[1].model != "unknown-id" {
		t.Fatalf("upstream A saw %+v", a)
	}
	if len(b) != 1 || b[0].model != "model-b" || b[0].auth != "" {
		t.Fatalf("upstream B saw %+v", b)
	}
}

func TestProxyMapsUnpinnedAnthropicIdsToDefault(t *testing.T) {
	// Claude Code's background calls send real Anthropic ids (claude-haiku-4-5-20251001)
	// regardless of ANTHROPIC_DEFAULT_HAIKU_MODEL; they must ride the default
	// leg's upstream id instead of 404ing at the backend, while ordinary
	// unknown ids keep forwarding (single-remote tier split depends on it).
	table := proxyRouteTable{
		Default: proxyRoute{BaseURL: "http://up/v1", UpstreamModel: "primary-id", Label: "default"},
		Policy:  RouteLocalFirst,
	}
	cases := map[string]string{
		"claude-haiku-4-5-20251001": "primary-id",
		"claude-3-5-haiku-latest":   "primary-id",
		"muse-spark-1.2":            "muse-spark-1.2", // passthrough preserved
		"":                          "primary-id",
	}
	for req, want := range cases {
		route, model := table.resolve(req)
		if model != want {
			t.Errorf("resolve(%q) model = %q, want %q", req, model, want)
		}
		if route.BaseURL != "http://up/v1" {
			t.Errorf("resolve(%q) route = %q, want default", req, route.BaseURL)
		}
	}
}

func TestResolveSecondaryEndpoint_RouterSKUBare(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key":"k","tool_format":"tool_calls"}]}`)
	// A user remote MIRRORS an OAICA router SKU under its bare id (opencode
	// zen proxies our SKUs). The primary is a daemon model; the sonnet tier
	// names a bare router SKU. The router must win — the remote mirror must
	// NOT hijack the bare name (2026-09-01 fleet 401 "Model not supported").
	stubBareIndex(t, map[string][]string{"oaica-35b-a3b-vision": {"box/oaica-35b-a3b-vision"}})
	stubCloudFetch(t, []oaicaModelEntry{{ID: "oaica-35b-a3b-vision"}}, nil)

	primary, err := resolveLaunchEndpoint("ollama/glm-5.3-flash:cloud")
	if err != nil {
		t.Fatal(err)
	}
	sec, err := resolveSecondaryEndpoint(primary, "oaica-35b-a3b-vision")
	if err != nil {
		t.Fatal(err)
	}
	if sec := sec.Source; sec != sourceRouter {
		t.Fatalf("secondary source = %s, want router (bare router SKU must not be hijacked by a mirroring remote)", sec)
	}
	// a bare id NOT on the router keeps the generic path (daemon here)
	if ep, err := resolveSecondaryEndpoint(primary, "glm-5.3-flash:cloud"); err != nil || ep.Source != sourceDaemon {
		t.Fatalf("bare daemon secondary = %+v, %v", ep, err)
	}
}

// Regression (2026-09-02 .46 fleet): primary on a user remote that MIRRORS
// router SKUs (opencode zen serves glm-5.3-flash AND oaica-35b-a3b-vision);
// plan myplan's sonnet_model "oaica-35b-a3b-vision" then hit the "un-namespaced
// secondary = on the primary's remote" contract and went to zen with zen's
// key → 401 "Model oaica-35b-a3b-vision is not supported". A bare oaica-*
// id is the router's own SKU regardless of the primary's source.
func TestBuildTierPlan_RouterSKUSecondaryOnUserRemotePrimary(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	writeRemotes(t, `{"remotes":[{"name":"opencode-go","base_url":"https://zen.test/v1","api_key":"zenk","tool_format":"tool_calls"}]}`)
	stubBareIndex(t, map[string][]string{"glm-5.3-flash": {"opencode-go/glm-5.3-flash"}})
	t.Setenv("OAICA_HOST", "https://api.example.test")
	t.Setenv("OAICA_API_KEY", "sk-oaica")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "oaica-35b-a3b-vision"}}, nil)
	stubDaemon(t)

	plan, err := buildTierPlan("glm-5.3-flash", "oaica-35b-a3b-vision", "", false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.Secondary.Source != sourceRouter {
		t.Fatalf("secondary source = %s, want router", plan.Secondary.Source)
	}
	if plan.Secondary.BaseURL != "https://api.example.test/v1" || plan.Secondary.Token != "sk-oaica" {
		t.Fatalf("secondary endpoint = %+v", plan.Secondary)
	}
	r, _ := plan.Routes.resolve("oaica-35b-a3b-vision")
	if r.BaseURL != "https://api.example.test/v1" || r.Key != "sk-oaica" {
		t.Fatalf("sonnet route = %+v", r)
	}
	// Explicit "<remote>/<id>" still keeps the same-remote contract.
	plan2, err := buildTierPlan("glm-5.3-flash", "opencode-go/other-model", "", false)
	if err != nil {
		t.Fatal(err)
	}
	r2, _ := plan2.Routes.resolve("opencode-go/other-model")
	if r2.BaseURL != "https://zen.test/v1" || r2.Key != "zenk" {
		t.Fatalf("namespaced secondary route = %+v", r2)
	}
}

// Native Anthropic (claude/*, anthropic/*) bypasses the OAICA translation
// proxy entirely (runNative, tier_routing.go's Run) -- it has no way to
// serve one tier of a split, so --sonnet-model/--haiku-model must refuse it
// with a clear message instead of the generic "not found" resolveLaunchEndpoint
// produces for an unrecognized id (2026-09-02).
// Native Anthropic (claude/*, anthropic/*) resolves as a real endpoint for
// --sonnet-model/--haiku-model too (2026-09-02: previously rejected — the
// proxy grew a native-passthrough leg, anthropic_openai_proxy.go's
// nativeAnthropicPassthrough, specifically so a split with a native tier
// can exist at all). Both legs go through the SAME local proxy as any
// other split; buildTierPlan itself never needs to know a leg is native —
// that's entirely proxyRoute.NativePassthrough's concern downstream.
func TestBuildTierPlan_NativeClaudeUsableAsSecondaryTier(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	t.Setenv("OAICA_API_KEY", "sk-cust")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "oaica-35b-a3b-vision"}}, nil)
	stubDaemon(t)

	plan, err := buildTierPlan("oaica-35b-a3b-vision", "claude/opus", "", false)
	if err != nil {
		t.Fatalf("--sonnet-model claude/opus: %v", err)
	}
	if plan.Secondary.Source != sourceNativeAnthropic {
		t.Fatalf("secondary source = %s, want native-anthropic", plan.Secondary.Source)
	}
	if plan.Secondary.UpstreamModel != "opus" {
		t.Fatalf("secondary upstream model = %q, want opus (the Claude Code --model alias)", plan.Secondary.UpstreamModel)
	}
	r, _ := plan.Routes.resolve("claude/opus")
	if !r.NativePassthrough {
		t.Fatalf("resolved route NativePassthrough = false, want true: %+v", r)
	}

	plan2, err := buildTierPlan("oaica-35b-a3b-vision", "", "anthropic/haiku", false)
	if err != nil {
		t.Fatalf("--haiku-model anthropic/haiku: %v", err)
	}
	if plan2.Haiku.Source != sourceNativeAnthropic || plan2.Haiku.UpstreamModel != "haiku" {
		t.Fatalf("haiku endpoint = %+v", plan2.Haiku)
	}
}

// TestBuildTierPlan_NativePrimaryDisguisesOaicaSecondary covers the OTHER
// direction from TestBuildTierPlan_NativeClaudeUsableAsSecondaryTier: a
// NATIVE primary with an OAICA secondary/haiku. This is exactly the
// combination that hit Claude Code's real session-restore validation
// ("Session model oaica-35b-a3b-vision could not be restored ... using
// claude-sonnet-5 instead", 2026-09-02) — routeForDisguised must set
// DisplayModel on the oaica leg so a resumed session validates cleanly,
// while the REAL upstream model (UpstreamModel) stays untouched: only the
// id echoed back to Claude Code changes, not what actually gets requested.
func TestBuildTierPlan_NativePrimaryDisguisesOaicaSecondary(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	t.Setenv("OAICA_API_KEY", "sk-cust")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "oaica-35b-a3b-vision"}}, nil)
	stubDaemon(t)

	plan, err := buildTierPlan("claude/fable", "oaica-35b-a3b-vision", "oaica-35b-a3b-vision", false)
	if err != nil {
		t.Fatalf("native primary + oaica split: %v", err)
	}
	if plan.Secondary.Source == sourceNativeAnthropic {
		t.Fatalf("secondary should resolve to the real oaica endpoint, not native: %+v", plan.Secondary)
	}
	if plan.Secondary.UpstreamModel != "oaica-35b-a3b-vision" {
		t.Fatalf("secondary UpstreamModel = %q, want the REAL model — DisplayModel must never change what's actually requested", plan.Secondary.UpstreamModel)
	}

	r, upstream := plan.Routes.resolve("oaica-35b-a3b-vision")
	if r.NativePassthrough {
		t.Fatalf("the oaica leg's own route must not be NativePassthrough (that's the primary's route, not this one): %+v", r)
	}
	if r.DisplayModel != nativeSonnetDisplayModel {
		t.Fatalf("DisplayModel = %q, want %q (Claude Code's own recognized sonnet id, marked distinguishable)", r.DisplayModel, nativeSonnetDisplayModel)
	}
	if !strings.Contains(r.DisplayModel, oaicaDisplayModelSuffix) {
		t.Fatalf("DisplayModel %q must carry the OAICA-distinguishing suffix — never present an unmarked Anthropic id", r.DisplayModel)
	}
	if upstream != "oaica-35b-a3b-vision" {
		t.Fatalf("resolve() upstream model = %q, want the real model unaffected by DisplayModel", upstream)
	}
}

// TestBuildTierPlan_NonNativePrimaryNeverSetsDisplayModel guards the
// byte-identical-when-unaffected contract: DisplayModel must stay empty for
// every ordinary (non-native-primary) split — Claude Code only owns real
// session-restore validation when running as the actual native binary, so
// disguising the model id anywhere else would be pure noise with no bug to
// fix and a real cost (a human reading that transcript loses the true model
// label for no reason).
func TestBuildTierPlan_NonNativePrimaryNeverSetsDisplayModel(t *testing.T) {
	noRemotes(t)
	writeRemotes(t, `{"remotes":[{"name":"box","base_url":"http://box:8080/v1","api_key":"k"}]}`)
	stubBareIndex(t, map[string][]string{})
	stubDaemon(t, "glm-5.3-flash:cloud")

	plan, err := buildTierPlan("glm-5.3-flash:cloud", "box/kat-awq", "", false)
	if err != nil {
		t.Fatal(err)
	}
	r, _ := plan.Routes.resolve("box/kat-awq")
	if r.DisplayModel != "" {
		t.Fatalf("DisplayModel = %q on a non-native primary, want empty", r.DisplayModel)
	}
}

// TestClaudeCodeModelAlias covers the strip-our-prefix fix (2026-09-02):
// ANTHROPIC_DEFAULT_*_MODEL env vars are read by the REAL Claude Code
// binary, which only knows its own bare aliases, not our "claude/"/
// "anthropic/" picker syntax — a native primary with a haiku-only split
// sent "claude/fable" straight into that env var and Claude Code rejected
// it outright ("issue with the selected model").
func TestClaudeCodeModelAlias(t *testing.T) {
	cases := map[string]string{
		"claude/fable":         "fable",
		"claude/opus":          "opus",
		"claude/sonnet":        "sonnet",
		"anthropic/fable":      "fable",
		"anthropic/opus":       "opus",
		"oaica-35b-a3b-vision": "oaica-35b-a3b-vision", // non-native: unchanged
		"glm-5.3-flash:cloud":  "glm-5.3-flash:cloud",  // non-native: unchanged
		"box/kat-awq":          "box/kat-awq",          // non-native: unchanged
	}
	for in, want := range cases {
		if got := claudeCodeModelAlias(in); got != want {
			t.Errorf("claudeCodeModelAlias(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestBuildTierPlan_EnvVarsStripNativePrefix confirms envVars() actually
// uses claudeCodeModelAlias on all four ANTHROPIC_DEFAULT_*/CLAUDE_CODE_*
// slots that carry a model id, for a native primary with an OAICA
// haiku-only split (sonnet == primary, only haiku differs) — the exact
// combination that surfaced the bug.
func TestBuildTierPlan_EnvVarsStripNativePrefix(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	t.Setenv("OAICA_API_KEY", "sk-cust")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "oaica-35b-a3b-vision"}}, nil)
	stubDaemon(t)

	plan, err := buildTierPlan("claude/fable", "", "oaica-35b-a3b-vision", false)
	if err != nil {
		t.Fatalf("native primary + oaica haiku-only split: %v", err)
	}
	env := plan.envVars("http://127.0.0.1:0", "tok")
	want := map[string]string{
		"ANTHROPIC_DEFAULT_OPUS_MODEL":   "fable",
		"ANTHROPIC_DEFAULT_SONNET_MODEL": "fable",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":  "oaica-35b-a3b-vision",
		"CLAUDE_CODE_AUTO_MODE_MODEL":    "fable",
	}
	got := map[string]string{}
	for _, kv := range env {
		for k := range want {
			if strings.HasPrefix(kv, k+"=") {
				got[k] = strings.TrimPrefix(kv, k+"=")
			}
		}
	}
	for k, w := range want {
		if got[k] != w {
			t.Errorf("%s = %q, want %q (env: %v)", k, got[k], w, env)
		}
	}
}

// TestBuildTierPlan_OpusplanTriggersOnHaikuOnlySplit is the opusplan-
// trigger-widening fix: previously only SecondaryName != PrimaryName
// entered opusplan mode, so a haiku-only split (sonnet == primary, haiku
// differs) launched with a plain --model <primary> instead — for a native
// primary, that sent our own picker-namespaced string straight to --model
// with no opusplan preset to route around it. Exercised through Run()'s
// own claudeModel logic isn't directly unit-testable without executing the
// child process, so this test pins the plan-level facts the fix depends
// on: HaikuName alone differing from PrimaryName is real and visible on
// the built plan.
func TestBuildTierPlan_OpusplanTriggersOnHaikuOnlySplit(t *testing.T) {
	noRemotes(t)
	t.Setenv("OAICA_HOST", "https://api.example.test")
	t.Setenv("OAICA_API_KEY", "sk-cust")
	stubCloudFetch(t, []oaicaModelEntry{{ID: "oaica-35b-a3b-vision"}}, nil)
	stubDaemon(t)

	plan, err := buildTierPlan("claude/fable", "", "oaica-35b-a3b-vision", false)
	if err != nil {
		t.Fatal(err)
	}
	if plan.SecondaryName != plan.PrimaryName {
		t.Fatalf("SecondaryName = %q, want == PrimaryName %q for a haiku-only split", plan.SecondaryName, plan.PrimaryName)
	}
	if plan.HaikuName == plan.PrimaryName {
		t.Fatalf("HaikuName = %q, want != PrimaryName — this is the split under test", plan.HaikuName)
	}
	// The condition Run() actually checks (tier_routing.go): true here is
	// exactly what makes it enter opusplan mode instead of a plain --model.
	entersOpusplan := plan.SecondaryName != plan.PrimaryName || plan.HaikuName != plan.PrimaryName
	if !entersOpusplan {
		t.Fatal("opusplan trigger condition is false for a haiku-only split — this is the exact regression the fix closes")
	}
}

// A plain native launch (no split requested) must stay on the fully-
// untouched runNative path — Run() checks this at the model level before
// buildTierPlan is ever called, so it isn't exercised here; this test only
// guards the resolution buildTierPlan itself would produce if it WERE
// called with no split, in case Run()'s gate is ever bypassed by a future
// caller.
func TestResolveLaunchEndpoint_NativeClaude(t *testing.T) {
	noRemotes(t)
	ep, err := resolveLaunchEndpoint("claude/fable")
	if err != nil {
		t.Fatal(err)
	}
	if ep.Source != sourceNativeAnthropic || ep.UpstreamModel != "fable" {
		t.Fatalf("resolveLaunchEndpoint(claude/fable) = %+v", ep)
	}
	ep2, err := resolveLaunchEndpoint("anthropic/sonnet")
	if err != nil {
		t.Fatal(err)
	}
	if ep2.Source != sourceNativeAnthropic || ep2.UpstreamModel != "sonnet" {
		t.Fatalf("resolveLaunchEndpoint(anthropic/sonnet) = %+v", ep2)
	}
}
