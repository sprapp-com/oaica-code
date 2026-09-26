package launch

// auth_store.go — `oaica auth login`'s credential store, the piece that lets
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
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
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

// errAuthStoreUnparseable marks a store whose bytes are not the JSON this
// version understands. It is distinguished from "could not read it at all"
// because the write path may quarantine the former (the bytes are all there,
// they just need a human) and must not touch the latter.
var errAuthStoreUnparseable = errors.New("not valid JSON")

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
		return authStoreFile{Version: authStoreVersion, Providers: map[string]authCredential{}}, path, fmt.Errorf("%s is %w: %w", path, errAuthStoreUnparseable, err)
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
	// Members the struct does not model survive the rewrite — see
	// store_document.go.
	b, err := storeDocumentMergeValue(f, path)
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

// updateAuthStore is the ONLY way a caller mutates the store: it holds the
// cross-process lock across load → mutate → save, so two `oaica auth login`
// commands (a provisioning script, two terminals) cannot each read the same
// snapshot and have the last write silently drop the other's credential.
// Eleven concurrent logins used to leave one key (2026-09-26 audit, fourth
// round). Read-only callers keep using loadAuthStore directly.
//
// A store that cannot be PARSED does not block the write. It is moved aside —
// same directory, same mode, a name that says what it is — and the mutation
// starts from an empty store, with a notice on warn naming both files. Refusing
// instead meant one stray byte (a truncated file, a hand-edit, JSON from another
// tool) made every `oaica auth login` fail forever with a parse error, and the
// only way out was deleting credentials the user could not read: worse than the
// file being unreadable, and the same reasoning that keeps a corrupt plans.json
// from blocking every launch (2026-09-26 audit).
//
// A store that cannot be READ (permissions, a device error) is still an error.
// Its bytes are not necessarily recoverable by hand, and moving it would be
// guessing at a file that might be a mount the user cares about.
func updateAuthStore(warn io.Writer, mutate func(*authStoreFile) error) error {
	path := authStorePath()
	if path == "" {
		return fmt.Errorf("cannot locate ~/.oaica/auth.json (no home directory) — set OAICA_AUTH_FILE")
	}
	return fileutil.WithFileLock(path, func() error {
		f, _, err := loadAuthStore()
		if err != nil {
			if !errors.Is(err, errAuthStoreUnparseable) {
				return err
			}
			moved, qerr := quarantineAuthStore(path)
			if qerr != nil {
				return fmt.Errorf("%w, and it could not be moved aside either: %v", err, qerr)
			}
			fmt.Fprintf(warn, "warning: %v.\n", err)
			fmt.Fprintf(warn, "warning: it has been kept as %s and a new store written. Any credentials it held are in that file, which nothing has deleted.\n", moved)
			f = authStoreFile{Version: authStoreVersion, Providers: map[string]authCredential{}}
		}
		snapshot := storeDocumentSnapshot(f)
		if err := mutate(&f); err != nil {
			return err
		}
		if !storeDocumentChanged(snapshot, f) {
			// A rewrite with nothing to say re-serialises a partial view over
			// the file: `oaica auth logout <provider not logged in>` reported
			// success and deleted every member the struct does not model
			// (2026-09-26 audit). See store_document.go.
			return nil
		}
		return saveAuthStore(f, path)
	})
}

// quarantineStamp is the timestamp in a quarantined store's name. A seam so a
// test can pin it and exercise two quarantines landing on the same name — the
// case that used to destroy the first one.
var quarantineStamp = func() string { return time.Now().UTC().Format("20060102-150405") }

// quarantineAuthStore moves an unreadable store aside and returns where it
// went. The name carries the time, so two runs cannot overwrite each other's
// evidence: the file may hold credentials the user paid for, and the point is
// that it is still there afterwards.
//
// The timestamp is only to the second, and os.Rename REPLACES an existing
// destination, so the name alone is not the guarantee — two quarantines inside
// one second (a provisioning script writing the store twice, a repair that
// corrupts it again immediately) left one file and destroyed the credentials in
// the other, exactly what the notice above it promises did not happen. The
// destination is therefore claimed by finding a free name first. Only caller is
// updateAuthStore, which holds the store's cross-process lock, so no other
// oaica can be claiming a name in this directory at the same time.
func quarantineAuthStore(path string) (string, error) {
	base := fmt.Sprintf("%s.unreadable-%s", path, quarantineStamp())
	dest := base
	for n := 2; ; n++ {
		_, err := os.Lstat(dest)
		if errors.Is(err, fs.ErrNotExist) {
			break
		}
		if err != nil {
			return "", err
		}
		dest = fmt.Sprintf("%s-%d", base, n)
	}
	if err := os.Rename(path, dest); err != nil {
		return "", err
	}
	return dest, nil
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
// keys apart in `oaica auth list`, not enough to use.
//
// How much may be shown at each end scales with the key. A fixed four-and-four
// discloses EIGHT characters, and these outputs are documented as pasteable
// into a bug report — for the short keys some gateways issue ("sk-1a2b3c4d5",
// nine characters) that is the key: every character but one, in
// `oaica auth login`, `oaica auth list` and `oaica signin` (2026-09-26 audit,
// fifth round). At most a fifth of the key is shown from each end, so the
// hidden part is always the larger part, and the cap stays 4 for long keys so
// the familiar "sk-a********1234" shape is unchanged.
func maskKey(key string) string {
	key = strings.TrimSpace(key)
	if key == "" {
		return "(none)"
	}
	// Counted and sliced in RUNES, not bytes: a multibyte key sliced by byte
	// printed invalid UTF-8 (half a character at each end) and was masked by
	// halves, because nine two-byte runes are eighteen bytes and len/5 showed
	// four characters where the rule allows one (2026-09-26 audit, seventh
	// round).
	r := []rune(key)
	if len(r) <= 8 {
		return strings.Repeat("*", len(r))
	}
	n := len(r) / 5
	if n > 4 {
		n = 4
	}
	return string(r[:n]) + strings.Repeat("*", 8) + string(r[len(r)-n:])
}

func sortedAuthProviders(f authStoreFile) []string {
	out := make([]string, 0, len(f.Providers))
	for name := range f.Providers {
		out = append(out, name)
	}
	sort.Strings(out)
	return out
}
