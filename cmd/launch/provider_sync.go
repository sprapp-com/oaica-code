package launch

// provider_sync.go — `oaica remote sync`: pull the hosted oaica overlay so a
// new provider, a new billing plan on an existing one, an endpoint fix or a
// corrected window reaches every user with one command, same shape as
// model_sync.go's `oaica model sync`.
//
// The file this syncs is providers/oaica.json — the overlay, not the ported
// models.dev catalog (that one is `oaica model catalog sync`, catalog_sync.go).
// The overlay is what we edit, and it is applied per key at read time
// (catalog_overlay.go), so a synced copy can correct what it names and can
// never blank a field it omits.

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/ollama/ollama/cmd/internal/fileutil"
)

const defaultProviderSyncURL = "https://raw.githubusercontent.com/sprapp-com/oaica-code/main/cmd/launch/providers/oaica.json"

// ProviderSyncReport is what ProviderSync returns for the caller to print.
type ProviderSyncReport struct {
	URL       string
	Count     int
	FromCache bool
}

// ProviderSync fetches the overlay at url (empty = defaultProviderSyncURL)
// and writes it to ~/.oaica/cache/providers/oaica.json, where oaicaOverlay()
// picks it up as a per-key override layer on top of the embedded copy.
// ETag-cached; offline falls back to the last good copy.
func ProviderSync(url string) (ProviderSyncReport, error) {
	if strings.TrimSpace(url) == "" {
		url = defaultProviderSyncURL
	}
	// display is the URL as the caller may print it. The request must keep any
	// mirror credential in the userinfo (`--url https://KEY@mirror/…` is how a
	// private mirror authenticates), but the report line `oaica provider sync`
	// echoes — and every error string — must not, exactly as ModelSync's report
	// already promises for the identical input (2026-09-26 audit).
	display := redactBaseURL(url)

	cachePath, err := oaicaOverlayCachePath()
	if err != nil {
		return ProviderSyncReport{URL: display}, err
	}

	etag := loadCatalogETag(cachePath+".etag", display, defaultProviderSyncURL)

	body, newEtag, fromCache, err := fetchProviderCatalogBody(url, etag)
	if err != nil {
		return ProviderSyncReport{URL: display}, err
	}

	// Parsed before it is written, and a body that does not parse is never
	// written: the synced cache overrides the embedded copy per key, so caching
	// one bad response (a captive-portal login page, a truncated transfer, a
	// proxy's HTML error) used to empty the provider catalogue for every later
	// run with the count reported as 0 (2026-09-26 audit, third round).
	f, ferr := parseProviderCatalogFileChecked(body)
	if ferr != nil {
		if fromCache {
			return ProviderSyncReport{URL: display}, fmt.Errorf("the cached provider catalog at %s is not readable as one (%v) — remove that file and run this again while online", cachePath, ferr)
		}
		return ProviderSyncReport{URL: display}, fmt.Errorf("%s returned a body that is not a provider catalog (%v) — the cached copy was left untouched", display, ferr)
	}
	if !fromCache {
		if err := os.MkdirAll(filepath.Dir(cachePath), 0o700); err != nil {
			return ProviderSyncReport{URL: display}, err
		}
		if err := fileutil.WriteFileAtomic(cachePath, body, 0o600); err != nil {
			return ProviderSyncReport{URL: display}, err
		}
		saveCatalogETag(cachePath+".etag", display, newEtag)
		saveCatalogCacheSource(cachePath, display)
	}

	return ProviderSyncReport{URL: display, Count: len(f.Providers), FromCache: fromCache}, nil
}

// fetchProviderCatalogBody is fetchCatalogBody (catalog_sync.go) for this
// catalogue's fixed cache path, plus one guard the shared helper cannot carry.
//
// fetchCatalogBody serves whatever body sits at the cache path it is handed,
// which is right for the models.dev catalogue (one path, one source) but not
// for this one: a cache written from a private mirror would answer a request
// for the default catalogue, and the report would name the URL that was ASKED
// FOR while handing back the mirror's contents (2026-09-26 audit, tenth round —
// the transport-error sibling of the third-round 304 fix). So the cached body
// is only accepted when it is known to have come from this URL
// (catalogCacheSourcePath). On a 304 that is already implied by the ETag
// binding, which is why the check is applied to the result of the fetch rather
// than inside it: whatever path was used to serve the cache, the source must
// vouch for the URL.
func fetchProviderCatalogBody(url, etag string) (body []byte, newEtag string, fromCache bool, err error) {
	cachePath, err := oaicaOverlayCachePath()
	if err != nil {
		return nil, "", false, err
	}
	body, newEtag, fromCache, err = fetchCatalogBody(url, etag, cachePath)
	if err != nil || !fromCache {
		return body, newEtag, fromCache, err
	}
	display := redactBaseURL(url)
	if !catalogCacheSourceMatches(cachePath, display, defaultProviderSyncURL) {
		return nil, "", false, fmt.Errorf("can't reach %s, and the catalogue cached at %s came from a different source — run this again while online", display, cachePath)
	}
	return body, newEtag, fromCache, nil
}

// parseProviderCatalogFileChecked decodes an overlay body — the provider
// catalogue this sync fetches — for the sync path, which must not cache a body
// it could not read.
//
// "It parses as JSON" is not the question. `{"message":"Not Found"}` — a proxy
// or CDN answering 200 for a path that does not exist — decodes cleanly into
// "this catalogue has zero providers", as do `{}`, `null` and any unrelated
// object. Each was written over the previous cache and reported as
// `synced 0 provider(s) ... (fresh)` with exit 0; because the synced copy is
// merged into the provider directory, every provider the document does not
// mention then kept whatever the embedded overlay said and every row only the
// cache could have added disappeared, and the next run offline re-read the
// garbage and called it a success again (2026-09-26 audit, sixth round).
//
// A document with no "providers" MEMBER is not an empty overlay, and
// "providers": null is not an empty LIST. This is parseModelCatalog's rule
// (model_sync.go), which its two siblings never got. The member is required
// even though the overlay has three sections: the file we ship and sync always
// carries its provider rows, and accepting a document that names none of them
// would re-open exactly the hole above for a body whose other members decode
// cleanly.
func parseProviderCatalogFileChecked(b []byte) (oaicaOverlayFile, error) {
	var members map[string]json.RawMessage
	if err := json.Unmarshal(b, &members); err != nil {
		return oaicaOverlayFile{}, err
	}
	if _, ok := members["providers"]; !ok {
		return oaicaOverlayFile{}, fmt.Errorf(`the document has no "providers" member, so it is not the oaica overlay (refusing to read it as an empty one)`)
	}
	var f oaicaOverlayFile
	if err := json.Unmarshal(b, &f); err != nil {
		return oaicaOverlayFile{}, err
	}
	if f.Providers == nil {
		return oaicaOverlayFile{}, fmt.Errorf(`"providers" is null, not a provider list`)
	}
	return f, nil
}
