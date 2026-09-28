package launch

import (
	"fmt"
	"os"
	"strings"
	"testing"
)

// findBuiltinRemote returns a pointer to the entry named name, or nil.
// Every builtin is env-gated, so tests asserting "gated when env unset" for
// one provider must look up by name rather than assume an index or length —
// shared env vars (zai and zai-coding-plan both key off Z_AI_API_KEY) mean
// setting one provider's key can add more than one row.
func findBuiltinRemote(t *testing.T, remotes []userRemote, name string) *userRemote {
	t.Helper()
	for i := range remotes {
		if remotes[i].Name == name {
			return &remotes[i]
		}
	}
	return nil
}

func TestOpenAIBase(t *testing.T) {
	tests := []struct {
		name string
		rem  userRemote
		want string
	}{
		{
			name: "default v1 version, bare base",
			rem:  userRemote{Name: "deepseek", BaseURL: "https://api.deepseek.com"},
			want: "https://api.deepseek.com/v1",
		},
		{
			name: "base already carries /v1 is de-duplicated",
			rem:  userRemote{Name: "deepseek", BaseURL: "https://api.deepseek.com/v1"},
			want: "https://api.deepseek.com/v1",
		},
		{
			name: "zai v4 version",
			rem:  userRemote{Name: "zai", BaseURL: "https://api.z.ai/api/paas", Version: "v4"},
			want: "https://api.z.ai/api/paas/v4",
		},
		{
			name: "version slash normalization",
			rem:  userRemote{Name: "zai", BaseURL: "https://api.z.ai/api/paas/", Version: "/v4/"},
			want: "https://api.z.ai/api/paas/v4",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.rem.openAIBase(); got != tt.want {
				t.Fatalf("openAIBase() = %q, want %q", got, tt.want)
			}
		})
	}
}

// clearAllCatalogKeys clears every provider catalog env var (data-driven —
// covers new providers/plans automatically), so a test asserting an exact
// builtinRemotes() shape isn't at the mercy of the developer's own shell.
func clearAllCatalogKeys(t *testing.T) {
	t.Helper()
	// Builtins are gated on an env var OR a stored credential (auth_store.go);
	// hermeticTestEnv points OAICA_AUTH_FILE at a nonexistent path, so this
	// loop only has to handle the env half.
	for _, p := range providerCatalog() {
		if p.APIKeyEnv != "" {
			t.Setenv(p.APIKeyEnv, "")
		}
		// And the row's upstream env[] names, which gate a row that declares
		// no api_key_env of its own — otherwise a developer's own shell decides
		// whether such a row is offered (firstSetEnv).
		for _, name := range p.Env {
			if name = strings.TrimSpace(name); name != "" {
				t.Setenv(name, "")
			}
		}
	}
}

func TestBuiltinRemotes_ZAIKeyGate(t *testing.T) {
	clearAllCatalogKeys(t)

	// zai-coding-plan is a separate catalog entry with its own env var, so
	// the gate-when-unset assertion here is scoped to "zai" specifically.
	if got := findBuiltinRemote(t, builtinRemotes(), zaiName); got != nil {
		t.Fatalf("builtinRemotes() contains %v, want no %q entry when %s unset", got, zaiName, zaiEnvKey)
	}

	t.Setenv(zaiEnvKey, "zai-secret")
	z := findBuiltinRemote(t, builtinRemotes(), zaiName)
	if z == nil {
		t.Fatalf("builtinRemotes() missing %q after setting %s", zaiName, zaiEnvKey)
	}
	if z.Name != zaiName {
		t.Fatalf("builtin name = %q, want %q", z.Name, zaiName)
	}
	if z.BaseURL != providerCatalogBaseURL(t, zaiName) {
		t.Fatalf("builtin base_url = %q, want catalog value", z.BaseURL)
	}
	if z.APIKeyEnv != zaiEnvKey {
		t.Fatalf("builtin api_key_env = %q, want %q", z.APIKeyEnv, zaiEnvKey)
	}
	if z.Version != "v4" {
		t.Fatalf("builtin version = %q, want v4", z.Version)
	}
	if got := z.key(); got != "zai-secret" {
		t.Fatalf("builtin key() = %q, want zai-secret", got)
	}
	if got, want := z.openAIBase(), "https://api.z.ai/api/paas/v4"; got != want {
		t.Fatalf("builtin openAIBase() = %q, want %q", got, want)
	}
}

// providerCatalogBaseURL looks up a provider's base_url from the live
// catalog, so tests assert against the actual data source (providers.json)
// instead of a second, potentially-drifting hardcoded copy of the URL.
func providerCatalogBaseURL(t *testing.T, name string) string {
	t.Helper()
	for _, p := range providerCatalog() {
		if p.Name == name {
			return p.BaseURL
		}
	}
	t.Fatalf("provider %q not found in catalog", name)
	return ""
}

