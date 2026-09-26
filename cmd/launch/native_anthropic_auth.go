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
	"net/http"
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

// oauthBetaHeaderValue is the anthropic-beta value an OAuth bearer from
// `claude /login` requires — Anthropic rejects the bearer without it.
//
// It has to be applied wherever oaica INJECTS such a credential, and not
// merely hoped for from the client: whether a client sends this beta is a
// function of how that client is authenticated, and on a native leg the client
// is a child oaica pointed at a loopback proxy whose Authorization header is
// the PROXY's token (anthropicPassthrough's header loop replaces it). Two
// requests are on this footing — resolveNativeModelAlias's /v1/models GET,
// which has no client to copy headers from at all, and every POST the
// /v1/messages passthrough forwards with the native credential. Without the
// header an OAuth-only user's lookup 401s (which is what made native alias
// resolution silently no-op for exactly the users the native picker rows exist
// for) and every turn of theirs 401s.
//
// anthropicPassthrough and applyNativeAnthropicAuth are where it is applied;
// both merge it into what the client sent rather than replacing it, because the
// client's own beta values carry prompt caching and dropping one would be a
// silent downgrade of a request the fix was not about.
const oauthBetaHeaderValue = "oauth-2025-04-20"

// applyNativeAnthropicAuth puts auth on req the way the corresponding live
// Anthropic request would carry it: the credential header, plus the beta
// header when the credential is an OAuth bearer.
func applyNativeAnthropicAuth(req *http.Request, auth nativeAnthropicAuth) {
	req.Header.Set(auth.Header, auth.Value)
	if auth.Header == "Authorization" {
		req.Header.Set("anthropic-beta", mergeAnthropicBeta(req.Header.Get("anthropic-beta"), oauthBetaHeaderValue))
	}
}

// mergeAnthropicBeta appends beta to a client-supplied anthropic-beta value,
// comma-separated the way the header is defined, skipping it when it is already
// listed. The caller's value is preserved in place, so a client's own betas
// (prompt-caching-2024-07-31 among them) survive a header oaica had to add.
func mergeAnthropicBeta(clientValue, beta string) string {
	for _, v := range strings.Split(clientValue, ",") {
		if strings.EqualFold(strings.TrimSpace(v), beta) {
			return clientValue
		}
	}
	if strings.TrimSpace(clientValue) == "" {
		return beta
	}
	return clientValue + "," + beta
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
