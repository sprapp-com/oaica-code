package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// useTempAuthStore points the credential store at a path under t.TempDir, so
// no test can read or write the developer's real ~/.oaica/auth.json. Every
// test that touches stored credentials must call this first: a machine with a
// real login would otherwise make the gating assertions pass (or fail) for
// the wrong reason.
func useTempAuthStore(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "auth.json")
	t.Setenv("OAICA_AUTH_FILE", path)
	return path
}

func TestAuthStore_MissingFileIsEmptyNotAnError(t *testing.T) {
	path := useTempAuthStore(t)
	f, gotPath, err := loadAuthStore()
	if err != nil {
		t.Fatalf("loadAuthStore() on a missing file errored: %v", err)
	}
	if gotPath != path {
		t.Fatalf("loadAuthStore() path = %q, want %q", gotPath, path)
	}
	if len(f.Providers) != 0 {
		t.Fatalf("loadAuthStore() providers = %v, want empty", f.Providers)
	}
	if storedAuthKey(zaiName) != "" || hasStoredAuth(zaiName) {
		t.Fatal("storedAuthKey/hasStoredAuth reported a credential with no file on disk")
	}
}

func TestAuthStore_CorruptFileDegradesToEmpty(t *testing.T) {
	path := useTempAuthStore(t)
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	// storedAuthKey/hasStoredAuth must swallow the parse error: a corrupt
	// store downgrades a provider to "needs a key", it does not abort a launch.
	if got := storedAuthKey(zaiName); got != "" {
		t.Fatalf("storedAuthKey() = %q on a corrupt store, want empty", got)
	}
	if hasStoredAuth(zaiName) {
		t.Fatal("hasStoredAuth() = true on a corrupt store")
	}
	// AuthList surfaces the error instead of hiding it, so the user learns
	// the file needs attention.
	if err := AuthList(&bytes.Buffer{}); err == nil {
		t.Fatal("AuthList() on a corrupt store returned nil, want an error naming the file")
	}
}

