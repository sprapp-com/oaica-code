package launch

// auth_store.go — `oaica provider login`'s credential store, the piece that lets
// a provider be used WITHOUT exporting an env var first. Until this existed
// a built-in provider was gated on `os.Getenv(APIKeyEnv)` alone
// (user_remotes.go's builtinRemotes), so a subscription plan a user had
// already paid for was invisible in the picker until they went and exported
// a variable — or hand-edited remotes.json, which then froze that provider's
// endpoint into the file (see remote_cli.go's note on why builtins are never
// baked in).
//
// Shape is deliberately close to opencode's ~/.oaica-equivalent auth.json:
// one entry per provider, an explicit `type` so an OAuth credential can be
// added later without a schema change, and never a bare string map. The
// file is 0600 and secrets are never printed by any command here.
//
// Resolution order for a remote's bearer, highest first:
//  1. the env var named by api_key_env — a secret stays off disk;
//  2. this store, keyed by the remote/provider name;
//  3. an `api_key` written inline in ~/.oaica/remotes.json.
//
// A user's own remotes.json entry of the same name still wins over the
// catalog (loadUserRemotes' existing dedupe); the store only supplies a
// credential, never an endpoint.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// authCredentialTypeAPIKey is the only type implemented. "oauth" is a
// declared constant rather than an accepted input: OAuth providers (GitHub
// Copilot is the one models.dev carries) need a device/redirect flow and a
// refresh token, and accepting the string before that flow exists would
// write credential entries nothing can refresh.
const (
	authCredentialTypeAPIKey = "api_key"
	authCredentialTypeOAuth  = "oauth"
)

type authCredential struct {
	Type    string    `json:"type"`
	Key     string    `json:"key,omitempty"`
	Label   string    `json:"label,omitempty"`
	SavedAt time.Time `json:"saved_at,omitzero"`
}

type authStoreFile struct {
	Version   int                       `json:"version"`
	Providers map[string]authCredential `json:"providers"`
}

const authStoreVersion = 1

// authStorePath is ~/.oaica/auth.json, overridable for tests and for a
// shared/read-only home (same escape hatch as userRemotesPath).
func authStorePath() string {
	if p := strings.TrimSpace(os.Getenv("OAICA_AUTH_FILE")); p != "" {
		return p
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}
	return filepath.Join(home, ".oaica", "auth.json")
}

// loadAuthStore reads the store. A missing or unreadable file is an empty
// store, not an error: losing credentials downgrades a provider to "needs a
// key again" and must never break an unrelated launch.
func loadAuthStore() (authStoreFile, string, error) {
	path := authStorePath()
	f := authStoreFile{Version: authStoreVersion, Providers: map[string]authCredential{}}
	if path == "" {
		return f, "", nil
	}
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return f, path, nil
		}
		return f, path, err
	}
	var onDisk authStoreFile
	if err := json.Unmarshal(b, &onDisk); err != nil {
		return authStoreFile{Version: authStoreVersion, Providers: map[string]authCredential{}}, path, fmt.Errorf("%s: %w", path, err)
	}
	if onDisk.Providers != nil {
		f.Providers = onDisk.Providers
	}
	return f, path, nil
}

func saveAuthStore(f authStoreFile, path string) error {
	if path == "" {
		return fmt.Errorf("cannot locate ~/.oaica/auth.json (no home directory) — set OAICA_AUTH_FILE")
	}
	f.Version = authStoreVersion
	if f.Providers == nil {
		f.Providers = map[string]authCredential{}
	}
	b, err := json.MarshalIndent(f, "", "  ")
	if err != nil {
		return err
	}
	b = append(b, '\n')
	if err := writeAtomic(path, b); err != nil {
		return err
	}
	// writeAtomic creates 0600; re-assert in case the file pre-existed with
	// looser permissions from a hand-edit.
	return os.Chmod(path, 0o600)
}

// storedAuthKey returns the API key saved for provider, or "".
func storedAuthKey(provider string) string {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		return ""
	}
	f, _, err := loadAuthStore()
	if err != nil {
		return ""
	}
	c, ok := f.Providers[provider]
	if !ok || c.Type != authCredentialTypeAPIKey {
		return ""
	}
	return strings.TrimSpace(c.Key)
}

// hasStoredAuth reports whether provider has ANY usable stored credential —
// what builtinRemotes gates on, so a logged-in plan shows up without an env
// var.
func hasStoredAuth(provider string) bool {
	return storedAuthKey(provider) != ""
}

// maskKey renders a key for display without printing it: enough to tell two
// keys apart in `oaica provider list`, not enough to use.
func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "(none)"
	}
	if len(key) <= 8 {
		return strings.Repeat("*", len(key))
	}
	return key[:4] + strings.Repeat("*", 8) + key[len(key)-4:]
}

func sortedAuthProviders(f authStoreFile) []string {
	out := make([]string, 0, len(f.Providers))
	for name := range f.Providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
