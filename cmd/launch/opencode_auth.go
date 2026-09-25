package launch

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

// opencodeAuthEntry is one provider credential in opencode's auth.json
// (~/.local/share/opencode/auth.json — XDG_DATA_HOME/opencode on Linux,
// same tree macOS/Windows use via os.UserHomeDir()/.local/share). Verified
// against opencode's real on-disk format: a flat map of provider id to
// {"type":"api","key":"..."}.
//
// The oauth fields exist because opencode also stores subscription logins
// (its `auth login` against a plan provider that uses a browser flow) as
// {"type":"oauth","access":...,"refresh":...,"expires":<ms>}. oaica reads an
// unexpired access token from there but never refreshes it — that lifecycle
// belongs to opencode (see auth_external.go's read-only rule).
type opencodeAuthEntry struct {
	Type    string `json:"type"`
	Key     string `json:"key"`
	Access  string `json:"access"`
	Refresh string `json:"refresh"`
	// Expires is an epoch stamp; opencode writes milliseconds. Read by
	// opencodeTokenExpired (auth_external.go), which normalizes the unit.
	Expires int64 `json:"expires"`
}

// OpencodeAuthPath returns the path to opencode's auth.json.
func OpencodeAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "auth.json"), nil
}

// opencodeAuthPaths lists every place opencode's store may live, highest
// priority first: an explicit OPENCODE_AUTH_FILE (tests, and a non-standard
// install), XDG_DATA_HOME, then the two locations opencode has used over
// time — ~/.local/share and the older ~/.config. OpencodeAuthPath (the
// WRITE path, `oaica signin opencode:...`) deliberately stays on the modern
// single location; this list is for READING a store some version of opencode
// already wrote.
func opencodeAuthPaths() []string {
	if p := strings.TrimSpace(os.Getenv("OPENCODE_AUTH_FILE")); p != "" {
		return []string{p}
	}
	var out []string
	if xdg := strings.TrimSpace(os.Getenv("XDG_DATA_HOME")); xdg != "" {
		out = append(out, filepath.Join(xdg, "opencode", "auth.json"))
	}
	if home, err := os.UserHomeDir(); err == nil {
		out = append(out, filepath.Join(home, ".local", "share", "opencode", "auth.json"))
		out = append(out, filepath.Join(home, ".config", "opencode", "auth.json"))
	}
	return out
}

func readOpencodeAuth(path string) (map[string]opencodeAuthEntry, error) {
	auth := make(map[string]opencodeAuthEntry)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return auth, nil
		}
		return nil, err
	}
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("failed to parse opencode auth.json: %w", err)
	}
	return auth, nil
}

// opencodeCredentialFor turns one provider's raw entry into the credential
// shape the reuse layer wants (auth_external.go), deciding usability:
//
//   - "api" — the format opencode actually writes, a plain bearer. Usable.
//   - "oauth" — usable only while its access token is unexpired (plus skew).
//     Reported with a Reason rather than silently dropped, so `oaica auth
//     list` can say "expired, re-login" instead of "needs key" to someone who
//     did log in.
//   - anything else — reported with a Reason too: an entry type this package
//     does not model is a fact about the user's store, not a provider that
//     needs a key.
//
// ok is false only when the store has no entry at all for this provider.
func opencodeCredentialFor(entries map[string]opencodeAuthEntry, provider string) (externalCredential, bool) {
	entry, ok := entries[provider]
	if !ok {
		// opencode ids are lowercase models.dev ids, and our catalog uses the
		// same strings; the case-insensitive pass is for a hand-edited store.
		for id, e := range entries {
			if strings.EqualFold(id, provider) {
				entry, ok = e, true
				break
			}
		}
	}
	if !ok {
		return externalCredential{}, false
	}
	switch strings.ToLower(strings.TrimSpace(entry.Type)) {
	case "api", "api_key":
		key := strings.TrimSpace(entry.Key)
		if key == "" {
			return externalCredential{}, false
		}
		return externalCredential{Key: key}, true
	case "oauth":
		token := strings.TrimSpace(entry.Access)
		if token == "" {
			return externalCredential{Reason: fmt.Sprintf("opencode's OAuth entry for %s has no access token — run `opencode auth login %s`", provider, provider)}, true
		}
		if opencodeTokenExpired(entry.Expires, nowUTC()) {
			return externalCredential{Reason: fmt.Sprintf("opencode's OAuth token for %s expired — run `opencode auth login %s`", provider, provider)}, true
		}
		return externalCredential{Key: token}, true
	default:
		return externalCredential{Reason: fmt.Sprintf("opencode's %s credential has unsupported type %q", provider, entry.Type)}, true
	}
}

// opencodeCredential reads one provider's credential out of the first
// readable opencode store (opencodeAuthPaths).
func opencodeCredential(provider string) (externalCredential, bool) {
	for _, path := range opencodeAuthPaths() {
		entries, err := readOpencodeAuth(path)
		if err != nil {
			continue
		}
		if cred, ok := opencodeCredentialFor(entries, provider); ok {
			return cred, true
		}
	}
	return externalCredential{}, false
}

// OpencodeKnownProviders lists provider ids already present in opencode's
// auth.json, sorted, for offering as signin choices alongside "add a new one".
func OpencodeKnownProviders() []string {
	path, err := OpencodeAuthPath()
	if err != nil {
		return nil
	}
	auth, err := readOpencodeAuth(path)
	if err != nil {
		return nil
	}
	names := make([]string, 0, len(auth))
	for name := range auth {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// SaveOpencodeAPIKey writes/overwrites one provider's API key in opencode's
// auth.json, preserving every other provider entry untouched.
func SaveOpencodeAPIKey(provider, key string) error {
	path, err := OpencodeAuthPath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	auth, err := readOpencodeAuth(path)
	if err != nil {
		return err
	}
	auth[provider] = opencodeAuthEntry{Type: "api", Key: key}

	data, err := json.MarshalIndent(auth, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(path, data, "opencode")
}
