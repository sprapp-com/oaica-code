package cmd

// updatecheck.go — a best-effort, non-blocking "a newer oaica is available"
// notice, printed to stderr before a command runs. Modeled on the pattern
// every CLI tool with a hosted install path uses (npm, homebrew, etc): check
// rarely, cache the result, never let the check itself slow down or fail a
// real command.
//
// Why this exists: a real incident (2026-08-29) needed a client-side fix
// (the context-fit clamp) shipped as oaica 0.4.3, but there was no way to
// tell an already-installed client "you should upgrade" short of asking
// them directly. This closes that gap for every future fix the same way.

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ollama/ollama/version"
)

// updateCheckURL serves a bare "version=X.Y.Z\ncommit=..." file (see
// docs/RELEASE.md's release process) -- already the source of truth
// install.sh and every other version check use, so there's nothing new to
// keep in sync here. A package var, not a const, so tests can point it at
// a local httptest server -- see updateCheckURLForTest in the test file.
var updateCheckURL = "https://oaica.com/download/VERSION.txt"

// updateCheckURLForTest points updateCheckURL at url and returns a func
// that restores the real one -- call via defer.
func updateCheckURLForTest(url string) func() {
	old := updateCheckURL
	updateCheckURL = url
	return func() { updateCheckURL = old }
}

// updateCheckInterval bounds how often this hits the network at all -- once
// per invocation would be needless load on oaica.com and a latency tax on
// every command. A cache file records the last check time; anything more
// recent than this is skipped entirely (not even a network attempt).
const updateCheckInterval = 20 * time.Hour

// updateCheckTimeout bounds the network call itself. This runs before the
// user's actual command; a slow or unreachable oaica.com must not add
// perceptible delay to every invocation.
const updateCheckTimeout = 1500 * time.Millisecond

type updateCheckCache struct {
	LastChecked   time.Time `json:"last_checked"`
	LatestVersion string    `json:"latest_version"`
	// Notified guards against printing the SAME available version on every
	// single invocation for 20 hours straight -- once the user has seen it,
	// stay quiet about that version until a newer one shows up (or the
	// cache is cleared). Re-notifies automatically once LatestVersion
	// changes to something newer than what was last shown.
	Notified string `json:"notified,omitempty"`
}

func updateCheckCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "update_check.json"), nil
}

func loadUpdateCheckCache() updateCheckCache {
	path, err := updateCheckCachePath()
	if err != nil {
		return updateCheckCache{}
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return updateCheckCache{}
	}
	var c updateCheckCache
	json.Unmarshal(b, &c) //nolint:errcheck -- a corrupt cache just means "check again"
	return c
}

func saveUpdateCheckCache(c updateCheckCache) {
	path, err := updateCheckCachePath()
	if err != nil {
		return
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = os.WriteFile(path, b, 0o600)
}

// fetchLatestVersion parses "version=X.Y.Z" from updateCheckURL. Returns ""
// on any failure (network, timeout, unparseable) -- every caller treats
// that as "skip silently", matching this check's whole design: never let
// an update notice become a reason a real command fails or hangs.
func fetchLatestVersion(ctx context.Context) string {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, updateCheckURL, nil)
	if err != nil {
		return ""
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return ""
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return ""
	}
	var buf [256]byte
	n, _ := resp.Body.Read(buf[:])
	for _, line := range strings.Split(string(buf[:n]), "\n") {
		if v, ok := strings.CutPrefix(strings.TrimSpace(line), "version="); ok {
			return v
		}
	}
	return ""
}

// semverLess reports whether a < b for bare "X.Y.Z" versions. Not a full
// semver parser (no pre-release/build metadata) -- deliberately, since
// docs/RELEASE.md's own version stamping only ever produces bare semver for
// a real release (see "A 0.0.0 binary is a dev build by definition"). A
// malformed component compares as 0, so a genuinely weird version string
// never blocks the notice outright, it just sorts low.
func semverLess(a, b string) bool {
	pa, pb := strings.Split(a, "."), strings.Split(b, ".")
	for i := 0; i < 3; i++ {
		var na, nb int
		if i < len(pa) {
			na, _ = strconv.Atoi(pa[i])
		}
		if i < len(pb) {
			nb, _ = strconv.Atoi(pb[i])
		}
		if na != nb {
			return na < nb
		}
	}
	return false
}

// checkForUpdate is the entry point, called once per CLI invocation from
// NewCLI's PersistentPreRun. Fully non-blocking in spirit even though it
// runs synchronously with a short timeout: the timeout is short enough
// (updateCheckTimeout) that even a worst-case hang is imperceptible, and
// the common case (cached, no network call at all) is instant.
func checkForUpdate() {
	if version.Version == "0.0.0" {
		return // dev build; never nag a developer about "outdated" dev builds
	}
	if os.Getenv("OAICA_NO_UPDATE_CHECK") != "" {
		return
	}
	cache := loadUpdateCheckCache()
	latest := cache.LatestVersion
	if time.Since(cache.LastChecked) >= updateCheckInterval {
		ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
		defer cancel()
		if v := fetchLatestVersion(ctx); v != "" {
			latest = v
		}
		cache.LatestVersion = latest
		cache.LastChecked = time.Now()
		saveUpdateCheckCache(cache)
	}
	if latest == "" || !semverLess(version.Version, latest) {
		return
	}
	if cache.Notified == latest {
		return // already told the user about this exact version
	}
	fmt.Fprintf(os.Stderr, "\n\033[33m! oaica update available: %s -> %s\033[0m\n  Run: curl -fsSL https://oaica.com/install.sh | bash\n\n",
		version.Version, latest)
	cache.Notified = latest
	saveUpdateCheckCache(cache)
}
