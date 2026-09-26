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
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
	"github.com/ollama/ollama/version"
)

// updateCheckURL serves the release's own VERSION.txt -- a bare
// "version=X.Y.Z\ncommit=..." file. It points at the GitHub release rather
// than oaica.com/download/, which is what docs/RELEASE.md calls the source of
// truth: the release asset is written by the same workflow that builds the
// archives, so it cannot drift. The oaica.com copy is published by a manual
// Pages deploy nothing forces anyone to run, and it did drift -- on
// 2026-09-26 it still served 0.5.45 while the newest release was oaica-v0.5.46,
// so every installed client was told it was up to date (2026-09-26 audit). A
// package var, not a const, so tests can point it at a local httptest server --
// see updateCheckURLForTest in the test file.
var updateCheckURL = "https://github.com/sprapp-com/oaica-code/releases/latest/download/VERSION.txt"

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

// updateCheckRetryInterval is how long a FAILED attempt is remembered before
// trying again. Shorter than updateCheckInterval because a failure says
// nothing about the release — but not zero, because this runs before every
// command and an offline machine would otherwise wait out updateCheckTimeout
// on each one.
const updateCheckRetryInterval = 30 * time.Minute

// updateCheckTimeout bounds the network call itself. This runs before the
// user's actual command; a slow or unreachable oaica.com must not add
// perceptible delay to every invocation.
const updateCheckTimeout = 1500 * time.Millisecond

type updateCheckCache struct {
	// LastChecked is the last SUCCESSFUL check — the timestamp the freshness
	// of LatestVersion rests on. A failed attempt does not move it, so one
	// timeout (a hotel wifi, a 404, a captive portal) no longer silences the
	// notice for the whole interval (2026-09-26 audit, third round).
	LastChecked time.Time `json:"last_checked"`
	// LastAttempt is the last attempt of any kind, success or failure. It
	// backs off retries after a failure so a machine that is offline does not
	// pay updateCheckTimeout on every single invocation.
	LastAttempt   time.Time `json:"last_attempt,omitempty"`
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
	// 0o700, matching every other ~/.oaica creator. This check runs early on
	// many commands, so on a fresh machine it can be the FIRST thing to create
	// the directory — at 0o755 it left every later secret sitting in a
	// world-listable directory, which is not what README.md tells an operator
	// to expect. An already-existing directory is not touched (not ours to
	// silently rewrite); new ones are owner-only.
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return
	}
	b, err := json.Marshal(c)
	if err != nil {
		return
	}
	_ = fileutil.WriteFileAtomic(path, b, 0o600)
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
	// ReadAll, not one 256-byte Read. A single Read may return fewer bytes
	// than the file holds — as few as one — so a body of
	// "version=0.5.47\ncommit=…" could come back as "vers", parse to nothing,
	// and be treated as a failed check. Bounded anyway: this is a version
	// stamp, not a payload (2026-09-26 audit, third round).
	body, err := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if err != nil {
		return ""
	}
	for _, line := range strings.Split(string(body), "\n") {
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
		// A previous ATTEMPT that failed backs off for a shorter interval
		// rather than stamping LastChecked. The old code stamped it on every
		// attempt, so one failed check silenced the notice for 20 hours — the
		// exact failure the notice exists to prevent (2026-09-26 audit, third
		// round).
		recentFailure := cache.LastAttempt.After(cache.LastChecked) &&
			time.Since(cache.LastAttempt) < updateCheckRetryInterval
		if !recentFailure {
			ctx, cancel := context.WithTimeout(context.Background(), updateCheckTimeout)
			v := fetchLatestVersion(ctx)
			cancel()
			cache.LastAttempt = time.Now()
			if v != "" {
				cache.LatestVersion = v
				cache.LastChecked = time.Now()
			}
			saveUpdateCheckCache(cache)
		}
	}
	if latest == "" || !semverLess(version.Version, latest) {
		return
	}
	if cache.Notified == latest {
		return // already told the user about this exact version
	}
	// The install command names the release's own installer, not
	// oaica.com/install.sh: that copy is published by the same manual Pages
	// deploy that made this check report a stale version, so following it can
	// install something older than the notice just announced.
	fmt.Fprintf(os.Stderr, "\n\033[33m! oaica update available: %s -> %s\033[0m\n  Run: curl -fsSL https://github.com/sprapp-com/oaica-code/releases/latest/download/install.sh | bash\n\n",
		version.Version, latest)
	cache.Notified = latest
	saveUpdateCheckCache(cache)
}
