package launch

// cloud_limits_sync.go — `oaica model cloud-limits sync`: pull the hosted
// cloud-alias limits catalog, same ETag/offline-fallback shape as
// provider_sync.go and model_sync.go.

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

const defaultCloudLimitsSyncURL = "https://raw.githubusercontent.com/sprapp-com/oaica-code/main/cmd/launch/cloud_limits/cloud_limits.json"

// CloudLimitsSyncReport is what CloudLimitsSync returns for the caller to print.
type CloudLimitsSyncReport struct {
	URL       string
	Count     int
	FromCache bool
}

// CloudLimitsSync fetches the catalog at url (empty = default) and writes
// it to ~/.oaica/cache/cloud_limits/cloud_limits.json, where
// cloudLimitsFromCatalog() picks it up as an override on the embedded
// default.
func CloudLimitsSync(url string) (CloudLimitsSyncReport, error) {
	if strings.TrimSpace(url) == "" {
		url = defaultCloudLimitsSyncURL
	}
	// Same as ProviderSync: the request keeps the mirror credential, the report
	// the caller prints does not (2026-09-26 audit).
	display := redactBaseURL(url)

	cachePath, err := cloudLimitsCatalogCachePath()
	if err != nil {
		return CloudLimitsSyncReport{URL: display}, err
	}

	etag := loadCatalogETag(cachePath+".etag", display, defaultCloudLimitsSyncURL)

	body, newEtag, fromCache, err := fetchCloudLimitsBody(url, etag, cachePath)
	if err != nil {
		return CloudLimitsSyncReport{URL: display}, err
	}

	// The body is parsed BEFORE it is written, and a body that does not parse
	// is never written. The old code discarded this error and cached whatever
	// it received, so one bad response replaced the last good copy with
	// something cloudLimitsFromCatalog() cannot read — and that cache wins
	// over the embedded default, so every alias's limits silently fell back to
	// the built-in context size (`262144` → 1) until someone synced again
	// (2026-09-26 audit, third round).
	var f cloudLimitsCatalogFile
	if err := parseCloudLimitsCatalogFileChecked(body, &f); err != nil {
		if fromCache {
			return CloudLimitsSyncReport{URL: display}, fmt.Errorf("the cached cloud-limits catalog at %s is not readable as one (%v) — remove that file and run this again while online", cachePath, err)
		}
		return CloudLimitsSyncReport{URL: display}, fmt.Errorf("%s returned a body that is not a cloud-limits catalog (%v) — the cached copy was left untouched", display, err)
	}

	if !fromCache {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			return CloudLimitsSyncReport{URL: display}, err
		}
		if err := fileutil.WriteFileAtomic(cachePath, body, 0o600); err != nil {
			return CloudLimitsSyncReport{URL: display}, err
		}
		saveCatalogETag(cachePath+".etag", display, newEtag)
		saveCatalogCacheSource(cachePath, display)
	}

	return CloudLimitsSyncReport{URL: display, Count: len(f.Limits), FromCache: fromCache}, nil
}

func fetchCloudLimitsBody(url, etag, cachePath string) (body []byte, newEtag string, fromCache bool, err error) {
	if strings.HasPrefix(url, "file://") {
		b, rerr := os.ReadFile(strings.TrimPrefix(url, "file://"))
		return b, "", false, rerr
	}

	// The --url may carry the mirror's credential as userinfo
	// (https://KEY@mirror/catalog.json). The request keeps it — that is how
	// the mirror authenticates — but nothing written down or printed may:
	// every message below, and the request builder's own parse error
	// (NewRedactedRequest), use the redacted form. docs/ENTERPRISE.md names
	// the catalog caches as a place a credential must never reach.
	display := redactBaseURL(url)
	req, err := NewRedactedRequest(http.MethodGet, url, nil)
	if err != nil {
		return nil, "", false, err
	}
	if etag != "" {
		req.Header.Set("If-None-Match", etag)
	}
	resp, err := (&http.Client{Timeout: 10 * time.Second}).Do(req)
	if err != nil {
		// Offline: only a cache known to have come from THIS url is usable —
		// see catalogCacheSourcePath (2026-09-26 audit, tenth round).
		if catalogCacheSourceMatches(cachePath, display, defaultCloudLimitsSyncURL) {
			if b, rerr := os.ReadFile(cachePath); rerr == nil {
				return b, etag, true, nil
			}
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
		return nil, "", false, fmt.Errorf("%s: HTTP %d: %s", display, resp.StatusCode, strings.TrimSpace(string(errBody)))
	}

	b, err := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if err != nil {
		return nil, "", false, err
	}
	return b, resp.Header.Get("ETag"), false, nil
}
