package launch

// catalog_cache_etag.go — the ETag a synced catalog cache was stored with,
// bound to the URL that issued it.
//
// Both ProviderSync and CloudLimitsSync kept the ETag in a plain-text
// `…json.etag` sibling file, so the URL it came from was never written down
// anywhere. Two silent failures followed (2026-09-26 audit, third round):
//
//   - syncing once from a private mirror and then from the default catalog
//     sent the MIRROR's validator to GitHub. A 304 in reply then served the
//     local cache — the mirror's content — and the command reported success
//     with the other catalog's count and the new URL printed beside it. It
//     also handed a private endpoint's validator to a public one;
//   - a response that carried no ETag left the previous one in place, so the
//     next sync sent a validator for a body that had already been replaced.
//
// model_sync.go already stores its URL in the cache for exactly this reason;
// these two now record it beside the ETag.

import (
	"encoding/json"
	"os"
	"strings"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

// catalogETagBinding is the on-disk shape: which catalog this validator
// belongs to, and the validator itself.
type catalogETagBinding struct {
	URL  string `json:"url"`
	ETag string `json:"etag"`
}

// loadCatalogETag returns the stored validator for url, or "" when the stored
// one belongs to a different catalog.
//
// url is the REDACTED form on purpose: a credential must never be written to
// disk (docs/ENTERPRISE.md names the catalog caches), and the redacted form is
// stable across a key rotation, which is what makes it usable as an identity.
// legacyDefaultURL is the package's own default catalog, the only one a
// pre-binding (bare-word) etag file can belong to.
func loadCatalogETag(path, url, legacyDefaultURL string) string {
	b, err := os.ReadFile(path)
	if err != nil {
		return ""
	}
	var f catalogETagBinding
	if json.Unmarshal(b, &f) == nil {
		if strings.TrimSpace(f.URL) != url {
			return ""
		}
		return strings.TrimSpace(f.ETag)
	}
	// Written before the URL was recorded: a bare validator with nothing to
	// bind it to. Guessing wrong here means sending a foreign validator, so it
	// is only usable for the default catalog it was written for.
	if url != redactBaseURL(legacyDefaultURL) {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// catalogCacheSourcePath is where the URL a cached catalogue BODY came from is
// recorded, beside the body itself.
//
// The validator file above cannot serve as that record: it is deliberately
// REMOVED when a response carries no ETag, while the cached body stays. Without
// a record of its own, the offline fallback read whatever body happened to be
// in the single fixed cache path regardless of the URL asked for, so syncing
// once from a private mirror and then from the default catalogue while offline
// reported a successful sync of the DEFAULT catalogue with the MIRROR's
// contents and its count (2026-09-26 audit, tenth round — the transport-error
// sibling of the third-round 304 fix).
func catalogCacheSourcePath(cachePath string) string { return cachePath + ".url" }

// saveCatalogCacheSource records url as the source of the cached body at
// cachePath. url is the REDACTED form, for the same reason the validator
// binding uses it.
func saveCatalogCacheSource(cachePath, url string) {
	_ = fileutil.WriteFileAtomic(catalogCacheSourcePath(cachePath), []byte(url+"\n"), 0o600)
}

// catalogCacheSourceMatches reports whether the cached body is known to have
// come from url.
//
// A cache written before the source was recorded has no record at all; only
// the package's own default catalogue may claim one, because guessing otherwise
// would re-open the hole this closes. (A mirror user's first sync after
// upgrading refuses the stale cache and says to sync while online, which is the
// honest answer.)
func catalogCacheSourceMatches(cachePath, url, legacyDefaultURL string) bool {
	b, err := os.ReadFile(catalogCacheSourcePath(cachePath))
	if err != nil {
		return url == redactBaseURL(legacyDefaultURL)
	}
	return strings.TrimSpace(string(b)) == url
}

// saveCatalogETag records the validator for url, or REMOVES the file when the
// response carried none — keeping a validator for a body that has since been
// replaced is the second failure above.
func saveCatalogETag(path, url, etag string) {
	if strings.TrimSpace(etag) == "" {
		_ = os.Remove(path)
		return
	}
	b, err := json.Marshal(catalogETagBinding{URL: url, ETag: etag})
	if err != nil {
		return
	}
	_ = fileutil.WriteFileAtomic(path, b, 0o600)
}
