package launch

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
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

// OpencodeAuthPath returns the modern path to opencode's auth.json. It is the
// WRITE path only for a caller that has no readers — everything on the read
// side goes through opencodeAuthPaths, which honours $OPENCODE_AUTH_FILE and
// $XDG_DATA_HOME, so a writer using this constant directly could report
// success for a key no reader would ever look for (2026-09-26 audit).
// opencodeStorePath is the path the WRITER should use.
func OpencodeAuthPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".local", "share", "opencode", "auth.json"), nil
}

// opencodeStorePath is the single store `oaica signin opencode:<provider>`
// reads and writes: the FIRST location a reader would consult. Writing
// anywhere else is what let the command print "Saved API key" while
// opencodeCredential found nothing — with $OPENCODE_AUTH_FILE set (a
// non-standard install, or a test) the readers consult that path and no other.
func opencodeStorePath() (string, error) {
	if paths := opencodeAuthPaths(); len(paths) > 0 {
		return paths[0], nil
	}
	return "", errors.New("no opencode auth store location could be resolved")
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
	path, err := opencodeStorePath()
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

// SaveOpencodeAPIKey writes one provider's API key into the opencode store
// readers use, preserving every other provider entry untouched.
//
// It refuses to replace an entry that is not itself an API key. An OAuth entry
// carries a refresh token and an expiry that this command cannot re-mint — the
// overwrite left the user with a store that looked signed in and was not, and
// only `opencode auth login <provider>` could put it back (2026-09-26 audit).
func SaveOpencodeAPIKey(provider, key string) error {
	path, err := opencodeStorePath()
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	// The store is read and written as the raw JSON document it is, not
	// through opencodeAuthEntry: that struct carries five of the members a
	// provider entry can have, and marshalling it back over the file deleted
	// every other one — for every provider in the file, so adding a key for
	// one provider silently signed the user out of another whose entry type
	// oaica does not model (2026-09-26 audit). UseNumber is the same rule's
	// other half: a number this package cannot represent must survive the
	// round-trip rather than come back as a float.
	doc, err := opencodeAuthReadDocument(path)
	if err != nil {
		return err
	}
	entry := doc[provider]
	if entry == nil {
		entry = map[string]any{}
	}
	if existing, _ := entry["type"].(string); existing != "" {
		if t := strings.ToLower(strings.TrimSpace(existing)); t != "api" && t != "api_key" {
			return fmt.Errorf("opencode already has a %q credential for %s (access token, refresh token and expiry) that an API key cannot replace — run `opencode auth login %s`, or remove that entry by hand first", existing, provider, provider)
		}
	}
	entry["type"] = "api"
	entry["key"] = key
	doc[provider] = entry

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return err
	}
	return fileutil.WriteWithBackup(path, data, "opencode")
}

// opencodeAuthReadDocument reads opencode's auth.json as the document it is:
// one map per provider, untouched by any Go struct. A file that cannot be
// parsed is REFUSED rather than rewritten — oaica writes one provider entry,
// so overwriting a document it cannot read would delete the user's other
// logins (store_document.go's discipline).
func opencodeAuthReadDocument(path string) (map[string]map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return map[string]map[string]any{}, nil
		}
		return nil, err
	}
	doc := map[string]map[string]any{}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	if err := dec.Decode(&doc); err != nil {
		return nil, fmt.Errorf("refusing to update %s: it is not valid JSON (%v) — oaica writes only the one provider entry it was asked for, so rewriting a file it cannot read would delete every other login in it", path, err)
	}
	return doc, nil
}
