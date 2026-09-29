package launch

// catalog_sync.go — `oaica model catalog sync`: fetch the models.dev payload,
// check it against the contract, and either adopt it or refuse it and keep the
// last good copy.
//
// Nothing auto-fetches. A launch reads whatever is cached and works offline;
// an explicit command is the only thing that talks to the network. That is a
// deliberate divergence from opencode, which treats the catalog as live
// infrastructure with a 5-minute refresh and a cross-process file lock — oaica
// is a one-shot CLI with no resident process to host a background refresher,
// so paying a 5 MB fetch on a timer would only tax launches that are already
// network-bound.
//
// Mechanics are model_sync.go's: ETag revalidation, a last-good fallback when
// the network is down, and file:// for air-gapped hosts and tests.

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

const defaultCatalogSyncURL = "https://models.dev/api.json"

func catalogCachePath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".oaica", "cache", "catalog", "modelsdev.json"), nil
}

// CatalogSyncReport is what CatalogSync returns for the caller to print.
type CatalogSyncReport struct {
	URL       string
	Providers int
	Models    int
	FromCache bool
	Unchanged bool
	Refused   bool
}

// CatalogSync fetches the payload at url (empty = defaultCatalogSyncURL),
// validates it, and adopts it only if it passes. A contract failure is
// returned as an error naming the field, with the cached catalog left
// untouched — the refusal is the entire safety story while the drift tooling
// (Plan B) does not exist yet.
func CatalogSync(url string) (CatalogSyncReport, error) {
	if strings.TrimSpace(url) == "" {
		url = defaultCatalogSyncURL
	}

	// display is what any report or message may print: a --url may carry a mirror
	// credential, and the same redacted form is the identity the cache and its ETag
	// are bound to, as ProviderSync's are (2026-09-29 audit, round 115, F115-L2-2/3).
	display := redactBaseURL(url)

	cachePath, err := catalogCachePath()
	if err != nil {
		return CatalogSyncReport{URL: display}, err
	}

	etag := loadCatalogETag(cachePath+".etag", display, defaultCatalogSyncURL)

	body, newEtag, fromCache, err := fetchCatalogBody(url, etag, cachePath)
	if err != nil {
		return CatalogSyncReport{URL: display}, err
	}
	if fromCache && !catalogCacheSourceMatches(cachePath, display, defaultCatalogSyncURL) {
		return CatalogSyncReport{URL: display}, fmt.Errorf("can't reach %s, and the catalog cached at %s came from a different source — run this again while online", display, cachePath)
	}
	// A cache that will not parse, with the ETag of the whole body beside it (an
	// interrupted write left it), answers 304 for ever, and the byte-identical early
	// return below reported "unchanged, 0 providers" with exit 0 until the upstream's
	// ETag happened to change. Fetch it again without the ETag; the fresh body and its
	// new ETag replace both (2026-09-29 audit, round 112, F112-L2-2). Removing the old
	// ETag first, and writing the ETag atomically, were measured not load-bearing.
	if fromCache && etag != "" {
		if _, perr := parseModelsDevCatalog(body); perr != nil {
			body, newEtag, fromCache, err = fetchCatalogBody(url, "", cachePath)
			if err != nil {
				return CatalogSyncReport{URL: display}, err
			}
		}
	}

	// The body IS the cache — a 304, or the transport fell back to it because the source
	// could not be reached. That is not "unchanged": a sync that did not hear from its
	// source must not say it did, and a cache that will not parse must not be reported as
	// a good catalog (ProviderSync refuses the same state). The byte-identical check below
	// always matched here, so an offline run over a torn cache answered "unchanged, 0
	// providers" with exit 0 (2026-09-29 audit, round 116, F116-L2-1).
	if fromCache {
		f, perr := parseModelsDevCatalog(body)
		if perr != nil {
			return CatalogSyncReport{URL: display}, fmt.Errorf("the cached catalog at %s is not readable as a models.dev payload (%v) — run this again while online", cachePath, perr)
		}
		pn, mn := f.counts()
		return CatalogSyncReport{URL: display, Providers: pn, Models: mn, FromCache: true}, nil
	}

	// Byte-identical to what we already hold: nothing to validate, nothing to
	// write. (The ETag path usually catches this; file:// has no ETag.)
	if prev, rerr := os.ReadFile(cachePath); rerr == nil && sameBytes(prev, body) {
		f, _ := parseModelsDevCatalog(body)
		pn, mn := f.counts()
		// The bytes are proven to be THIS source's, so the source and validator are
		// recorded as ProviderSync always does: left as they were, a cache confirmed
		// against D stayed bound to the mirror it was first fetched from, D was refetched in
		// full on every sync, and D unreachable was refused for holding "a different
		// source's" cache (2026-09-29 audit, round 117, F117-L2-1).
		saveCatalogETag(cachePath+".etag", display, newEtag)
		saveCatalogCacheSource(cachePath, display)
		return CatalogSyncReport{URL: display, Providers: pn, Models: mn, Unchanged: true}, nil
	}

	// The contract check runs on the RAW body and before the parse, because a
	// field retyped upstream (limit.context arriving as a string) fails
	// json.Unmarshal outright — and the only thing that can then name the
	// FIELD is the raw tree. Reporting "not valid JSON" there would refuse the
	// payload correctly but tell a human nothing about what moved.
	if fails := validateModelsDevContractRaw(body); len(fails) > 0 {
		lines := make([]string, 0, len(fails))
		for i, fl := range fails {
			if i == 5 {
				lines = append(lines, fmt.Sprintf("... and %d more", len(fails)-i))
				break
			}
			lines = append(lines, fl.String())
		}
		return CatalogSyncReport{URL: display, Refused: true},
			fmt.Errorf("refused: %s does not match the fields oaica reads:\n  %s\n(cached catalog left in place)",
				display, strings.Join(lines, "\n  "))
	}

	// Backstop for a shape the contract does not constrain (a field we read but
	// do not validate, or a nested type the raw walk does not reach): an
	// unreadable body is never cached.
	f, perr := parseModelsDevCatalog(body)
	if perr != nil {
		return CatalogSyncReport{URL: display, Refused: true},
			fmt.Errorf("refused: %s is not readable as the models.dev catalog: %w (cached catalog left in place)", display, perr)
	}

	if !fromCache {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			return CatalogSyncReport{URL: display}, err
		}
		// Atomic, as ProviderSync's cache is: os.WriteFile truncates the live path
		// first, so a write that failed part-way (a full disk, Ctrl-C, SIGKILL during
		// `oaica model catalog sync`) destroyed the last good catalog and left the
		// picker with none (2026-09-29 audit, round 112, F112-L2-2). The body goes
		// first and the ETag after it, so an ETag never names a body that is not there.
		if err := fileutil.WriteFileAtomic(cachePath, body, 0o600); err != nil {
			return CatalogSyncReport{URL: display}, err
		}
		saveCatalogETag(cachePath+".etag", display, newEtag)
		saveCatalogCacheSource(cachePath, display)
	}

	pn, mn := f.counts()
	return CatalogSyncReport{URL: display, Providers: pn, Models: mn, FromCache: fromCache}, nil
}

