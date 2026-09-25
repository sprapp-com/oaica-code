package launch

import (
	"bytes"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"
)

// useOpencodeStore points the opencode reuse chain at a fixture file and
// returns its path. Every test that asserts anything about reused
// credentials must call this: without it the lookup reads the developer's
// real ~/.local/share/opencode/auth.json.
func useOpencodeStore(t *testing.T, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "opencode-auth.json")
	if body != "" {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("OPENCODE_AUTH_FILE", path)
	return path
}

// The format opencode actually writes: {"type":"api","key":"..."}. Verified
// against a real store (opencode_auth.go), and the one thing a reader of
// another tool's file cannot afford to get wrong — a reader that only
// understood "api_key" would report every logged-in provider as unconfigured.
func TestExternalAuth_ReadsOpencodeAPIShape(t *testing.T) {
	useOpencodeStore(t, `{"zai-coding-plan":{"type":"api","key":"oc-key"}}`)
	cred, ok := externalAuthFor(externalSourceOpencode, "zai-coding-plan")
	if !ok {
		t.Fatal("externalAuthFor() found nothing in a store that has the provider")
	}
	if cred.Key != "oc-key" {
		t.Fatalf("credential key = %q, want oc-key", cred.Key)
	}
	if cred.Source != externalSourceOpencode {
		t.Fatalf("credential source = %q, want %q", cred.Source, externalSourceOpencode)
	}
	if cred.Reason != "" {
		t.Fatalf("credential reported a reason for a usable key: %q", cred.Reason)
	}
}

// An oauth entry with a live access token is a usable bearer; one past its
// expiry is reported (not silently dropped) so callers can say "re-login"
// instead of "needs key" to someone who did log in.
func TestExternalAuth_OpencodeOAuth(t *testing.T) {
	future := time.Now().Add(24 * time.Hour).UnixMilli()
	past := time.Now().Add(-time.Hour).UnixMilli()

	useOpencodeStore(t, `{"live":{"type":"oauth","access":"tok-live","refresh":"r","expires":`+strconv.FormatInt(future, 10)+`},
	                      "dead":{"type":"oauth","access":"tok-dead","refresh":"r","expires":`+strconv.FormatInt(past, 10)+`},
	                      "bare":{"type":"oauth"}}`)

	live, ok := externalAuthFor(externalSourceOpencode, "live")
	if !ok || live.Key != "tok-live" {
		t.Fatalf("live oauth token: got %+v ok=%v, want the access token", live, ok)
	}
	dead, ok := externalAuthFor(externalSourceOpencode, "dead")
	if !ok {
		t.Fatal("an expired oauth entry must be reported, not treated as absent")
	}
	if dead.Key != "" {
		t.Fatalf("expired token returned a key %q — we must not send an expired credential", dead.Key)
	}
	if !strings.Contains(dead.Reason, "expired") || !strings.Contains(dead.Reason, "opencode auth login dead") {
		t.Fatalf("expired token reason = %q, want it to say expired and name the fix", dead.Reason)
	}
	bare, _ := externalAuthFor(externalSourceOpencode, "bare")
	if bare.Key != "" || bare.Reason == "" {
		t.Fatalf("oauth entry with no access token = %+v, want a reason and no key", bare)
	}
}

// A store that predates a field, or was hand-edited, must not be read as
// "expired in year 56000" or "already expired" — the unit is inferred.
func TestOpencodeTokenExpired_UnitAndSkew(t *testing.T) {
	now := time.Date(2026, 9, 25, 12, 0, 0, 0, time.UTC)
	tests := []struct {
		name    string
		expires int64
		want    bool
	}{
		{"zero means opencode recorded no expiry", 0, false},
		{"milliseconds, one hour out", now.Add(time.Hour).UnixMilli(), false},
		{"milliseconds, one hour ago", now.Add(-time.Hour).UnixMilli(), true},
		{"seconds, one hour out", now.Add(time.Hour).Unix(), false},
		{"seconds, one hour ago", now.Add(-time.Hour).Unix(), true},
		{"inside the skew window is already unusable", now.Add(time.Minute).UnixMilli(), true},
		{"just past the skew window is still usable", now.Add(10 * time.Minute).UnixMilli(), false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := opencodeTokenExpired(tt.expires, now); got != tt.want {
				t.Fatalf("opencodeTokenExpired(%d) = %v, want %v", tt.expires, got, tt.want)
			}
		})
	}
}

