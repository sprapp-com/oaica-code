package launch

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// stubBareIndex replaces the network-backed bare-id index for a test and
// restores it on cleanup.
func stubBareIndex(t *testing.T, idx map[string][]string) {
	t.Helper()
	old := bareRemoteModelIndex
	bareRemoteModelIndex = func() map[string][]string { return idx }
	t.Cleanup(func() { bareRemoteModelIndex = old })
}

// stubUserRemoteModels replaces the per-remote /v1/models sweep for a test and
// restores it on cleanup. This is the seam BELOW stubBareIndex: it also feeds
// modelInventory.load, so a test that exercises the inventory (ResolveAgentModel,
// showOrPull, ...) against a remotes.json with unreachable hosts must stub it
// or pay fetchRemoteModels' 6s timeout per call.
func stubUserRemoteModels(t *testing.T, models []LaunchModel, errs []error) {
	t.Helper()
	old := userRemoteLaunchModels
	userRemoteLaunchModels = func() ([]LaunchModel, []error) { return models, errs }
	t.Cleanup(func() { userRemoteLaunchModels = old })
}

// The default bare-id index is built from userRemoteLaunchModels, so stubbing
// the sweep is enough to keep resolveBareRemoteModel off the network.
func TestBareRemoteModelIndex_BuiltFromUserRemoteSweep(t *testing.T) {
	stubUserRemoteModels(t, []LaunchModel{
		{Name: "box/kat-awq", Remote: true},
		{Name: "other/deepseek-chat", Remote: true},
		{Name: "box/deepseek-chat", Remote: true},
		{Name: "llama3.2"}, // no "/" — never a bare-remote candidate
	}, nil)

	idx := bareRemoteModelIndex()
	if got := idx["kat-awq"]; len(got) != 1 || got[0] != "box/kat-awq" {
		t.Fatalf("idx[kat-awq] = %v, want [box/kat-awq]", got)
	}
	if got := idx["deepseek-chat"]; len(got) != 2 {
		t.Fatalf("idx[deepseek-chat] = %v, want both owners", got)
	}
	if _, ok := idx["llama3.2"]; ok {
		t.Fatal("un-namespaced entry must not appear in the bare index")
	}
}

// modelInventory.load reads user remotes through the same seam, so an
// inventory test never needs a reachable remote to see remote entries.
func TestModelInventoryLoad_UsesUserRemoteSweepSeam(t *testing.T) {
	t.Setenv("OAICA_HOST", "https://api.example.test") // skip local_servers.json discovery
	stubUserRemoteModels(t, []LaunchModel{{Name: "box/kat-awq", Remote: true}}, nil)
	stubCloudFetch(t, nil, errors.New("router down"))

	models, err := newModelInventory(nil).Load(context.Background())
	if err != nil {
		t.Fatalf("Load() error = %v, want remote entry to keep the inventory usable", err)
	}
	if len(models) != 1 || models[0].Name != "box/kat-awq" || !models[0].Remote {
		t.Fatalf("Load() = %+v, want just the stubbed remote entry", models)
	}
}

func writeRemotes(t *testing.T, body string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "remotes.json")
	if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAICA_REMOTES_FILE", p)
}

// The "Download <model>?" trap: `--model kat-awq` (the id the user's own box
// advertises) must resolve to that remote, not fall through to the Ollama
// registry pull path.
func TestFindUserRemoteForModel_BareNameResolvesWhenUnique(t *testing.T) {
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	writeRemotes(t, `{"remotes":[{"name":"kat-awq","base_url":"http://127.0.0.1:30099/v1"}]}`)
	stubBareIndex(t, map[string][]string{"kat-awq": {"kat-awq/kat-awq"}})

	r, bare, ok := findUserRemoteForModel("kat-awq")
	if !ok {
		t.Fatal("bare id served by exactly one remote must resolve")
	}
	if r.Name != "kat-awq" || bare != "kat-awq" {
		t.Fatalf("resolved (%q, %q), want (kat-awq, kat-awq)", r.Name, bare)
	}
}

// Ambiguity must NOT be guessed: two boxes serving "deepseek-chat" means the
// user has to say which one. Silently picking one would route traffic to the
// wrong endpoint.
func TestFindUserRemoteForModel_BareNameAmbiguousDoesNotResolve(t *testing.T) {
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	writeRemotes(t, `{"remotes":[{"name":"a","base_url":"http://a/v1"},{"name":"b","base_url":"http://b/v1"}]}`)
	stubBareIndex(t, map[string][]string{"deepseek-chat": {"a/deepseek-chat", "b/deepseek-chat"}})

	if _, _, ok := findUserRemoteForModel("deepseek-chat"); ok {
		t.Fatal("bare id served by two remotes must stay unresolved (ambiguous)")
	}
}