// loadModelsDevCatalog returns the cached catalog and whether one exists. It
// never fetches: the picker calls this on every build, offline or not.
func loadModelsDevCatalog() (modelsDevFile, bool) {
	path, err := catalogCachePath()
	if err != nil {
		return modelsDevFile{}, false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return modelsDevFile{}, false
	}
	f, err := parseModelsDevCatalog(b)
	if err != nil {
		return modelsDevFile{}, false
	}
	return f, true
}

// catalogAgeDays reports how stale the cached payload is, or 0 when it is
// missing or unreadable. The picker shows this past 30 days so staleness is
// visible rather than silent.
func catalogAgeDays(now time.Time) int {
	path, err := catalogCachePath()
	if err != nil {
		return 0
	}
	fi, err := os.Stat(path)
	if err != nil {
		return 0
	}
	days := int(now.Sub(fi.ModTime()).Hours() / 24)
	if days < 0 {
		return 0
	}
	return days
}

// CatalogStatus is the cached catalog's provenance, for `oaica model catalog
// status`: where it came from, how old it is, what it hashes to, and what it
// holds. No network — the command answers on a box with no route out, which is
// exactly when somebody wants to know what is cached.
func CatalogStatus() (CatalogSyncReport, string) {
	path, err := catalogCachePath()
	if err != nil {
		return CatalogSyncReport{}, "catalog: no cache path (" + err.Error() + ")"
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return CatalogSyncReport{}, fmt.Sprintf("catalog: none cached at %s — run `oaica model catalog sync`", path)
	}
	f, perr := parseModelsDevCatalog(b)
	if perr != nil {
		return CatalogSyncReport{}, fmt.Sprintf("catalog: %s is not readable as a models.dev payload (%v) — run `oaica model catalog sync`", path, perr)
	}
	sum := sha256.Sum256(b)
	models := 0
	for _, p := range f.Providers {
		models += len(p.Models)
	}
	days := catalogAgeDays(time.Now())
	age := fmt.Sprintf("%d days", days)
	if days == 0 {
		age = "today"
	} else if days == 1 {
		age = "1 day"
	}
	line := fmt.Sprintf("catalog: %s — %d providers, %d models, synced %s ago, sha256 %s",
		path, len(f.Providers), models, age, hex.EncodeToString(sum[:])[:16])
	return CatalogSyncReport{Providers: len(f.Providers), Models: models, FromCache: true}, line
}

