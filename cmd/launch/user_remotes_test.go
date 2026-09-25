package launch

import (
	"fmt"
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
		if p.APIKeyEnv == "" {
			continue // a catalog row with no credential (e.g. a reference-only entry) is never a builtin
		}
		t.Setenv(p.APIKeyEnv, "")
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
	r := userRemote{Name: "minimax-coding-plan", BaseURL: "https://api.minimax.io/anthropic/v1", Wire: "anthropic"}
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
	r := userRemote{Name: "minimax-coding-plan", BaseURL: "https://api.minimax.io/anthropic/v1"}
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
