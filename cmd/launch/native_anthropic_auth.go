package launch

// native_anthropic_auth.go — the credential the native-passthrough proxy
// route (anthropic_openai_proxy.go's nativeAnthropicPassthrough) sends to
// api.anthropic.com, resolved fresh on every request so a mid-session
// re-login or key rotation takes effect immediately (same live-resolution
// principle as proxyRoute.resolveKey, one level more involved because
// native mode has two distinct credential shapes to pick between).
//
// NO REFRESH: an OAuth access token nearing/past its expiresAt is used
// as-is. Actually refreshing it needs Anthropic's real token endpoint and
// client_id, which this project has no verified source for — guessing it
// and writing a bad response back to ~/.claude/.credentials.json risks
// corrupting the user's real Claude Code login (2026-09-02 decision).
// An expired token simply gets Anthropic's own 401 back through the proxy
// untouched, exactly what would happen running Claude Code natively with
// that same stale token — the user re-runs `claude /login` as normal.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
)

// nativeAnthropicAuth is what the passthrough handler needs to authenticate
// upstream: either an OAuth bearer (Scheme "Bearer", requires the
// anthropic-beta oauth header Claude Code itself sends) or a plain API key
// (Scheme "x-api-key").
type nativeAnthropicAuth struct {
	Header string // "Authorization" or "x-api-key"
	Value  string // "Bearer <token>" for Authorization, or the raw key for x-api-key
}

// claudeCredentialsFile mirrors the one field this package reads out of
// ~/.claude/.credentials.json — the file `claude /login` writes. Every
// other field in that file (refreshToken, scopes, subscriptionType, ...) is
// intentionally not modeled: this package only ever reads it, never writes.
type claudeCredentialsFile struct {
	ClaudeAiOauth struct {
		AccessToken string `json:"accessToken"`
	} `json:"claudeAiOauth"`
}

// resolveNativeAnthropicAuth picks the credential exactly as native mode's
// own environment would have: ANTHROPIC_API_KEY wins when set (matches
// Claude Code's own precedence — an explicit key beats a stored login),
// otherwise the OAuth access token from claude /login's credentials file.
// Empty Value with ok=false means neither is available — the caller must
// fail the request rather than send an empty credential upstream.
func resolveNativeAnthropicAuth() (nativeAnthropicAuth, bool) {
	if key := strings.TrimSpace(os.Getenv("ANTHROPIC_API_KEY")); key != "" {
		return nativeAnthropicAuth{Header: "x-api-key", Value: key}, true
	}
	token, ok := readClaudeOAuthAccessToken()
	if !ok {
		return nativeAnthropicAuth{}, false
	}
	return nativeAnthropicAuth{Header: "Authorization", Value: "Bearer " + token}, true
}

// readClaudeOAuthAccessTokenFn is a var so tests can point it at a fixture
// file instead of the real ~/.claude/.credentials.json.
var readClaudeOAuthAccessTokenFn = readClaudeCredentialsFile

func readClaudeOAuthAccessToken() (string, bool) {
	tok, err := readClaudeOAuthAccessTokenFn()
	if err != nil || tok == "" {
		return "", false
	}
	return tok, true
}

func readClaudeCredentialsFile() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	b, err := os.ReadFile(filepath.Join(home, ".claude", ".credentials.json"))
	if err != nil {
		return "", err
	}
	var creds claudeCredentialsFile
	if err := json.Unmarshal(b, &creds); err != nil {
		return "", err
	}
	return strings.TrimSpace(creds.ClaudeAiOauth.AccessToken), nil
}