func sameBytes(a, b []byte) bool {
	ha, hb := sha256.Sum256(a), sha256.Sum256(b)
	return hex.EncodeToString(ha[:]) == hex.EncodeToString(hb[:])
}

// fetchCatalogBody is the generic ETag fetch-and-cache shared with
// provider_sync.go. file:// reads the path directly and carries no ETag;
// a transport error falls back to whatever is already cached.
func fetchCatalogBody(url, etag, cachePath string) (body []byte, newEtag string, fromCache bool, err error) {
	if strings.HasPrefix(url, "file://") {
		b, rerr := os.ReadFile(strings.TrimPrefix(url, "file://"))
		return b, "", false, rerr
	}

	// A --url may carry a mirror's credential as userinfo
	// (https://KEY@mirror/api.json) — that is how a private mirror
	// authenticates. The request keeps it; nothing printed or wrapped may:
	// every message below uses the redacted form (redact.go, and the same rule
	// provider_sync.go states for the identical input).
	display := redactBaseURL(url)
	req, err := NewRedactedRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second, CheckRedirect: credentialSafeRedirect}).Do(req)
	if err != nil {
		if b, rerr := os.ReadFile(cachePath); rerr == nil {
			return b, etag, true, nil
		}
		return nil, "", false, fmt.Errorf("couldn't reach %s: %w", display, redactErr(err))
	}
	defer resp.Body.Close()

	switch {
	case resp.StatusCode == http.StatusNotModified:
		b, rerr := os.ReadFile(cachePath)
		if rerr != nil {
			return nil, "", false, fmt.Errorf("%s returned 304 but no cache exists", display)
		}
		return b, etag, true, nil
	case resp.StatusCode != http.StatusOK:
		errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		return nil, "", false, fmt.Errorf("%s: HTTP %d: %s", display, resp.StatusCode, PrintableCell(redactCredentials(strings.TrimSpace(string(errBody)))))
	}

	// 16 MiB cap: the live payload is ~5 MB, and the cap exists so a hostile or
	// broken response cannot exhaust memory.
	b, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, "", false, err
	}
	return b, resp.Header.Get("ETag"), false, nil
}