func TestBuiltinRemotes_OllamaCloudKeyGate(t *testing.T) {
	clearAllCatalogKeys(t)
	if got := findBuiltinRemote(t, builtinRemotes(), ollamaCloudName); got != nil {
		t.Fatalf("builtinRemotes() contains %v, want no %q entry when %s unset", got, ollamaCloudName, ollamaCloudEnvKey)
	}

	t.Setenv(ollamaCloudEnvKey, "ollama-secret")
	o := findBuiltinRemote(t, builtinRemotes(), ollamaCloudName)
	if o == nil {
		t.Fatalf("builtinRemotes() missing %q after setting %s", ollamaCloudName, ollamaCloudEnvKey)
	}
	if o.Name != ollamaCloudName {
		t.Fatalf("builtin name = %q, want %q", o.Name, ollamaCloudName)
	}
	// Must not collide with the "ollama/" source prefix that selects the
	// local daemon in resolveLaunchEndpoint.
	if hasSourcePrefix(o.Name + "/x") {
		t.Fatalf("builtin name %q collides with a source prefix", o.Name)
	}
	if o.APIKeyEnv != ollamaCloudEnvKey {
		t.Fatalf("builtin api_key_env = %q, want %q", o.APIKeyEnv, ollamaCloudEnvKey)
	}
	if got := o.key(); got != "ollama-secret" {
		t.Fatalf("builtin key() = %q, want ollama-secret", got)
	}
	if got, want := o.openAIBase(), "https://ollama.com/v1"; got != want {
		t.Fatalf("builtin openAIBase() = %q, want %q", got, want)
	}
	if o.ToolFormat != "tool_calls" {
		t.Fatalf("builtin tool_format = %q, want tool_calls", o.ToolFormat)
	}
}

func TestBuiltinRemotes_MergedIntoLoad(t *testing.T) {
	t.Setenv(zaiEnvKey, "zai-secret")
	t.Setenv("OAICA_REMOTES_FILE", t.TempDir()+"/does-not-exist.json")

	remotes, err := loadUserRemotes()
	if err != nil {
		t.Fatalf("loadUserRemotes() error: %v", err)
	}
	found := false
	for _, r := range remotes {
		if r.Name == zaiName {
			found = true
		}
	}
	if !found {
		t.Fatalf("builtin %s not merged into loadUserRemotes(): %+v", zaiName, remotes)
	}
}

// A provider whose catalog row declares its models must still produce picker
// rows when /v1/models cannot be swept: z.ai's Coding Plan and MiniMax's
// serve no model list at all on their Anthropic-compatible endpoints, so
// without the declared list a paid plan would be invisible in the picker.
func TestRemoteLaunchModels_DeclaredCatalogModelsSurviveFailedSweep(t *testing.T) {
	// CatalogOrigin: the declared list is the VENDOR's, and remoteLaunchModels
	// applies it only to the catalog's own row — a user remote of the same
	// name keeps none of it (see CatalogOrigin).
	r := userRemote{Name: "minimax-coding-plan", BaseURL: "https://api.minimax.io/anthropic/v1",
		Wire: "anthropic", CatalogOrigin: true}
	models, err := remoteLaunchModels(r, nil, fmt.Errorf("HTTP 404"))
	if err != nil {
		t.Fatalf("remoteLaunchModels() error = %v, want the declared list to absorb a failed sweep", err)
	}
	declared := providerCatalogDeclaredModels("minimax-coding-plan")
	if len(models) != len(declared) {
		t.Fatalf("rows = %d, want one per declared model (%d)", len(models), len(declared))
	}
	var m3 *LaunchModel
	for i := range models {
		if models[i].Name == "minimax-coding-plan/MiniMax-M3" {
			m3 = &models[i]
		}
	}
	if m3 == nil {
		t.Fatalf("MiniMax-M3 missing from %v", models)
	}
	if m3.ContextLength != declared["MiniMax-M3"].Context || m3.ContextLength == 0 {
		t.Fatalf("MiniMax-M3 context = %d, want the declared %d", m3.ContextLength, declared["MiniMax-M3"].Context)
	}
	if !m3.ToolCapable || !m3.Remote || m3.Wire != "anthropic" {
		t.Fatalf("MiniMax-M3 = %+v, want a tool-capable remote row on the anthropic wire", *m3)
	}
}