// Allowlist: a provider whose catalog row says nothing about external stores
// can never be authenticated by one, however exactly the ids match. This is
// the whole safety property of auth_via.
func TestExternalAuth_UndeclaredProviderNeverReadsStore(t *testing.T) {
	useOpencodeStore(t, `{"openai":{"type":"api","key":"someone-elses-key"}}`)

	if got := externalAuthKey("", "openai"); got != "" {
		t.Fatalf("externalAuthKey with no auth_via = %q, want empty", got)
	}
	if got := externalAuthKey("some-unknown-store", "openai"); got != "" {
		t.Fatalf("externalAuthKey with an unknown source = %q, want empty", got)
	}
	// openai's catalog row deliberately does NOT declare auth_via (its plan
	// credential is a subscription OAuth shape, not a bearer), so no catalog
	// row may source a key for it from opencode.
	for _, e := range providerCatalog() {
		if e.Name == "openai" || e.Name == "anthropic" || e.Name == "github-copilot" {
			if e.AuthVia != "" {
				t.Fatalf("%s must not declare auth_via %q — its credential is a subscription OAuth shape whose bearer needs different headers", e.Name, e.AuthVia)
			}
		}
	}
}

// Rows that DO declare it must actually resolve through the store, and a
// declared row with nothing in the store must stay gated (that is what makes
// the row "needs a key" rather than "works").
func TestBuiltinRemotes_ReusesOpencodeLogin(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{}`)

	if got := findBuiltinRemote(t, builtinRemotes(), "zai-coding-plan"); got != nil {
		t.Fatal("builtinRemotes() has zai-coding-plan with no credential anywhere")
	}

	useOpencodeStore(t, `{"zai-coding-plan":{"type":"api","key":"oc-key"},"minimax-coding-plan":{"type":"api","key":"mm-key"}}`)
	z := findBuiltinRemote(t, builtinRemotes(), "zai-coding-plan")
	if z == nil {
		t.Fatal("builtinRemotes() missing zai-coding-plan after opencode logged in to it")
	}
	if got := z.key(); got != "oc-key" {
		t.Fatalf("reused key = %q, want oc-key", got)
	}
	if got := findBuiltinRemote(t, builtinRemotes(), "minimax-coding-plan"); got == nil {
		t.Fatal("builtinRemotes() missing minimax-coding-plan — a second declared row must reuse too")
	}
	// A provider that did not declare auth_via stays unavailable even though
	// the store has a similarly-shaped id.
	if got := findBuiltinRemote(t, builtinRemotes(), "openai"); got != nil {
		t.Fatal("openai became available from opencode's store without declaring auth_via")
	}
}

// Precedence: an env var, then a key the user gave oaica explicitly, then
// another tool's stored login, then an inline remotes.json key.
func TestRemoteKey_EnvBeatsStoreWhichBeatsExternalWhichBeatsInline(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{"zai":{"type":"api","key":"oc-key"},"openrouter":{"type":"api","key":"oc-openrouter"}}`)

	r := userRemote{Name: zaiName, BaseURL: "https://api.z.ai/api/paas", APIKeyEnv: zaiEnvKey, APIKey: "inline-key", AuthVia: externalSourceOpencode}

	if got := r.key(); got != "oc-key" {
		t.Fatalf("key() with only an external login = %q, want oc-key", got)
	}
	if err := AuthLogin(&bytes.Buffer{}, zaiName, "stored-key"); err != nil {
		t.Fatal(err)
	}
	if got := r.key(); got != "stored-key" {
		t.Fatalf("key() with an explicit oaica login = %q, want the stored key to outrank the external one", got)
	}
	t.Setenv(zaiEnvKey, "env-key")
	if got := r.key(); got != "env-key" {
		t.Fatalf("key() with the env var set = %q, want env-key", got)
	}

	// Undeclared row of the same NAME: the external store is invisible to it,
	// so its inline key is what gets used (the name matches zai, which has a
	// stored credential by now — that is exactly the leak auth_via prevents,
	// so the assertion is on the store, not the name).
	plain := userRemote{Name: openrouterName, APIKey: "inline-key"}
	if got := plain.key(); got != "inline-key" {
		t.Fatalf("key() without auth_via = %q, want the inline key (an external credential must not leak to an undeclared row)", got)
	}
}