func TestAuthStore_EmptyProvidersObjectIsUsable(t *testing.T) {
	path := useTempAuthStore(t)
	if err := os.WriteFile(path, []byte(`{"version":1,"providers":null}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if storedAuthKey(zaiName) != "" {
		t.Fatal("storedAuthKey() found a credential in a null providers map")
	}
	// A login against a null map must not panic on assignment to a nil map.
	var out bytes.Buffer
	if err := AuthLogin(&out, zaiName, "zai-secret"); err != nil {
		t.Fatalf("AuthLogin() against null providers map: %v", err)
	}
	if got := storedAuthKey(zaiName); got != "zai-secret" {
		t.Fatalf("storedAuthKey() = %q, want zai-secret", got)
	}
}

func TestAuthLogin_StoresKeyAndLocksFileDown(t *testing.T) {
	path := useTempAuthStore(t)
	var out bytes.Buffer
	if err := AuthLogin(&out, zaiName, "  zai-secret  "); err != nil {
		t.Fatalf("AuthLogin(): %v", err)
	}
	if got := storedAuthKey(zaiName); got != "zai-secret" {
		t.Fatalf("storedAuthKey() = %q, want the trimmed key", got)
	}

	f, _, err := loadAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	c := f.Providers[zaiName]
	if c.Type != authCredentialTypeAPIKey {
		t.Fatalf("credential type = %q, want %q", c.Type, authCredentialTypeAPIKey)
	}
	if c.Label != providerCatalogPlanLabel(t, zaiName) {
		t.Fatalf("credential label = %q, want the catalog plan label", c.Label)
	}
	if c.SavedAt.IsZero() {
		t.Fatal("credential saved_at is zero")
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store mode = %o, want 600", perm)
	}
	// The key must never be echoed back to the terminal.
	if strings.Contains(out.String(), "zai-secret") {
		t.Fatalf("AuthLogin() printed the key: %q", out.String())
	}
	if !strings.Contains(out.String(), maskKey("zai-secret")) {
		t.Fatalf("AuthLogin() output %q lacks the masked key", out.String())
	}
}

// A pre-existing 0644 store (hand-edit, or a copy from a looser system) must
// be tightened by the next write rather than left world-readable.
func TestAuthLogin_TightensLooseFilePermissions(t *testing.T) {
	path := useTempAuthStore(t)
	if err := os.WriteFile(path, []byte(`{"version":1,"providers":{}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := AuthLogin(&bytes.Buffer{}, zaiName, "zai-secret"); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Fatalf("store mode after write = %o, want 600", perm)
	}
}

func TestAuthLogin_UnknownProviderRejected(t *testing.T) {
	useTempAuthStore(t)
	t.Setenv("OAICA_REMOTES_FILE", filepath.Join(t.TempDir(), "remotes.json"))

	err := AuthLogin(&bytes.Buffer{}, "definitely-not-a-provider", "k")
	if err == nil {
		t.Fatal("AuthLogin() accepted a provider neither in the catalog nor in remotes.json")
	}
	if !strings.Contains(err.Error(), "not a provider oaica knows") {
		t.Fatalf("AuthLogin() error = %v, want it to say the provider is unknown", err)
	}
}

// A name that is only in the user's own remotes.json is a legitimate login
// target — that is how a self-hosted or unlisted endpoint gets a stored key.
func TestAuthLogin_AcceptsUserRemoteName(t *testing.T) {
	useTempAuthStore(t)
	dir := t.TempDir()
	path := filepath.Join(dir, "remotes.json")
	body := `{"remotes":[{"name":"my-box","base_url":"http://127.0.0.1:9/v1","api_key_env":"MY_BOX_KEY"}]}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OAICA_REMOTES_FILE", path)
	t.Setenv("MY_BOX_KEY", "")

	if err := AuthLogin(&bytes.Buffer{}, "my-box", "box-key"); err != nil {
		t.Fatalf("AuthLogin() on a user remote: %v", err)
	}
	if got := storedAuthKey("my-box"); got != "box-key" {
		t.Fatalf("storedAuthKey(my-box) = %q, want box-key", got)
	}
}

func TestAuthLogin_NonTTYWithoutKeyErrors(t *testing.T) {
	useTempAuthStore(t)
	// The test binary's stdin is not a terminal under `go test`, so an
	// omitted key must produce a clear error rather than blocking on a read
	// from a pipe that will never deliver one.
	if err := AuthLogin(&bytes.Buffer{}, zaiName, ""); err == nil {
		t.Fatal("AuthLogin() with no key and a non-terminal stdin returned nil, want an error")
	}
}

// The whole point of the store: a paid plan becomes selectable without an
// exported env var. Ordering (env > store > inline) is asserted separately.
func TestBuiltinRemotes_GatedOnStoredCredential(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)

	if got := findBuiltinRemote(t, builtinRemotes(), zaiName); got != nil {
		t.Fatalf("builtinRemotes() has %q with neither env nor stored key", zaiName)
	}
	if err := AuthLogin(&bytes.Buffer{}, zaiName, "zai-secret"); err != nil {
		t.Fatal(err)
	}
	z := findBuiltinRemote(t, builtinRemotes(), zaiName)
	if z == nil {
		t.Fatalf("builtinRemotes() missing %q after a stored login", zaiName)
	}
	if got := z.key(); got != "zai-secret" {
		t.Fatalf("stored key() = %q, want zai-secret", got)
	}
	// zai-coding-plan shares Z_AI_API_KEY, but a stored credential is keyed
	// on the provider name, so logging into "zai" must not also unlock it.
	if got := findBuiltinRemote(t, builtinRemotes(), "zai-coding-plan"); got != nil {
		t.Fatal("a stored credential for zai leaked to zai-coding-plan")
	}
}

func TestRemoteKey_EnvBeatsStoredWhichBeatsInline(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)

	r := userRemote{Name: zaiName, BaseURL: "https://api.z.ai/api/paas", APIKeyEnv: zaiEnvKey, APIKey: "inline-key"}

	// 3rd: inline key only.
	if got := r.key(); got != "inline-key" {
		t.Fatalf("key() with no env and no store = %q, want inline-key", got)
	}
	// 2nd: the store overrides the inline key.
	if err := AuthLogin(&bytes.Buffer{}, zaiName, "stored-key"); err != nil {
		t.Fatal(err)
	}
	if got := r.key(); got != "stored-key" {
		t.Fatalf("key() with a stored credential = %q, want stored-key", got)
	}
	// 1st: the env var wins over everything on disk.
	t.Setenv(zaiEnvKey, "env-key")
	if got := r.key(); got != "env-key" {
		t.Fatalf("key() with the env var set = %q, want env-key", got)
	}
}

func TestAuthLogout_RemovesCredentialCaseInsensitively(t *testing.T) {
	useTempAuthStore(t)
	if err := AuthLogin(&bytes.Buffer{}, zaiName, "zai-secret"); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := AuthLogout(&out, strings.ToUpper(zaiName)); err != nil {
		t.Fatalf("AuthLogout() with a differently-cased name: %v", err)
	}
	if got := storedAuthKey(zaiName); got != "" {
		t.Fatalf("storedAuthKey() = %q after logout, want empty", got)
	}
	if strings.Contains(out.String(), "zai-secret") {
		t.Fatalf("AuthLogout() printed the key: %q", out.String())
	}

	// Logging out something that was never stored is a no-op, not an error:
	// scripts run it defensively.
	out.Reset()
	if err := AuthLogout(&out, zaiName); err != nil {
		t.Fatalf("AuthLogout() on an absent credential: %v", err)
	}
	if !strings.Contains(out.String(), "nothing to remove") {
		t.Fatalf("AuthLogout() output = %q, want it to report nothing was removed", out.String())
	}
}

// Logout must not silently claim the provider is unusable: a key supplied by
// the environment or inline still works, and the output says so.
func TestAuthLogout_NotesEnvStillSuppliesTheKey(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	if err := AuthLogin(&bytes.Buffer{}, zaiName, "zai-secret"); err != nil {
		t.Fatal(err)
	}
	t.Setenv(zaiEnvKey, "env-key")

	var out bytes.Buffer
	if err := AuthLogout(&out, zaiName); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(out.String(), zaiEnvKey) {
		t.Fatalf("AuthLogout() output = %q, want it to mention %s", out.String(), zaiEnvKey)
	}
}

func TestAuthList_ReportsStatusesAndMasksKeys(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)

	// Stored-only: ready, via the store.
	if err := AuthLogin(&bytes.Buffer{}, zaiName, "zai-secret-value"); err != nil {
		t.Fatal(err)
	}
	// Env: ready, via the env var — and the env var is the reported source.
	t.Setenv(ollamaCloudEnvKey, "ollama-secret-value")

	var out bytes.Buffer
	if err := AuthList(&out); err != nil {
		t.Fatalf("AuthList(): %v", err)
	}
	got := out.String()
	for _, want := range []string{
		"PROVIDER",
		"STATUS",
		"CREDENTIAL",
		"zai",
		"stored",
		"env:" + ollamaCloudEnvKey,
		"needs key",
		"run: oaica auth login " + zaiName,
	} {
		if !strings.Contains(got, want) {
			t.Fatalf("AuthList() output missing %q:\n%s", want, got)
		}
	}
	// No raw secret may appear, in any form.
	for _, secret := range []string{"zai-secret-value", "ollama-secret-value"} {
		if strings.Contains(got, secret) {
			t.Fatalf("AuthList() leaked %q:\n%s", secret, got)
		}
	}
}

// A stored credential for a provider the catalog does not carry would
// otherwise be invisible, and `auth logout <it>` would look like a no-op.
func TestAuthList_ShowsStoreOnlyEntries(t *testing.T) {
	useTempAuthStore(t)
	f, path, err := loadAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	f.Providers["retired-box"] = authCredential{Type: authCredentialTypeAPIKey, Key: "retired-secret-value"}
	if err := saveAuthStore(f, path); err != nil {
		t.Fatal(err)
	}

	var out bytes.Buffer
	if err := AuthList(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "retired-box") || !strings.Contains(got, "outside the catalog") {
		t.Fatalf("AuthList() output lacks the store-only section:\n%s", got)
	}
	if strings.Contains(got, "retired-secret-value") {
		t.Fatalf("AuthList() leaked a store-only key:\n%s", got)
	}
}

func TestMaskKey(t *testing.T) {
	tests := []struct {
		name string
		key  string
		want string
	}{
		{"empty", "", "(none)"},
		{"blank", "   ", "(none)"},
		{"short key is fully starred", "sk-1234", "*******"},
		{"eight chars is still fully starred", "12345678", "********"},
		{"long key keeps only its ends", "sk-abcdefghijklmnop-1234", "sk-a********1234"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := maskKey(tt.key); got != tt.want {
				t.Fatalf("maskKey(%q) = %q, want %q", tt.key, got, tt.want)
			}
		})
	}
}

// The store is keyed by provider name and read per call, so a login in one
// process is visible to the next — no restart, no caching layer to invalidate.
func TestStoredAuthKey_ReadsFreshFromDisk(t *testing.T) {
	path := useTempAuthStore(t)
	if err := os.WriteFile(path, []byte(`{"version":1,"providers":{"zai":{"type":"api_key","key":"first"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := storedAuthKey(zaiName); got != "first" {
		t.Fatalf("storedAuthKey() = %q, want first", got)
	}
	if err := os.WriteFile(path, []byte(`{"version":1,"providers":{"zai":{"type":"api_key","key":"second"}}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := storedAuthKey(zaiName); got != "second" {
		t.Fatalf("storedAuthKey() = %q after an external rewrite, want second", got)
	}
}

func TestStoredAuthKey_IgnoresNonAPIKeyCredentialTypes(t *testing.T) {
	path := useTempAuthStore(t)
	body := `{"version":1,"providers":{"zai":{"type":"` + authCredentialTypeOAuth + `","key":"an-oauth-token"}}}`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	// An oauth entry is not a bearer that can be sent as-is; resolving it as
	// one would send a token nothing has refreshed.
	if got := storedAuthKey(zaiName); got != "" {
		t.Fatalf("storedAuthKey() = %q for an oauth entry, want empty", got)
	}
}

func TestSortedAuthProviders(t *testing.T) {
	f := authStoreFile{Providers: map[string]authCredential{
		"kimi-code-plan-global": {Type: authCredentialTypeAPIKey, Key: "k"},
		"minimax":               {Type: authCredentialTypeAPIKey, Key: "m"},
		"zai":                   {Type: authCredentialTypeAPIKey, Key: "z"},
	}}
	got := sortedAuthProviders(f)
	want := []string{"kimi-code-plan-global", "minimax", "zai"}
	if len(got) != len(want) {
		t.Fatalf("sortedAuthProviders() = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("sortedAuthProviders() = %v, want %v", got, want)
		}
	}
}

// nowUTC is a seam so saved_at is assertable rather than "some time".
func TestAuthLogin_UsesTheNowSeam(t *testing.T) {
	useTempAuthStore(t)
	fixed := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	orig := nowUTC
	nowUTC = func() time.Time { return fixed }
	t.Cleanup(func() { nowUTC = orig })

	if err := AuthLogin(&bytes.Buffer{}, zaiName, "zai-secret"); err != nil {
		t.Fatal(err)
	}
	f, _, err := loadAuthStore()
	if err != nil {
		t.Fatal(err)
	}
	if got := f.Providers[zaiName].SavedAt; !got.Equal(fixed) {
		t.Fatalf("saved_at = %v, want %v", got, fixed)
	}
}

// providerCatalogPlanLabel reads the expected label from providers.json rather
// than hardcoding a second copy that drifts when labels are reworded.
func providerCatalogPlanLabel(t *testing.T, name string) string {
	t.Helper()
	for _, p := range providerCatalog() {
		if p.Name == name {
			return p.PlanLabel
		}
	}
	t.Fatalf("provider catalog has no %q entry", name)
	return ""
}