// A remote with neither a sweep nor a declared list must keep reporting the
// sweep failure — the picker's error list is how the user learns a box is
// unreachable.
func TestRemoteLaunchModels_UndeclaredRemoteStillReportsSweepFailure(t *testing.T) {
	if _, err := remoteLaunchModels(userRemote{Name: "somebox", BaseURL: "http://box/v1"}, nil, fmt.Errorf("dial tcp: refused")); err == nil {
		t.Fatal("remoteLaunchModels() = nil error, want the sweep failure surfaced")
	}
}

// Swept ids must keep their own list order and win a collision with a
// declared id, so a provider that DOES answer /v1/models is unaffected by
// having a declared list as well.
func TestRemoteLaunchModels_SweptIDsWinAndKeepOrder(t *testing.T) {
	r := userRemote{Name: "minimax-coding-plan", BaseURL: "https://api.minimax.io/anthropic/v1", CatalogOrigin: true}
	models, err := remoteLaunchModels(r, []string{"Custom-Live", "MiniMax-M3"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if models[0].Name != "minimax-coding-plan/Custom-Live" || models[1].Name != "minimax-coding-plan/MiniMax-M3" {
		t.Fatalf("swept ids lost their order: %v, %v", models[0].Name, models[1].Name)
	}
	var m3Count int
	for _, m := range models {
		if m.Name == "minimax-coding-plan/MiniMax-M3" {
			m3Count++
		}
	}
	if m3Count != 1 {
		t.Fatalf("MiniMax-M3 appears %d times, want once (swept id wins over the declared duplicate)", m3Count)
	}
}

// TestRemoteEndpoint_TokenEnvNamesOneVariable: TokenEnv exists so a long-lived
// proxy re-reads the credential from the environment on every request instead
// of trusting the value resolved at launch (see its doc — the 2026-08-29
// incident). A row naming two variables carried the raw comma-joined string,
// which os.Getenv cannot resolve, so the live re-read was dead and every
// request fell back to the launch-time key (2026-09-26 audit).
func TestRemoteEndpoint_TokenEnvNamesOneVariable(t *testing.T) {
	withTempHome(t)
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{}`)
	t.Setenv("OPENCODE_API_KEY", "sk-live-opencode")

	ep, ok := resolveRemoteEndpoint("opencode-go/glm-5")
	if !ok {
		t.Fatal("opencode-go must resolve with OPENCODE_API_KEY exported")
	}
	if ep.Token == "" {
		t.Fatal("the endpoint resolved without a credential — the fixture is wrong")
	}
	if v := os.Getenv(ep.TokenEnv); v == "" {
		t.Errorf("TokenEnv = %q names no environment variable, so a proxy built from this endpoint can never re-read "+
			"the credential: resolveKey falls back to the launch-time key for the process's whole lifetime", ep.TokenEnv)
	}
}

// TestRemoteLaunchModels_UserRowDoesNotInheritCatalogDeclaredModels: the
// catalog's declared model list is looked up by NAME, and loadUserRemotes
// deliberately lets a user's own row win over a catalog row of the same name
// ("the catalog only ever supplies a default, never overrides a user's explicit
// config"). Looking the declaration up by name then offered every model the
// catalog declares for that name against the user's box — ids that box never
// advertised, which a launch would POST to it (2026-09-26 audit).
func TestRemoteLaunchModels_UserRowDoesNotInheritCatalogDeclaredModels(t *testing.T) {
	withTempHome(t)
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	writeRemotes(t, `{"remotes":[{"name":"minimax-coding-plan","base_url":"http://127.0.0.1:31999/anthropic","wire":"anthropic"}]}`)

	r, _, ok := findUserRemoteForModel("minimax-coding-plan/my-local-model")
	if !ok {
		t.Fatal("the user's own row must resolve (user config wins over the catalog)")
	}
	if r.BaseURL != "http://127.0.0.1:31999/anthropic" {
		t.Fatalf("resolved the wrong row: %+v", r)
	}

	// This box answers its own (tiny) model list, with no sweep error.
	models, err := remoteLaunchModels(r, []string{"my-local-model"}, nil)
	if err != nil {
		t.Fatalf("remoteLaunchModels: %v", err)
	}
	var foreign []string
	for _, m := range models {
		if id, ok := strings.CutPrefix(m.Name, r.Name+"/"); ok && id != "my-local-model" {
			foreign = append(foreign, id)
		}
	}
	if len(foreign) > 0 {
		t.Errorf("the picker offers %d model id(s) this remote never advertised (%v), because the catalog's "+
			"declared list for the NAME %q is applied to the user's own row; a launch would POST %q to %s",
			len(foreign), foreign, r.Name, foreign[0], r.BaseURL)
	}
}

// A catalog row's env[] is ordered by popularity upstream, not by what a given
// user holds: taking env[0] hides this provider from anyone with the second
// variable set, which is what the fixture's zai-coding-plan would do to a
// Z_AI_API_KEY holder (2026-09-26 models.dev plan, task 5).
func TestBuiltinRemotes_MultiEnvFirstSetWins(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	clearAllCatalogKeys(t)
	writeModelsDevCache(t, modelsDevFixture)
	t.Setenv("Z_AI_API_KEY", "k") // the SECOND variable in zai-coding-plan's env[]

	r := findBuiltinRemote(t, builtinRemotes(), "zai-coding-plan")
	if r == nil {
		t.Fatal("zai-coding-plan must be offered when its second env var is set")
	}
	if r.APIKeyEnv != "Z_AI_API_KEY" {
		t.Fatalf("gated on %q, want the variable that is actually set", r.APIKeyEnv)
	}
}

// The other half: a row whose credential is not held must not be offered at
// all — it would launch a request with no bearer.
func TestBuiltinRemotes_NoEnvSetMeansNotOffered(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	clearAllCatalogKeys(t)
	writeModelsDevCache(t, modelsDevFixture)

	remotes := builtinRemotes()
	for _, name := range []string{"zai-coding-plan", "groq"} {
		if r := findBuiltinRemote(t, remotes, name); r != nil {
			t.Fatalf("%s offered with no credential set: %+v", name, *r)
		}
	}
	t.Setenv("GROQ_API_KEY", "k")
	if r := findBuiltinRemote(t, builtinRemotes(), "groq"); r == nil {
		t.Fatal("groq must be offered once GROQ_API_KEY is set")
	} else if r.APIKeyEnv != "GROQ_API_KEY" {
		t.Fatalf("groq gated on %q, want GROQ_API_KEY", r.APIKeyEnv)
	}
}

// A row the overlay marks hidden cannot work through a plain base URL (the
// SDK-signed group: Bedrock, Vertex, Azure). Offering it offers the user a
// provider that cannot succeed, however well credentialed they are.
func TestBuiltinRemotes_HiddenProviderNotOffered(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	clearAllCatalogKeys(t)
	writeModelsDevCache(t, modelsDevFixture)
	writeOverlayCache(t, `{"providers":[{"name":"sdk-signed-vendor","base_url":"https://sdk.example/v1","env":["SDK_SIGNED_KEY"],"hidden":true}]}`)

	t.Setenv("SDK_SIGNED_KEY", "k")
	if r := findBuiltinRemote(t, builtinRemotes(), "sdk-signed-vendor"); r != nil {
		t.Fatalf("hidden provider offered: %+v", *r)
	}
	// The flag is not a blanket drop: the same row without it IS offered.
	writeOverlayCache(t, `{"providers":[{"name":"sdk-signed-vendor","base_url":"https://sdk.example/v1","env":["SDK_SIGNED_KEY"]}]}`)
	if r := findBuiltinRemote(t, builtinRemotes(), "sdk-signed-vendor"); r == nil {
		t.Fatal("the same row without hidden must be offered when its credential is set")
	}
}

// A row with no endpoint cannot be launched at all, whatever credentials the
// user holds: models.dev states some vendors' endpoints per model or not at
// all, and the catalog keeps those rows rather than dropping them.
func TestBuiltinRemotes_NoEndpointNotOffered(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	clearAllCatalogKeys(t)
	writeModelsDevCache(t, modelsDevFixture)
	writeOverlayCache(t, `{"providers":[{"name":"endpointless","api_key_env":"ENDPOINTLESS_KEY","env":["ENDPOINTLESS_KEY"]}]}`)
	t.Setenv("ENDPOINTLESS_KEY", "k")

	if r := findBuiltinRemote(t, builtinRemotes(), "endpointless"); r != nil {
		t.Fatalf("a row with no base URL was offered: %+v", *r)
	}
}

// A user's own remotes.json row of the same name wins outright: its URL and
// its key, not just its URL.
func TestBuiltinRemotes_UserRemoteBeatsCatalogIncludingItsKey(t *testing.T) {
	setLaunchTestHome(t, t.TempDir())
	clearAllCatalogKeys(t)
	writeModelsDevCache(t, modelsDevFixture)
	writeRemotes(t, `{"remotes":[{"name":"groq","base_url":"http://my-groq-proxy:8080/v1","api_key":"mine"}]}`)

	if r := findBuiltinRemote(t, builtinRemotes(), "groq"); r != nil {
		t.Fatalf("builtin groq must be shadowed by the user's row, got %+v", *r)
	}
	got, ok := findUserRemoteByName("groq")
	if !ok || got.BaseURL != "http://my-groq-proxy:8080/v1" || got.APIKey != "mine" {
		t.Fatalf("user's groq = %+v, %v", got, ok)
	}
}