// A corrupt or absent store must degrade to "no credential", never break a
// launch: this file belongs to another tool and can be mid-write.
func TestExternalAuth_CorruptOrMissingStoreIsEmpty(t *testing.T) {
	useOpencodeStore(t, "{not json")
	if cred, ok := externalAuthFor(externalSourceOpencode, zaiName); ok {
		t.Fatalf("corrupt store produced %+v, want nothing", cred)
	}
	useOpencodeStore(t, "")
	if cred, ok := externalAuthFor(externalSourceOpencode, zaiName); ok {
		t.Fatalf("missing store produced %+v, want nothing", cred)
	}
}

// The command printed as "the fix" and the command --via executes must be the
// same one, or the docs and the behavior drift apart.
func TestExternalLoginCommandMatchesExecutedArgv(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)

	want := "opencode auth login zai-coding-plan"
	if got := externalLoginCommand(externalSourceOpencode, "zai-coding-plan"); got != want {
		t.Fatalf("externalLoginCommand() = %q, want %q", got, want)
	}
	argv, ok := externalLoginArgv(externalSourceOpencode, "zai-coding-plan")
	if !ok {
		t.Fatal("externalLoginArgv() unknown for opencode")
	}
	if got := strings.Join(argv, " "); got != want {
		t.Fatalf("externalLoginArgv() = %q, want %q", got, want)
	}
	if _, ok := externalLoginArgv("no-such-source", "zai"); ok {
		t.Fatal("externalLoginArgv() accepted an unknown source")
	}
}

// AuthLoginVia must refuse an unknown source before it spawns anything, and
// must name the supported ones.
func TestAuthLoginVia_UnknownSource(t *testing.T) {
	var out bytes.Buffer
	err := AuthLoginVia(&out, "zai", "not-a-store")
	if err == nil {
		t.Fatal("AuthLoginVia() with an unknown source returned nil")
	}
	if !strings.Contains(err.Error(), "unknown credential source") || !strings.Contains(err.Error(), externalSourceOpencode) {
		t.Fatalf("AuthLoginVia() error = %v, want it to name the source and list opencode", err)
	}
	if out.Len() != 0 {
		t.Fatalf("AuthLoginVia() wrote %q before failing", out.String())
	}
	// A missing provider name is a usage error, not a delegation.
	if err := AuthLoginVia(&out, "", externalSourceOpencode); err == nil {
		t.Fatal("AuthLoginVia() with no provider returned nil")
	}
}

// AuthList must attribute a reused credential to the store it came from, so
// "where is this key from" is answerable without reading any file by hand.
func TestAuthList_AttributesReusedCredential(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	useOpencodeStore(t, `{"zai-coding-plan":{"type":"api","key":"oc-secret-value"}}`)

	var out bytes.Buffer
	if err := AuthList(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, externalSourceOpencode) {
		t.Fatalf("AuthList() does not name the external source:\n%s", got)
	}
	if strings.Contains(got, "oc-secret-value") {
		t.Fatalf("AuthList() leaked the reused key:\n%s", got)
	}
	// A provider that is not in the store must point at the tool's own login
	// rather than at `oaica auth login`, since that is the flow auth_via names.
	if !strings.Contains(got, "run: opencode auth login minimax-coding-plan") {
		t.Fatalf("AuthList() does not point at opencode's login for a declared-but-absent provider:\n%s", got)
	}
}

// An expired reused token must be explained, not reported as an unconfigured
// provider — otherwise the user re-authenticates in the wrong tool.
func TestAuthList_ExplainsExpiredReusedCredential(t *testing.T) {
	useTempAuthStore(t)
	clearAllCatalogKeys(t)
	past := time.Now().Add(-time.Hour).UnixMilli()
	useOpencodeStore(t, `{"zai-coding-plan":{"type":"oauth","access":"tok","refresh":"r","expires":`+strconv.FormatInt(past, 10)+`}}`)

	var out bytes.Buffer
	if err := AuthList(&out); err != nil {
		t.Fatal(err)
	}
	got := out.String()
	if !strings.Contains(got, "could not use") || !strings.Contains(got, "expired") {
		t.Fatalf("AuthList() does not explain the expired credential:\n%s", got)
	}
	if !strings.Contains(got, "run: opencode auth login zai-coding-plan") {
		t.Fatalf("AuthList() does not name the fix for the expired credential:\n%s", got)
	}
	if strings.Contains(got, "tok") && strings.Contains(got, "access") {
		t.Fatalf("AuthList() leaked token material:\n%s", got)
	}
}
