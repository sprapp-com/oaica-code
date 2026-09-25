package launch

// auth_external.go — credentials OTHER agent CLIs have already stored, reused
// instead of re-prompted. `oaica auth login <provider>` was the only way to
// make a paid plan usable (auth_store.go); this layer lets a provider that
// opencode has ALREADY logged in to work in oaica with no second login, no
// duplicated secret, and no re-implementation of a login flow oaica does not
// own.
//
// Read-only, always: nothing here writes another tool's file. A refresh we
// performed ourselves would race that tool's own refresh and, on a bad
// response, corrupt a login the user needs for work — the same reasoning
// native_anthropic_auth.go records for not refreshing Claude Code's token.
//
// Allowlist, never a name collision: a provider is only allowed to read an
// external store when its catalog row declares `auth_via` (provider_catalog.go).
// Two tools naming a provider the same thing is a coincidence, not evidence
// that one's key belongs to the other, so an undeclared row can never source a
// credential here — the picker's old behavior (env var or `oaica auth login`)
// stays exactly as it was for every row that says nothing.
//
// Precedence stays env > oaica store > external > inline remotes.json
// (userRemote.key). An external store is an external store: a key the user
// explicitly handed oaica wins over one another tool happened to leave on disk.

import (
	"encoding/json"
	"os"
	"strings"
	"time"
)

// externalSourceOpencode is the value a catalog row writes in auth_via to
// source its credential from opencode's own login. Its store is keyed by
// provider id in exactly the shape oaica's provider names already use
// (`zai-coding-plan`, `minimax-coding-plan`, ... — verified against a real
// store 2026-09-25), which is what makes reuse possible with no mapping table.
const externalSourceOpencode = "opencode"

// externalCredential is the outcome of consulting one external store for one
// provider. Key == "" with Reason != "" means the provider IS present in that
// store but the credential is unusable right now (an expired OAuth token) —
// the caller prints Reason rather than pretending the provider is unconfigured.
type externalCredential struct {
	Key    string // usable bearer; "" when none
	Source string // e.g. "opencode" — the store it came from
	Reason string // why an existing credential could not be used
}

// externalAuthSource is one supported store. paths() lists candidates in
// priority order (first readable wins); read() turns its bytes into a
// credential for a provider id.
type externalAuthSource struct {
	Name  string
	paths func() []string
	// loginArgv is the command that populates this store for a provider —
	// both printed as the fix ("opencode auth login zai-coding-plan") and
	// exec'd verbatim by `oaica auth login --via opencode` (auth_cli.go), so
	// the two can never drift apart. The user runs THEIR tool's own login:
	// that is the whole point, no oaica-specific flow to learn or maintain.
	loginArgv func(provider string) []string
	read      func([]byte, string) (externalCredential, bool)
}

// externalAuthSources is the registry. Adding a store (codex's chatgpt OAuth,
// Claude Code's credentials file, Gemini's, ...) is one entry here plus an
// `auth_via` value in providers.json — never a new login flow in this package.
func externalAuthSources() []externalAuthSource {
	return []externalAuthSource{opencodeAuthSource()}
}

// externalAuthSourceByName resolves a catalog row's auth_via value.
func externalAuthSourceByName(name string) (externalAuthSource, bool) {
	want := strings.ToLower(strings.TrimSpace(name))
	if want == "" {
		return externalAuthSource{}, false
	}
	for _, s := range externalAuthSources() {
		if strings.ToLower(s.Name) == want {
			return s, true
		}
	}
	return externalAuthSource{}, false
}

// externalAuthFor consults the store a provider's catalog row declares. ok is
// false when nothing is declared, the declaration names an unknown store, or
// the store simply has no entry for this provider — callers treat all three
// the same way (fall through to the next credential source). A present-but-
// unusable credential comes back with ok true and Reason set, so the picker can
// say "expired, re-login" instead of "needs key".
func externalAuthFor(via, provider string) (externalCredential, bool) {
	src, ok := externalAuthSourceByName(via)
	if !ok {
		return externalCredential{}, false
	}
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return externalCredential{}, false
	}
	for _, path := range src.paths() {
		b, err := os.ReadFile(path)
		if err != nil {
			continue
		}
		cred, ok := src.read(b, provider)
		if !ok {
			continue
		}
		cred.Source = src.Name
		return cred, true
	}
	return externalCredential{}, false
}