func TestFindUserRemoteForModel_BareNameUnknownDoesNotResolve(t *testing.T) {
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	writeRemotes(t, `{"remotes":[{"name":"kat-awq","base_url":"http://127.0.0.1:30099/v1"}]}`)
	stubBareIndex(t, map[string][]string{"kat-awq": {"kat-awq/kat-awq"}})

	if _, _, ok := findUserRemoteForModel("llama3.2"); ok {
		t.Fatal("a genuinely local model name must not be hijacked as a remote")
	}
}

// Explicit "<remote>/<id>" is unchanged and still wins even when the bare
// index would have been ambiguous.
func TestFindUserRemoteForModel_ExplicitFormUnaffected(t *testing.T) {
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	writeRemotes(t, `{"remotes":[{"name":"a","base_url":"http://a/v1"},{"name":"b","base_url":"http://b/v1"}]}`)
	stubBareIndex(t, map[string][]string{"deepseek-chat": {"a/deepseek-chat", "b/deepseek-chat"}})

	r, bare, ok := findUserRemoteForModel("b/deepseek-chat")
	if !ok || r.Name != "b" || bare != "deepseek-chat" {
		t.Fatalf("explicit form resolved (%v, %q, %q), want (true, b, deepseek-chat)", ok, r.Name, bare)
	}
}

// Vendor-prefixed OpenRouter ids round-trip through the explicit form: the
// bare part must keep its "vendor/" (remoteDisplayID no longer strips it).
func TestFindUserRemoteForModel_OpenRouterVendorIDKeptWhole(t *testing.T) {
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "or-key")
	writeRemotes(t, `{"remotes":[]}`)
	stubBareIndex(t, map[string][]string{})

	r, bare, ok := findUserRemoteForModel("openrouter/deepseek/deepseek-chat")
	if !ok || r.Name != openrouterName {
		t.Fatalf("openrouter builtin did not resolve: ok=%v name=%q", ok, r.Name)
	}
	if bare != "deepseek/deepseek-chat" {
		t.Fatalf("bare upstream id = %q, want deepseek/deepseek-chat (vendor prefix must survive)", bare)
	}
}

// clearCatalogKeys empties every credential env var a built-in remote
// could key off of — tests asserting an exact builtinRemotes() shape must
// run against a clean environment (dev boxes often export a dozen keys).
func clearCatalogKeys(t *testing.T) {
	t.Helper()
	for _, r := range catalogProviders {
		t.Setenv(r.APIKeyEnv, "")
	}
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "")
	t.Setenv(ollamaCloudEnvKey, "")
}

func TestBuiltinRemotes_OpenRouterKeyGate(t *testing.T) {
	clearCatalogKeys(t)
	if got := builtinRemotes(); len(got) != 0 {
		t.Fatalf("builtinRemotes() = %v, want none with no keys", got)
	}

	t.Setenv(openrouterEnvKey, "or-secret")
	got := builtinRemotes()
	if len(got) != 1 || got[0].Name != openrouterName {
		t.Fatalf("builtinRemotes() = %v, want exactly the openrouter remote", got)
	}
	or := got[0]
	if or.key() != "or-secret" {
		t.Fatalf("key() = %q, want or-secret", or.key())
	}
	if want := "https://openrouter.ai/api/v1"; or.openAIBase() != want {
		t.Fatalf("openAIBase() = %q, want %q", or.openAIBase(), want)
	}
	if d := or.Descriptor(); d.ToolFormat != "tool_calls" || !d.ToolReliable {
		t.Fatalf("descriptor = %+v, want reliable tool_calls (Claude Code gate must pass)", d)
	}
}

// A remotes.json entry named "openrouter" must override the builtin so a user
// can pin a different base URL, key, or tool_format without a code change.
func TestLoadUserRemotes_UserDefinedOverridesBuiltin(t *testing.T) {
	t.Setenv(zaiEnvKey, "")
	t.Setenv(openrouterEnvKey, "or-secret")
	writeRemotes(t, `{"remotes":[{"name":"openrouter","base_url":"http://proxy.local/api","api_key":"custom"}]}`)

	remotes, err := loadUserRemotes()
	if err != nil {
		t.Fatal(err)
	}
	var seen int
	for _, r := range remotes {
		if r.Name == openrouterName {
			seen++
			if r.BaseURL != "http://proxy.local/api" || r.key() != "custom" {
				t.Fatalf("user-defined openrouter did not win: %+v", r)
			}
		}
	}
	if seen != 1 {
		t.Fatalf("openrouter appears %d times, want exactly 1 (no builtin duplicate)", seen)
	}
}