// externalAuthKey is the bare "can this provider authenticate right now" check
// used by userRemote.key() and builtinRemotes' gate.
func externalAuthKey(via, provider string) string {
	cred, ok := externalAuthFor(via, provider)
	if !ok {
		return ""
	}
	return cred.Key
}

// externalLoginCommand is what a user runs to populate the external store for
// this provider, e.g. "opencode auth login zai-coding-plan". Shown in `oaica
// auth list` and by the picker's "needs key" rows, so the fix for a gated
// provider is stated in terms of the tool that actually owns the credential.
func externalLoginCommand(via, provider string) string {
	src, ok := externalAuthSourceByName(via)
	if !ok {
		return ""
	}
	return strings.Join(src.loginArgv(provider), " ")
}

// externalLoginArgv is the command itself, for the delegating
// `oaica auth login --via <source>` path (auth_cli.go's AuthLoginVia).
func externalLoginArgv(via, provider string) ([]string, bool) {
	src, ok := externalAuthSourceByName(via)
	if !ok {
		return nil, false
	}
	return src.loginArgv(provider), true
}

// externalStoreExists reports whether the named source has a store on this
// machine at all. It decides which login a "needs key" row points at: a user
// who already runs opencode should be told `opencode auth login X` (one login
// for both tools), while a user who has never installed it should be told
// `oaica auth login X` rather than sent to install something else first.
func externalStoreExists(via string) bool {
	src, ok := externalAuthSourceByName(via)
	if !ok {
		return false
	}
	for _, path := range src.paths() {
		if _, err := os.Stat(path); err == nil {
			return true
		}
	}
	return false
}

// externalLoginArgvString is the printable form, "" when nothing is declared
// (so callers do not print a dangling "run: ").
func externalLoginArgvString(via, provider string) string {
	argv, ok := externalLoginArgv(via, provider)
	if !ok {
		return ""
	}
	return strings.Join(argv, " ")
}

// externalAuthSourceNames lists the accepted --via values, for error text.
func externalAuthSourceNames() []string {
	var out []string
	for _, s := range externalAuthSources() {
		out = append(out, s.Name)
	}
	return out
}

// opencodeAuthSource reads opencode's login store — the file opencode's own
// `auth login` writes and `oaica signin opencode:<provider>` also writes to
// (opencode_auth.go, which owns the format: {"type":"api","key":...} and
// {"type":"oauth","access":...,"expires":<ms>}). The parsing lives there so
// the reader and the writer can never disagree about what the file looks like;
// this is only the registry entry wiring it into the reuse chain.
func opencodeAuthSource() externalAuthSource {
	return externalAuthSource{
		Name:  externalSourceOpencode,
		paths: opencodeAuthPaths,
		loginArgv: func(provider string) []string {
			return []string{"opencode", "auth", "login", provider}
		},
		read: readOpencodeCredential,
	}
}

// readOpencodeCredential adapts one store's bytes to an externalCredential,
// via the format owner's parser.
func readOpencodeCredential(b []byte, provider string) (externalCredential, bool) {
	var entries map[string]opencodeAuthEntry
	if json.Unmarshal(b, &entries) != nil {
		return externalCredential{}, false
	}
	return opencodeCredentialFor(entries, provider)
}

// opencodeOAuthSkew is how far ahead of expiry a token stops counting as
// usable. A token valid for the next 30 seconds is a request that fails
// upstream with a 401 mid-conversation; re-logging in first is the better
// failure.
const opencodeOAuthSkew = 5 * time.Minute

// opencodeTokenExpired reports whether an expiry stamp has passed (minus the
// skew). A zero stamp means opencode did not record one; that is treated as
// still-valid, matching the "no refresh, let the upstream 401 speak" rule in
// native_anthropic_auth.go rather than refusing a credential that may work.
func opencodeTokenExpired(expires int64, now time.Time) bool {
	if expires <= 0 {
		return false
	}
	secs := expires
	// opencode writes milliseconds since epoch. A stamp that large cannot be
	// seconds (1e12 seconds is year 33658), so the unit is unambiguous and a
	// seconds-shaped file from an older store still reads correctly.
	if secs > 1_000_000_000_000 {
		secs /= 1000
	}
	return now.Add(opencodeOAuthSkew).Unix() >= secs
}
